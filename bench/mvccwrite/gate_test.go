package mvccwrite

// gate_test.go — the short-layer regression gates on write concurrency
// (rmp #2297).
//
// The benchmark in scaling_test.go produces the sprint's number. This file
// makes it binding: a change that re-serialises the writers turns `make ci` RED
// rather than merely slow.
//
// # Two instruments, because one of them cannot survive a shared machine
//
// [measureScaling] is the headline instrument: throughput at N writers divided
// by throughput at ONE writer. It is what "write throughput scales with writer
// count" means, and it is the number the audit and docs/benchmarks quote.
//
// It also has a systematic bias that a shared machine makes severe. A process
// with more runnable threads gets a larger share of a loaded host, so the
// one-writer arm is starved harder than the N-writer arm and the RATIO comes
// out too high. This is not noise that averages away; it is a bias with a
// direction. It was found the hard way — the first version of this file turned
// `make ci` red with a one-writer arm running 4.2x slower than in isolation
// while the eight-writer arm lost only 1.2x, reporting 3.05x for work that was
// serialised under a single mutex.
//
// [measureSerialisationRatio] is the load-immune instrument, and it is the one
// with a future. It compares throughput at N writers against throughput of the
// SAME work at the SAME N writers with every unit taken under one external
// mutex. Both arms have identical goroutine counts, so whatever share of the
// machine the process gets, it gets in both — the bias cancels. What it reads
// is direct: how much does adding a global lock to the write path cost? On an
// engine that already holds a global lock, the answer is nothing.
//
// Measured on an Apple M4 (10 cores) at head c97118fe, under `-race`, first
// idle and then with a CPU load generator saturating every core at 2x:
//
//	                              idle          loaded (10 cores at 2x)
//	engine, serialisation ratio   0.97–1.02     0.73–0.76
//	parallel control, same ratio  6.92–7.26     3.41–6.93
//
// The two populations do not overlap in either condition. That an external
// global mutex around every engine write costs 0% is the defect stated in one
// number, and it is the number that must move.
//
// # The gates are RATCHETS, exactly like tckExecutionBaseline
//
// Every assertion that gates the ENGINE is a FLOOR — the robust direction,
// since both instruments degrade under load and a floor only ever gets easier
// to clear. The one ceiling in the file is asserted on the load-immune
// instrument only, for the reason given in
// [TestWriteScalingInstrument_SeesSerialisation].
//
// The floors start at their ENTRY values, because at entry the engine is
// single-writer by construction and there is no higher number to enforce. Each
// task that raises the measured value RAISES the constant; a constant is never
// lowered to make a red build green.

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// writeScalingFloor is the minimum ratio [TestWriteScalingGate] accepts:
// throughput at gateWriters concurrent writers divided by throughput at one
// writer, on the store-less engine.
//
// # Why 0.60
//
// Measured at head c97118fe on an Apple M4 (10 cores) with the race detector
// enabled — which is how `make ci` runs it, and which costs ten times the
// workload itself:
//
//	go test -race -run=TestWriteScalingGate -count=5 ./bench/mvccwrite/
//	=> 0.873, 0.891, 0.895, 0.880, 0.887   (worst 0.873, spread 2.5%)
//	   0.794 with every core saturated by an external load generator
//
// The floor sits 31% below the worst of the five idle runs and 24% below the
// loaded one. That is far more headroom than the spread needs, deliberately: a
// gate that goes red on a busy machine gets softened, and a softened gate is
// not a gate.
//
// # This value is WEAK, and that is a statement about the engine
//
// A floor below 1.0 can only catch a serialised engine becoming MORE
// serialised. It cannot catch the ABSENCE of scaling, because at head c97118fe
// there is no scaling to lose: cypher.Engine.writeMu (cypher/api.go:1069) and
// lpg.Graph.visMu (graph/lpg/lpg.go:565) make the write path single-writer by
// construction. Both gates acquire their power when rmp #2304 retires the
// barrier and these constants are ratcheted to [writeScalingTarget] or above.
const writeScalingFloor = 0.60

