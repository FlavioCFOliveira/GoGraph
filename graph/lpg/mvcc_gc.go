package lpg

// mvcc_gc.go — how reclamation is DRIVEN.
//
// # Where the driver lives, and where it used to live
//
// MVCC P4a (rmp #2288) put the driver on the writer that had just committed, at
// the end of [Graph.endWrite], while the visibility barrier was still held. Two
// arguments carried that placement, and rmp #2307 invalidated both: the sweep
// "needs writer exclusion", which the barrier supplied and nothing does now; and
// a background ticker "would wake to do nothing", which is true of a ticker and
// not of a demand-started goroutine.
//
// So MVCC C2 (rmp #2308) moved the driver into a bounded background vacuum —
// mvcc_vacuum.go, where the reasoning and the prior art live. What remains here
// is the accounting that decides WHEN the vacuum is worth waking, and the
// synchronous [Graph.ReclaimNow] a caller can still ask for directly.
//
// # The debt counter
//
// Waking the vacuum on every commit would make a one-property write signal a
// seven-store sweep. Instead each write adds its version count to a debt, and the
// vacuum is woken only once the debt passes [reclaimThreshold]. The bound that
// follows is stated rather than implied: between sweeps the graph holds at most
// the threshold in versions ABOVE whatever a long-lived reader is legitimately
// holding back, and both quantities are readable — [Graph.VersionCount] for the
// total and [mvcc.Horizon.Active] for the readers responsible.

import "github.com/FlavioCFOliveira/GoGraph/graph/mvcc"

// reclaimThreshold is how many versions may accumulate before the vacuum is
// woken.
//
// It trades wake frequency against retained memory. At 4096 the sweep amortises
// to one seven-store pass per 4096 modifications, while the retained ceiling is
// roughly 4096 records, which at the measured 24 to 32 bytes each is around
// 128 KiB. Both halves of that trade are stated so a future change to the
// constant is made against numbers rather than against taste.
const reclaimThreshold = 4096

// reclaimDebtCeiling is the hard bound on the debt: the point at which a
// committer stops merely signalling the vacuum and WAITS for it.
//
// # Why a ceiling is owed once the sweep is asynchronous (rmp #2308)
//
// While the sweep ran ON the committer, the churn bound was true instantaneously:
// the write that pushed the debt past the threshold could not proceed until it had
// swept. A background vacuum breaks that — a writer in a tight loop outruns the
// sweeper, and the retained count is then bounded only by the write rate times the
// pass period, which is not a bound the module's mandate accepts. Measured on the
// first asynchronous build: 24 576 transactional modifications peaked at 9 232
// retained records against a stated bound of 8 192, and 24 576 direct-API writes
// finished holding 14 589.
//
// So the threshold stays a SIGNAL and this becomes the BOUND. Below it a commit
// pays one atomic add and, occasionally, one channel send. At it, the commit
// blocks until the vacuum has completed a pass — which is the backpressure the
// reliability mandate prescribes ("callers either receive a typed error or block"),
// and it is bounded rather than open-ended: it waits for a PASS, never for the
// watermark, so a long-lived reader that legitimately pins every version delays a
// committer by one pass and no more. See [Graph.awaitVacuumProgress].
//
// At four thresholds the wait is rare — a writer has to get 16 384 versions ahead
// of a sweeper that runs continuously while it is making progress — and the
// instantaneous ceiling it buys is around 512 KiB of version records.
const reclaimDebtCeiling = 4 * reclaimThreshold

// chargeReclaimDebt records that a transaction created n version records, wakes
// the vacuum when the accumulated debt has passed [reclaimThreshold], and applies
// backpressure once it has passed [reclaimDebtCeiling].
//
// In the common case this is the whole of what a commit pays for reclamation: one
// atomic add and one comparison. No sweep, no lock, nothing O(stores).
//
// The caller must hold no shard lock: a wake may start a goroutine that takes
// them, and the ceiling wait would then hold one while waiting for the sweeper
// that needs it.
func (g *Graph[N, W]) chargeReclaimDebt(n int64) {
	if n <= 0 {
		return
	}
	debt := g.reclaimDebt.Add(n)
	if debt < reclaimThreshold {
		// A DEBT WITH NO SWEEPER ALIVE IS NOT AMORTISED, IT IS STRANDED (rmp #2424).
		//
		// The threshold amortises the SIGNAL, and that is sound only while a sweeper
		// exists to do the work eventually. With none alive this debt is invisible to
		// everything: the churn signal fires only above the threshold, and the drain
		// signal only when a reader leaves. A workload that stops just short of the
		// threshold after the sweeper's idle exit therefore keeps its versions for the
		// life of the process.
		//
		// Observed exactly that, once in 71 runs of the whole graph/lpg package under
		// peer load: 4 883 records held against a bound of 4 096 with ZERO active
		// readers and `Running:false Backlog:3767`. Neither of the two states
		// [MVCCStats] admits for exceeding the bound — a reader holding versions back,
		// or a burst the vacuum has not caught up with — and the retention was
		// permanent.
		//
		// [Graph.vacuumLoop]'s exit path already covers the symmetric case: it
		// re-checks the wake CHANNEL after clearing `running`, so a sender that reads
		// `running` false starts a sweeper itself and one that reads it true is
		// guaranteed to have left its signal first. That cover never engaged here,
		// because a sub-threshold charge left no signal at all.
		//
		// `running` is tested BEFORE calling [Graph.wakeVacuum] so the ordinary path —
		// a sweeper already alive — pays one atomic load of a line written only when a
		// sweeper starts or exits, rather than a channel send per commit.
		if !g.vac.running.Load() {
			g.wakeVacuum()
		}
		return
	}
	g.wakeVacuum()
	if debt >= reclaimDebtCeiling {
		g.awaitVacuumProgress()
	}
}

