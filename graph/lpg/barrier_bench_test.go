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
// debuglock. rmp #2344 removed Graph.View together with its benchmarks
// (BenchmarkBarrier_View, BenchmarkBarrier_ViewParallel); what remains here is
// the bare RWMutex floor and the write side, BenchmarkBarrier_ApplyAtomically.
//
// Layer: short (bench; skipped unless -bench is set).

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// BenchmarkBarrier_BareRWMutex is the RLock/RUnlock pair alone, on a mutex
// nothing else touches. It was the floor for BenchmarkBarrier_View, the
// acceptance bar for rmp #2168 (a released Graph.View was to sit on top of it
// and no higher); rmp #2344 removed Graph.View and that benchmark.
//
// It also shows the anti-scaling of a shared RWMutex at 10 cores: sync.RWMutex
// admits concurrent readers but every RLock increments one shared counter, so an
// empty critical section degenerates into cache-line ping-pong on that word.
// The schema barrier is an [mvcc.Gate], not a sync.RWMutex, for that reason
// (rmp #2337).
func BenchmarkBarrier_BareRWMutex(b *testing.B) {
	var mu sync.RWMutex
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mu.RLock()
		mu.RUnlock() //nolint:gocritic,staticcheck // the empty critical section IS the measurement: the bare RWMutex floor
	}
}

// BenchmarkBarrier_BareRWMutexParallel is the parallel floor. Its counterpart,
// BenchmarkBarrier_ViewParallel, was removed with Graph.View by rmp #2344.
func BenchmarkBarrier_BareRWMutexParallel(b *testing.B) {
	var mu sync.RWMutex
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.RLock()
			mu.RUnlock() //nolint:gocritic,staticcheck // the empty critical section IS the measurement: the bare RWMutex floor
		}
	})
}

// BenchmarkBarrier_ApplyAtomically measures the write-path cost of the
// visibility barrier (Lock/Unlock plus the adjacency commit window) with an
// empty transaction.
func BenchmarkBarrier_ApplyAtomically(b *testing.B) {
	g := New[string, int64](adjlist.Config{})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.ApplyAtomically(func() error { return nil })
	}
}
