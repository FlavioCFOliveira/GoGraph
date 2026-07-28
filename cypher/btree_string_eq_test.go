package cypher_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newBtreeStringEngine seeds n :K nodes carrying the string key sk, plus any
// extra rows the caller supplies, and creates a BTREE-only index on sk.
//
// A btree is the point: a string equality already reaches the default (hash)
// index, so the gap this file guards bites only a user who explicitly asked for
// a btree on a string property — reasonable when the same property also serves
// range predicates (rmp #2231).
func newBtreeStringEngine(t *testing.T, n int, extra ...map[string]any) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	run := func(cy string, params map[string]any) {
		t.Helper()
		res, err := eng.RunInTxAny(ctx, cy, params)
		if err != nil {
			t.Fatalf("%s: %v", cy, err)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("%s: close: %v", cy, cerr)
		}
	}
	for i := 0; i < n; i++ {
		run("CREATE (:K {sk: $s})", map[string]any{"s": fmt.Sprintf("s%d", i)})
	}
	for _, e := range extra {
		run("CREATE (:K {sk: $s})", e)
	}
	// BTREE explicitly: the default index type is the hash, which already served
	// equality, so a default index would not exercise the rewrite at all.
	run("CREATE INDEX k_sk FOR (n:K) ON (n.sk) OPTIONS {indexType: 'btree'}", nil)
	return eng
}

// rowsFor runs a read query and returns the sk values it produced, sorted.
func rowsFor(t *testing.T, eng *cypher.Engine, cy string, params map[string]any) []string {
	t.Helper()
	bound, err := cypher.BindParams(params)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	res, err := eng.Run(context.Background(), cy, bound)
	if err != nil {
		t.Fatalf("run %q: %v", cy, err)
	}
	var out []string
	for res.Next() {
		out = append(out, fmt.Sprint(res.Record()["k"]))
	}
	if rerr := res.Err(); rerr != nil {
		t.Fatalf("run %q: drain: %v", cy, rerr)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("run %q: close: %v", cy, cerr)
	}
	sort.Strings(out)
	return out
}

// TestBTreeStringEq_UsesTheIndex is the regression gate on the rewrite itself:
// a string equality against a btree-ONLY index must seek, not scan.
//
// It asserts on the PHYSICAL plan rather than on a timing, so it cannot pass by
// luck on a fast machine: before the fix the plan was a bare NodeByLabelScan and
// the query cost 4.27 ms with 59 831 allocs/op at 20 000 nodes, the allocation
// count tracking the node population.
func TestBTreeStringEq_UsesTheIndex(t *testing.T) {
	t.Parallel()
	// Above the selectivity gate's floor. The rewrite deliberately does NOT force
	// a seek: it hands the degenerate range to the same rangeCountWins gate the
	// numeric path uses, which keeps the scan for a label too small for an index
	// to pay for itself (the engine seeks from ~1024 nodes upward). A test seeded
	// below that floor would assert the gate away rather than the rewrite.
	eng := newBtreeStringEngine(t, 2000)

	plan, err := eng.Explain("MATCH (a:K {sk: 's250'}) RETURN a", nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexRangeScan") {
		t.Errorf("a string equality on a btree index must seek the index as the degenerate "+
			"closed range, not scan the label:\n%s", plan)
	}
	// The residual Filter is what makes the superset safe; losing it would be a
	// correctness change, not an optimisation.
	if !strings.Contains(plan, "Filter") {
		t.Errorf("the range seek's residual Filter must be retained so the seek can only "+
			"narrow what is examined, never change what is admitted:\n%s", plan)
	}
}

