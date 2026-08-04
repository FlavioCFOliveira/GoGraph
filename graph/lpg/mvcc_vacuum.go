package lpg

// mvcc_vacuum.go — MVCC C2 (rmp #2308): reclamation as a bounded background
// vacuum instead of a step on the commit path.
//
// # What this replaces, and why the old placement cannot survive
//
// Until this file, reclamation ran INLINE on the writer that had just
// committed: [Graph.endWrite] allocated the commit timestamp, published it, and
// then swept the version chains while the visibility barrier was still held.
// Two things were wrong with that, and only the first is a cost:
//
//   - every committer paid for other transactions' garbage. The sweep is
//     O(objects carrying history) across seven stores, and it landed on whichever
//     transaction happened to push the debt over the threshold.
//   - the sweep had no exclusion to stand on once the barrier went. The old
//     [Graph.ReclaimNow] contract demanded "the visibility barrier in write
//     mode, or otherwise exclude concurrent writers", and rmp #2307 removed the
//     barrier from the write path. What actually excludes a concurrent writer
//     is the PER-SHARD lock each reclaimer takes — verified body by body, see
//     [Graph.sweepUnit] — and that is a property of the reclaimers, not of where
//     they are called from.
//
// So the driver moves off the commit path, and the contract is restated in terms
// of the lock that really does the work.
//
// # Prior art: every reference MVCC engine puts this in the background
//
// Memgraph (memgraph/memgraph, read 2026-08-04 at commit 0e8aa326,
// src/storage/v2/inmemory/storage.cpp) runs `gc_runner_.Run("Storage GC", …)` on
// `config_.gc.interval` and stops it in `~InMemoryStorage` (:609-628). Three of
// its decisions are adopted here:
//
//   - ONE run at a time, taken with `std::try_to_lock` on `gc_lock_` and
//     abandoned rather than queued when another run holds it (:2966-2971). Here
//     that is [vacuumState.running].
//   - the GC does NOT exclude writers wholesale. It takes `main_lock_` SHARED
//     for the transactional mode, with the reason stated in the source — an
//     aggressive sweep escalates to unique "otherwise a shared hold, so slow GC
//     does not block everyone". Here the equivalent is that the sweep takes only
//     one shard lock at a time and never the barrier.
//   - the sweep is OBSERVABLE: `gc_latency_seconds` and a `gc_progress_` record
//     published for `SHOW TRANSACTIONS`.
//
// PostgreSQL (postgres/postgres, read 2026-08-04 at commit 589eb4c3,
// src/backend/postmaster/autovacuum.c) drives its launcher on a naptime with a
// FLOOR and a CEILING — `MIN_AUTOVAC_SLEEPTIME` 100 ms and
// `MAX_AUTOVAC_SLEEPTIME` 300 s (:149-150, :847-915) — and the sleep is
// interruptible by a latch when a worker exits. That floor-and-ceiling backoff,
// interruptible by an explicit wake, is the shape of [Graph.vacuumLoop]'s wait.
// Its horizon comes from `GetOldestNonRemovableTransactionId`
// (src/backend/storage/ipc/procarray.c:1944), which is what
// [mvcc.Horizon.Oldest] answers here.
//
// InnoDB's purge coordinator was NOT read for this task; the two references
// above settle every decision it would have informed, and the module's rule is
// to cite what was read rather than what was recalled.
//
// # Why the goroutine is not permanent
//
// Memgraph's GC thread lives for the life of the storage, and it can: a Memgraph
// process owns one storage. A GoGraph process owns as many [Graph] values as its
// caller cares to make, including one per test, and a permanent goroutine per
// graph would be a leak by any measure the module accepts — `goleak.VerifyTestMain`
// guards this very package.
//
// So the vacuum is DEMAND-STARTED and SELF-TERMINATING. It starts when there is
// work, sweeps until two consecutive passes free nothing, and exits. Restarting
// it costs one goroutine spawn, and the wake sources between them cover every
// way new work can appear:
//
//   - a commit whose debt crosses [reclaimThreshold] — the churn case;
//   - a reader or writer LEAVING the horizon while versions are retained — the
//     drain case, which no commit signals, because the watermark advances when
//     the oldest reader goes away and not when anything is written.
//
// [Graph.Close] gives a caller that wants determinism an explicit end.
//
// # The bound on one pass
//
// [vacuumRecordsPerPass] caps how many records a single pass may release before
// it yields to the workload. It is a real bound and not a formality: a graph
// that has just dropped a million versions is swept across several passes with
// the workload running in between, instead of in one uninterruptible sweep. The
// unit cursor ([vacuumState.cursor]) makes that safe to do — the next pass
// resumes at the store the last one stopped on, so a store cannot be starved by
// an earlier one that keeps hitting the cap.

