package cypher_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// The cost gate's two thresholds, restated here so the fixture's sizes are
// visibly chosen against them rather than by coincidence: a label must hold at
// least 1024 nodes, and the merged posting count must stay within 10 % of the
// label population.
const (
	seekSetPopulation = 4000 // > 1024, so the population floor is cleared
	seekSetBudget     = 400  // 10 % of the population
)

// seekSetFixture builds a :P label of seekSetPopulation nodes with a unique
// string name and a hash index over it, so each key's posting list holds exactly
// one node and a key count converts directly into a posting count.
func seekSetFixture(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	run := func(q string) {
		t.Helper()
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("setup close %q: %v", q, err)
		}
	}
	run(fmt.Sprintf(
		`UNWIND range(1, %d) AS i CREATE (:P {id: i, name: 'name-' + toString(i)})`, seekSetPopulation))
	// A shared name on two nodes, so a single key can carry a posting list of two
	// and duplicate-suppression has something to suppress.
	run(`CREATE (:P {id: -1, name: 'shared'}), (:P {id: -2, name: 'shared'})`)
	run(`CREATE INDEX p_name FOR (n:P) ON (n.name)`)
	return eng
}

// keyList renders a Cypher list literal of n distinct existing keys.
func keyList(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "'name-%d'", i)
	}
	b.WriteByte(']')
	return b.String()
}

// TestSeekSet_AccessPath is acceptance criterion (1): an UNWIND-bound key set
// reaches the index with ONE access path.
func TestSeekSet_AccessPath(t *testing.T) {
	eng := seekSetFixture(t)

	cases := []struct {
		name    string
		query   string
		wantOp  string
		absentO string
	}{
		{
			name:   "two-key set seeks",
			query:  `UNWIND ['name-7','name-9'] AS k MATCH (a:P {name: k}) RETURN a`,
			wantOp: "NodeByIndexSeekSet",
		},
		{
			name:   "duplicate keys still seek: the set is deduplicated before probing",
			query:  `UNWIND ['name-7','name-7'] AS k MATCH (a:P {name: k}) RETURN a`,
			wantOp: "NodeByIndexSeekSet",
		},
		{
			name:   "a type-incompatible key does not prevent the seek",
			query:  `UNWIND ['name-7', 7] AS k MATCH (a:P {name: k}) RETURN a`,
			wantOp: "NodeByIndexSeekSet",
		},
		{
			name:   "a NULL key does not prevent the seek",
			query:  `UNWIND ['name-7', null] AS k MATCH (a:P {name: k}) RETURN a`,
			wantOp: "NodeByIndexSeekSet",
		},
		{
			// A single key belongs to the ordinary seek: routing it through the set
			// operator would add a merge for no gain.
			name:    "a one-key set uses the single-key seek, not the set operator",
			query:   `UNWIND ['name-7'] AS k MATCH (a:P {name: k}) RETURN a`,
			wantOp:  "NodeByIndexSeek",
			absentO: "NodeByIndexSeekSet",
		},
		{
			// A runtime key set cannot be enumerated at plan time.
			name:    "a parameter key list is not served and scans",
			query:   `UNWIND $keys AS k MATCH (a:P {name: k}) RETURN a`,
			wantOp:  "NodeByLabelScan",
			absentO: "NodeByIndexSeek",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := eng.Explain(tc.query, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if !strings.Contains(plan, tc.wantOp) {
				t.Errorf("want %s in the plan\n%s", tc.wantOp, plan)
			}
			if tc.absentO != "" && strings.Contains(plan, tc.absentO) {
				t.Errorf("did not want %s in the plan\n%s", tc.absentO, plan)
			}
		})
	}
}

// TestSeekSet_CostGate is acceptance criterion (2): the gate declines when the key
// set covers a large fraction of the label, and the plan reverts to a scan.
//
// The spike measured why this is mandatory rather than tidy. The plan being
// replaced costs Θ(N + rows), not Θ(rows·N), because the Apply materialises the
// label scan once and joins the keys against it. The gain is therefore N/rows and
// vanishes as the key count approaches N — so an ungated rewrite would pay for one
// probe per key only to arrive at a posting list the size of the scan it replaced.
func TestSeekSet_CostGate(t *testing.T) {
	eng := seekSetFixture(t)

	cases := []struct {
		name     string
		keys     int
		wantSeek bool
	}{
		{name: "well inside the budget", keys: 2, wantSeek: true},
		{name: "just inside the budget", keys: seekSetBudget, wantSeek: true},
		{name: "one key past the budget", keys: seekSetBudget + 1, wantSeek: false},
		{name: "far past the budget", keys: seekSetBudget * 3, wantSeek: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := `UNWIND ` + keyList(tc.keys) + ` AS k MATCH (a:P {name: k}) RETURN a`
			plan, err := eng.Explain(q, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			gotSeek := strings.Contains(plan, "NodeByIndexSeekSet")
			if gotSeek != tc.wantSeek {
				t.Errorf("%d keys: seek = %v, want %v\n%s", tc.keys, gotSeek, tc.wantSeek, plan)
			}
			if !tc.wantSeek && !strings.Contains(plan, "NodeByLabelScan") {
				t.Errorf("%d keys: declining the seek must leave a scan\n%s", tc.keys, plan)
			}
		})
	}
}

