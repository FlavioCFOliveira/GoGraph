package cypher

// read_phase_attribution_bench_test.go — rmp #2292: attribute the read-cost delta
// under a concurrent writer to a PHASE of the read path, per operation.
//
// # Why this exists, and why it is not a profile
//
// bench/mvccwrite establishes the fact: an indexed point lookup costs +12.38% when a
// single writer commits at 1000/s, against this task's ≤2.5% bound, and the overhead is
// FIXED per read rather than per row. It cannot say WHICH phase pays it, and three
// attempts to answer that with a CPU profile were each invalid for a different reason:
//
//	1. the wrong arm — profiling the 2000-row SCAN, where a sub-microsecond fixed cost is
//	   ~1% of the run and invisible;
//	2. a single-arm inference about a DIFFERENTIAL — a profile shows where time goes in
//	   THAT arm, which is not where the difference between two arms comes from;
//	3. un-normalised `pprof -base` — the two arms complete different numbers of reader
//	   iterations in the same wall-clock budget, so subtracting their sample counts
//	   measures the iteration-count difference, not the per-read cost difference. Every
//	   reader symbol came out negative, which is not a speed-up.
//
// So this measures per OPERATION instead of per RUN. It runs CUMULATIVE PREFIXES of the
// read path as separate benchmarks — parse, then +snapshot, then +build, then the whole
// read — with the identical throttled writer behind each. Differencing adjacent prefixes
// gives each phase's cost per read, and differencing the two writer arms gives the phase
// that carries the delta. A prefix benchmark cannot suffer any of the three failures
// above: both arms execute the same phases the same number of times, and the metric is
// already normalised per operation by the framework.
//
// The phases mirror [Engine.runRead] exactly and in its order. If that path changes,
// change this in step or the attribution silently stops meaning anything.

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// phaseAcctNodes matches bench/mvccwrite's population so the two instruments measure the
// same graph. The reader is an indexed seek, so this size is not load-bearing for the
// reader's cost — it is here so the writer's seek has the same index shape.
const phaseAcctNodes = 2000

// phaseWriteRate is bench/mvccwrite's realisticWriteRate: the commit rate at which the
// +12.38% breach was measured. Reproducing the breach is the precondition for
// attributing it, so this must not drift from that constant.
const phaseWriteRate = 1000

// phaseReadQuery is the indexed point lookup that exhibits the breach. A seek, not a
// scan: the overhead is fixed per read, so it is ~12% of this and ~4% of a 2000-row scan.
const phaseReadQuery = "MATCH (n:Acct {id: $id}) RETURN n.bal AS b"

// newPhaseRig builds the store-less engine, its index and its population. Store-less on
// purpose: durability cost is not what this task is attributing, and mixing it in would
// add a phase that the +12.38% measurement did not contain either.
func newPhaseRig(tb testing.TB) *Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	ctx := context.Background()
	if _, err := eng.RunInTx(ctx, "CREATE INDEX acct_id FOR (n:Acct) ON (n.id)", nil); err != nil {
		tb.Fatalf("CREATE INDEX: %v", err)
	}
	for i := 0; i < phaseAcctNodes; i++ {
		if _, err := eng.RunInTx(ctx, "CREATE (n:Acct {id: $id, bal: 0})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(i))}); err != nil {
			tb.Fatalf("seed %d: %v", i, err)
		}
	}
	return eng
}

// startPhaseWriters launches writers throttled to phaseWriteRate commits/s each, and
// returns a stop function that reports the achieved rate.
//
// Writers touch the TOP half of the id space and the reader the BOTTOM, exactly as
// bench/mvccwrite's indexed arm does: the reader must never seek a row a writer is
// versioning, so what is measured is the cost of writing HAPPENING AT ALL, not the cost
// of reading a contended row.
func startPhaseWriters(b *testing.B, eng *Engine, writers int) func() {
	return startPhaseWritersOfKind(b, eng, writers, writerSet)
}

// writerKind selects what the background transactions DO, so the shared state they
// touch can be bisected.
type writerKind int

const (
	// writerSet is the workload the +12.38% was measured under: an indexed seek plus a
	// property SET, which mints a new version on an existing chain.
	writerSet writerKind = iota
	// writerReadOnly commits a transaction that WRITES NOTHING. It still registers a
	// transaction, takes and releases a writer snapshot, and drives the whole
	// transaction machinery — but it mints no version and leaves no churn.
	//
	// It separates "a transaction exists concurrently" from "versions exist
	// concurrently", which the SET arm confounds.
	writerReadOnly
)

