package lpg

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// bulkDeleteBatch is the number of nodes each bulk-delete benchmark arm
// actually removes while the timer runs. It is held constant across arms so
// that the only thing varying between them is how many tombstones the graph
// ALREADY carries — which is precisely the term rmp #2400 says the per-node
// cost must not depend on.
const bulkDeleteBatch = 2_000

// buildDeleteGraph interns preloaded+bulkDeleteBatch nodes and drives the first
// preloaded of them into the tombstone set, returning the keys of the batch that
// is still live.
//
// The preload goes through [Graph.RestoreTombstones] rather than a loop of
// [Graph.RemoveNode] for one reason: RestoreTombstones already takes a single
// clone for the whole slice, so the setup is linear even on the defective build.
// A preload built out of RemoveNode calls would itself be quadratic, and at
// 160 000 tombstones it would take longer to arrange the benchmark than to run
// it. The bitmap it leaves behind is byte-for-byte the one the RemoveNode loop
// would have left, which is all the timed section depends on.
func buildDeleteGraph(tb testing.TB, preloaded int) (*Graph[string, float64], []string) {
	tb.Helper()
	g := New[string, float64](adjlist.Config{Directed: true})
	total := preloaded + bulkDeleteBatch
	ids := make([]graph.NodeID, 0, preloaded)
	live := make([]string, 0, bulkDeleteBatch)
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode(%q): %v", key, err)
		}
		if i < preloaded {
			id, ok := g.AdjList().Mapper().Lookup(key)
			if !ok {
				tb.Fatalf("node %q not mapped", key)
			}
			ids = append(ids, id)
			continue
		}
		live = append(live, key)
	}
	g.RestoreTombstones(ids)
	if got := g.TombstoneCount(); got != preloaded {
		tb.Fatalf("preload: TombstoneCount = %d, want %d", got, preloaded)
	}
	return g, live
}

// BenchmarkBulkNodeDelete removes a FIXED batch of nodes from graphs that differ
// only in how many tombstones they already hold. Under rmp #2400 the per-node
// cost is O(existing tombstones), so ns/op rises linearly across the arms; once
// the cost is independent of the accumulated set, every arm reports the same
// ns/op within noise. This is the benchmark whose before/after benchstat is the
// evidence for the fix.
func BenchmarkBulkNodeDelete(b *testing.B) {
	for _, preloaded := range []int{0, 20_000, 40_000, 80_000, 160_000} {
		b.Run(fmt.Sprintf("preloaded=%d", preloaded), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				g, live := buildDeleteGraph(b, preloaded)
				b.StartTimer()
				for _, key := range live {
					g.RemoveNode(key)
				}
				b.StopTimer()
				if got := g.TombstoneCount(); got != preloaded+bulkDeleteBatch {
					b.Fatalf("TombstoneCount = %d, want %d", got, preloaded+bulkDeleteBatch)
				}
			}
			// Report the cost of removing ONE node, which is the quantity that
			// must not depend on the arm, rather than the cost of the batch.
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*bulkDeleteBatch), "ns/node")
		})
	}
}
