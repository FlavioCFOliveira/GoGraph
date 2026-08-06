package lpg

// mvcc_existence_bench_test.go — rmp #2311: does the tombstone bitmap earn its keep?
//
// # The question, and why it must be measured
//
// Two structures answer "does this node exist": the copy-on-write tombstone bitmap
// ([Graph.IsTombstoned], a lock-free roaring Contains) and the versioned life store
// ([Graph.NodeExistsAsOf], a per-shard RLock plus two map probes). The task's rule is
// that the bitmap may survive ONLY as a documented accelerator of the versioned truth,
// and only if measurement shows it is materially faster — never as an independent
// answer, and never on preference.
//
// So this benchmark is the decision input, not a performance report. It compares the
// two on the SAME ids, in the two regimes that matter: a graph with no removal at all
// (the common case, where the bitmap is empty and the life store has births) and one
// with churn.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// benchExistenceGraph builds size nodes and removes every removeEvery-th one.
func benchExistenceGraph(b *testing.B, size, removeEvery int) (*Graph[string, float64], []graph.NodeID) {
	b.Helper()
	g := New[string, float64](adjlist.Config{Directed: true})
	ids := make([]graph.NodeID, 0, size)
	for i := 0; i < size; i++ {
		k := fmt.Sprintf("n%07d", i)
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		id, ok := g.adj.Mapper().Lookup(k)
		if !ok {
			b.Fatalf("node %s not interned", k)
		}
		ids = append(ids, id)
		if removeEvery > 0 && i%removeEvery == 0 {
			g.RemoveNode(k)
		}
	}
	return g, ids
}

// BenchmarkExistence compares the two answers to "does this node exist".
//
// The snapshot arm uses a live read snapshot, which is the shape every MVCC reader
// actually takes; passing nil would make NodeExistsAsOf delegate straight to the
// bitmap and measure nothing.
func BenchmarkExistence(b *testing.B) {
	for _, churn := range []int{0, 8} {
		name := "clean"
		if churn > 0 {
			name = fmt.Sprintf("removed=1in%d", churn)
		}
		g, ids := benchExistenceGraph(b, 100000, churn)
		snap := g.BeginRead()

		b.Run(name+"/bitmap", func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var live int
			for i := 0; i < b.N; i++ {
				if !g.IsTombstoned(ids[i%len(ids)]) {
					live++
				}
			}
			runtimeSink = live
		})
		b.Run(name+"/versioned", func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var live int
			for i := 0; i < b.N; i++ {
				if g.NodeExistsAsOf(ids[i%len(ids)], snap) {
					live++
				}
			}
			runtimeSink = live
		})

		g.EndRead(snap)
		_ = g.Close()
	}
}

// runtimeSink keeps the benchmark loops from being optimised away.
var runtimeSink int
