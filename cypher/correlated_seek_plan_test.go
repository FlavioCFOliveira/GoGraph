package cypher_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// boundSeekFixture builds a labelled population with a string-keyed hash index
// over :P(name), plus a second label :Q used to bind keys from data rather than
// from a literal.
func boundSeekFixture(t *testing.T) *cypher.Engine {
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
	run(`UNWIND range(1, 200) AS i CREATE (:P {id: i, name: 'name-' + toString(i)})`)
	run(`CREATE (:Q {want: 'name-7'}), (:Q {want: 'name-9'})`)
	run(`CREATE INDEX p_name FOR (n:P) ON (n.name)`)
	return eng
}

// namesOf runs q and collects the "nm" column, sorted, so result sets can be
// compared irrespective of row order.
func namesOf(t *testing.T, eng *cypher.Engine, q string, params map[string]any) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, params)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	var out []string
	for res.Next() {
		rec := res.Record()
		v, ok := rec["nm"]
		if !ok {
			t.Fatalf("run %q: no column nm in %v", q, rec)
		}
		// StringValue.String() quotes; the raw string is wanted here so the
		// expectations in the tables below read as the data does.
		if sv, isStr := v.(expr.StringValue); isStr {
			out = append(out, string(sv))
			continue
		}
		out = append(out, fmt.Sprintf("%v", v))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	sort.Strings(out)
	return out
}

// TestBoundKeySeek_ExplainAccessPath is acceptance criterion (1) of task #2182: a
// key bound by WITH reaches the index instead of a full label scan.
//
// The UNWIND case was asserted to STILL scan, as the boundary of #2182: serving a
// key SET needs one index probe per distinct key with the postings OR-ed into a
// single bitmap, plus a cost gate, which was task #2183. That arrival is now
// visible here, exactly as intended — the UNWIND case plans a NodeByIndexSeekSet.
// It became visible when #2367 lowered rangeSeekMinLabelPopulation to 64 and this
// 200-node fixture rose above the floor.
//
// The operator check is EXACT rather than a substring, which is what the arrival
// exposed: `strings.Contains(plan, "NodeByIndexSeek")` is also satisfied by
// "NodeByIndexSeekSet", so the UNWIND case failed reporting a single-key seek that
// was not there. The two are separate access paths and the table distinguishes
// them.
func TestBoundKeySeek_ExplainAccessPath(t *testing.T) {
	eng := boundSeekFixture(t)
	params := map[string]expr.Value{"p": expr.StringValue("name-7")}

	cases := []struct {
		name  string
		query string
		// wantSeek is the SINGLE-KEY NodeByIndexSeek; wantSeekSet is the key-set
		// NodeByIndexSeekSet (#2183). They are different operators and a plan
		// carrying one must not be reported as carrying the other.
		wantSeek    bool
		wantSeekSet bool
	}{
		{
			name:     "literal key seeks (pre-existing behaviour, the control)",
			query:    `MATCH (a:P {name: 'name-7'}) RETURN a.name AS nm`,
			wantSeek: true,
		},
		{
			name:     "WITH-bound literal key seeks",
			query:    `WITH 'name-7' AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			wantSeek: true,
		},
		{
			name:     "WITH-bound parameter key seeks",
			query:    `WITH $p AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			wantSeek: true,
		},
		{
			name:     "WITH-bound key in a conjunction seeks",
			query:    `WITH 'name-7' AS k MATCH (a:P) WHERE a.name = k AND a.id > 0 RETURN a.name AS nm`,
			wantSeek: true,
		},
		{
			name:        "UNWIND-bound key takes the key-SET path (#2183), not the single-key seek",
			query:       `UNWIND ['name-7', 'name-9'] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			wantSeek:    false,
			wantSeekSet: true,
		},
		{
			name:     "data-bound key still scans: not row-invariant, must not be pushed",
			query:    `MATCH (q:Q) WITH q.want AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			wantSeek: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := eng.Explain(tc.query, params)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if got := planHasSingleKeySeek(plan); got != tc.wantSeek {
				t.Errorf("NodeByIndexSeek present = %v, want %v\nplan:\n%s", got, tc.wantSeek, plan)
			}
			if got := strings.Contains(plan, "NodeByIndexSeekSet"); got != tc.wantSeekSet {
				t.Errorf("NodeByIndexSeekSet present = %v, want %v\nplan:\n%s",
					got, tc.wantSeekSet, plan)
			}
		})
	}
}