// TestBTreeStringEq_SeekAndScanAgree is the differential gate (rmp #2231 AC 4):
// the seek must return exactly what the scan returns.
//
// The scan arm is produced by defeating the rewrite rather than by disabling the
// index: `WHERE a.sk = $k` with the key as a PARAMETER is not a plain string
// literal, so extractSingleStringCmp declines it and the plan falls back to a
// label scan plus filter. Both arms therefore run against the SAME engine and the
// SAME index, and any divergence is the rewrite's fault alone — the discipline a
// differential needs to be worth anything, since two arms sharing broken code go
// green over a real defect.
func TestBTreeStringEq_SeekAndScanAgree(t *testing.T) {
	t.Parallel()

	// Keys chosen to probe the boundaries the byte-ordered btree has to get right.
	const (
		empty    = ""
		multi    = "ключ-日本語-🔑" // multi-byte UTF-8, several encodings wide
		prefixed = "s1"         // a strict prefix of other keys (s10, s100, …)
	)
	// Seeded ABOVE the selectivity gate's floor, and the two arms are then proved
	// to take DIFFERENT plans below. Without both, this differential would be
	// worthless: at a few hundred nodes the gate keeps the literal form on a scan
	// too, so the test would compare the scan against itself and pass over any
	// defect the rewrite introduced.
	eng := newBtreeStringEngine(t, 2000,
		map[string]any{"s": empty},
		map[string]any{"s": multi},
	)

	seekPlan, err := eng.Explain(`MATCH (a:K {sk: "s42"}) RETURN a.sk AS k`, nil)
	if err != nil {
		t.Fatalf("Explain seek arm: %v", err)
	}
	scanBound, err := cypher.BindParams(map[string]any{"k": "s42"})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	scanPlan, err := eng.Explain("MATCH (a:K) WHERE a.sk = $k RETURN a.sk AS k", scanBound)
	if err != nil {
		t.Fatalf("Explain scan arm: %v", err)
	}
	if !strings.Contains(seekPlan, "NodeByIndexRangeScan") {
		t.Fatalf("the seek arm does not seek, so this differential compares nothing:\n%s", seekPlan)
	}
	if strings.Contains(scanPlan, "NodeByIndexRangeScan") {
		t.Fatalf("the scan arm also seeks, so this differential compares nothing:\n%s", scanPlan)
	}

	for _, key := range []string{
		"s42",       // present, ordinary
		prefixed,    // present, and a prefix of many others
		empty,       // the minimum key
		multi,       // multi-byte UTF-8
		"absent-xy", // absent
		"s999999",   // absent, beyond the seeded range
	} {
		t.Run("key="+strings.ReplaceAll(key, " ", "_"), func(t *testing.T) {
			t.Parallel()
			// Seek arm: literal key, so the rewrite fires.
			seek := rowsFor(t, eng,
				fmt.Sprintf("MATCH (a:K {sk: %q}) RETURN a.sk AS k", key), nil)
			// Scan arm: parameterised key, which the rewrite declines.
			scan := rowsFor(t, eng,
				"MATCH (a:K) WHERE a.sk = $k RETURN a.sk AS k", map[string]any{"k": key})

			if len(seek) != len(scan) {
				t.Fatalf("seek returned %d rows, scan returned %d for key %q\nseek=%v\nscan=%v",
					len(seek), len(scan), key, seek, scan)
			}
			for i := range seek {
				if seek[i] != scan[i] {
					t.Fatalf("seek and scan disagree at row %d for key %q: %q vs %q",
						i, key, seek[i], scan[i])
				}
			}
		})
	}
}

// TestBTreeStringEq_TemporalIsNotSeekMatchable pins the hazard the rewrite must
// not reintroduce (rmp #2231 AC 3).
//
// A Cypher temporal is stored as an SOH-tagged string, so its raw encoded form
// IS a string at the storage layer. projectStringPropValue therefore refuses
// those encodings for both the hash and the btree index, because a temporal is
// not equal to any plain Cypher string and indexing its raw form would let a
// pathological string literal seek-match a node the scan+filter path rejects.
//
// The rewrite inherits that exclusion rather than restating it — it seeks the
// same btree, whose keys never contain a temporal — and this test is what keeps
// that true. A future change that indexed temporals for ordering would fail here
// before it could produce a wrong row.
func TestBTreeStringEq_TemporalIsNotSeekMatchable(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	mustRun := func(cy string, params map[string]any) {
		t.Helper()
		res, err := eng.RunInTxAny(ctx, cy, params)
		if err != nil {
			t.Fatalf("%s: %v", cy, err)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("%s: close: %v", cy, cerr)
		}
	}

	// A node whose indexed property is a DATE, alongside ordinary string rows so
	// the index is not degenerate.
	for i := 0; i < 50; i++ {
		mustRun("CREATE (:T {p: $s})", map[string]any{"s": fmt.Sprintf("v%d", i)})
	}
	mustRun("CREATE (:T {p: date('2020-01-02')})", nil)
	mustRun("CREATE INDEX t_p FOR (n:T) ON (n.p) OPTIONS {indexType: 'btree'}", nil)

	// Every plausible raw encoding of the temporal: the tag bytes Cypher uses for
	// its six temporal types, each followed by the ISO form. None may match.
	for tag := 1; tag <= 6; tag++ {
		raw := string(rune(tag)) + "2020-01-02"
		got := rowsFor(t, eng, "MATCH (a:T) WHERE a.p = $k RETURN a.p AS k",
			map[string]any{"k": raw})
		if len(got) != 0 {
			t.Errorf("a string literal equal to a temporal's raw encoding (tag %d) matched %d "+
				"row(s); a temporal is not equal to any plain string: %v", tag, len(got), got)
		}
	}

	// The plain ISO string must not match the date either — same reason, and the
	// literal form is the one the rewrite handles.
	if got := rowsFor(t, eng, `MATCH (a:T {p: "2020-01-02"}) RETURN a.p AS k`, nil); len(got) != 0 {
		t.Errorf("the ISO string form matched the DATE-valued node (%d rows): %v", len(got), got)
	}

	// Control: the date still matches itself, so the exclusion has not made the
	// property unqueryable.
	if got := rowsFor(t, eng, "MATCH (a:T) WHERE a.p = date('2020-01-02') RETURN a.p AS k", nil); len(got) != 1 {
		t.Errorf("the DATE-valued node must still be found by a date comparison; got %d rows: %v",
			len(got), got)
	}
}
