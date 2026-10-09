package lpg

// readscale_1671_bench_test.go — empirical read-scaling baseline for #1671.
//
// Measures how a reader doing a realistic per-row read of labels + a property
// scales with goroutine count while a background writer commits multi-op
// transactions via Graph.ApplyAtomically. When this was written the reader
// bracketed the read in Graph.View, the read side of the visMu RWMutex, and the
// writer excluded every reader for its whole apply. rmp #2344 removed
// Graph.View: the reader now calls the direct accessors, each of which reads
// the latest committed version through a snapshot on its own stack and takes
// no lock, so this measures the lock-free end-state (#1671) directly.
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

// BenchmarkReadScale1671_ViewBarrier measures the read path with NO concurrent
// writer (pure reader/reader scaling). The reads take no lock; the name records
// the Graph.View barrier it originally measured, removed by rmp #2344.
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
			hot = g.HasNodeLabelByID(id, "Hot")
			if pv, ok := g.NodePropertyByID(id, "v"); ok {
				iv, _ := pv.Int64()
				v = iv
			}
			if hot {
				atomic.AddInt64(&ctr, v)
			}
		}
	})
	_ = ctr
}

// BenchmarkReadScale1671_ViewUnderWriter measures the read path while a single
// background writer commits 8-op transactions via ApplyAtomically. The writer
// holds the schema barrier exclusively, but the reads take no lock, so the
// writer no longer excludes them — the reader/writer exclusion #1671 removed.
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
			_ = g.ApplyAtomicallyTx(func(tx WriteTx) error {
				for k := 0; k < 8; k++ {
					key := "n" + strconv.Itoa((w+k)&(len(ids)-1))
					_ = g.Writer(tx).SetNodeProperty(key, "v", Int64Value(int64(w+k)))
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
			hot = g.HasNodeLabelByID(id, "Hot")
			if pv, ok := g.NodePropertyByID(id, "v"); ok {
				iv, _ := pv.Int64()
				v = iv
			}
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