import (
	"context"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// vacuumUnit names one store the sweep reclaims independently.
//
// The unit is the granularity at which a pass can stop and resume, so it is
// deliberately coarse: each of these reclaimers is already self-limiting through
// its own active-record counter, and splitting one into shard ranges would buy
// nothing a lower [vacuumRecordsPerPass] does not buy more simply.
type vacuumUnit uint8

const (
	unitLabels vacuumUnit = iota
	unitProps
	unitEdgeSide
	unitAdjacency
	unitNodeLife
	unitAdjStamps
	unitIndexRemovals
	// vacuumUnitCount is the number of units, and therefore the length of one
	// full sweep.
	vacuumUnitCount
)

const (
	// vacuumRecordsPerPass bounds how many version records one pass may release
	// before it returns and lets the workload run.
	//
	// At 65 536 records — around 2 MiB at the measured 24 to 32 bytes each — a
	// pass is short enough that a query waiting on one of the shard locks it
	// takes does not notice, and long enough that draining a large mutation
	// costs a handful of passes rather than thousands. The cap is checked
	// BETWEEN units, never inside one, because a unit holds a shard lock and
	// abandoning it half-swept would leave the shard's chain in a state the next
	// pass cannot distinguish from a fresh one.
	vacuumRecordsPerPass = 65536

	// vacuumIdlePasses is how many consecutive passes must free nothing before
	// the vacuum goroutine exits.
	//
	// Two rather than one: a single pass can free nothing because it ran in the
	// window between a commit allocating its timestamp and publishing it, which
	// says nothing about whether there is work. Two consecutive says the
	// watermark genuinely cannot move anything.
	vacuumIdlePasses = 2

	// vacuumMinBackoff and vacuumMaxBackoff bound the wait between passes that
	// free nothing. A pass that frees something does not wait at all — draining
	// is the one thing worth doing at full speed.
	//
	// The floor keeps a graph whose versions are pinned by a long-lived reader
	// from re-sweeping in a tight loop; the ceiling keeps the vacuum responsive
	// once that reader finally goes away, even if its departure somehow signals
	// nothing. PostgreSQL's launcher has the same two guards for the same two
	// reasons (autovacuum.c:149-150).
	vacuumMinBackoff = time.Millisecond
	vacuumMaxBackoff = 100 * time.Millisecond

	// vacuumWaitYields, vacuumWaitSleeps and vacuumWaitSleep bound the ceiling
	// wait in [Graph.awaitVacuumProgress]: 256 yields, then up to 64 sleeps of
	// 100 µs, so at most about 6.4 ms of backpressure before the commit is let
	// through regardless. The yields come first because the sweeper is runnable
	// and the expected wait is a scheduling quantum.
	vacuumWaitYields = 256
	vacuumWaitSleeps = 64
	vacuumWaitSleep  = 100 * time.Microsecond
)

// vacuumState is everything the background vacuum owns.
//
// It is a field of [Graph] rather than a separate object because the sweep is
// generic in the graph's type parameters and the reclaimers are methods on it;
// an independent type would have to carry the graph anyway.
type vacuumState struct {
	// wake carries a demand signal to a running sweeper. Buffered at one: the
	// signal is a level ("there may be work"), not a queue, so a second one
	// while the first is unconsumed says nothing new and is dropped.
	wake chan struct{}
	// stop is closed by [Graph.Close] and is the only thing that ends a pass
	// early.
	stop chan struct{}
	// mu serialises starting against closing, so Close cannot return while a
	// sweeper is between the "not running" test and its own wg.Add.
	mu sync.Mutex
	// closed is guarded by mu.
	closed    bool
	closeOnce sync.Once
	// running says a sweeper GOROUTINE is alive. Read without mu on the wake
	// path, which is why the exit path re-checks for a buffered wake after
	// clearing it; see [Graph.vacuumLoop].
	running atomic.Bool
	// sweeping is the single-sweeper slot, held by whoever is actually sweeping
	// right now — the vacuum's pass or a synchronous [Graph.ReclaimNow]. It is
	// separate from running because the two questions are different: a vacuum
	// goroutine that is between passes is running and not sweeping, and a
	// ReclaimNow caller is sweeping with no goroutine of its own.
	//
	// The reclaimers are excluded from WRITERS by their own per-shard locks, but
	// two concurrent SWEEPS would walk and sever the same chains, so exactly one
	// runs.
	sweeping atomic.Bool
	wg       sync.WaitGroup
	// lastWatermark is the newest watermark a pass has already used. It is what
	// makes the drain wake precise: a horizon release that does not move the
	// watermark past it can free nothing, so it wakes nobody. Monotonic, so it
	// cannot latch.
	lastWatermark atomic.Uint64
	// cursor is the unit the next pass starts at, so a pass that stops at the
	// record cap resumes where it left off instead of restarting at unit zero
	// and starving the later stores.
	cursor atomic.Uint64
	// starts, exits, passes and reclaimed are the lifecycle counters the
	// observability mandate requires of a goroutine the library owns.
	starts    atomic.Uint64
	exits     atomic.Uint64
	passes    atomic.Uint64
	reclaimed atomic.Int64
	// capped counts passes that stopped at [vacuumRecordsPerPass] rather than
	// completing a full sweep. A non-zero value is not a fault — it is the bound
	// doing its job — but a value that keeps growing says the workload is
	// producing garbage faster than one pass can clear.
	capped atomic.Uint64
}

// initVacuum prepares the vacuum's channels. Called from [New]; the zero value
// is not usable, and no other constructor exists.
func (v *vacuumState) init() {
	v.wake = make(chan struct{}, 1)
	v.stop = make(chan struct{})
}

// acquireSweeper blocks until the caller owns the single-sweeper slot.
//
// The wait is bounded because every holder releases it within one pass, and a
// pass is bounded by [vacuumRecordsPerPass]. Yielding rather than sleeping,
// because the expected wait is zero: the only contender is the vacuum, and only
// while it is mid-pass.
func (v *vacuumState) acquireSweeper() {
	for !v.sweeping.CompareAndSwap(false, true) {
		runtime.Gosched()
	}
}

// tryAcquireSweeper takes the slot if it is free, and reports whether it did.
func (v *vacuumState) tryAcquireSweeper() bool {
	return v.sweeping.CompareAndSwap(false, true)
}

// releaseSweeper hands the slot back.
func (v *vacuumState) releaseSweeper() { v.sweeping.Store(false) }

// closedNow reports whether [Graph.Close] has run.
func (v *vacuumState) closedNow() bool {
	select {
	case <-v.stop:
		return true
	default:
		return false
	}
}

// VacuumStats is a point-in-time picture of the background vacuum.
//
// It is the vacuum's half of what [Graph.MVCCStats] reports about the substrate:
// that says how much is retained and why, this says what the sweep has been
// doing about it.
type VacuumStats struct {
	// Running says a sweeper goroutine is alive right now.
	Running bool
	// Starts and Exits are how many times a sweeper has been spawned and has
	// terminated. They differ by at most one, and by exactly one while Running.
	Starts uint64
	Exits  uint64
	// Passes is how many sweeps have run, and Reclaimed how many records they
	// released in total.
	Passes    uint64
	Reclaimed int64
	// CappedPasses is how many passes stopped at the per-pass record bound.
	CappedPasses uint64
	// Backlog is the reclamation debt not yet swept — versions created since the
	// last pass began.
	Backlog int64
	// RecordsPerPass is the explicit per-pass upper bound on work.
	RecordsPerPass int
}

// VacuumStats returns the current state of the background vacuum.
//
// Safe for concurrent use.
func (g *Graph[N, W]) VacuumStats() VacuumStats {
	return VacuumStats{
		Running:        g.vac.running.Load(),
		Starts:         g.vac.starts.Load(),
		Exits:          g.vac.exits.Load(),
		Passes:         g.vac.passes.Load(),
		Reclaimed:      g.vac.reclaimed.Load(),
		CappedPasses:   g.vac.capped.Load(),
		Backlog:        g.reclaimDebt.Load(),
		RecordsPerPass: vacuumRecordsPerPass,
	}
}

// Close releases the background resources this graph owns — currently the MVCC
// vacuum goroutine — and waits for them to terminate.
//
// It is idempotent and safe to call concurrently with any other operation. The
// graph remains readable afterwards and writes still record versions; what stops
// is the sweep, so a caller that closes and then keeps writing accumulates
// versions with nothing to release them. That is the caller's choice to make,
// and it is why Close is a shutdown rather than a pause.
//
// A caller that never closes leaks nothing: the vacuum is demand-started and
// exits on its own once two consecutive passes free nothing. Close exists for the
// owner that wants the goroutine gone at a known instant — a test that asserts on
// goroutine counts, or an embedder tearing a graph down while the process lives
// on. Note that [store.DB] is NOT such an owner: it owns the WAL, the
// checkpointer and the snapshot writer, and never the in-memory graph, so nothing
// in the durability stack has a graph to close.
func (g *Graph[N, W]) Close() error { return g.CloseCtx(context.Background()) }

// CloseCtx is [Graph.Close] with a deadline on the join.
//
// The shutdown SIGNAL is always delivered, whatever ctx says: abandoning it would
// leave a goroutine running with no way to stop it, which is the leak this method
// exists to prevent. What ctx bounds is the WAIT for the sweeper to notice — and
// that wait is already bounded by one pass ([vacuumRecordsPerPass]), so a caller
// needs a deadline only when it cannot tolerate even that.
//
// It returns ctx.Err() when the deadline passed before the sweeper exited, and nil
// otherwise. A non-nil error means the goroutine is still winding down, not that
// the close failed: a later Close or CloseCtx joins it.
//
// The shape mirrors [store.DB.Close] / [store.DB.CloseCtx], which draws the same
// line between the part of a teardown that must always run and the part a caller
// may bound.
func (g *Graph[N, W]) CloseCtx(ctx context.Context) error {
	g.vac.closeOnce.Do(func() {
		g.vac.mu.Lock()
		g.vac.closed = true
		close(g.vac.stop)
		g.vac.mu.Unlock()
	})
	if ctx.Done() == nil {
		// An uncancellable context — which [Graph.Close] passes — needs no watcher
		// goroutine, and spawning one to wait for a wait would be its own small
		// leak on the very path that exists to remove one.
		g.vac.wg.Wait()
		return nil
	}
	joined := make(chan struct{})
	go func() {
		g.vac.wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
		return nil
	case <-ctx.Done():
		// The watcher outlives this call, bounded by the sweeper's own exit — which
		// the signal above has already requested and which one pass bounds.
		return ctx.Err()
	}
}

// wakeVacuum reports that there may be versions to reclaim.
//
// It never blocks and never sweeps. When a sweeper is already running it leaves a
// signal for it; otherwise it starts one. Both are cheap enough for a commit
// path: one non-blocking channel send and one atomic load in the common case
// where a sweeper is already alive.
func (g *Graph[N, W]) wakeVacuum() {
	if !g.mvccArmed {
		return
	}
	select {
	case g.vac.wake <- struct{}{}:
	default:
		// A signal is already pending; it means the same thing this one would.
	}
	if g.vac.running.Load() {
		return
	}
	g.startVacuum()
}

// wakeVacuumOnRelease reports that a reader or writer has left the horizon, which
// is the one way the reclamation watermark advances without anything being
// written.
//
// # Why it must exist
//
// A version is retained because some active transaction can still reach it. When
// that transaction ends, the versions become reclaimable and NOTHING is written —
// so the churn signal never fires and the residue would stay for the life of the
// graph. Measured on the read path this replaces: three live versions were enough
// to keep the label-bitmap filter armed and hold short-read throughput at 1 104
// op/s against a 215 232 op/s baseline, with the writer long since stopped.
//
// # Why it is precise rather than throttled
//
// The first shape wakes on one release in sixty-four, which was the throttle the
// old read-path sweep used. That is wrong here in both directions: it wakes for
// releases that free nothing (a reader leaving while an older one stays), and — the
// direction that actually broke — it DROPS the release that matters, because a
// workload with one long-lived reader takes exactly one release and has a 63-in-64
// chance of skipping it. Measured: a single long reader's departure left 16 385
// records retained with no wake pending.
//
// So the test is the real question instead: has the watermark moved past what the
// last pass already used? That is one comparison against [vacuumState.lastWatermark],
// it is monotonic so it cannot latch, and it is exactly true — a release that does
// not advance the watermark cannot free a single record, and one that does has
// work waiting.
//
// # Why only a READER's release takes it
//
// A writer's release advances the watermark too, but the versions behind it were
// already charged to the reclamation debt, so the churn signal accounts for them
// and this would only make the same sweep happen sooner. Sooner costs: with no
// reader registered the watermark IS the clock, so a wake per write release means
// one pass per commit instead of one per [reclaimThreshold] versions, which throws
// away the amortisation the debt counter exists for. See
// [Graph.releaseWriterSnapshot].
func (g *Graph[N, W]) wakeVacuumOnRelease() {
	if !g.mvccArmed || g.VersionCount() == 0 {
		return
	}
	wm := g.horizon.Oldest(g.mvccClock.ReadTS())
	if wm == 0 {
		// Reclamation is suspended: a reader could not be registered, so the
		// oldest is unknown and nothing may be freed.
		return
	}
	for {
		last := g.vac.lastWatermark.Load()
		if wm <= last {
			// No advance, so nothing this release could free. Silence is correct.
			return
		}
		if g.vac.lastWatermark.CompareAndSwap(last, wm) {
			break
		}
	}
	g.wakeVacuum()
}

// awaitVacuumProgress blocks until the vacuum has completed one pass or the debt
// has fallen back under [reclaimDebtCeiling].
//
// # Why waiting is sound and bounded
//
// It waits for a PASS, not for the watermark. A pass always completes: it sweeps
// at most [vacuumRecordsPerPass] records across at most [vacuumUnitCount] stores
// and returns, whether it freed anything or not. So a writer that has got a
// ceiling ahead of the sweeper is delayed by one pass and never by a reader's
// lifetime — which is the difference between backpressure and a stall.
//
// The hard iteration cap is the last resort for the states in which no pass will
// come: the graph has been [Graph.Close]d, or the sweeper is starting up. Letting
// the commit through is the right answer there — refusing a write because
// reclamation is unavailable would be a wrong answer rather than a slow one — and
// the condition is counted so it is visible rather than silent.
func (g *Graph[N, W]) awaitVacuumProgress() {
	v := &g.vac
	if v.closedNow() {
		// Nothing will sweep again; see above.
		metrics.IncCounter("lpg.mvcc.vacuum.pressure_unrelieved", 1)
		return
	}
	start := v.passes.Load()
	done := func() bool {
		return v.passes.Load() != start || g.reclaimDebt.Load() < reclaimDebtCeiling
	}
	// Yield first: the sweeper is runnable and the expected wait is a scheduling
	// quantum, not a sleep.
	for i := 0; i < vacuumWaitYields; i++ {
		if done() {
			return
		}
		runtime.Gosched()
	}
	for i := 0; i < vacuumWaitSleeps; i++ {
		if done() {
			return
		}
		time.Sleep(vacuumWaitSleep)
	}
	metrics.IncCounter("lpg.mvcc.vacuum.pressure_unrelieved", 1)
}

// startVacuum spawns the sweeper unless one is already alive or the graph is
// closed.
//
// The mutex is what makes [Graph.Close] sound: without it Close could observe a
// zero WaitGroup between a starter's "not running" test and its Add, and return
// while a sweeper was coming up.
func (g *Graph[N, W]) startVacuum() {
	v := &g.vac
	v.mu.Lock()
	if v.closed || v.running.Load() {
		v.mu.Unlock()
		return
	}
	v.running.Store(true)
	v.wg.Add(1)
	v.starts.Add(1)
	v.mu.Unlock()
	go g.vacuumLoop()
}

// vacuumLoop is the background sweeper.
//
// It sweeps at full speed while it is making progress and backs off when it is
// not, exactly as PostgreSQL's launcher does, and it exits once
// [vacuumIdlePasses] consecutive passes free nothing rather than waiting on a
// ticker that would wake to do nothing.
func (g *Graph[N, W]) vacuumLoop() {
	v := &g.vac
	// Named so a profile can attribute the sweep to the substrate rather than to
	// whichever goroutine happened to spawn it, per the observability mandate.
	pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(),
		pprof.Labels("gograph.goroutine", "lpg.mvcc.vacuum")))
	metrics.SetGauge("lpg.mvcc.vacuum.running", 1)
	defer func() {
		v.mu.Lock()
		v.running.Store(false)
		v.mu.Unlock()
		v.exits.Add(1)
		metrics.SetGauge("lpg.mvcc.vacuum.running", 0)
		metrics.IncCounter("lpg.mvcc.vacuum.exits", 1)
		v.wg.Done()
		// A wake that arrived while this goroutine was unwinding would otherwise
		// be LOST: its sender saw running still true, so it left the signal and
		// did not start anyone. Checked AFTER clearing running, which is what
		// makes the two halves cover each other — a sender that reads running
		// false starts a sweeper itself, and one that reads it true is
		// guaranteed to have left its signal before this test.
		if len(v.wake) > 0 {
			g.startVacuum()
		}
	}()

	timer := time.NewTimer(vacuumMinBackoff)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	backoff := vacuumMinBackoff
	idle := 0
	for {
		// CONSUME the pending wake before acting on it. The signal means "there may
		// be work" and this pass is the answer to it, so leaving it buffered makes
		// the exit path's "was a wake lost?" test permanently true and the vacuum
		// restarts itself forever: measured at 24 starts for 16 000 writes, with one
		// goroutine always alive, which goleak reported as a leak because it was
		// one.
		//
		// Drained HERE rather than in the backoff select, because a wake must not
		// cut the backoff short — see the wait at the bottom of the loop.
		select {
		case <-v.wake:
		default:
		}
		// Cleared BEFORE the pass, so versions created while it runs are charged
		// to the next one rather than being swallowed by this one's reset.
		g.reclaimDebt.Store(0)
		freed, capped, ran := g.vacuumPass()
		if !ran {
			// A synchronous sweep holds the slot. Wait for it rather than counting
			// an idle pass this never took — and SLEEP rather than yield, because
			// the holder is doing an unbounded full sweep and spinning through it
			// would burn a core the sweep itself wants.
			select {
			case <-v.stop:
				return
			case <-time.After(vacuumWaitSleep):
			}
			continue
		}
		// Incremented AFTER the pass completes, because [Graph.awaitVacuumProgress]
		// waits on exactly this counter to mean "a pass has finished".
		v.passes.Add(1)
		v.reclaimed.Add(int64(freed))
		if capped {
			v.capped.Add(1)
		}
		g.publishMVCCMetrics()
		g.publishVacuumMetrics()

		if freed > 0 {
			idle = 0
			backoff = vacuumMinBackoff
			// Draining is the one thing worth doing at full speed, so do not
			// wait — but do honour a shutdown between passes.
			select {
			case <-v.stop:
				return
			default:
			}
			continue
		}

		idle++
		if idle >= vacuumIdlePasses {
			// NOT YET, if versions arrived after this pass began. The debt is reset at
			// the top of every pass, so a non-zero value here means work landed that no
			// pass has looked at — and nothing will wake this goroutine for it: the
			// churn signal fires only above [reclaimThreshold] and the drain signal only
			// when a READER leaves, so a writer that stops just short of the threshold
			// satisfies neither.
			//
			// Measured before this check, on TestMVCCBound_SustainedWritesStayFlat: the
			// sweeper exited holding 5 648 records against a bound of 4 096, with a
			// backlog of 2 715 and no active reader — nothing was holding them back and
			// nothing was going to free them. It passed intermittently, because whether
			// the last pass ran after the last write is a matter of scheduling.
			if g.reclaimDebt.Load() > 0 {
				idle = 0
				continue
			}
			return
		}
		if backoff *= 2; backoff > vacuumMaxBackoff {
			backoff = vacuumMaxBackoff
		}
		// A wake during the wait is deliberately NOT allowed to cut it short. The
		// wait exists because the last pass freed nothing, and a wake does not
		// change the watermark; honouring it immediately is how a graph pinned by
		// a long reader would spin.
		timer.Reset(backoff)
		select {
		case <-v.stop:
			return
		case <-timer.C:
		}
	}
}