func startPhaseWritersOfKind(b *testing.B, eng *Engine, writers int, kind writerKind) func() {
	ctx := context.Background()
	var (
		stop    = make(chan struct{})
		wg      sync.WaitGroup
		written atomic.Int64
	)
	interval := time.Second / phaseWriteRate
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				id := int64(phaseAcctNodes/2 + (w*97+i)%(phaseAcctNodes/2))
				var err error
				switch kind {
				case writerReadOnly:
					_, err = eng.RunInTx(ctx, "MATCH (n:Acct {id: $id}) RETURN n.bal AS b",
						map[string]expr.Value{"id": expr.IntegerValue(id)})
				default:
					_, err = eng.RunInTx(ctx, "MATCH (n:Acct {id: $id}) SET n.bal = $v",
						map[string]expr.Value{
							"id": expr.IntegerValue(id),
							"v":  expr.IntegerValue(int64(i)),
						})
				}
				if err == nil {
					written.Add(1)
				}
			}
		}(w)
	}
	start := time.Now()
	return func() {
		elapsed := time.Since(start)
		close(stop)
		wg.Wait()
		got := written.Load()
		b.ReportMetric(float64(got), "writes")
		if writers == 0 {
			return
		}
		rate := float64(got) / elapsed.Seconds()
		b.ReportMetric(rate, "writes/s")
		// The framework probes with b.N=1 before it settles on a real iteration count,
		// and such a run finishes in nanoseconds — before the writer's first tick. Only
		// a run long enough to have permitted many ticks can be judged, so the guards
		// below are scoped to it. Without this the assertion fails every ramp-up probe
		// and no arm ever produces a figure.
		if elapsed < 100*interval {
			return
		}
		if got == 0 {
			b.Fatalf("%d writers landed ZERO writes over %s: this arm measured an idle "+
				"graph and its figure is not a figure for a contended one", writers, elapsed)
		}
		if ceiling := float64(writers*phaseWriteRate) * 2; rate > ceiling {
			b.Fatalf("the throttle did not hold: %0.f writes/s against an intended %d, so this "+
				"arm is a saturating one and cannot be compared with the +12.38%% figure",
				rate, writers*phaseWriteRate)
		}
	}
}

// readPhase names how much of [Engine.runRead] a prefix benchmark executes.
type readPhase int

const (
	// phaseParse is step 1: parse, analyse, plan-cache lookup, the write rejection,
	// the parameter presence and type checks, and the per-query now-aware registry.
	// Everything before the read view is opened.
	phaseParse readPhase = iota
	// phaseSnapshot adds step 1c: BeginRead and EndRead — the MVCC snapshot and its
	// reclamation-horizon slot. This is the phase an MVCC explanation predicts.
	phaseSnapshot
	// phaseBuild adds step 2: buildReadPhysical, which resolves every cost gate and
	// assembles the physical operator tree at the snapshot's instant.
	phaseBuild
	// phaseBuildAtPresent is phaseBuild with the snapshot WITHHELD from the builder —
	// the horizon slot is still taken and released, so that cost is identical, but the
	// build reads current values with no version walk.
	//
	// It is a DISCRIMINATOR, not a candidate implementation: passing nil is what the
	// rendering paths and a writer do, and it is not correct for a read. Differencing
	// it against phaseBuild separates "building AT AN INSTANT costs more when versions
	// exist" from "building costs more when a writer is running for any other reason".
	phaseBuildAtPresent
	// phaseFull adds step 3: exec.Run and materialise. The whole read.
	phaseFull
)

// runReadPrefix executes runRead up to and including phase. It is a deliberate
// transcription of [Engine.runRead] rather than a refactor of it: the production path
// must not grow a phase parameter for a benchmark's benefit, and a transcription that
// drifts is caught by TestReadPhasePrefixMatchesRunRead.
func (e *Engine) runReadPrefix(ctx context.Context, phase readPhase, query string, params map[string]expr.Value) error {
	entry, _, err := e.parseAndAnalyse(query)
	if err != nil {
		return err
	}
	if entry.semaErr != nil {
		return entry.semaErr
	}
	plan := entry.plan
	// entry.containsWrite, not ir.ContainsWrite(plan): runRead reads the memo
	// since rmp #2693, and the whole value of this transcription is that it
	// executes what runRead executes. Walking the plan here would leave three
	// per-node Children allocations in the 1parse arm that the production path no
	// longer makes, and the attribution would describe a read nobody performs.
	if entry.containsWrite {
		return ErrWriteInReadOnlyTx
	}
	if err := checkParamPresence(entry.paramRefs, params); err != nil {
		return err
	}
	if err := checkParamTypesCached(entry, params); err != nil {
		return err
	}
	queryReg := newNowAwareRegistry(e.reg, time.Now())
	if phase == phaseParse {
		return nil
	}

	snap := e.g.BeginRead()
	defer e.g.EndRead(snap)
	if phase == phaseSnapshot {
		return nil
	}

	buildAt := snap
	if phase == phaseBuildAtPresent {
		buildAt = nil
	}
	op, cols, err := e.buildReadPhysical(ctx, entry, plan, params, queryReg, nil, buildAt)
	if err != nil {
		return err
	}
	if phase == phaseBuild || phase == phaseBuildAtPresent {
		return nil
	}

	rs := exec.Run(ctx, op, cols)
	r := newResultWithLimit(rs, cols, nil, nil, nil, e.maxResultRows, e.maxResultBytes)
	r.globalMem = e.globalMem
	r.notifications = entry.notifications
	r.materialize()
	for r.Next() {
		_ = r.Record()
	}
	return r.Err()
}

