package lpg

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// BenchmarkAbortCostSideHistory is one abort of a transaction that wrote one
// node property while a reader holds history on many OTHER objects in the
// label, property and per-handle side stores (ACID audit round 6, finding M1):
// the abort must cost what the transaction wrote, not what the graph holds.
func BenchmarkAbortCostSideHistory(b *testing.B) {
	const held = 100000
	shapes := []struct {
		name  string
		build func(g *Graph[string, float64])
	}{
		{"none", func(*Graph[string, float64]) {}},
		{fmt.Sprintf("props=%d", held), func(g *Graph[string, float64]) {
			for i := 0; i < held; i++ {
				_ = g.SetNodeProperty(fmt.Sprintf("p%d", i), "v", Int64Value(int64(i)))
			}
		}},
		{fmt.Sprintf("labels=%d", held), func(g *Graph[string, float64]) {
			for i := 0; i < held; i++ {
				_ = g.SetNodeLabel(fmt.Sprintf("l%d", i), "L")
			}
		}},
		{fmt.Sprintf("handleProps=%d", held), func(g *Graph[string, float64]) {
			for i := 0; i < held; i++ {
				src := fmt.Sprintf("e%d", i)
				h, _ := g.AddEdgeH(src, "n", 1)
				_ = g.SetEdgePropertyByHandle(src, "n", h, "w", Int64Value(int64(i)))
			}
		}},
	}
	for _, sh := range shapes {
		b.Run(sh.name, func(b *testing.B) {
			g := New[string, float64](adjlist.Config{})
			defer func() { _ = g.Close() }()
			_ = g.AddNode("n")
			// The reader is taken BEFORE the history is written, so every version
			// the shape writes is still needed by it and stays in its chain.
			rd := g.BeginRead()
			defer g.EndRead(rd)
			sh.build(g)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tx := g.BeginVersionedTx()
				_ = g.Writer(tx).SetNodeProperty("n", "k", Int64Value(int64(i)))
				doomForTest(tx)
				g.EndVersionedTx(tx)
			}
		})
	}
}