// TestSeekSet_DeclinedHintIsDroppedNotEvaluated is the regression test for a
// defect this task introduced and then measured.
//
// The rewrite pushes the key set into the Apply's inner arm as a disjunction, so
// the seek can recognise it. When the cost gate declines, that disjunction is
// redundant — it is implied by the Selection retained above the Apply — but it is
// NOT free: evaluating a k-term disjunction over every node of the label costs
// Θ(k·N). Measured at N=20 000 with 2 001 keys, leaving it in place cost 2 952 ms
// against 19.4 ms with it dropped, and ~12 ms for the plan that existed before the
// rewrite. A gate that "declines" into a plan 150× slower than the one it was
// meant to preserve is not a gate.
//
// The fix drops an unclaimed hint at build time. This test pins it structurally
// rather than by timing: the declined plan must contain exactly ONE Selection —
// the retained one — so the hint is provably absent, which makes the declined plan
// identical in shape to the pre-rewrite plan.
func TestSeekSet_DeclinedHintIsDroppedNotEvaluated(t *testing.T) {
	eng := seekSetFixture(t)

	declined := `UNWIND ` + keyList(seekSetBudget+1) + ` AS k MATCH (a:P {name: k}) RETURN a`
	plan, err := eng.Explain(declined, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := strings.Count(plan, "Selection"); got != 1 {
		t.Errorf("a declined key set must leave exactly 1 Selection (the retained one), got %d — "+
			"the pushed hint is still in the plan and will be evaluated per node\n%s", got, plan)
	}

	// The same query with a claimable key set keeps its retained Selection too, so
	// the count above is not merely counting the hint's absence everywhere.
	claimed := `UNWIND ['name-1','name-2'] AS k MATCH (a:P {name: k}) RETURN a`
	plan, err = eng.Explain(claimed, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := strings.Count(plan, "Selection"); got != 1 {
		t.Errorf("a claimed key set renders as the seek plus the retained Selection, got %d Selections\n%s", got, plan)
	}
	if !strings.Contains(plan, "NodeByIndexSeekSet") {
		t.Errorf("want the key-set seek\n%s", plan)
	}
}

// TestSeekSet_PopulationFloor asserts the other half of the gate: below the
// population floor no seek is attempted, because a scan of a few hundred nodes is
// a few microseconds and an index descent cannot win.
func TestSeekSet_PopulationFloor(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		// 500 nodes: under the 1024 floor.
		`UNWIND range(1, 500) AS i CREATE (:S {name: 'n-' + toString(i)})`,
		`CREATE INDEX s_name FOR (n:S) ON (n.name)`,
	} {
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		for res.Next() {
		}
		_ = res.Close()
	}

	plan, err := eng.Explain(`UNWIND ['n-1','n-2'] AS k MATCH (a:S {name: k}) RETURN a`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if strings.Contains(plan, "NodeByIndexSeekSet") {
		t.Errorf("a 500-node label is below the population floor; want a scan\n%s", plan)
	}
}

// TestSeekSet_ResultIdentity is acceptance criterion (3): result-identity
// established DIFFERENTIALLY against the scan plan.
//
// Each case runs the same query twice — once where the seek fires and once on an
// engine whose label is too small for the gate to allow it, so the second run is
// the scan plan by construction — and requires identical rows. This is stronger
// than asserting expected rows: it cannot pass by both plans being wrong the same
// way only if the expectation is also checked, which it is.
func TestSeekSet_ResultIdentity(t *testing.T) {
	seekEng := seekSetFixture(t)

	// The differential partner: the same data and index, but a label population
	// under the floor, so no seek can fire and every query takes the scan path.
	scanEng := func() *cypher.Engine {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		e := cypher.NewEngine(g)
		for _, q := range []string{
			`UNWIND range(1, 20) AS i CREATE (:P {id: i, name: 'name-' + toString(i)})`,
			`CREATE (:P {id: -1, name: 'shared'}), (:P {id: -2, name: 'shared'})`,
			`CREATE INDEX p_name FOR (n:P) ON (n.name)`,
		} {
			res, err := e.RunAny(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("scan-engine setup: %v", err)
			}
			for res.Next() {
			}
			_ = res.Close()
		}
		return e
	}()

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "distinct keys",
			query: `UNWIND ['name-7','name-9'] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7", "name-9"},
		},
		{
			// One row per (input row, matched node) pair: the duplicate key must
			// NOT be collapsed even though the probe deduplicates it.
			name:  "duplicate row keys each emit a row",
			query: `UNWIND ['name-7','name-7'] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7", "name-7"},
		},
		{
			// Two nodes share this name, so one key yields two rows; with the key
			// twice, four.
			name:  "a multi-node posting list crosses with duplicate keys",
			query: `UNWIND ['shared','shared'] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"shared", "shared", "shared", "shared"},
		},
		{
			name:  "type-mismatch key contributes nothing",
			query: `UNWIND ['name-7', 7] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7"},
		},
		{
			name:  "NULL key contributes nothing",
			query: `UNWIND ['name-7', null] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7"},
		},
		{
			// Cross-type numeric equality must not be served by the string-keyed
			// hash index: 7 and 7.0 are equal under openCypher, and a string index
			// cannot express that.
			name:  "cross-type numeric keys match nothing on a string index",
			query: `UNWIND [7, 7.0] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  nil,
		},
		{
			name:  "a key with no match contributes nothing",
			query: `UNWIND ['name-7','absent'] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7"},
		},
		{
			name:  "every key absent yields no rows",
			query: `UNWIND ['absent-1','absent-2'] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  nil,
		},
		{
			// The retained filter must still reject rows the seek admits.
			name:  "a conjoined non-key predicate still filters",
			query: `UNWIND ['name-7','name-9'] AS k MATCH (a:P) WHERE a.name = k AND a.id > 8 RETURN a.name AS nm`,
			want:  []string{"name-9"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seekRows := namesOf(t, seekEng, tc.query, nil)
			scanRows := namesOf(t, scanEng, tc.query, nil)

			if !equalStrings(seekRows, tc.want) {
				t.Errorf("seek plan: got %v, want %v", seekRows, tc.want)
			}
			if !equalStrings(scanRows, tc.want) {
				t.Errorf("scan plan: got %v, want %v", scanRows, tc.want)
			}
			if !equalStrings(seekRows, scanRows) {
				t.Errorf("seek and scan plans disagree: %v vs %v", seekRows, scanRows)
			}
		})
	}
}

