package prometheus

// inc_counter_bench_test.go — empirical evidence for #1519.
//
// IncCounter / ObserveLatency are on a module-wide hot path (storage, Cypher,
// and Bolt all emit metrics). Before #1519 every call sanitized the name (a
// strings.Builder allocation for the dotted in-tree names) and took an RWMutex
// RLock + map lookup, even for an already-established series. After, an
// established series resolves through a single lock-free sync.Map load keyed by
// the raw name, with no per-call allocation. The parallel benchmark exposes the
// removed lock contention. Layer: short.
//
// Run with:
//
//	go test -run=^$ -bench='BenchmarkIncCounter|BenchmarkObserveLatency' -benchmem -count=6 ./internal/metrics/prometheus/

import (
	"testing"
	"time"
)

// dotted in-tree metric name (needs sanitization: '.' -> '_').
const benchCounterName = "store.snapshot.WriteLabels.calls"
const benchHistName = "cypher.exec.query.latency"

func BenchmarkIncCounter(b *testing.B) {
	r := New()
	r.IncCounter(benchCounterName, 1) // establish the series
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.IncCounter(benchCounterName, 1)
	}
}

func BenchmarkIncCounterParallel(b *testing.B) {
	r := New()
	r.IncCounter(benchCounterName, 1)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.IncCounter(benchCounterName, 1)
		}
	})
}

func BenchmarkObserveLatency(b *testing.B) {
	r := New()
	r.ObserveLatency(benchHistName, time.Millisecond)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.ObserveLatency(benchHistName, time.Millisecond)
	}
}
