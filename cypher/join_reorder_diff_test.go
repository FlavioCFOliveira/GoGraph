package cypher

// join_reorder_diff_test.go — differential tests for the disjoint-component
// ordering peephole (#2091).
//
// Each representative query runs on a reorder-ENABLED engine and a reorder-
// DISABLED engine over the same graph. For unordered queries the result is a
// bag, so both sides are sorted to canonical form before comparison; for queries
// with a downstream total ORDER BY the exact row SEQUENCE is compared (the sort
// masks any emission-order change, so ON and OFF must be byte-identical). A
// separate assertion on joinReorderBuildCount confirms the swap fired for the
// cases that must reorder and did NOT fire for the suppression cases (a bare
// LIMIT, a collect()), so the test cannot silently pass by never triggering.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildReorderGraph creates a graph with, for each (label → count) entry,
// `count` nodes carrying that single label and a unique integer "k" property.
// Node keys are globally unique across labels so every node is distinct.
func buildReorderGraph(t *testing.T, spec map[string]int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	// Deterministic label order so label-id interning is reproducible.
	labels := make([]string, 0, len(spec))
	for l := range spec {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	k := 0
	for _, lbl := range labels {
		for i := 0; i < spec[lbl]; i++ {
			key := fmt.Sprintf("n%d", k)
			if err := g.AddNode(key); err != nil {
				t.Fatal(err)
			}
			if err := g.SetNodeProperty(key, "k", lpg.Int64Value(int64(k))); err != nil {
				t.Fatal(err)
			}
			if err := g.SetNodeLabel(key, lbl); err != nil {
				t.Fatal(err)
			}
			k++
		}
	}
	return g
}

// drainRows runs q and returns each row rendered as a canonical string, in
// EMISSION order (no sorting).
func drainRows(t *testing.T, e *Engine, q string) []string {
	t.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	cols := res.Columns()
	var out []string
	for res.Next() {
		rec := res.Record()
		var sb []byte
		for i, c := range cols {
			if i > 0 {
				sb = append(sb, '|')
			}
			sb = append(sb, fmt.Sprintf("%s=%v", c, rec[c])...)
		}
		out = append(out, string(sb))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
	return out
}

// assertReorderIdentical runs q on a reorder-ENABLED and a reorder-DISABLED
// engine and asserts identical results: as a sorted bag when ordered==false, as
// an exact sequence when ordered==true. wantTrigger asserts whether the enabled
// run actually swapped a Cartesian's arms.
func assertReorderIdentical(t *testing.T, g *lpg.Graph[string, float64], q string, wantTrigger, ordered bool) {
	t.Helper()
	on := NewEngine(g)
	off := NewEngineWithOptions(g, EngineOptions{DisableJoinReorder: true})

	before := joinReorderBuildCount.Load()
	gotOn := drainRows(t, on, q)
	triggered := joinReorderBuildCount.Load() > before
	gotOff := drainRows(t, off, q)

	if !ordered {
		sort.Strings(gotOn)
		sort.Strings(gotOff)
	}
	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch for %q: reorder=%d default=%d", q, len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			kind := "bag"
			if ordered {
				kind = "sequence"
			}
			t.Fatalf("%s row %d differs for %q:\n  reorder = %s\n  default = %s", kind, i, q, gotOn[i], gotOff[i])
		}
	}
	if wantTrigger && !triggered {
		t.Fatalf("expected the disjoint reorder to fire for %q, but it did not", q)
	}
	if !wantTrigger && triggered {
		t.Fatalf("did NOT expect the disjoint reorder to fire for %q, but it did", q)
	}
}

func TestJoinReorder_Differential_TwoComponents_Skewed(t *testing.T) {
	// Written order drives the LARGER label first (Big outer, Small inner), so the
	// peephole swaps to drive Small. Big=80, Small=4.
	g := buildReorderGraph(t, map[string]int{"Big": 80, "Small": 4})
	assertReorderIdentical(t, g, "MATCH (a:Big), (b:Small) RETURN a.k AS ak, b.k AS bk", true, false)
}

func TestJoinReorder_Differential_TwoComponents_AlreadySmallerFirst(t *testing.T) {
	// Written order already drives the smaller label first (Small outer, Big
	// inner): no swap, still result-identical.
	g := buildReorderGraph(t, map[string]int{"Big": 80, "Small": 4})
	assertReorderIdentical(t, g, "MATCH (a:Small), (b:Big) RETURN a.k AS ak, b.k AS bk", false, false)
}