// TestSeekSet_DeclinedGateIsResultIdentical checks the boundary the cost gate
// creates: a key set one past the budget takes the scan, and must return exactly
// what the same set one under the budget would have returned by seeking.
//
// This is the gate's own correctness test. A gate that changed results as it
// switched paths would be worse than no gate.
func TestSeekSet_DeclinedGateIsResultIdentical(t *testing.T) {
	eng := seekSetFixture(t)

	for _, keys := range []int{seekSetBudget, seekSetBudget + 1} {
		q := `UNWIND ` + keyList(keys) + ` AS k MATCH (a:P {name: k}) RETURN a.name AS nm`
		got := namesOf(t, eng, q, nil)
		if len(got) != keys {
			t.Fatalf("%d keys: got %d rows, want %d", keys, len(got), keys)
		}
		want := make([]string, 0, keys)
		for i := 1; i <= keys; i++ {
			want = append(want, fmt.Sprintf("name-%d", i))
		}
		sort.Strings(want)
		if !equalStrings(got, want) {
			t.Fatalf("%d keys: rows differ from the expected key set", keys)
		}
	}
}

// TestSeekSet_MultiLabelPatternIsNotServed records a PRE-EXISTING gap this task
// measured rather than introduced, and pins the current behaviour so the gap's
// eventual closure is a deliberate change to this test.
//
// Task #2183's acceptance criterion (4) asked that MATCH (a:A:B {k: nm}) plan one
// bitmap intersection rather than a seek feeding a filter. That criterion's
// premise does not hold: the planner has NO multi-label bitmap-intersection access
// path. label.Index.Intersect is variadic but the Cypher layer calls it with a
// single label (see lpgLabelResolver.ResolveLabelBitmap), and the implemented
// multi-label strategy is the min-cardinality anchor scan of #2077 — scan the
// smallest label, re-check the rest as a filter.
//
// The consequence is broader than the key set: for a multi-label pattern, NEITHER
// the key-set seek NOR the ordinary single-key literal seek fires, because the
// Selection's child is another Selection (the second label's predicate) rather
// than a scan leaf. So there is nothing here for a key-set seek to compose with,
// and the composition cannot be built until the intersection access path is.
func TestSeekSet_MultiLabelPatternIsNotServed(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		`UNWIND range(1, 4000) AS i CREATE (:A:B {name: 'ab-' + toString(i)})`,
		`CREATE INDEX ab_name FOR (n:A) ON (n.name)`,
	} {
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		for res.Next() {
		}
		_ = res.Close()
	}

	for _, tc := range []struct{ name, query string }{
		{"key set", `UNWIND ['ab-7','ab-9'] AS k MATCH (a:A:B {name: k}) RETURN a`},
		{"single literal key", `MATCH (a:A:B {name: 'ab-7'}) RETURN a`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := eng.Explain(tc.query, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if strings.Contains(plan, "NodeByIndexSeek") {
				t.Fatalf("a multi-label pattern unexpectedly reached the index — the gap "+
					"this test pins may have been closed; update it deliberately\n%s", plan)
			}
			if !strings.Contains(plan, "NodeByLabelScan") {
				t.Errorf("want the min-label scan path\n%s", plan)
			}
		})
	}

	// Whatever the access path, the rows must be right.
	got := namesOf(t, eng, `UNWIND ['ab-7','ab-9'] AS k MATCH (a:A:B {name: k}) RETURN a.name AS nm`, nil)
	if !equalStrings(got, []string{"ab-7", "ab-9"}) {
		t.Errorf("got %v, want [ab-7 ab-9]", got)
	}
}