// vacuumPass sweeps up to [vacuumRecordsPerPass] records and reports how many it
// released, whether it stopped at the cap, and whether it ran at all.
//
// It starts at [vacuumState.cursor] and advances round-robin, so a pass that
// stops early resumes at the store it stopped on.
func (g *Graph[N, W]) vacuumPass() (freed int, capped, ran bool) {
	// A synchronous [Graph.ReclaimNow] may hold the slot. Abandoning the pass
	// rather than queueing behind it is Memgraph's choice too — `std::try_to_lock`
	// on `gc_lock_`, storage.cpp:2966 — and for the same reason: the run that
	// holds the slot is doing the work this one would have done. Reported as "did
	// not run" so the loop does not count it towards its idle exit.
	if !g.vac.tryAcquireSweeper() {
		return 0, false, false
	}
	defer g.vac.releaseSweeper()

	watermark := g.horizon.Oldest(g.mvccClock.ReadTS())
	if watermark == 0 {
		// Reclamation is suspended: a reader could not be registered with the
		// horizon, so the oldest reader is unknown and nothing may be freed. See
		// [mvcc.Horizon.Oldest]. It DID run: a pass that is entitled to free
		// nothing is a legitimate idle pass, and counting it lets the goroutine
		// exit instead of spinning for a registration it cannot influence.
		return 0, false, true
	}
	// Record the watermark this pass is about to use, so a horizon release that
	// does not advance past it wakes nobody. Monotonic: never move it backwards,
	// or a release that genuinely advanced the watermark could be silenced by a
	// later pass that ran at an older one.
	for {
		last := g.vac.lastWatermark.Load()
		if watermark <= last || g.vac.lastWatermark.CompareAndSwap(last, watermark) {
			break
		}
	}
	const units = uint64(vacuumUnitCount)
	cur := g.vac.cursor.Load() % units
	for n := uint64(0); n < units; n++ {
		u := vacuumUnit((cur + n) % units)
		if got := g.sweepUnit(u, watermark); got > 0 {
			freed += got
			// Yield only after doing work: the shard locks this unit took are
			// released, so this is the point at which a query blocked behind one
			// of them should get the processor. A unit that freed nothing took no
			// contended lock worth yielding for.
			runtime.Gosched()
		}
		if freed >= vacuumRecordsPerPass {
			g.vac.cursor.Store((cur + n + 1) % units)
			return freed, true, true
		}
	}
	g.vac.cursor.Store(cur)
	return freed, false, true
}