// writeConcurrencyFloor is the minimum ratio [TestWriteConcurrencyGate]
// accepts: how much faster the engine is WITHOUT an external global mutex
// serialising its writes than with one, at the same writer count.
//
// # Why 0.50
//
// Measured at head c97118fe under `-race`, best of gateRepeats: 0.97–1.02 idle,
// 0.73–0.76 with every core saturated. The floor sits 32% below the worst
// loaded observation.
//
// A value of 1.0 means an external global mutex around every write is free —
// the engine was already serialised, so nothing was taken away. That is exactly
// what the module does today, and it is why this floor is below 1.0 rather than
// above it. When rmp #2304 lets writers apply concurrently, the same
// measurement rises to the parallel population (measured 6.9–7.3 on the control
// workload) and this constant is ratcheted to [writeScalingTarget] or above.
const writeConcurrencyFloor = 0.50

// writeScalingTarget is where [writeScalingFloor] and [writeConcurrencyFloor]
// both go once rmp #2304 retires the exclusive visibility barrier: 3x at
// gateWriters=8 concurrent writers, i.e. 37.5% parallel efficiency on an
// eight-core machine.
//
// It is deliberately far below linear. The sprint's claim is that throughput
// SCALES, not that it scales perfectly: a WAL append, a commit-timestamp mint
// and a publication step remain genuinely serial sections even under ideal
// MVCC, and Amdahl bounds what is left. 3x is the number that separates "the
// writers run concurrently" from "the writers do not", with room on either side
// for a loaded machine — the parallel control clears it by 2.3x even with every
// core saturated.
//
// Today it is used by the two instrument-validation tests, which show that both
// instruments have the power the gates will need once they are ratcheted to it.
const writeScalingTarget = 3.0

// walWriteScalingFloor is the minimum ratio [TestWALWriteScalingGate] accepts on
// the WAL-backed engine: throughput at gateWriters concurrent writers divided by
// throughput at one writer.
//
// # This is the gate with power, and it is the one rmp #2304 earned
//
// The two floors above measure the store-less wiring, where the outermost lock is
// cypher.Engine.writeMu and NOT the barrier: lockWriter takes writeMu only when
// no store is attached (cypher/api.go:1188-1190), and it is held for the whole
// statement. So retiring visMu could not and did not move that arm — it measures
// 0.838x at sixteen writers after #2304 against 0.835x before — and the audit says
// so directly (docs/audit-mvcc-sole-cc-2026-08-02.md §3.1: the store-less baseline
// "is writeMu + visMu and nothing else", and removing the other mechanisms without
// the apply gate "buys nothing"). Ratcheting those two constants to
// [writeScalingTarget] now would turn `make ci` red over a lock rmp #2306 owns, so
// they stay where they are, with this note explaining why rather than leaving the
// header's ratchet plan looking unfulfilled.
//
// The WAL arm is the one visMu was gating, and it moved by an order of magnitude.
// wal.Writer.SyncGroup's leader/follower coalescing was already built and already
// unreachable, because visMu spanned the fsync while the store semaphore had been
// released just before it (audit §11, steps 9c/9d). Measured medians over
// -count=10 at -benchtime=400x, before and after:
//
//	writers   before             after              scaling
//	      1   266.6 commits/s    267.0 commits/s     1.000
//	      2   268.4              380.2               1.424
//	      4   268.0              550.1               2.060
//	      8   268.2             1096.0               4.104
//	     16   268.0             2161.0               8.094
//	     32   270.0             4041.0             15.130
//
// Flat at 1.00x across a 32x change in offered concurrency became 15.1x, with the
// single-writer arm unchanged — so nothing was traded for it.
//
// # RATCHETED to writeScalingTarget (rmp #2320)
//
// The WAL arm is the one the exclusive barrier gates, and rmp #2304 measured what
// removing it is worth — then had to put it back, because the write path did not
// yet CARRY its transaction. rmp #2320 threaded it, the shared bracket landed for
// good, and this floor moved from a "does not get WORSE" 0.90 to the sprint's
// actual target.
//
// Measured at the ratchet, best of gateRepeats per run:
//
//	go test -count=3 -run=TestWALWriteScalingGate ./bench/mvccwrite/
//	=> 4.99x, 5.12x, 4.27x   (worst single observation 3.22x)
//
//	go test -race -count=3 -run=TestWALWriteScalingGate ./bench/mvccwrite/
//	=> 4.26x, 4.65x, 4.97x   (worst single observation 3.16x)
//
// So the gate clears its floor by 42% at worst and the worst SINGLE observation
// still clears it, which is the margin that matters since the gate reads the best
// of three: a loaded host can only depress a ratio, and a build that has gone back
// to serialising writers cannot reach 3x even once. Before the ratchet this arm was
// flat at ~1.00x, because visMu spanned the fsync and wal.Writer.SyncGroup's
// leader/follower coalescing was unreachable.
//
// The full sweep at higher concurrency is in scaling_test.go and recorded in
// docs/benchmarks/; it reaches 15.13x at 32 writers.
const walWriteScalingFloor = writeScalingTarget

