package lpg

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// scanBenchN is the graph size the tombstone-scan benchmarks build. It mirrors
// the 200k-node graph the rmp #2039 audit used to demonstrate the negative
// scaling of the old global-RWMutex read path.
const scanBenchN = 200_000

// buildScanGraph builds a scanBenchN-node graph and returns it together with
// the interned NodeIDs in insertion order. When deleteEvery > 0 every
// deleteEvery-th node is removed (tombstoned), exercising the post-delete read
// path; deleteEvery == 0 leaves the graph on the never-deleted fast path.
func buildScanGraph(tb testing.TB, deleteEvery int) (*Graph[string, float64], []graph.NodeID) {
	tb.Helper()
	g := New[string, float64](adjlist.Config{Directed: true})
	ids := make([]graph.NodeID, scanBenchN)
	for i := 0; i < scanBenchN; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		id, ok := g.AdjList().Mapper().Lookup(key)
		if !ok {
			tb.Fatalf("node %q not mapped", key)
		}
		ids[i] = id
	}
	if deleteEvery > 0 {
		for i := 0; i < scanBenchN; i += deleteEvery {
			g.RemoveNode(fmt.Sprintf("n%d", i))
		}
	}
	return g, ids
}

// scanLive sweeps every id once through IsTombstoned, mimicking the per-node
// liveness check the Cypher AllNodesScan / count(*) hot path performs. It
// returns the live count so the compiler cannot elide the loop.
func scanLive(g *Graph[string, float64], ids []graph.NodeID) int {
	live := 0
	for _, id := range ids {
		if !g.IsTombstoned(id) {
			live++
		}
	}
	return live
}

// BenchmarkTombstoneScanClean sweeps IsTombstoned across a 200k-node graph that
// has never deleted a node — the lock-free fast-path baseline (tombstoneActive
// == 0). Run with -cpu=1,2,4,8: each op is one full scan, and ns/op must fall
// as cores rise. It is the reference the tombstoned-path benchmark below must
// stay within a small constant factor of.
func BenchmarkTombstoneScanClean(b *testing.B) {
	g, ids := buildScanGraph(b, 0)
	if g.TombstoneCount() != 0 {
		b.Fatalf("clean graph carries %d tombstones", g.TombstoneCount())
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var sink int
		for pb.Next() {
			sink += scanLive(g, ids)
		}
		if sink < 0 {
			b.Fatal("unreachable")
		}
	})
}

// BenchmarkTombstoneScanTombstoned sweeps IsTombstoned across a 200k-node graph
// with ~10% of nodes tombstoned — the path rmp #2039 fixed. Under the old
// per-node global RWMutex.RLock this scaled NEGATIVELY (8 cores slower than 1,
// ~88% of time spent in the lock). With the lock-free copy-on-write bitmap it
// must scale POSITIVELY with cores and stay within a small constant factor of
// BenchmarkTombstoneScanClean. Run with -cpu=1,2,4,8.
func BenchmarkTombstoneScanTombstoned(b *testing.B) {
	g, ids := buildScanGraph(b, 10) // remove every 10th node (~10% tombstoned)
	if g.TombstoneCount() == 0 {
		b.Fatal("expected the graph to carry tombstones")
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var sink int
		for pb.Next() {
			sink += scanLive(g, ids)
		}
		if sink < 0 {
			b.Fatal("unreachable")
		}
	})
}
