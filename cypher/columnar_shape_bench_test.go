package cypher_test

// columnar_shape_bench_test.go — cost evidence for the columnar shapes newly
// admitted by #2186.
//
// The round-3 audit measured what falling off the columnar path costs on shapes that
// return an identical multiset either way (docs/audit-2026-07-26-streams/s05-runtime.md
// F1): a conjunction cost 2.9x the time and 3374x the allocations of the bare
// comparison, and adding a label to a traversal's far endpoint — a label every node
// already carried — cost 4.4x the time and 1640x the allocations.
//
// These benchmarks reproduce those pairs so the improvement is measured against the
// audit's own baseline rather than asserted. Read each *_Cliff arm against the
// *_Baseline arm above it: the pair returns the identical multiset, so any difference
// is pure execution-path cost.
//
//	go test -run=^$ -bench='BenchmarkColumnarShape' -benchmem -count=6 -benchtime=10x ./cypher/
//
// Layer: short.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// shapeBenchNodes matches the audit's traversal fixture size.
const shapeBenchNodes = 20_000

// seedShapeGraph builds a labelled chain: shapeBenchNodes nodes all carrying label P
// and properties v (the index) and w (index % 7), each with one out-edge of type K to
// the next. Every node carries P, so adding :P to a pattern endpoint cannot change any
// result — which is what makes the labelled arms exact controls.
func seedShapeGraph(b *testing.B) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	keys := make([]string, shapeBenchNodes)
	for i := 0; i < shapeBenchNodes; i++ {
		k := "n" + strconv.Itoa(i)
		keys[i] = k
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			b.Fatalf("SetNodeProperty v: %v", err)
		}
		if err := g.SetNodeProperty(k, "w", lpg.Int64Value(int64(i%7))); err != nil {
			b.Fatalf("SetNodeProperty w: %v", err)
		}
	}
	for i := 0; i+1 < shapeBenchNodes; i++ {
		if err := g.AddEdge(keys[i], keys[i+1], 1.0); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel(keys[i], keys[i+1], "K")
	}
	return g
}

// benchShape runs q over the seeded graph with default engine options, so the path
// selected is the one a real caller gets.
func benchShape(b *testing.B, q string) {
	silenceBenchLogs(b)
	g := seedShapeGraph(b)
	eng := cypher.NewEngine(g)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, q)
	}
}

// ── the conjunction pair: one Selection carrying an AND ──

func BenchmarkColumnarShape_ScanBaseline(b *testing.B) {
	benchShape(b, "MATCH (n:P) WHERE n.v > 10 RETURN n.v")
}

func BenchmarkColumnarShape_ScanConjunctionSameProp(b *testing.B) {
	benchShape(b, "MATCH (n:P) WHERE n.v > 10 AND n.v < 19000 RETURN n.v")
}

func BenchmarkColumnarShape_ScanConjunctionTwoProps(b *testing.B) {
	benchShape(b, "MATCH (n:P) WHERE n.v > 10 AND n.w >= 0 RETURN n.v")
}

func BenchmarkColumnarShape_ScanIn(b *testing.B) {
	benchShape(b, "MATCH (n:P) WHERE n.w IN [0,1,2,3,4,5,6] RETURN n.v")
}

// ── the traversal pair: adding a far-endpoint label every node already carries
// splits one Selection into two stacked ones ──

func BenchmarkColumnarShape_HopBaseline(b *testing.B) {
	benchShape(b, "MATCH (a:P)-[:K]->(m) WHERE m.v > 10 RETURN m.v")
}

func BenchmarkColumnarShape_HopLabelledFarEnd(b *testing.B) {
	benchShape(b, "MATCH (a:P)-[:K]->(m:P) WHERE m.v > 10 RETURN m.v")
}

func BenchmarkColumnarShape_HopAnchorFilter(b *testing.B) {
	benchShape(b, "MATCH (a:P)-[:K]->(m) WHERE a.v > 10 RETURN m.v")
}

func BenchmarkColumnarShape_HopAnchorFilterLabelledFarEnd(b *testing.B) {
	benchShape(b, "MATCH (a:P)-[:K]->(m:P) WHERE a.v > 10 RETURN m.v")
}

func BenchmarkColumnarShape_HopAnchorAndPostFilter(b *testing.B) {
	benchShape(b, "MATCH (a:P)-[:K]->(m) WHERE a.v > 10 AND m.v < 19000 RETURN m.v")
}

// ── the LIMIT pair: a semantically inert LIMIT on a result smaller than the limit
// broke the chunk chain at the plan root ──

func BenchmarkColumnarShape_ScanNoLimit(b *testing.B) {
	benchShape(b, "MATCH (n:P) WHERE n.v > 10 RETURN n.v")
}

func BenchmarkColumnarShape_ScanInertLimit(b *testing.B) {
	benchShape(b, "MATCH (n:P) WHERE n.v > 10 RETURN n.v LIMIT 20000")
}