const (
	// gateWriters is the concurrency the gates measure at. Eight is below the
	// core count of any machine this is expected to run on, so a failure means
	// serialisation rather than oversubscription.
	gateWriters = 8
	// walGateOps is [gateOps] for the WAL-backed arm, three orders of magnitude
	// smaller because every commit there pays a real fsync: a single-writer arm
	// runs at ~267 commits/s against the store-less engine's ~338 000, so gateOps
	// would take 45 seconds per arm and blow the short layer's budget on its own.
	walGateOps = 160
	// gateOps is the total number of units of work per arm, split across the
	// writers. Sized so one arm costs tens of milliseconds without the race
	// detector and stays comfortably inside the short layer's per-package
	// budget with it.
	gateOps = 12000
	// gateRepeats is how many times each arm pair is measured. Every assertion
	// in this file reads the BEST ratio observed except the one ceiling, which
	// reads the worst. Taking the best is the correct statistic for a floor:
	// contention for the host can only depress a ratio, so the maximum removes
	// false failures, while a genuinely serialised build cannot produce a high
	// ratio even once.
	gateRepeats = 3
)

// spread is the range of a repeated ratio measurement. `n` is what makes it
// initialisable: a zero min cannot be distinguished from an unset one, and a
// sentinel drawn from the value space cannot carry "nothing measured yet".
type spread struct {
	min, max float64
	n        int
}

func (s spread) observe(v float64) spread {
	if s.n == 0 || v < s.min {
		s.min = v
	}
	if s.n == 0 || v > s.max {
		s.max = v
	}
	s.n++
	return s
}

// passesGate is the verdict every gate in this file returns. The two
// instrument-validation tests route through it as well, so what they
// demonstrate is the REAL predicate flipping, not a restatement of it that
// could drift away from the gates it claims to validate.
func passesGate(ratio, floor float64) bool { return ratio >= floor }

// measureScaling runs `unit` twice — once on a single writer, once on `writers`
// writers — with the same total number of units, and returns the spread of the
// throughput ratio over [gateRepeats] repetitions.
//
// `unit` receives the writer index and the per-writer sequence number, so a
// caller can give each writer a disjoint key space.
//
// Read [spread.max] for a floor. Do NOT assert a ceiling on this instrument on
// a machine that is not idle: see this file's header for the bias and the
// evidence.
func measureScaling(tb testing.TB, writers, totalOps int, label string, unit func(writer, i int) error) spread {
	tb.Helper()
	var s spread
	for r := 0; r < gateRepeats; r++ {
		one := mustRunArm(tb, 1, totalOps, unit)
		many := mustRunArm(tb, writers, totalOps/writers, unit)
		ratio := many.commitsPerSec() / one.commitsPerSec()
		tb.Logf("%s scaling: 1 writer %.0f/s (%.0f ns), %d writers %.0f/s (%.0f ns) => %.3fx",
			label, one.commitsPerSec(), one.nsPerCommit(),
			many.writers, many.commitsPerSec(), many.nsPerCommit(), ratio)
		s = s.observe(ratio)
	}
	return s
}

// measureSerialisationRatio runs `unit` twice at the SAME writer count — once
// as given, once with every invocation taken under one external mutex — and
// returns the spread of the throughput ratio over [gateRepeats] repetitions.
//
// Because both arms run the same number of goroutines, they compete for the
// host on equal terms and the bias that makes [measureScaling] unusable on a
// busy machine cancels out. What the ratio measures is how much of the work was
// genuinely concurrent: a workload that already serialises itself loses nothing
// when a second lock is added, and reports ~1.0.
func measureSerialisationRatio(tb testing.TB, writers, totalOps int, label string, unit func(writer, i int) error) spread {
	tb.Helper()
	var s spread
	for r := 0; r < gateRepeats; r++ {
		free := mustRunArm(tb, writers, totalOps/writers, unit)
		var serialiser sync.Mutex
		locked := mustRunArm(tb, writers, totalOps/writers, func(w, i int) error {
			serialiser.Lock()
			defer serialiser.Unlock()
			return unit(w, i)
		})
		ratio := free.commitsPerSec() / locked.commitsPerSec()
		tb.Logf("%s serialisation: %d writers free %.0f/s, under one mutex %.0f/s => %.3fx",
			label, writers, free.commitsPerSec(), locked.commitsPerSec(), ratio)
		s = s.observe(ratio)
	}
	return s
}