func TestJoinReorder_Differential_TwoComponents_Balanced(t *testing.T) {
	// Equal cardinalities → no strict improvement → no swap, still identical.
	g := buildReorderGraph(t, map[string]int{"L": 30, "R": 30})
	assertReorderIdentical(t, g, "MATCH (a:L), (b:R) RETURN a.k AS ak, b.k AS bk", false, false)
}

func TestJoinReorder_Differential_TwoComponents_AllNodesArm(t *testing.T) {
	// One arm is an all-nodes scan (b), whose cardinality is the exact live total.
	// Written order drives Big (80) then all-nodes (84); Big < 84, so the written
	// order already drives the smaller side → no swap. Result identity regardless.
	g := buildReorderGraph(t, map[string]int{"Big": 80, "Small": 4})
	assertReorderIdentical(t, g, "MATCH (a:Big), (b) RETURN a.k AS ak, b.k AS bk", false, false)
}

func TestJoinReorder_Differential_TwoComponents_AllNodesArmSwaps(t *testing.T) {
	// All-nodes arm drives first (84 rows), small label inner (4). 4 < 84 → swap.
	g := buildReorderGraph(t, map[string]int{"Big": 80, "Small": 4})
	assertReorderIdentical(t, g, "MATCH (a), (b:Small) RETURN a.k AS ak, b.k AS bk", true, false)
}

func TestJoinReorder_Differential_ThreeComponents_Skewed(t *testing.T) {
	// Big=40, Med=12, Small=3. Written order Apply(Apply(Big,Med),Small): the inner
	// pair swaps (Med<Big) and the top pair swaps (Small < Big*Med), so two swaps.
	g := buildReorderGraph(t, map[string]int{"Big": 40, "Med": 12, "Small": 3})
	assertReorderIdentical(t, g,
		"MATCH (a:Big), (b:Med), (c:Small) RETURN a.k AS ak, b.k AS bk, c.k AS ck", true, false)
}

func TestJoinReorder_Differential_ThreeComponents_NoSwap(t *testing.T) {
	// Written order Apply(Apply(P,Q),S) with P=2,Q=2,S=10. The inner pair is equal
	// (no swap), and the top pair's outer is the P*Q=4-row product, which is
	// smaller than S=10, so the written order already drives the smaller side — no
	// swap anywhere. Result identity regardless.
	g := buildReorderGraph(t, map[string]int{"P": 2, "Q": 2, "S": 10})
	assertReorderIdentical(t, g,
		"MATCH (a:P), (b:Q), (c:S) RETURN a.k AS ak, b.k AS bk, c.k AS ck", false, false)
}

func TestJoinReorder_Differential_TotalOrderBy_Safe(t *testing.T) {
	// A total ORDER BY (id(a), id(b) covering the projected node columns) is a
	// RESET enabler: the reorder is order-safe and FIRES, and the sort masks the
	// emission-order change so the exact row SEQUENCE is identical ON vs OFF.
	g := buildReorderGraph(t, map[string]int{"Big": 60, "Small": 5})
	assertReorderIdentical(t, g,
		"MATCH (a:Big), (b:Small) RETURN a, b ORDER BY id(a), id(b)", true, true)
}

func TestJoinReorder_Differential_BareLimit_Suppressed(t *testing.T) {
	// A bare LIMIT without a dominating total sort selects WHICH rows survive, so
	// the reorder MUST be suppressed. Both engines return the same first-k bag.
	g := buildReorderGraph(t, map[string]int{"Big": 80, "Small": 4})
	assertReorderIdentical(t, g,
		"MATCH (a:Big), (b:Small) RETURN a.k AS ak, b.k AS bk LIMIT 7", false, false)
}

func TestJoinReorder_Differential_FeedingCollect_Suppressed(t *testing.T) {
	// collect() builds a list in arrival order, a value trap under bag comparison,
	// so the reorder MUST be suppressed. The single collected-list row is identical.
	g := buildReorderGraph(t, map[string]int{"Big": 40, "Small": 3})
	assertReorderIdentical(t, g,
		"MATCH (a:Big), (b:Small) RETURN collect(a.k) AS ks", false, false)
}

func TestJoinReorder_Differential_Skip_Suppressed(t *testing.T) {
	// A bare SKIP is the SKIP-side analogue of the bare LIMIT case: it drops the
	// first rows in arrival order, so the reorder MUST be suppressed.
	g := buildReorderGraph(t, map[string]int{"Big": 60, "Small": 4})
	assertReorderIdentical(t, g,
		"MATCH (a:Big), (b:Small) RETURN a.k AS ak, b.k AS bk SKIP 5 LIMIT 5", false, false)
}
