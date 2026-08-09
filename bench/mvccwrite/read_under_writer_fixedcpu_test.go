package mvccwrite

// read_under_writer_fixedcpu_test.go — the UNCONFOUNDED read-under-writer instrument
// (rmp #2342).
//
// # What was wrong with the previous one
//
// docs/benchmarks/mvcc-read-under-writer-2026-08-05.md measured read latency rising
// from 68.08 µs at 0 writers to 144.82 µs at 8 (+112.72%, n=6, p=0.002) and said
// plainly that it was NOT an attribution: the arms differ in TOTAL CPU DEMAND as well
// as in write rate, so a 0-writer arm is simply an idler machine. The figure is an
// UPPER BOUND on what concurrent writing costs a reader, and nothing more.
//
// The same report records that its first explanation was REFUTED — a CPU profile
// found neither suspectNodes nor correctBitmap in the top cumulative profile at all.
// So the largest measured read-side cost in the MVCC substrate was unattributed and
// the one hypothesis tried was wrong.
//
// # What this one holds fixed
//
// The number of BUSY GOROUTINES is constant across every arm. Only the split changes:
// arm `writers=k` runs k goroutines committing and (busy−k) running a cost-matched
// NON-writing unit, so the machine is equally loaded at k=0 and k=8 and the write
// rate is the only independent variable.
//
// The filler is [allocUnit], the same unit BenchmarkAllocScalingControl uses, padded
// to the measured cost of THIS BENCHMARK'S OWN WRITE STATEMENT. It allocates a
// commit's object count and volume and touches NO shared structure, so it consumes a
// writer's share of CPU and allocator without producing any version work.
//
// # The calibration target is measured here, and the first version got it wrong
//
// The first version calibrated the filler with [calibrateSpin] against
// [nsPerCommitTarget] — the cost of `CREATE (n:Account {id: $id})`, which is what
// BenchmarkAllocScalingControl matches. This benchmark's writer is a different
// statement: an indexed seek plus a SET. The mismatch showed up in the arms' own
// telemetry — fillers ran ~69k units where eight writers landed ~19k commits, so the
// filler was ~3.6x cheaper than the thing it was standing in for and the arms were
// NOT equally loaded.
//
// That is the same error the alloc control already recorded once: matching the SHAPE
// without matching the RATE matches nothing. So the target is measured, here, on the
// host and on the actual write statement, and the arms report both counts so the
// match can be checked rather than trusted.
//
// # What it therefore measures
//
// The difference between arms is version work and the read path's exposure to it —
// not CPU scarcity. A flat curve here against a rising curve in the confounded
// benchmark would mean the +112.72% was CPU competition; a curve that survives means
// concurrent writing genuinely costs the reader, and the next question is which
// mechanism.
//
// It is an INSTRUMENT, not a gate: no threshold is asserted.

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// fixedCPUBusy is the number of busy goroutines held constant across every arm.
//
// Eight, to match the writer counts the superseded benchmark used, so the two can be
// read against each other arm for arm.
const fixedCPUBusy = 8