// mustRunArm is [runArm] with the error and the empty-arm case turned into test
// failures, since neither is a measurement.
//
// A SERIALIZATION CONFLICT is called out by name (rmp #2300 AC5). Every gate in
// this file drives [commit], which gives each writer a disjoint 2^40 id space, so
// two writers never touch the same object and a first-updater-wins refusal here is
// not a legitimate outcome — it means conflict detection is refusing writers that do
// not conflict, which would make the whole measurement meaningless as well as being
// a defect in its own right. Reported as itself rather than as a generic "arm
// failed", so the message says what broke.
func mustRunArm(tb testing.TB, writers, perWriter int, unit func(writer, i int) error) arm {
	tb.Helper()
	got, err := runArm(writers, perWriter, unit)
	if errors.Is(err, mvcc.ErrSerializationConflict) {
		tb.Fatalf("%d-writer arm reported a SERIALIZATION CONFLICT on a DISJOINT key "+
			"space: %v.\nEach writer owns 2^40 ids of its own (see commit), so no two "+
			"writers touch the same object and no conflict is possible between them. "+
			"Either the conflict predicate is refusing disjoint writers, or a writer is "+
			"colliding with its OWN earlier transaction because the commit frontier has "+
			"not advanced past it (rmp #2298) — the second is legitimate MVCC and means "+
			"this gate needs a retry, not a softened floor.", writers, err)
	}
	if err != nil {
		tb.Fatalf("%d-writer arm: %v", writers, err)
	}
	if got.commitsPerSec() <= 0 {
		tb.Fatalf("%d-writer arm made no progress", writers)
	}
	return got
}

// newGateEngine builds the store-less engine the gates measure — the wiring in
// which nothing but the concurrency control can be responsible for the answer —
// with its parse and plan-cache costs already paid.
func newGateEngine(t *testing.T) (*cypher.Engine, context.Context) {
	t.Helper()
	r := newRig(t, wiringMem)
	t.Cleanup(func() {
		if err := r.close(); err != nil {
			t.Errorf("close rig: %v", err)
		}
	})
	warmUp(t, r.eng)
	return r.eng, context.Background()
}

// TestWriteScalingGate gates the sprint's headline number: concurrent writers
// must deliver at least [writeScalingFloor] times the throughput of one writer.
func TestWriteScalingGate(t *testing.T) {
	eng, ctx := newGateEngine(t)
	got := measureScaling(t, gateWriters, gateOps, "engine/mem", func(writer, i int) error {
		return commit(ctx, eng, writer, i)
	})

	if !passesGate(got.max, writeScalingFloor) {
		t.Fatalf("write scaling regressed: %d writers deliver %.3fx the throughput of one (best of %d), floor is %.2fx.\n"+
			"Either a change re-serialised the write path, or the machine is loaded. Re-run on an idle machine "+
			"before touching the floor: a softened gate is not a gate.",
			gateWriters, got.max, gateRepeats, writeScalingFloor)
	}
}

// newWALGateEngine builds the WAL-backed engine — the durable production wiring,
// in which the store's single-writer semaphore is released after the WAL append so
// concurrent committers can share one fsync, and in which visMu used to prevent
// exactly that.
func newWALGateEngine(t *testing.T) (*cypher.Engine, context.Context) {
	t.Helper()
	r := newRig(t, wiringWAL)
	t.Cleanup(func() {
		if err := r.close(); err != nil {
			t.Errorf("close rig: %v", err)
		}
	})
	warmUp(t, r.eng)
	return r.eng, context.Background()
}

