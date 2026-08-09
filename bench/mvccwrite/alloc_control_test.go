package mvccwrite

// alloc_control_test.go — the allocation-matched parallel control for the
// write-scaling harness (rmp #2338).
//
// # Why the existing control is not enough
//
// gate_test.go's `control/parallel` is a CPU-bound spin that shares nothing and
// allocates nothing. It measures ~6.3x at eight writers on an Apple M4, and it is
// exactly the right control for the question it was built for: can the instrument
// tell a concurrent build from a serialised one.
//
// It is the WRONG ceiling to compare engine write throughput against. A commit of
// `CREATE (n:Account {id: $id})` costs 43 allocations and 4.1 KB, measured at both one
// and thirty-two writers, and the allocator and the garbage collector are SHARED. A
// workload allocating at that rate cannot reach a non-allocating workload's scaling
// however perfectly lock-free it is, so comparing 2.1x against 6.3x silently charges
// the concurrency control for a cost the Go runtime imposes on any program of this
// allocation profile.
//
// This control closes that gap: N goroutines, each allocating the same shape and
// volume per unit as one commit, touching NO shared structure of any kind. Whatever
// scaling it reaches is the ceiling an allocation-matched, perfectly parallel Go
// program achieves on this machine. The distance between it and
// BenchmarkWriteScaling/mem is what the engine's own concurrency control owes, and
// the distance between it and the CPU-bound control is what the runtime owes.
//
// It is a CONTROL, not a target: nothing here is a gate and no threshold is asserted.

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// allocsPerCommit and bytesPerCommit are the measured allocation profile of one
// autocommit `CREATE (n:Account {id: $id})` on the `mem` wiring, from
// `BenchmarkWriteScaling/mem -benchmem`.
//
// THESE CONSTANTS MUST BE RE-MEASURED WHENEVER THE WRITE PATH'S ALLOCATION
// PROFILE CHANGES. The entire value of this control is that its allocation rate
// is the engine's; a stale pair silently compares the engine against the ceiling
// of a program it no longer resembles.
//
// Current, at head 232be262 + rmp #2339: 43 allocs/op, 4151 B/op at one writer,
// 41 allocs/op, 3832 B/op at thirty-two — flat in the writer count, so the
// profile is the statement's and not an artefact of contention.
//
// Previously, at head bc9ffc8a (rmp #2338's ledger): 56 allocs/op, 4242 B/op at
// one writer. rmp #2339 removed thirteen objects per commit from the write
// path's transient state; see docs/benchmarks/ for the ledger and the arms.
//
// The control matches the COUNT and the total VOLUME, not the exact size histogram:
// what the allocator and the collector scale on is object count and bytes, and a
// faithful histogram would tie this control to one statement shape for no gain.
const (
	allocsPerCommit = 43
	bytesPerCommit  = 4151
)

// nsPerCommitTarget is the single-writer cost of one commit on the `mem` wiring,
// measured alongside the allocation profile above: 2690 ns at head 232be262 +
// rmp #2339 (2892 ns at bc9ffc8a, before it).
//
// It is re-measured with the pair above and for the same reason: allocations per
// SECOND is the quantity that matters, so a profile and a per-unit cost from
// different builds describe no program at all.
//
// # Matching the RATE, not just the shape, and why the first version was wrong
//
// The first version of this control allocated the right count and volume per unit
// and nothing else, so a unit cost 709 ns against a commit's 2892 ns. It therefore
// allocated FOUR TIMES FASTER per second than the engine does, saturated the
// allocator far sooner, and reported a ceiling of 1.28x — a number that flatters the
// finding by charging the runtime with a load the engine never applies.
//
// What the allocator and the collector scale on is bytes and objects PER SECOND, so
// a control that does not match the per-unit cost does not match the rate. The spin
// below pads each unit to nsPerCommitTarget with work that allocates nothing and
// shares nothing, so the control's allocation rate at any writer count is the
// engine's. calibrateSpin resolves the iteration count on the machine actually
// running the benchmark rather than hard-coding one measured elsewhere.
const nsPerCommitTarget = 2690

// allocSink is written by every unit so the compiler cannot prove the allocations
// dead. It is per-goroutine — the whole point of this control is that nothing is
// shared — and is returned rather than stored globally for the same reason.
type allocSink struct {
	chunks [][]byte
	total  int
}