// TestBoundKeySeek_ResultIdentity is the correctness half of #2182: for every key
// form, bound or inline, seekable or not, the rows must be exactly those the
// pre-rewrite scan-and-filter plan returned.
//
// Each case states its own expectation rather than comparing against a sibling
// query, so a rewrite that broke BOTH forms symmetrically could not pass.
func TestBoundKeySeek_ResultIdentity(t *testing.T) {
	eng := boundSeekFixture(t)

	cases := []struct {
		name   string
		query  string
		params map[string]any
		want   []string
	}{
		{
			name:  "WITH-bound literal returns the single match",
			query: `WITH 'name-7' AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7"},
		},
		{
			name:   "WITH-bound parameter returns the single match",
			query:  `WITH $p AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			params: map[string]any{"p": "name-7"},
			want:   []string{"name-7"},
		},
		{
			name:  "WITH-bound key with no match returns nothing",
			query: `WITH 'absent' AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  nil,
		},
		{
			name:  "conjunction keeps the non-key predicate: id > 0 admits the row",
			query: `WITH 'name-7' AS k MATCH (a:P) WHERE a.name = k AND a.id > 0 RETURN a.name AS nm`,
			want:  []string{"name-7"},
		},
		{
			// The pushed predicate must not be allowed to admit a row the
			// retained filter rejects: the seek narrows, the filter decides.
			name:  "conjunction keeps the non-key predicate: id > 1000 rejects the row",
			query: `WITH 'name-7' AS k MATCH (a:P) WHERE a.name = k AND a.id > 1000 RETURN a.name AS nm`,
			want:  nil,
		},
		{
			// Acceptance criterion (3). An integer key against a string-keyed
			// index is not merely unservable — under openCypher an integer never
			// equals a string, so the answer is no rows, and it must be reached
			// by falling back rather than by declining to answer.
			name:  "acceptance (2): type-incompatible integer key returns no rows",
			query: `WITH 7 AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  nil,
		},
		{
			name:  "acceptance (3): NULL literal key returns no rows",
			query: `WITH null AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  nil,
		},
		{
			name:  "UNWIND-bound keys return every match",
			query: `UNWIND ['name-7', 'name-9'] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7", "name-9"},
		},
		{
			// Duplicate keys must yield duplicate rows: one output row per
			// (input row, matched node) pair.
			name:  "duplicate UNWIND keys yield one row each",
			query: `UNWIND ['name-7', 'name-7'] AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7", "name-7"},
		},
		{
			name:  "data-bound key returns every match",
			query: `MATCH (q:Q) WITH q.want AS k MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7", "name-9"},
		},
		{
			// The key variable shadows nothing and the receiver is the scanned
			// node on both sides: a.name = a.name must not be mistaken for a
			// bound key and pushed as one.
			name:  "self-comparison is not a bound key",
			query: `WITH 1 AS k MATCH (a:P) WHERE a.name = a.name AND a.id = 7 RETURN a.name AS nm`,
			want:  []string{"name-7"},
		},
		{
			name:  "mirror operand order seeks the same row",
			query: `WITH 'name-7' AS k MATCH (a:P) WHERE k = a.name RETURN a.name AS nm`,
			want:  []string{"name-7"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := namesOf(t, eng, tc.query, tc.params)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestBoundKeySeek_OptionalMatchKeepsItsNullRow covers the case where predicate
// pushdown classically goes wrong.
//
// OPTIONAL MATCH must emit one NULL-padded row when nothing matches. A predicate
// pushed into the wrong side of an optional join turns that row into no row at
// all — a silent row-loss defect. The pass only matches *ir.Apply, and an optional
// join is *ir.OptionalApply, so it declines by construction; this asserts the
// observable consequence rather than the type check, so a future refactor that
// unified the two operators would fail here.
func TestBoundKeySeek_OptionalMatchKeepsItsNullRow(t *testing.T) {
	eng := boundSeekFixture(t)

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "match found",
			query: `WITH 'name-7' AS k OPTIONAL MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"name-7"},
		},
		{
			// The row must survive as NULL, not vanish.
			name:  "no match still yields one row",
			query: `WITH 'absent' AS k OPTIONAL MATCH (a:P {name: k}) RETURN a.name AS nm`,
			want:  []string{"null"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := namesOf(t, eng, tc.query, nil)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v (%d rows), want %v (%d rows)", got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestBoundKeySeek_MissingParameterKeyStillErrors records that a bound key whose
// parameter was never supplied remains a ParameterMissing error and does not
// become an empty result.
//
// The distinction matters to this rewrite. At plan time a missing parameter
// resolves to NULL (astLiteralToValue), which is what makes the seek decline
// during Explain. Execution never gets that far: the engine raises
// ParameterMissing first, as openCypher requires. So the rewrite must not be able
// to turn a missing parameter into a silent "no rows" by seeking on NULL.
func TestBoundKeySeek_MissingParameterKeyStillErrors(t *testing.T) {
	eng := boundSeekFixture(t)
	const q = `WITH $absent AS k MATCH (a:P {name: k}) RETURN a.name AS nm`

	res, err := eng.RunAny(context.Background(), q, nil)
	if err == nil {
		for res.Next() {
		}
		err = res.Err()
		if cerr := res.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		t.Fatal("want a ParameterMissing error for an unsupplied bound key, got none")
	}
	if !strings.Contains(err.Error(), "ParameterMissing") {
		t.Fatalf("want a ParameterMissing error, got %v", err)
	}

	// Explain must still succeed — it is a diagnostic and resolves the absent
	// parameter to NULL, which declines the seek rather than failing.
	if _, err := eng.Explain(q, nil); err != nil {
		t.Fatalf("Explain with an absent parameter: %v", err)
	}
}

// TestBoundKeySeek_ParamsAreNotFoldedIntoTheCachedPlan guards the one way this
// rewrite could corrupt results across invocations.
//
// The logical plan is cached by query text. A rewrite that substituted a
// parameter's VALUE would bake the first caller's key into the plan and serve it
// to every later caller — a wrong-results bug that no single-invocation test can
// see, because the first run is always correct. The pass moves the *ast.Parameter
// NODE instead, leaving resolution to the physical build.
//
// Running the same query three times with three different keys, the third
// repeating the first, catches both a stale fold and a first-run-only fold.
func TestBoundKeySeek_ParamsAreNotFoldedIntoTheCachedPlan(t *testing.T) {
	eng := boundSeekFixture(t)
	const q = `WITH $p AS k MATCH (a:P {name: k}) RETURN a.name AS nm`

	for _, want := range []string{"name-7", "name-9", "name-7"} {
		got := namesOf(t, eng, q, map[string]any{"p": want})
		if len(got) != 1 || got[0] != want {
			t.Fatalf("key %q: got %v, want [%s] — a parameter value may have been "+
				"folded into the cached plan", want, got, want)
		}
	}

	// A key that matches nothing, run last, must not resurrect an earlier fold.
	if got := namesOf(t, eng, q, map[string]any{"p": "absent"}); len(got) != 0 {
		t.Fatalf("absent key: got %v, want no rows", got)
	}
}

// TestBoundKeySeek_RewriteIsIdempotent asserts the pass leaves no site it would
// rewrite again. Re-running it on an already-rewritten plan must be a no-op,
// because the plan is cached and shared: a pass that re-applied itself would
// stack a Selection per Explain call and grow the plan without bound.
func TestBoundKeySeek_RewriteIsIdempotent(t *testing.T) {
	eng := boundSeekFixture(t)
	const q = `WITH 'name-7' AS k MATCH (a:P {name: k}) RETURN a.name AS nm`

	first, err := eng.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := eng.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("plan changed on call %d\nfirst:\n%s\nagain:\n%s", i+2, first, again)
		}
	}
}

// planHasSingleKeySeek reports whether plan contains the SINGLE-KEY
// NodeByIndexSeek operator, and not merely a name it is a prefix of.
//
// "NodeByIndexSeekSet" contains "NodeByIndexSeek", so a substring test cannot
// tell the two apart — and once the key-set path started firing on this fixture,
// that ambiguity reported a single-key seek in a plan that had none. The operator
// name is followed by a space, a newline or the end of the line whenever it is
// really the single-key one.
func planHasSingleKeySeek(plan string) bool {
	const op = "NodeByIndexSeek"
	for i := 0; ; {
		j := strings.Index(plan[i:], op)
		if j < 0 {
			return false
		}
		end := i + j + len(op)
		if end == len(plan) || plan[end] == ' ' || plan[end] == '\n' || plan[end] == '[' {
			return true
		}
		i = end
	}
}