// TestWALWriteScalingGate gates what rmp #2304 delivered: on the durable wiring,
// concurrent writers must deliver at least [walWriteScalingFloor] times the
// throughput of one.
//
// It is the regression test for the barrier's removal. Restoring an exclusive hold
// anywhere across the commit path — the fsync in particular — returns this arm to
// the flat 1.00x it measured before, which is far below the floor.
func TestWALWriteScalingGate(t *testing.T) {
	eng, ctx := newWALGateEngine(t)
	got := measureScaling(t, gateWriters, walGateOps, "engine/wal", func(writer, i int) error {
		return commit(ctx, eng, writer, i)
	})

	if !passesGate(got.max, walWriteScalingFloor) {
		t.Fatalf("WAL write scaling regressed: %d writers deliver %.3fx the throughput of one (best of %d), "+
			"floor is %.2fx.\nThe usual cause is an exclusive lock reintroduced across the commit path, which "+
			"makes wal.Writer.SyncGroup's leader/follower coalescing unreachable again and returns this arm to "+
			"1.00x. Re-run on an idle machine before touching the floor: a softened gate is not a gate.",
			gateWriters, got.max, gateRepeats, walWriteScalingFloor)
	}
}

// TestWriteConcurrencyGate gates the load-immune measure: putting an external
// global mutex around every engine write must cost at least
// [writeConcurrencyFloor] of the throughput. It is the gate that will detect a
// silently re-serialised write path once rmp #2304 lands, because unlike
// [TestWriteScalingGate] its verdict does not depend on how busy the host is.
func TestWriteConcurrencyGate(t *testing.T) {
	eng, ctx := newGateEngine(t)
	got := measureSerialisationRatio(t, gateWriters, gateOps, "engine/mem", func(writer, i int) error {
		return commit(ctx, eng, writer, i)
	})

	if !passesGate(got.max, writeConcurrencyFloor) {
		t.Fatalf("write concurrency regressed: the write path runs only %.3fx faster without an external global "+
			"mutex than with one (best of %d), floor is %.2fx. It is doing less concurrent work than it was.",
			got.max, gateRepeats, writeConcurrencyFloor)
	}
}

// spinSink keeps the compiler from eliminating the control workload.
var spinSink atomic.Uint64

// spinIterations sizes one spinUnit at roughly 20 microseconds on a modern
// core — long enough that goroutine scheduling is noise against it, short
// enough that a whole arm is milliseconds.
const spinIterations = 20000

// spinUnit is the synthetic unit of work the instrument-validation tests
// measure: a dependent multiply-add chain, which occupies one core and shares
// nothing, so N of them on N cores take the same wall-clock time as one.
func spinUnit(int, int) error {
	var x uint64 = 1
	for i := 0; i < spinIterations; i++ {
		x = x*6364136223846793005 + 1442695040888963407
	}
	spinSink.Add(x)
	return nil
}

// requireCores skips when the machine cannot demonstrate the parallelism these
// tests are about. This is an environment precondition, not unfinished work:
// with fewer cores than writers the concurrent arm is oversubscribed and the
// measurement says nothing about the instrument.
func requireCores(t *testing.T) {
	t.Helper()
	if n := runtime.NumCPU(); n < gateWriters {
		t.Skipf("needs at least %d cores to demonstrate parallelism; this machine has %d", gateWriters, n)
	}
}

// minControlSpeedup is the parallel speed-up the CONTROL workload must actually
// achieve before the serialisation instrument's verdict means anything.
//
// It is deliberately far below the ideal gateWriters: the point is not to demand
// near-linear scaling, only to establish that the machine has spare capacity at
// all. Half the writer count is comfortably clear of the ~1.2x seen on a saturated
// machine (below) and comfortably below the ~6.1x a quiet one delivers.
const minControlSpeedup = gateWriters / 2

