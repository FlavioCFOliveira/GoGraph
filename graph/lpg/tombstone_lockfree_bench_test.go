package lpg

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// BenchmarkIsTombstonedReadParallel measures concurrent IsTombstoned calls on a
// graph that has never deleted a node — the AllNodesScan hot path. Run with
// -cpu 1,2,4,8 and compare ns/op: before the tombstoneActive==0 fast path each
// call took tombstoneMu.RLock, whose reader-count atomic bounced across cores
// and capped read scaling; the lock-free gate lets it scale toward NumCPU.
func BenchmarkIsTombstonedReadParallel(b *testing.B) {
	const n = 2000
	g := New[string, float64](adjlist.Config{Directed: true})
	ids := make([]graph.NodeID, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		id, ok := g.AdjList().Mapper().Lookup(key)
		if !ok {
			b.Fatalf("node %q not mapped", key)
		}
		ids[i] = id
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var i, sink int
		for pb.Next() {
			if g.IsTombstoned(ids[i]) {
				sink++
			}
			i++
			if i == n {
				i = 0
			}
		}
		if sink != 0 {
			b.Fatalf("unexpected tombstone hit on a never-deleted graph: %d", sink)
		}
	})
}