// padSink keeps the compiler from eliminating the calibration spin.
//
// ATOMIC, not a plain uint64. Every writer goroutine runs allocUnit, so a plain
// package-level store here would be a genuine data race and `go test -race` would
// report it — in a file whose entire purpose is to establish that the control shares
// NOTHING. An atomic store is race-free, and at one store per ~2000 spin iterations
// it is far off the measured path.
var padSink atomic.Uint64

// allocUnit performs one unit of allocation-matched, cost-matched work:
// allocsPerCommit distinct heap objects totalling bytesPerCommit bytes, each written
// to so the allocator cannot hand back a shared zero page and the collector must
// actually scan them, then a non-allocating spin of `pad` iterations to bring the
// unit's cost up to one commit's.
//
// It shares NOTHING between goroutines: the sink, the slice header, every chunk and
// the spin's accumulator are local to the call, and the only global is a
// write-only sink the compiler needs. Any scaling shortfall it shows is the Go
// runtime's allocator and collector, never contention on a data structure.
func allocUnit(pad int) error {
	const per = bytesPerCommit / allocsPerCommit
	s := allocSink{chunks: make([][]byte, 0, allocsPerCommit)}
	for i := 0; i < allocsPerCommit-1; i++ {
		b := make([]byte, per)
		// Touch the head and the tail so the pages are faulted and the object is
		// genuinely live for the collector.
		b[0] = byte(i)
		b[len(b)-1] = byte(i)
		s.chunks = append(s.chunks, b)
		s.total += len(b)
	}
	var acc uint64 = 1
	for i := 0; i < pad; i++ {
		acc = acc*6364136223846793005 + 1442695040888963407
	}
	padSink.Store(acc)
	// Keep the sink escaping so the whole graph of objects is retained for the
	// duration of the unit, as a commit's transient state is.
	if s.total < 0 || len(s.chunks) != allocsPerCommit-1 {
		return fmt.Errorf("alloc control: sink miscounted (%d chunks, %d bytes)", len(s.chunks), s.total)
	}
	return nil
}

// calibrateSpin resolves the pad iteration count that brings one allocUnit to
// nsPerCommitTarget on THIS machine, by bisecting on a single-goroutine measurement.
//
// It is measured rather than hard-coded because the whole value of this control is
// that its allocation RATE matches the engine's, and that is a property of the host.
func calibrateSpin(b *testing.B) int {
	b.Helper()
	measure := func(pad int) float64 {
		const reps = 20000
		start := time.Now()
		for i := 0; i < reps; i++ {
			if err := allocUnit(pad); err != nil {
				b.Fatalf("calibration unit failed: %v", err)
			}
		}
		return float64(time.Since(start).Nanoseconds()) / reps
	}
	// Warm up so the first timed pass is not paying page faults and cache misses.
	_ = measure(0)
	lo, hi := 0, 64
	for measure(hi) < nsPerCommitTarget && hi < 1<<20 {
		hi *= 2
	}
	for lo < hi {
		mid := (lo + hi) / 2
		if measure(mid) < nsPerCommitTarget {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// BenchmarkAllocScalingControl reports the scaling factor of a perfectly parallel,
// allocation-matched workload, at the same writer counts as BenchmarkWriteScaling.
//
// Read it as the ALLOCATION-AWARE ceiling for that benchmark. It shares no structure,
// takes no lock and has no critical section, so anything below linear here is the Go
// runtime's allocator and collector — the same cost the engine pays and cannot design
// away by changing its concurrency control.
func BenchmarkAllocScalingControl(b *testing.B) {
	pad := calibrateSpin(b)
	b.Logf("calibrated spin: %d iterations to reach ~%d ns/unit", pad, nsPerCommitTarget)
	var base float64
	for _, writers := range scalingWriters {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			perWriter := (b.N + writers - 1) / writers
			b.ResetTimer()
			got, err := runArm(writers, perWriter, func(_, _ int) error { return allocUnit(pad) })
			b.StopTimer()
			if err != nil {
				b.Fatalf("alloc control unit failed: %v", err)
			}
			if got.commits == 0 {
				b.Fatal("no units ran")
			}
			ups := got.commitsPerSec()
			if writers == 1 {
				base = ups
			}
			b.ReportMetric(ups, "commits/s")
			b.ReportMetric(got.nsPerCommit(), "ns/commit")
			if base > 0 {
				b.ReportMetric(ups/base, "scaling")
			}
		})
	}
}