// reclaimAfterDirectWrite accounts for a DIRECT Go-API mutation — one made
// outside any transaction — and wakes the vacuum when the debt is due.
//
// # Why this exists at all
//
// The transactional brackets charge their own versions in [Graph.endWrite]. They
// do NOT cover the public Go-API mutators, which a caller may drive without ever
// opening a transaction — and that is a supported, documented use of this
// package. Without this, such a caller leaks ONE version record per modification
// for the life of the process, and every subsequent read of the affected shard
// pays a side-map probe it can never stop paying. Measured before the fix: a
// 60 000 node build through the direct API left 120 000 live versions and made
// BenchmarkEngReadProjectLargeSerial 58 % slower, forever.
//
// # It no longer takes the barrier (rmp #2308)
//
// It used to acquire the visibility barrier and sweep synchronously, because the
// reclaimers were documented as needing writer exclusion. They need the per-shard
// lock, which they take themselves; see [Graph.sweepUnit]. So this is now pure
// accounting, and with it goes the guard that used to skip the charge whenever a
// transaction was open anywhere on the graph — a guard that, once two write
// brackets could overlap, would have suppressed every direct write's charge for
// as long as any transaction was in flight.
func (g *Graph[N, W]) reclaimAfterDirectWrite(tx *writeCtx) {
	if !g.mvccArmed {
		return
	}
	if tx != nil {
		// This write CARRIES a transaction, so it is not a direct write at all
		// and its bracket's endWrite owns the charge.
		return
	}
	g.chargeReclaimDebt(g.stamp.TakeUntracked())
}

// ReclaimNow frees every version no active reader can reach, synchronously, and
// returns how many records were released.
//
// The watermark is the oldest start timestamp among the readers registered with
// this graph's horizon, falling back to the clock's current value when none is
// registered — at which point every version a completed write superseded is
// unreachable and all of it goes. A reader that could not be registered
// suspends reclamation entirely rather than risk it; see [mvcc.Horizon.Oldest].
//
// The background vacuum is what keeps memory bounded in the ordinary case
// (mvcc_vacuum.go). This is exported for the two things the vacuum cannot give a
// caller: a bulk loader that knows it has just finished and wants the debt
// settled at a known instant rather than a few milliseconds later, and a test
// that needs the substrate's state to be a function of what it did rather than of
// when a goroutine woke.
//
// Unlike the vacuum's own pass it is UNBOUNDED — it sweeps every store to
// completion — which is what makes it a settlement rather than a step.
//
// # What the caller must exclude: nothing
//
// Concurrent WRITERS are excluded by each reclaimer's own per-shard lock; see
// [Graph.sweepUnit] for the body-by-body verification. Concurrent SWEEPS are
// excluded by this method itself — it takes the same single-sweeper slot the
// vacuum's pass takes, so the two can never walk the same chain, and the wait for
// it is bounded because a vacuum pass is bounded ([vacuumRecordsPerPass]).
//
// Safe for concurrent use.
func (g *Graph[N, W]) ReclaimNow() int {
	g.vac.acquireSweeper()
	defer g.vac.releaseSweeper()
	watermark := g.horizon.Oldest(g.mvccClock.ReadTS())
	if watermark == 0 {
		return 0
	}
	freed := 0
	for u := vacuumUnit(0); u < vacuumUnitCount; u++ {
		freed += g.sweepUnit(u, watermark)
	}
	return freed
}

// abortWake reports that a transaction has ABORTED, so the vacuum must run
// whether or not the reclamation debt is due.
//
// Aborted versions are not ordinary garbage (rmp #2318). They are the only thing
// masking the aborted transaction's writes from the stored value, so until the
// sweep withdraws them the object carries a value no reader may take at face
// value. Waiting for [reclaimThreshold] versions of churn to accumulate first
// would make that window unbounded, and abort is the rare path, so it pays for
// its own wake.
func (g *Graph[N, W]) abortWake(created int64) {
	if !g.mvccArmed {
		return
	}
	if created > 0 {
		g.reclaimDebt.Add(created)
	}
	// SYNCHRONOUS withdrawal first: a present-time read takes the stored value
	// directly, so the aborted transaction's writes must be out of it before this
	// returns. See [Graph.withdrawAbortedNow] for why there is no correct
	// asynchronous answer and what the scan costs.
	if freed := g.withdrawAbortedNow(); freed > 0 {
		g.reclaimDebt.Add(-int64(freed))
	}
	// The vacuum still runs, for anything the withdrawal could not reach — an
	// aborted version that a live reader's watermark still pins, and the ordinary
	// churn this transaction charged.
	g.wakeVacuum()
}

// VersionCount returns the total number of live version records across every
// store: node-label deltas, node-property deltas, adjacency entry versions, and
// the five per-edge side stores (rmp #2291).
//
// It is the memory the substrate is responsible for, and the quantity the
// bounded-resources mandate requires be observable rather than merely bounded.
//
// Safe for concurrent use.
func (g *Graph[N, W]) VersionCount() int64 {
	return g.labelDeltaActive.Load() +
		g.propDeltaActive.Load() +
		g.adj.VersionCount() +
		g.EdgeSideVersionCount() +
		g.nodeLifeActive.Load() +
		g.idxPendingActive.Load()
}

// Horizon returns the reader horizon this graph reclaims against.
//
// A reader registers its start timestamp with it for as long as it is active,
// so reclamation can tell which versions are still reachable. It is exported
// because the layer that owns a read's lifetime — the Cypher engine — lives in
// another package.
//
// Safe for concurrent use.
func (g *Graph[N, W]) Horizon() *mvcc.Horizon { return g.horizon }