// sweepUnit reclaims one store at watermark.
//
// # The exclusion contract, verified rather than assumed (rmp #2308)
//
// Every reclaimer below takes the SAME per-shard write lock the write path takes
// for that store, so the sweep is mutually excluded with a concurrent writer
// shard by shard and needs no barrier and no global lock. That was checked body
// by body rather than read off a doc comment, because two of the doc comments
// said otherwise:
//
//   - [Graph.reclaimLabelVersions] and [Graph.reclaimPropVersions] take
//     `sh.mu.Lock()` per shard, the same lock [Graph.setNodeLabelInfo] and
//     [Graph.setNodePropertyInfo] hold while pushing a delta.
//   - [Graph.reclaimEdgeSideVersions] takes `sh.mu.Lock()` on each of the five
//     per-edge side stores' shards.
//   - [adjlist.AdjList.Reclaim] takes `s.mu.Lock()` per shard, which is the lock
//     `storeEntry` — the only writer of an entry's version chain — is called
//     under from every one of its call sites. Its doc said "not safe to run
//     concurrently with writers on the same AdjList"; that was pessimistic
//     rather than true, and it is corrected there.
//   - [Graph.reclaimNodeLife] takes `sh.mu.Lock()` per life shard.
//   - [adjVersions.truncate] takes `sh.mu.Lock()` per stamp shard.
//   - [Graph.applyDeferredIndexRemovals] is the one that was NOT safe, and the
//     defect was real rather than theoretical: it collected the ready entries
//     under `idxDeferred.mu`, released the lock, and only then removed them from
//     the label bitmap. A writer re-adding the same label in that window found
//     nothing to cancel and then had its index entry deleted underneath it,
//     which loses the node from every later label scan — the one failure
//     direction the candidate-set discipline says nothing can recover from. It
//     now holds the lock across the bitmap removals, and the label-add path
//     cancels BEFORE it adds, so the two orders agree. See there.
//
// So the caller does NOT need writer exclusion. It needs only to be the single
// sweeper, which [vacuumState.running] guarantees, and which matters because
// the reclaimers mutate chains that a second sweep would be walking.
func (g *Graph[N, W]) sweepUnit(u vacuumUnit, watermark uint64) int {
	switch u {
	case unitLabels:
		return g.reclaimLabelVersions(watermark)
	case unitProps:
		return g.reclaimPropVersions(watermark)
	case unitEdgeSide:
		return g.reclaimEdgeSideVersions(watermark)
	case unitAdjacency:
		return g.adj.Reclaim(watermark)
	case unitNodeLife:
		// ABORTED life records first (rmp #2318): a birth or death stamped
		// [mvcc.AbortedTS] can never satisfy the watermark test, and dropping it
		// also has to reconcile the tombstone bitmap the aborted transaction
		// left behind. See [Graph.reclaimAbortedLife].
		return g.reclaimAbortedLife() + g.reclaimNodeLife(watermark)
	case unitAdjStamps:
		// The adjacency conflict stamps are bounded here and nowhere else. They
		// are pure write-side bookkeeping — one pair of timestamps per node a
		// transaction has touched, carrying no pre-image and taking part in no
		// rollback — so nothing else has a reason to remove them, and without
		// this sweep the map would grow to one entry per node ever written
		// transactionally and stay there for the life of the process. A stamp at
		// or below the watermark can no longer refuse anything, because
		// [mvcc.Conflicts] is false for a head below every live transaction's
		// start.
		return g.adjVer.clearAborted() + g.adjVer.truncate(watermark)
	case unitIndexRemovals:
		return g.applyDeferredIndexRemovals(watermark)
	}
	return 0
}

// publishVacuumMetrics exports the vacuum's lifecycle and utilisation counters.
//
// Called from the sweeper itself, which is off every request path, so the export
// costs nothing to the workload it describes.
func (g *Graph[N, W]) publishVacuumMetrics() {
	s := g.VacuumStats()
	metrics.SetGauge("lpg.mvcc.vacuum.passes", float64(s.Passes))
	metrics.SetGauge("lpg.mvcc.vacuum.reclaimed", float64(s.Reclaimed))
	metrics.SetGauge("lpg.mvcc.vacuum.capped_passes", float64(s.CappedPasses))
	metrics.SetGauge("lpg.mvcc.vacuum.backlog", float64(s.Backlog))
	metrics.SetGauge("lpg.mvcc.vacuum.records_per_pass", float64(s.RecordsPerPass))
	metrics.SetGauge("lpg.mvcc.vacuum.starts", float64(s.Starts))
}
