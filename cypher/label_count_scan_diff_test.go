package cypher

// label_count_scan_diff_test.go — differential and correctness-boundary tests
// for the label-scan count pushdown (#2004).
//
// The pushdown replaces the serial NodeByLabelScan + EagerAggregation pipeline
// with a direct read of the label's live-node count (LabelCountScan) for the two
// provably-equivalent shapes `MATCH (p:Label) RETURN count(*)` and
// `count(p)`. These tests prove:
//
//   - the pushdown returns results IDENTICAL to the serial path and actually
//     engaged (a diagnostic build counter confirms it), and
//   - it DECLINES for every shape where the label-node count would be a wrong
//     answer — count(p.prop) (null-skipping), count(DISTINCT p), a WHERE filter,
//     a multi-label / property-map pattern (which the translator realises as a
//     Selection above the scan), an implicit grouping, and an OPTIONAL-MATCH-null
//     variable — while still returning the correct value on the serial path.
//
// The boundary tests are the guard the CORRECTNESS constraint demands: they fail
// if a future change wrongly pushes down a null-bearing or filtered shape.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildLabelCountGraph creates n :Item nodes (i in [0,n)), each carrying an
// integer "v"=i and "g"=i%3 property. Even-indexed nodes additionally carry a
// "w" property, and every fourth node additionally carries the :Special label,
// so the boundary tests can distinguish count(*) from count(p.w) (null-skipping)
// and from a multi-label pattern.
func buildLabelCountGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := range n {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "Item"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Int64Value(int64(i%3))); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := g.SetNodeProperty(k, "w", lpg.Int64Value(int64(i))); err != nil {
				t.Fatal(err)
			}
		}
		if i%4 == 0 {
			if err := g.SetNodeLabel(k, "Special"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return g
}

// TestLabelCount_Differential proves the label-scan count pushdown returns
// results identical to the serial path, and that it actually engaged.
func TestLabelCount_Differential(t *testing.T) {
	// 200 :Item nodes > psTestThreshold (50), so the label count pushdown engages.
	g := buildLabelCountGraph(t, 200)
	on, off := engines(g)

	cases := []struct {
		name, query, want string
	}{
		{"count-star", `MATCH (p:Item) RETURN count(*) AS c`, "c=200"},
		{"count-var", `MATCH (p:Item) RETURN count(p) AS c`, "c=200"},
		{"count-star-default-alias", `MATCH (p:Item) RETURN count(*)`, "count(*)=200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := labelCountScanBuildCount.Load()
			beforeWorker := parallelCountScanBuildCount.Load()
			gotOn := drainSortedPS(t, on, tc.query)
			engaged := labelCountScanBuildCount.Load() > before
			// The worker (full-scan) count path must NOT fire for a label scan.
			if parallelCountScanBuildCount.Load() != beforeWorker {
				t.Errorf("parallel worker count reduce unexpectedly engaged for %q", tc.query)
			}
			gotOff := drainSortedPS(t, off, tc.query)
			assertEqualRows(t, tc.query, gotOn, gotOff)
			if !engaged {
				t.Fatalf("expected label count pushdown to engage for %q, but it did not", tc.query)
			}
			if len(gotOn) != 1 || gotOn[0] != tc.want {
				t.Fatalf("%s = %v, want [%s]", tc.query, gotOn, tc.want)
			}
		})
	}
}