// BenchmarkReadPhaseReadOnlyWriter is the second discriminator: same graph, same
// transaction rate, same machinery — but the background transactions MINT NO VERSION.
//
// Differencing it against BenchmarkReadPhaseAttribution separates the cost of a
// concurrent TRANSACTION from the cost of concurrent VERSIONS. Only the second is
// version work; the first is transaction-registration state that a reader shares with
// any concurrent transaction whether it writes or not.
func BenchmarkReadPhaseReadOnlyWriter(b *testing.B) {
	phases := []struct {
		name string
		p    readPhase
	}{
		{"1parse", phaseParse},
		{"2snapshot", phaseSnapshot},
		{"3build", phaseBuild},
		{"4full", phaseFull},
	}
	for _, ph := range phases {
		b.Run(ph.name, func(b *testing.B) {
			for _, writers := range []int{0, 1} {
				b.Run("writers="+strconv.Itoa(writers), func(b *testing.B) {
					eng := newPhaseRig(b)
					stop := startPhaseWritersOfKind(b, eng, writers, writerReadOnly)
					ctx := context.Background()
					params := map[string]expr.Value{"id": expr.IntegerValue(0)}

					b.ResetTimer()
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						params["id"] = expr.IntegerValue(int64(i % (phaseAcctNodes / 2)))
						if err := eng.runReadPrefix(ctx, ph.p, phaseReadQuery, params); err != nil {
							b.Fatalf("phase %s: %v", ph.name, err)
						}
					}
					b.StopTimer()
					stop()
				})
			}
		})
	}
}

// BenchmarkReadPhaseForeignWriter is the discriminator that separates SHARED STATE from
// AMBIENT LOAD.
//
// The writers here commit against a SECOND, independent engine holding an identical
// graph. They therefore impose the same goroutine count, the same allocation rate, the
// same scheduler pressure and the same CPU demand as the sibling benchmark's writers —
// and they touch not one byte the reader touches. No shared version chain, no shared
// horizon, no shared registry, no shared lock word.
//
// So the two benchmarks differ in exactly one thing, and differencing them is the whole
// point:
//
//   - a delta that SURVIVES here is the cost of another goroutine merely existing —
//     scheduling, GC, memory bandwidth — and nothing the MVCC design can be charged with;
//   - a delta that VANISHES here is the cost of sharing state with a writer, which is
//     precisely what this sprint's concurrency control is responsible for.
func BenchmarkReadPhaseForeignWriter(b *testing.B) {
	phases := []struct {
		name string
		p    readPhase
	}{
		{"1parse", phaseParse},
		{"2snapshot", phaseSnapshot},
		{"3build", phaseBuild},
		{"4full", phaseFull},
	}
	for _, ph := range phases {
		b.Run(ph.name, func(b *testing.B) {
			for _, writers := range []int{0, 1} {
				b.Run("writers="+strconv.Itoa(writers), func(b *testing.B) {
					eng := newPhaseRig(b)
					var foreign *Engine
					if writers > 0 {
						foreign = newPhaseRig(b)
					}
					stop := startPhaseWriters(b, foreign, writers)
					ctx := context.Background()
					params := map[string]expr.Value{"id": expr.IntegerValue(0)}

					b.ResetTimer()
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						params["id"] = expr.IntegerValue(int64(i % (phaseAcctNodes / 2)))
						if err := eng.runReadPrefix(ctx, ph.p, phaseReadQuery, params); err != nil {
							b.Fatalf("phase %s: %v", ph.name, err)
						}
					}
					b.StopTimer()
					stop()
				})
			}
		})
	}
}

// BenchmarkReadPhaseAttribution measures each cumulative prefix of the read path at 0 and
// 1 throttled writers.
//
// Read it by DIFFERENCING, twice. Adjacent prefixes within one writer arm give each
// phase's absolute cost; the same phase across the two arms gives the part of the
// +12.38% that phase carries. A phase whose cost is unchanged between the arms is
// exonerated however expensive it is in absolute terms — which is precisely the
// distinction the single-arm profile could not draw, and why it named plan building
// (large in both arms, therefore not the delta).
func BenchmarkReadPhaseAttribution(b *testing.B) {
	phases := []struct {
		name string
		p    readPhase
	}{
		{"1parse", phaseParse},
		{"2snapshot", phaseSnapshot},
		{"3build", phaseBuild},
		{"3xbuild_at_present", phaseBuildAtPresent},
		{"4full", phaseFull},
	}
	for _, ph := range phases {
		b.Run(ph.name, func(b *testing.B) {
			for _, writers := range []int{0, 1} {
				b.Run("writers="+strconv.Itoa(writers), func(b *testing.B) {
					eng := newPhaseRig(b)
					stop := startPhaseWriters(b, eng, writers)
					ctx := context.Background()
					params := map[string]expr.Value{"id": expr.IntegerValue(0)}

					b.ResetTimer()
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						params["id"] = expr.IntegerValue(int64(i % (phaseAcctNodes / 2)))
						if err := eng.runReadPrefix(ctx, ph.p, phaseReadQuery, params); err != nil {
							b.Fatalf("phase %s: %v", ph.name, err)
						}
					}
					b.StopTimer()
					stop()
				})
			}
		})
	}
}
