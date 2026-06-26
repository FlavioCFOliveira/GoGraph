package lpg

// readscale_1671_bench_test.go — empirical read-scaling baseline for #1671.
//
// Measures how a transactional reader (Graph.View bracketing a realistic
// per-row read of labels + a property) scales with goroutine count while a
// background writer commits multi-op transactions via Graph.ApplyAtomically.
// Under the current visMu RWMutex barrier, View takes the read side of visMu;
// a writer holding the write side excludes every reader for its whole apply.
// The benchmark quantifies that reader/writer exclusion so the lock-free
// snapshot end-state (#1671) can be compared against it with benchstat.
//
// Run: go test -run x -bench BenchmarkReadScale1671 -benchmem -cpu=1,8,64,256 ./graph/lpg/
//
// Layer: short (bench; skipped unless -bench is set).

import (
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func buildScaleGraph(tb testing.TB, nNodes int) (*Graph[string, float64], []graph.NodeID) {
	tb.Helper()
	g := New[string, float64](adjlist.Config{Directed: true})
	ids := make([]graph.NodeID, nNodes)
	for i := 0; i < nNodes; i++ {
		key := "n" + strconv.Itoa(i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if id, ok := g.AdjList().Mapper().Lookup(key); ok {
			ids[i] = id
		}
		if err := g.SetNodeLabel(key, "Hot"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "v", Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g, ids
}

// BenchmarkReadScale1671_ViewBarrier measures the transactional read path under
// the visMu barrier with NO concurrent writer (pure reader/reader scaling: many
// View readers, which should not block one another since visMu allows shared
// readers). This isolates the read-side lock overhead.
func BenchmarkReadScale1671_ViewBarrier(b *testing.B) {
	g, ids := buildScaleGraph(b, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	var ctr int64
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			id := ids[i&(len(ids)-1)]
			i++
			var hot bool
			var v int64
			g.View(func() {
				hot = g.HasNodeLabelByID(id, "Hot")
				if pv, ok := g.NodePropertyByID(id, "v"); ok {
					iv, _ := pv.Int64()
					v = iv
				}
			})
			if hot {
				atomic.AddInt64(&ctr, v)
			}
		}
	})
	_ = ctr
}

// BenchmarkReadScale1671_ViewUnderWriter measures the transactional read path
// while a single background writer commits 8-op transactions via
// ApplyAtomically. Under the barrier, the writer's write-lock excludes all
// readers for the apply duration — the reader/writer exclusion #1671 removes.
func BenchmarkReadScale1671_ViewUnderWriter(b *testing.B) {
	g, ids := buildScaleGraph(b, 4096)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = g.ApplyAtomically(func() error {
				for k := 0; k < 8; k++ {
					key := "n" + strconv.Itoa((w+k)&(len(ids)-1))
					_ = g.SetNodeProperty(key, "v", Int64Value(int64(w+k)))
				}
				return nil
			})
			w += 8
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	var ctr int64
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			id := ids[i&(len(ids)-1)]
			i++
			var hot bool
			var v int64
			g.View(func() {
				hot = g.HasNodeLabelByID(id, "Hot")
				if pv, ok := g.NodePropertyByID(id, "v"); ok {
					iv, _ := pv.Int64()
					v = iv
				}
			})
			if hot {
				atomic.AddInt64(&ctr, v)
			}
		}
	})
	b.StopTimer()
	close(stop)
	<-done
	_ = ctr
}