// TestLabelCount_DeclinesForUnsafeShapes proves the pushdown does NOT engage for
// any shape where the plain label-node count would be a wrong answer, and that
// the serial path returns the correct value for each. These are the correctness
// guards: each `want` differs from the bare :Item count (200) except where the
// count legitimately equals it, and the assertion that the pushdown declined
// fails if a future change over-eagerly substitutes the row count.
func TestLabelCount_DeclinesForUnsafeShapes(t *testing.T) {
	g := buildLabelCountGraph(t, 200)
	on, off := engines(g)

	cases := []struct {
		name, query, want string
	}{
		// count(p.w): only even-indexed :Item nodes carry "w" (100 of 200), so
		// count(p.w) MUST be 100, never the 200-row count. This is the null-skip
		// boundary — the single most important guard.
		{"count-property-with-nulls", `MATCH (p:Item) RETURN count(p.w) AS c`, "c=100"},
		// count(DISTINCT p) changes the result semantics; must not push down.
		{"count-distinct-var", `MATCH (p:Item) RETURN count(DISTINCT p) AS c`, "c=200"},
		// count(DISTINCT p.g) collapses to the 3 distinct group values.
		{"count-distinct-prop", `MATCH (p:Item) RETURN count(DISTINCT p.g) AS c`, "c=3"},
		// A WHERE between the scan and the aggregate changes which rows are
		// counted: v>=100 keeps 100 of 200.
		{"where-filter", `MATCH (p:Item) WHERE p.v >= 100 RETURN count(p) AS c`, "c=100"},
		// Multi-label pattern realises as Selection(:Special) above the scan, so
		// the child is not a bare label scan: every 4th node is :Special → 50.
		{"multi-label", `MATCH (p:Item:Special) RETURN count(p) AS c`, "c=50"},
		// Inline property map realises as a Selection above the scan: g==0 for
		// i in {0,3,...,198} → 67 nodes.
		{"property-map", `MATCH (p:Item {g: 0}) RETURN count(p) AS c`, "c=67"},
		// count(p.v) with an argument that is a property access, though every node
		// has "v" here (so the value equals 200), must still take the serial path.
		{"count-property-present", `MATCH (p:Item) RETURN count(p.v) AS c`, "c=200"},
		// A non-count aggregate never uses this fast path.
		{"sum", `MATCH (p:Item) RETURN sum(p.v) AS s`, "s=19900"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			beforeLabel := labelCountScanBuildCount.Load()
			beforeWorker := parallelCountScanBuildCount.Load()
			gotOn := drainSortedPS(t, on, tc.query)
			if labelCountScanBuildCount.Load() != beforeLabel {
				t.Errorf("label count pushdown unexpectedly engaged for %q", tc.query)
			}
			if parallelCountScanBuildCount.Load() != beforeWorker {
				t.Errorf("parallel worker count reduce unexpectedly engaged for %q", tc.query)
			}
			gotOff := drainSortedPS(t, off, tc.query)
			assertEqualRows(t, tc.query, gotOn, gotOff)
			if len(gotOn) != 1 || gotOn[0] != tc.want {
				t.Fatalf("%s = %v, want [%s]", tc.query, gotOn, tc.want)
			}
		})
	}
}

// TestLabelCount_GroupedDeclines proves an implicit grouping (a non-aggregate
// companion projection) turns the query into per-group counts and must not use
// the pushdown. The result is a set of grouped rows, not a single total.
func TestLabelCount_GroupedDeclines(t *testing.T) {
	g := buildLabelCountGraph(t, 30) // 30 :Item: g=0→10, g=1→10, g=2→10
	on, off := engines(g)

	const q = `MATCH (p:Item) RETURN p.g AS g, count(*) AS c`
	beforeLabel := labelCountScanBuildCount.Load()
	gotOn := drainSortedPS(t, on, q)
	if labelCountScanBuildCount.Load() != beforeLabel {
		t.Errorf("label count pushdown unexpectedly engaged for grouped %q", q)
	}
	gotOff := drainSortedPS(t, off, q)
	assertEqualRows(t, q, gotOn, gotOff)
	if len(gotOn) != 3 {
		t.Fatalf("grouped count = %v, want 3 groups", gotOn)
	}
}