// requireAvailableParallelism skips when the machine cannot currently SUPPLY the
// parallelism the instrument checks depend on, as opposed to merely owning enough
// cores for it.
//
// # Why runtime.NumCPU() is not enough
//
// [requireCores] asks whether the cores EXIST. It cannot see whether they are
// free, and `make ci` runs this package inside `go test -race ./...`, which
// executes packages in parallel — so these CPU-bound control arms compete with
// the rest of the module's race suite for the same cores.
//
// That matters because the two instruments are sensitive to load in OPPOSITE
// directions, despite the file comment above calling the serialisation one
// load-immune:
//
//   - measureScaling compares 1 writer against gateWriters. Under load BOTH arms
//     slow, so the ratio survives and can even inflate — a 9.177x was observed on
//     a machine whose true value is ~6x.
//   - measureSerialisationRatio compares gateWriters FREE against gateWriters
//     under one mutex. Under load the free arm collapses toward the serialised
//     one, so the ratio COMPRESSES.
//
// Measured, same code, same build, same machine:
//
//	load average 18, inside `go test -race ./...`:
//	    8 writers free  50380/s vs 1 writer 41384/s  =  1.2x available parallelism
//	    serialisation ratio 2.452x  ->  FAILED the 3.00x target
//	quiet machine, also under -race:
//	    8 writers free 288181/s vs 1 writer 47249/s  =  6.1x available parallelism
//	    serialisation ratio 6.900x  ->  passed comfortably
//
// Against minControlSpeedup = gateWriters/2 = 4, those two REAL measurements
// decide correctly and with a wide margin:
//
//	50380/41384  = 1.22x  ->  SKIP    (the failing run: no verdict is reported)
//	288181/47249 = 6.10x  ->  ASSERT  (the quiet run: the 6.90x check still runs)
//
// So the red was the machine, not the instrument and not the engine — and a gate
// that reports a false NO-GO trains people to ignore gates. The honest outcome
// when the machine cannot answer the question is to say so, which is the same
// environment-precondition reasoning [requireCores] already applies one step
// earlier.
//
// This cannot mask a genuine instrument defect: that would show up as a LOW
// serialisation ratio while available parallelism is HIGH, and the assertion still
// runs in exactly that case.
//
// measureAvailableParallelism returns how many times faster [gateWriters] free
// CPU-bound writers are than one, which is the CEILING any instrument built on
// the same synthetic workload can report: work that cannot run in parallel on
// this host at this moment cannot be shown to lose parallelism when serialised.
//
// It measures and reports; deciding what a given value means belongs to the
// caller, because the two callers need different verdicts from it — one treats a
// low value as an unmet precondition, the other as evidence that a shortfall it
// just observed came from the machine rather than from the instrument.
func measureAvailableParallelism(tb testing.TB, ops int) float64 {
	tb.Helper()
	one := mustRunArm(tb, 1, ops, spinUnit)
	many := mustRunArm(tb, gateWriters, ops/gateWriters, spinUnit)
	return many.commitsPerSec() / one.commitsPerSec()
}

// It returns the measured speed-up so a caller can report it.
func requireAvailableParallelism(t *testing.T, ops int) float64 {
	t.Helper()
	speedup := measureAvailableParallelism(t, ops)
	if speedup < minControlSpeedup {
		t.Skipf("the machine is not currently supplying the parallelism this check measures: "+
			"%d free CPU-bound writers scaled only %.2fx over 1 writer, below the %dx this "+
			"precondition needs. Nothing is concluded about the instrument; re-run with no "+
			"competing load (see the doc comment for the measured numbers).",
			gateWriters, speedup, minControlSpeedup)
	}
	return speedup
}

