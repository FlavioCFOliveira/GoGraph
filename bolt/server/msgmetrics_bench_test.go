package server

// msgmetrics_bench_test.go — what one per-message latency observation COSTS
// (rmp #2715). Layer: short.
//
// The contention ladder can show that a Bolt workload moved; it cannot say why,
// because a throughput delta is not an attribution. This benchmark supplies the
// other half: the cost of the emission itself, measured on its own, so a
// per-operation delta observed over the wire can be checked against a predicted
// one instead of merely asserted to be the histogram.
//
// The REAL backend is what matters. rmp #2698's finding was that the no-op path
// and the real path behave completely differently — and that the shape which
// looks obviously right, a cached metric handle, measured 0.081x — so a figure
// taken against the no-op default would price a path no operator runs. Both are
// benchmarked here, and the difference between them IS the enabled cost.
//
// Run with:
//
//	go test -run='^$' -bench=BenchmarkMsgObserve -benchmem -cpu=1,8,64 -count=6 ./bolt/server/

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	promreg "github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
)

// benchMsg is the message a RUN carries. It is an `any` because that is what
// Session.HandleMessage receives, so the type switch under test is the same one
// the server performs and not a devirtualised shortcut.
var benchMsg any = &proto.Run{Query: "MATCH (n) RETURN count(n)"}

// observeOnce is exactly the line HandleMessage runs, minus the defer: the type
// switch, the table index, and the Stopwatch pair.
func observeOnce(msg any) {
	metrics.Time(msgLatencySeries[msgKindOf(msg)]).Stop()
}

// BenchmarkMsgObserve_NoopBackend is the cost every deployment pays that has
// installed no backend — the module's default.
func BenchmarkMsgObserve_NoopBackend(b *testing.B) {
	metrics.SetBackend(nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		observeOnce(benchMsg)
	}
}

// BenchmarkMsgObserve_RealBackend is the cost an OBSERVED deployment pays. The
// series is established before the timer so the figure is the steady-state one
// rather than the first-sight sanitize and LoadOrStore.
func BenchmarkMsgObserve_RealBackend(b *testing.B) {
	metrics.SetBackend(promreg.New())
	b.Cleanup(func() { metrics.SetBackend(nil) })
	observeOnce(benchMsg)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		observeOnce(benchMsg)
	}
}

// BenchmarkMsgObserve_RealBackendParallel is the scaling question rmp #2698
// exists to answer, asked of THIS series rather than of a counter. A Bolt
// server's messages arrive on one goroutine per connection, so under load many
// goroutines emit into the same per-message histogram — and a histogram is
// three read-modify-writes per observation where a counter is one.
func BenchmarkMsgObserve_RealBackendParallel(b *testing.B) {
	metrics.SetBackend(promreg.New())
	b.Cleanup(func() { metrics.SetBackend(nil) })
	observeOnce(benchMsg)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			observeOnce(benchMsg)
		}
	})
}

// BenchmarkMsgKindOf isolates the type switch, so a change to its cost can be
// separated from a change to the metrics backend's.
func BenchmarkMsgKindOf(b *testing.B) {
	b.ReportAllocs()
	var sink msgKind
	for i := 0; i < b.N; i++ {
		sink = msgKindOf(benchMsg)
	}
	if sink != msgRun {
		b.Fatalf("msgKindOf = %d, want msgRun", sink)
	}
}
