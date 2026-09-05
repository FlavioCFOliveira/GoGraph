package count

// bench_test.go — the read- and write-path benchmarks that price a change to
// the cell representation (rmp #2696).
//
// The store's two paths have opposite shapes and must be priced separately:
//
//   - the WRITE path ([Store.Apply]) is what anti-scales. One hot relationship
//     type lands every KindE cell on one shard ([Store.eShardOf] keys on the
//     relationship type alone), so every writer meets one object;
//   - the READ path ([Store.CountE]) is what the planner runs on every
//     cardinality estimate (cypher/count_estimate.go:65). It is lock-free and
//     single-digit-nanosecond, so a striped counter that must SUM its stripes on
//     read pays for the write win exactly here. A change that speeds writes and
//     halves read throughput is a regression, and these benchmarks are what
//     makes that visible rather than assumed.
//
// Every benchmark is reported per operation, so a striped design's read cost is
// directly comparable against the single-cell one via benchstat.

import (
	"runtime"
	"strconv"
	"testing"
)

// benchRelType is the single hot relationship type. It matches the shape of
// bench/contention's index-count-hot workload: one type, therefore one shard.
const benchRelType = uint32(1)

// BenchmarkCountE_Serial prices the planner's read on an uncontended store —
// the cost every cardinality estimate pays whether or not anything is writing.
func BenchmarkCountE_Serial(b *testing.B) {
	s := New(0)
	s.Apply(EDelta(benchRelType, 1))
	b.ReportAllocs()
	b.ResetTimer()
	var sink int64
	for range b.N {
		sink += s.CountE(benchRelType)
	}
	if sink == 0 {
		b.Fatal("CountE returned 0 throughout: the cell is missing")
	}
}

// BenchmarkCountE_Parallel prices the same read under every core at once. It is
// the number that matters for a striped design: reads that were one load become
// a sum over stripes, and the sum's cache footprint is what this measures.
func BenchmarkCountE_Parallel(b *testing.B) {
	s := New(0)
	s.Apply(EDelta(benchRelType, 1))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var sink, n int64
		for pb.Next() {
			sink += s.CountE(benchRelType)
			n++
		}
		// Guarded on n: RunParallel's calibration pass can hand a goroutine
		// zero iterations, and an unguarded sink check turns that into a
		// spurious failure that DROPS the benchmark's result line.
		if n > 0 && sink == 0 {
			b.Error("CountE returned 0 throughout: the cell is missing")
		}
	})
}

// BenchmarkCountE_SpreadSerial reads across many relationship types, so the
// answer is not served from one warm line. It is the read shape a real schema
// produces, against BenchmarkCountE_Serial's single-key best case.
func BenchmarkCountE_SpreadSerial(b *testing.B) {
	const types = 4096
	s := New(0)
	for rt := range uint32(types) {
		s.Apply(EDelta(rt, 1))
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sink int64
	for i := range b.N {
		sink += s.CountE(uint32(i % types)) // G115: i%types is bounded by types.
	}
	if sink == 0 {
		b.Fatal("CountE returned 0 throughout: the cells are missing")
	}
}

// BenchmarkApply_HotParallel is the write path under contention: every
// goroutine increments the SAME cell. This is the defect index-count-hot
// measures, reduced to the smallest exercise that still shows it.
func BenchmarkApply_HotParallel(b *testing.B) {
	s := New(0)
	s.Apply(EDelta(benchRelType, 1))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Apply(EDelta(benchRelType, 1))
		}
	})
}

// BenchmarkApply_HotSerial is the same write with no contention at all, so a
// design's per-operation cost can be separated from its scaling. A striped
// counter is allowed to be slightly slower here; it is not allowed to be
// slower here AND no faster in parallel.
func BenchmarkApply_HotSerial(b *testing.B) {
	s := New(0)
	s.Apply(EDelta(benchRelType, 1))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s.Apply(EDelta(benchRelType, 1))
	}
}

// BenchmarkMixed_HotParallel reproduces index-count-hot's 1-in-10 write ratio
// in-process, so the microbenchmark and the contention observatory are driving
// the same shape and can corroborate one another.
func BenchmarkMixed_HotParallel(b *testing.B) {
	const writeEvery = 10
	s := New(0)
	s.Apply(EDelta(benchRelType, 1))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		var sink int64
		for pb.Next() {
			if i%writeEvery == 0 {
				s.Apply(EDelta(benchRelType, 1))
			} else {
				sink += s.CountE(benchRelType)
			}
			i++
		}
		// Guarded on i: see BenchmarkCountE_Parallel. A goroutine handed a
		// single iteration executes the WRITE branch and never reads.
		if i > writeEvery && sink == 0 {
			b.Error("CountE returned 0 throughout: the cell is missing")
		}
	})
}

// BenchmarkCells prices the footprint accessor across a realistic cell count.
// It takes every shard's exclusive lock, so a design that adds per-stripe state
// makes it walk more; it is on no request path, but a large regression here
// would slow the simulator's invariant checks.
func BenchmarkCells(b *testing.B) {
	for _, n := range []int{1, 1024} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := New(0)
			for rt := range uint32(n) { // G115: n is a small positive literal.
				s.Apply(EDelta(rt, 1))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if s.Cells() == 0 {
					b.Fatal("Cells() == 0")
				}
			}
		})
	}
}

// BenchmarkStoreFootprint reports the heap a single empty Store costs, so the
// module's minimum-resources mandate has a number rather than an assumption.
// The readers-biased shard lock buys its scaling with memory — a per-core slot
// array per shard, each slot owning a whole cache line — and this is where that
// price is stated.
//
// The metric is reported AFTER the timed loop, never before: b.ResetTimer
// deletes user-reported metrics, so a ReportMetric call ahead of it silently
// vanishes from the output.
func BenchmarkStoreFootprint(b *testing.B) {
	var before, after runtime.MemStats
	stores := make([]*Store, 0, b.N)
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range b.N {
		stores = append(stores, New(0))
	}
	runtime.ReadMemStats(&after)
	if len(stores) != b.N {
		b.Fatalf("built %d stores, want %d", len(stores), b.N)
	}
	runtime.KeepAlive(stores)
	b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/float64(b.N), "B/store")
}