// TestWriteScalingInstrument_SeesConcurrency validates the positive direction of
// both instruments. An instrument that cannot fail proves nothing, and the usual
// validation — run the gate against a build that has the defect — is not
// available here in the usual direction: the only build that exists HAS the
// defect, and injecting more serialisation into an already fully serialised
// engine barely moves either ratio, because both arms slow down together.
//
// So the validation points the gates' own measurement code at a synthetic
// CPU-bound workload that genuinely runs in parallel. Both instruments must
// clear [writeScalingTarget] — the value the gates ratchet to once rmp #2304
// lands — so the demonstration is against the number they will actually
// enforce.
func TestWriteScalingInstrument_SeesConcurrency(t *testing.T) {
	requireCores(t)
	// A CONTROL, so its precondition is the environment's ability to run two arms
	// differently — which coverage instrumentation removes by making every basic
	// block longer and more uniform. Measured under `make cover-gate`: available
	// parallelism probed 13.63x while the serialisation ratio compressed to 2.432x
	// and reported a false NO-GO (rmp #2319). The ENGINE gates above keep asserting
	// here; this control asserts in the -race arm of the same `make ci`.
	testlayers.RequireUninstrumented(t, "the serialisation ratio of genuinely parallel "+
		"CPU-bound work forced through one mutex, which must exceed the sprint's scaling target")
	const ops = gateOps / 4
	// Probe the machine BEFORE asserting anything, and skip rather than return a
	// false verdict when it cannot supply the parallelism either instrument needs.
	// See [requireAvailableParallelism] for the measurement that motivated this.
	t.Logf("available parallelism: %.2fx", requireAvailableParallelism(t, ops))

	scaling := measureScaling(t, gateWriters, ops, "control/parallel", spinUnit)
	if !passesGate(scaling.max, writeScalingTarget) {
		t.Fatalf("the scaling instrument cannot see concurrency: %d independent CPU-bound writers on %d cores "+
			"measured only %.3fx (best of %d), below the sprint target of %.2fx. TestWriteScalingGate reads the "+
			"same measurement, so until this passes its verdict cannot be trusted.",
			gateWriters, runtime.NumCPU(), scaling.max, gateRepeats, writeScalingTarget)
	}

	serial := measureSerialisationRatio(t, gateWriters, ops, "control/parallel", spinUnit)
	if !passesGate(serial.max, writeScalingTarget) {
		// A shortfall here has two possible causes, and they demand opposite verdicts:
		// the instrument is blind (a real failure, which is what this control exists to
		// catch), or the machine stopped supplying the parallelism the instrument needs
		// (an unmet precondition, which concludes nothing). The probe above cannot
		// separate them, because it runs BEFORE this measurement and measures the
		// 1-vs-N quantity that [measureSerialisationRatio] deliberately avoids. Drift
		// across a single run is real and was measured: in the run that motivated this
		// (rmp #2326), the identical 8-writer free arm reported 100k-150k/s during
		// [measureScaling] and only 65k-89k/s here, minutes later.
		//
		// So re-probe NOW, against the target this assertion enforces rather than
		// against minControlSpeedup: a ratio cannot exceed the parallelism actually
		// available while it is being measured.
		if avail := measureAvailableParallelism(t, ops); !passesGate(avail, writeScalingTarget) {
			t.Skipf("the machine stopped supplying the parallelism this check measures: the "+
				"serialisation ratio reached only %.3fx against a %.2fx target, and re-probing "+
				"immediately afterwards found just %.2fx of available parallelism — which is the "+
				"ceiling of the ratio itself. Nothing is concluded about the instrument; re-run "+
				"with no competing load.", serial.max, writeScalingTarget, avail)
		}
		t.Fatalf("the serialisation instrument cannot see concurrency: forcing genuinely parallel work through "+
			"one mutex cost only %.3fx (best of %d), below the sprint target of %.2fx. TestWriteConcurrencyGate "+
			"reads the same measurement, so until this passes its verdict cannot be trusted.",
			serial.max, gateRepeats, writeScalingTarget)
	}
}

// TestWriteScalingInstrument_SeesSerialisation validates the negative direction:
// the same work, changed in exactly one respect — every unit is already taken
// under a mutex — must measure BELOW [writeScalingTarget], so a gate ratcheted
// to it would FAIL. This is the artificial re-serialisation the gates exist to
// catch.
//
// Only the load-immune instrument is asserted on. Both of its arms run the same
// number of goroutines over the same already-serialised work, so they degrade
// together and the ratio stays near 1.0 whatever else the host is doing. The
// scaling instrument is measured and logged for comparison but NOT asserted on:
// a ceiling on a one-versus-N ratio is exactly the assertion that a busy machine
// breaks, and it did — see this file's header.
func TestWriteScalingInstrument_SeesSerialisation(t *testing.T) {
	// The same class as its positive sibling: it requires the re-serialised arm to
	// measure BELOW the target, which is a statement about the environment's ability
	// to separate the arms at all. Included by the audit rmp #2319 asked for rather
	// than because it was observed failing — an instrument of this class that is
	// guarded only once it goes red is guarded by luck.
	testlayers.RequireUninstrumented(t, "the scaling ratio of work that is already "+
		"fully serialised, which must measure below the sprint's scaling target")
	requireCores(t)
	const ops = gateOps / 4
	var inner sync.Mutex
	serialised := func(w, i int) error {
		inner.Lock()
		defer inner.Unlock()
		return spinUnit(w, i)
	}

	// Logged as evidence, deliberately not asserted on.
	_ = measureScaling(t, gateWriters, ops, "control/serialised", serialised)

	serial := measureSerialisationRatio(t, gateWriters, ops, "control/serialised", serialised)
	if passesGate(serial.min, writeScalingTarget) {
		t.Fatalf("the serialisation instrument cannot see re-serialisation: work already behind a mutex still "+
			"measured %.3fx (worst of %d), at or above the sprint target of %.2fx. A gate that passes serialised "+
			"work would let rmp #2304 be silently undone.", serial.min, gateRepeats, writeScalingTarget)
	}
}