// TestLabelCount_OptionalMatchNullDeclines proves that counting an
// OPTIONAL-MATCH-bound variable that can be null does NOT push down and returns
// the null-EXCLUDING count. This is the shape the openCypher TCK does not
// directly co-test (bare count over an OPTIONAL-null variable), so it is pinned
// here so the optimisation cannot regress it.
func TestLabelCount_OptionalMatchNullDeclines(t *testing.T) {
	// 100 :Item nodes; even-indexed ones KNOW an :Other node. count(b) over the
	// optional expansion must be 50 (odd-indexed items bind b to null, excluded),
	// never the 100-row count.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := range 100 {
		k := fmt.Sprintf("i%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "Item"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		o := fmt.Sprintf("o%d", i)
		if err := g.AddNode(o); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(o, "Other"); err != nil {
			t.Fatal(err)
		}
		// even-indexed Item -> Other
		if err := g.AddEdgeLabeled(fmt.Sprintf("i%d", i*2), o, 0, "KNOWS"); err != nil {
			t.Fatal(err)
		}
	}
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})
	off := NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})

	const q = `MATCH (a:Item) OPTIONAL MATCH (a)-[:KNOWS]->(b:Other) RETURN count(b) AS c`
	beforeLabel := labelCountScanBuildCount.Load()
	gotOn := drainSortedPS(t, on, q)
	if labelCountScanBuildCount.Load() != beforeLabel {
		t.Errorf("label count pushdown unexpectedly engaged for optional-null %q", q)
	}
	gotOff := drainSortedPS(t, off, q)
	assertEqualRows(t, q, gotOn, gotOff)
	if len(gotOn) != 1 || gotOn[0] != "c=50" {
		t.Fatalf("count(b) over optional match = %v, want [c=50]", gotOn)
	}
}

// TestLabelCount_SmallGraphStaysSerial proves the threshold gate keeps a small
// labelled graph on the serial path, so small-query latency is unaffected.
func TestLabelCount_SmallGraphStaysSerial(t *testing.T) {
	g := buildLabelCountGraph(t, psTestThreshold) // exactly at threshold → strict > fails
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})

	for _, q := range []string{
		`MATCH (p:Item) RETURN count(*) AS c`,
		`MATCH (p:Item) RETURN count(p) AS c`,
	} {
		before := labelCountScanBuildCount.Load()
		got := drainSortedPS(t, on, q)
		if labelCountScanBuildCount.Load() != before {
			t.Errorf("label count pushdown engaged at-threshold for %q (should stay serial)", q)
		}
		if len(got) != 1 || got[0] != fmt.Sprintf("c=%d", psTestThreshold) {
			t.Fatalf("%s = %v, want [c=%d]", q, got, psTestThreshold)
		}
	}
}

// TestLabelCount_UnknownLabel proves that counting an unknown label yields 0 on
// both paths. The pushdown may or may not engage (an unknown label resolves to a
// zero count directly); either way the answer is 0.
func TestLabelCount_UnknownLabel(t *testing.T) {
	g := buildLabelCountGraph(t, 200)
	on, off := engines(g)

	const q = `MATCH (p:Ghost) RETURN count(*) AS c`
	gotOn := drainSortedPS(t, on, q)
	gotOff := drainSortedPS(t, off, q)
	assertEqualRows(t, q, gotOn, gotOff)
	if len(gotOn) != 1 || gotOn[0] != "c=0" {
		t.Fatalf("count over unknown label = %v, want [c=0]", gotOn)
	}
}

// TestLabelCount_AfterDelete proves the pushed-down count reflects deletions:
// the label index strips deleted nodes, so the direct count equals the live
// count and stays bit-identical to the serial scan after a delete.
func TestLabelCount_AfterDelete(t *testing.T) {
	g := buildLabelCountGraph(t, 200)
	// Delete 40 :Item nodes directly on the graph (indices 0..39). RemoveNode
	// strips the node from every label bitmap, so the direct count drops to 160.
	for i := range 40 {
		g.RemoveNode(fmt.Sprintf("n%d", i))
	}
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})
	off := NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})

	const q = `MATCH (p:Item) RETURN count(p) AS c`
	before := labelCountScanBuildCount.Load()
	gotOn := drainSortedPS(t, on, q)
	if labelCountScanBuildCount.Load() == before {
		t.Fatalf("expected label count pushdown to engage for %q", q)
	}
	gotOff := drainSortedPS(t, off, q)
	assertEqualRows(t, q, gotOn, gotOff)
	if len(gotOn) != 1 || gotOn[0] != "c=160" {
		t.Fatalf("count after delete = %v, want [c=160]", gotOn)
	}
}
