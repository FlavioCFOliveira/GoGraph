package metrics_test

// emit_backend_bench_test.go — what INSTALLING a backend costs, versus the
// no-op default the module ships (rmp #2698, acceptance criterion 1).
//
// The contention observatory's `metrics-emit` workload installs the real
// Prometheus-compatible registry and reports how that configuration SCALES.
// It does not report what the configuration COSTS, because it never drives the
// no-op default: a workload whose every operation is an empty method behind an
// atomic pointer load would measure the harness. The scaling number therefore
// cannot answer "is enabling metrics expensive", and that question decides
// whether the scaling number is worth acting on at all.
//
// This benchmark answers it. Both arms drive the SAME operation mix through the
// SAME package-level entry points every wired call site uses — a counter on
// every operation, a latency observation on one in four, a gauge on one in
// sixteen — and differ only in which backend is installed. The difference
// between the two arms is, by construction, the cost of the backend.
//
// The mix is copied from bench/contention's metricsOp so the two instruments
// measure the same program; see bench/contention/workloads_unreached.go.
//
// Run with:
//
//	go test -run='^$' -bench='BenchmarkEmit' -benchmem -cpu=1,8 -count=6 ./internal/metrics/
//
// -cpu is the goroutine ladder: BenchmarkEmitParallel spawns GOMAXPROCS
// goroutines, so -cpu=1,8 reads the same two rungs the sweep quotes.

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
)

// Sampling reciprocals, mirroring bench/contention's metricsLatencyEvery and
// metricsGaugeEvery. They are duplicated rather than exported from the bench
// module because internal/metrics must not import its own benchmark harness.
const (
	emitLatencyEvery = 4
	emitGaugeEvery   = 16
)

// Metric names, mirroring the workload's. Held as constants so the concat the
// ceiling arm performs is absent here: this benchmark measures the backend, not
// the harness's name construction.
const (
	emitCounterName = "bench.contention.ops"
	emitLatencyName = "bench.contention.latency"
	emitGaugeName   = "bench.contention.gauge"
)

// emitOp is one metrics-emit operation, driven through the package-level entry
// points so the measured path includes the atomic backend load and the
// interface dispatch every real call site pays.
func emitOp(worker, iter int) {
	metrics.IncCounter(emitCounterName, 1)
	if (worker+iter)%emitLatencyEvery == 0 {
		metrics.ObserveLatency(emitLatencyName, time.Duration(iter%1000)*time.Microsecond)
	}
	if (worker+iter)%emitGaugeEvery == 0 {
		metrics.SetGauge(emitGaugeName, float64(iter))
	}
}

// installNoop selects the shipped default: an empty method set behind an
// atomic.Pointer.
func installNoop(b *testing.B) {
	b.Helper()
	metrics.SetBackend(nil)
	b.Cleanup(func() { metrics.SetBackend(nil) })
}

// installPrometheus selects the real backend, with all three series already
// established so the benchmark measures steady-state emission rather than
// first-sight registration.
func installPrometheus(b *testing.B) {
	b.Helper()
	reg := prometheus.New()
	metrics.SetBackend(reg)
	emitOp(0, 0)
	b.Cleanup(func() { metrics.SetBackend(nil) })
}

func BenchmarkEmit(b *testing.B) {
	b.Run("noop", func(b *testing.B) {
		installNoop(b)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			emitOp(0, i)
		}
	})
	b.Run("prometheus", func(b *testing.B) {
		installPrometheus(b)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			emitOp(0, i)
		}
	})
}

// BenchmarkEmitParallel drives GOMAXPROCS goroutines, each with its own worker
// id so the sampled observations land on the same 1-in-4 and 1-in-16 cadence
// the serial arm uses.
func BenchmarkEmitParallel(b *testing.B) {
	run := func(b *testing.B) {
		var next atomic.Int64
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			worker := int(next.Add(1))
			for i := 0; pb.Next(); i++ {
				emitOp(worker, i)
			}
		})
	}
	b.Run("noop", func(b *testing.B) {
		installNoop(b)
		run(b)
	})
	b.Run("prometheus", func(b *testing.B) {
		installPrometheus(b)
		run(b)
	})
}
