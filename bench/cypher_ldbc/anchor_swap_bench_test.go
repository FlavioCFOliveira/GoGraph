package cypher_ldbc_test

// anchor_swap_bench_test.go — benchmark demonstrating the single-edge
// anchor-swap peephole (#2090) win: a written DirIn expand `(a:Hub)<-[:R]-(b:Leaf)`
// anchored on the hub (walking the hub's huge R in-degree) is re-rooted onto the
// leaf (walking the leaf's tiny R out-degree), flipping the reverse DirIn expand
// to a forward DirOut expand.
//
// The fixture is the adversarial hub of docs/reordering-design.md §5.1: one :Hub
// node with a large R in-degree (asBenchOtherPop incoming edges from :Other
// nodes), and asBenchLeafPop :Leaf nodes exactly ONE of which has a single R
// out-edge into the hub. The written plan scans the hub (N=1) and walks its
// ~asBenchOtherPop+1 incoming R-edges — a DirIn expand whose per-in-edge cost is
// Θ(source out-degree) (§5.1). The swap re-roots onto :Leaf: scan the leaves
// (N=asBenchLeafPop) and walk a single out-edge — a DirOut expand. Both plans emit
// the identical single (a,b) match (Leaf-0 → Hub); the delta is the pure
// examined-edge win, proven result-identical by the differential test in
// cypher/anchor_swap_diff_test.go.
//
// The count-store the swap consults is recomputed at engine construction from the
// live graph (cypher.NewEngineWithOptions → recomputeCountStore, O(V+E)), so the
// benchmark builds the fixture through the lpg API — no write path needed — and
// the exact D(label,relType,dir) cells are populated identically to production.
//
// Run:
//
//	go test -run='^$' -bench=BenchmarkAnchorSwap -benchmem -count=10 ./bench/cypher_ldbc/...

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// asBenchOtherPop is the :Hub node's R in-degree from :Other nodes. Large enough
// that walking every hub in-edge (the written DirIn plan) dominates a single
// out-edge walk (the re-rooted DirOut plan), while staying quick to build.
const asBenchOtherPop = 20_000

// asBenchLeafPop is the :Leaf population; exactly one leaf (index 0) has an R
// out-edge into the hub, so D(Leaf,R,OUT) = 1 and N(Leaf) = asBenchLeafPop. The
// written cost is c_s·N(Hub) + c_e·D(Hub,R,IN) = 1 + 8·(asBenchOtherPop+1); the
// candidate cost is c_s·N(Leaf) + c_e·D(Leaf,R,OUT) = asBenchLeafPop + 8. With a
// margin of 2 the swap fires by a wide margin.
const asBenchLeafPop = 50

// anchorSwapBenchQuery anchors :Hub first with an incoming edge, so the default
// IR builds a DirIn expand rooted on the hub — exactly the shape the peephole
// re-roots onto the leaf as a DirOut expand.
const anchorSwapBenchQuery = "MATCH (a:Hub)<-[:R]-(b:Leaf) RETURN a.tag AS at, b.i AS bi"

// buildAnchorSwapBenchGraph seeds the adversarial hub graph via the lpg API. The
// engine recomputes an exact count-store from it at construction.
func buildAnchorSwapBenchGraph() *lpg.Graph[string, float64] {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	_ = g.AddNode("hub")
	_ = g.SetNodeLabel("hub", "Hub")
	_ = g.SetNodeProperty("hub", "tag", lpg.Int64Value(0))
	// asBenchOtherPop :Other nodes, each with one R edge into the hub → the hub's
	// large R in-degree.
	for i := 0; i < asBenchOtherPop; i++ {
		k := fmt.Sprintf("other%d", i)
		_ = g.AddNode(k)
		_ = g.SetNodeLabel(k, "Other")
		_ = g.SetNodeProperty(k, "i", lpg.Int64Value(int64(i)))
		_ = g.AddEdgeLabeled(k, "hub", 1.0, "R")
	}
	// asBenchLeafPop :Leaf nodes; only leaf 0 has an R out-edge into the hub.
	for i := 0; i < asBenchLeafPop; i++ {
		k := fmt.Sprintf("leaf%d", i)
		_ = g.AddNode(k)
		_ = g.SetNodeLabel(k, "Leaf")
		_ = g.SetNodeProperty(k, "i", lpg.Int64Value(int64(i)))
	}
	_ = g.AddEdgeLabeled("leaf0", "hub", 1.0, "R")
	g.SetIndexManager(index.NewManager())
	return g
}

// benchAnchorSwap runs the single-edge query with the anchor swap either enabled
// or disabled, on a freshly-seeded graph.
func benchAnchorSwap(b *testing.B, enabled bool) {
	b.Helper()
	g := buildAnchorSwapBenchGraph()
	engine := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		DisableAnchorSwap: !enabled,
		MaxResultRows:     cypher.MaxResultRowsUnlimited,
	})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := engine.Run(ctx, anchorSwapBenchQuery, nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		for res.Next() {
		}
		if e := res.Err(); e != nil {
			b.Fatalf("Err: %v", e)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// BenchmarkAnchorSwapHub_Swapped measures the optimised plan: the pattern
// re-rooted onto :Leaf as a DirOut expand (a single out-edge walk).
func BenchmarkAnchorSwapHub_Swapped(b *testing.B) {
	benchAnchorSwap(b, true)
}

// BenchmarkAnchorSwapHub_Written measures the legacy plan for the same query — the
// hub-anchored DirIn expand walking the hub's entire R in-degree, the baseline the
// peephole re-roots.
func BenchmarkAnchorSwapHub_Written(b *testing.B) {
	benchAnchorSwap(b, false)
}

// TestAnchorSwapBench_ResultsMatch is a fast guard run as part of the normal test
// layer: it confirms the benchmark query returns the identical (single) row under
// both plans, so the benchmark compares like with like.
func TestAnchorSwapBench_ResultsMatch(t *testing.T) {
	g := buildAnchorSwapBenchGraph()
	on := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: cypher.MaxResultRowsUnlimited})
	off := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableAnchorSwap: true, MaxResultRows: cypher.MaxResultRowsUnlimited})
	count := func(e *cypher.Engine) int {
		res, err := e.Run(context.Background(), anchorSwapBenchQuery, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for res.Next() {
			n++
		}
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		_ = res.Close()
		return n
	}
	non := count(on)
	noff := count(off)
	if non != noff {
		t.Fatalf("row-count mismatch: swap=%d written=%d", non, noff)
	}
	// The only Leaf→Hub edge is from leaf 0, so exactly one (a:Hub)<-[:R]-(b:Leaf)
	// match exists.
	if non != 1 {
		t.Fatalf("unexpected row count %d, want 1", non)
	}
}