// BenchmarkReadUnderWriter_FixedCPU reports reader latency against write rate with
// total CPU demand held fixed.
//
// Reported per arm: the reader's ns/op, the writes actually landed, and the filler
// units actually run. The last is the non-vacuity check — an arm whose filler did not
// run is an idler arm wearing a different name, and the confound is back.
func BenchmarkReadUnderWriter_FixedCPU(b *testing.B) {
	target := measureWriteUnitCost(b)
	pad := calibrateSpinTo(b, target)
	b.Logf("filler calibrated to %d spin iterations against this benchmark's own write "+
		"statement at %.0f ns/commit", pad, target)

	for _, writers := range []int{0, 1, 2, 4, 8} {
		b.Run("writers="+strconv.Itoa(writers), func(b *testing.B) {
			r := newRig(b, wiringMem)
			defer func() {
				if err := r.close(); err != nil {
					b.Errorf("close rig: %v", err)
				}
			}()
			seedFixedPopulation(b, r.eng)

			ctx := context.Background()
			var (
				stop    = make(chan struct{})
				wg      sync.WaitGroup
				written atomic.Int64
				filled  atomic.Int64
				werr    atomic.Pointer[error]
			)

			// k writers …
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						// Disjoint node per writer so writers do not conflict with each
						// other: this measures what writing costs a READER.
						id := int64((w*97 + i) % readUnderWriterNodes)
						if _, err := r.eng.RunInTx(ctx,
							"MATCH (n:Acct {id: $id}) SET n.bal = $v",
							map[string]expr.Value{
								"id": expr.IntegerValue(id),
								"v":  expr.IntegerValue(int64(i)),
							}); err == nil {
							written.Add(1)
						}
					}
				}(w)
			}

			// … and (busy − k) fillers, so the machine is equally loaded in every arm.
			for f := 0; f < fixedCPUBusy-writers; f++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						if err := allocUnit(pad); err != nil {
							e := err
							werr.Store(&e)
							return
						}
						filled.Add(1)
					}
				}()
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				res, err := r.eng.Run(ctx, "MATCH (n:Acct) RETURN count(n) AS c", nil)
				if err != nil {
					b.Fatalf("read: %v", err)
				}
				drain(b, res)
			}
			b.StopTimer()

			close(stop)
			wg.Wait()

			if p := werr.Load(); p != nil {
				b.Fatalf("filler failed: %v", *p)
			}
			b.ReportMetric(float64(written.Load()), "writes")
			b.ReportMetric(float64(filled.Load()), "filler-units")

			if writers > 0 && written.Load() == 0 {
				b.Fatalf("%d writers landed ZERO writes: this arm measured an idle graph and "+
					"its read figure is not a figure for a contended one", writers)
			}
			if writers < fixedCPUBusy && filled.Load() == 0 {
				b.Fatalf("%d filler goroutine(s) ran ZERO units: this arm is an IDLER arm "+
					"wearing a busy arm's name, and the CPU-demand confound this benchmark "+
					"exists to remove is back", fixedCPUBusy-writers)
			}
		})
	}
}

// measureWriteUnitCost times this benchmark's WRITE statement, single-goroutine, on a
// throwaway fixture, and returns its cost in nanoseconds.
//
// It is measured rather than taken from [nsPerCommitTarget] because that constant
// describes a different statement — see the file doc for what assuming it cost.
func measureWriteUnitCost(b *testing.B) float64 {
	b.Helper()
	r := newRig(b, wiringMem)
	defer func() {
		if err := r.close(); err != nil {
			b.Errorf("close calibration rig: %v", err)
		}
	}()
	seedFixedPopulation(b, r.eng)
	ctx := context.Background()

	const warm, reps = 200, 2000
	run := func(n int) {
		for i := 0; i < n; i++ {
			id := int64(i % readUnderWriterNodes)
			if _, err := r.eng.RunInTx(ctx,
				"MATCH (n:Acct {id: $id}) SET n.bal = $v",
				map[string]expr.Value{
					"id": expr.IntegerValue(id),
					"v":  expr.IntegerValue(int64(i)),
				}); err != nil {
				b.Fatalf("calibration write: %v", err)
			}
		}
	}
	run(warm) // plan cache, index binding and first-touch page faults
	start := time.Now()
	run(reps)
	return float64(time.Since(start).Nanoseconds()) / reps
}

// calibrateSpinTo is [calibrateSpin] against a caller-supplied target rather than the
// package constant, so a benchmark whose unit of work is not a bare CREATE can match
// its own.
func calibrateSpinTo(b *testing.B, targetNS float64) int {
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
	_ = measure(0) // warm up
	lo, hi := 0, 64
	for measure(hi) < targetNS && hi < 1<<22 {
		hi *= 2
	}
	for lo < hi {
		mid := (lo + hi) / 2
		if measure(mid) < targetNS {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
