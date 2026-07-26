package lpg

// barrier_bench_test.go — cost of the transaction-visibility barrier itself.
//
// Deliberately carries NO build tag. The re-entrancy guard is compiled only
// under -race or -tags gograph_debug (reentrancy_enabled.go) and is a no-op
// otherwise (reentrancy_disabled.go), so these benchmarks must be runnable in
// both builds: comparing them is the evidence that the guard has left the
// production read path.
//
//	go test -run x -bench BenchmarkBarrier -benchmem -count=6 ./graph/lpg/
//	go test -run x -bench BenchmarkBarrier -benchmem -count=6 -tags gograph_debug ./graph/lpg/
//
// The round-3 comparative audit measured the guard at 97-99% of every
// Graph.View — 1.65 us serial against 3.6 ns for the bare RWMutex pair — with a
// 64 B allocation per call, and read throughput HALVING from 1 to 10 cores
// because runtime.Stack serialises callers on the runtime's process-global
// debuglock. BenchmarkBarrier_ViewParallel is the scaling half of that claim.
//
// Layer: short (bench; skipped unless -bench is set).

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// BenchmarkBarrier_BareRWMutex is the floor for BenchmarkBarrier_View: the
// RLock/RUnlock pair alone, on a mutex nothing else touches. In a released build
// Graph.View must sit on top of this and no higher, which is the acceptance
// bar for rmp #2168.
//
// It is also the diagnostic for whatever anti-scaling survives at 10 cores:
// sync.RWMutex admits concurrent readers but every RLock increments one shared
// counter, so an empty critical section degenerates into cache-line ping-pong
// on that word. When this benchmark and BenchmarkBarrier_ViewParallel degrade
// together, the residual belongs to the RWMutex, not to anything the barrier
// adds — which is the case the lock-free snapshot work (#1671/#2051) addresses
// and this task does not.
func BenchmarkBarrier_BareRWMutex(b *testing.B) {
	var mu sync.RWMutex
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mu.RLock()
		mu.RUnlock() //nolint:gocritic,staticcheck // the empty critical section IS the measurement: this is the floor Graph.View is compared against
	}
}

// BenchmarkBarrier_BareRWMutexParallel is the parallel floor, to be compared
// with BenchmarkBarrier_ViewParallel at the same -cpu setting.
func BenchmarkBarrier_BareRWMutexParallel(b *testing.B) {
	var mu sync.RWMutex
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.RLock()
			mu.RUnlock() //nolint:gocritic,staticcheck // the empty critical section IS the measurement: this is the floor Graph.View is compared against
		}
	})
}

// BenchmarkBarrier_View measures the read-path cost of the visibility barrier
// with an empty body: in a released build this is exactly one RLock/RUnlock pair
// and must allocate zero bytes.
func BenchmarkBarrier_View(b *testing.B) {
	g := New[string, int64](adjlist.Config{Directed: true})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.View(func() {})
	}
}

// BenchmarkBarrier_ApplyAtomically measures the write-path cost of the
// visibility barrier (Lock/Unlock plus the adjacency commit window) with an
// empty transaction.
func BenchmarkBarrier_ApplyAtomically(b *testing.B) {
	g := New[string, int64](adjlist.Config{Directed: true})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.ApplyAtomically(func() error { return nil })
	}
}

// BenchmarkBarrier_ViewParallel measures aggregate View throughput across
// goroutines with an empty body. visMu admits concurrent readers, so ns/op must
// FALL as cores are added. Run with -cpu=1,10 and compare: under the enforcing
// guard the per-op cost roughly doubled from 1 to 10 cores instead, because
// every reader contended on the runtime's debuglock inside runtime.Stack rather
// than on the graph.
func BenchmarkBarrier_ViewParallel(b *testing.B) {
	g := New[string, int64](adjlist.Config{Directed: true})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			g.View(func() {})
		}
	})
}
