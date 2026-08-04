package lpg

// mvcc_gc.go — MVCC P4a (rmp #2288): driving reclamation from the write path.
//
// # Why arming versioning obliges this
//
// P6 (#2283, #2284) built the reclaimers; nothing called them, which was
// harmless only while nothing armed versioning. Arming it without a driver
// would make every modification leak a record until the process exits, and the
// module's bounded-resources mandate does not admit an unbounded structure with
// a follow-up ticket attached. So the driver lands with the arming.
//
// # Where it runs, and why not on a goroutine
//
// It runs on the writer that just committed, at the end of
// [Graph.endWrite], while the barrier is still held. That placement is not a
// convenience:
//
//   - It needs writer exclusion. The adjacency reclaimer takes each shard lock
//     and severs published chains; it is documented as safe against readers but
//     NOT against a concurrent writer, and the barrier is exactly what excludes
//     one.
//   - It self-throttles. A graph that is not being written is not accumulating
//     versions, so it needs no sweep, and a background ticker would wake to do
//     nothing — the module forbids unbounded goroutines and requires every
//     long-lived one to be observable, which is a cost with no benefit here.
//
// PostgreSQL reaches the same place from the opposite direction: its HOT-prune
// path cleans a page during an ordinary access rather than waiting for the
// VACUUM daemon, because the accessor already holds the right lock and already
// paid the cache miss.
//
// # The debt counter
//
// Sweeping on every commit would make a one-property write walk sixty-four
// adjacency shards. Instead each write adds its version count to a debt, and a
// sweep runs only once the debt passes [reclaimThreshold]. The bound that
// follows is stated rather than implied: between sweeps the graph holds at most
// the threshold in versions ABOVE whatever a long-lived reader is legitimately
// holding back, and both quantities are readable — [Graph.VersionCount] for the
// total and [mvcc.Horizon.Active] for the readers responsible.

import "github.com/FlavioCFOliveira/GoGraph/graph/mvcc"

// reclaimThreshold is how many versions may accumulate between reclamation
// sweeps.
//
// It trades sweep frequency against retained memory. At 4096 the sweep amortises
// to a 64-shard scan per 4096 modifications — under a hundredth of a shard visit
// per write — while the retained ceiling is roughly 4096 records, which at the
// measured 24 to 32 bytes each is around 128 KiB. Both halves of that trade are
// stated so a future change to the constant is made against numbers rather than
// against taste.
const reclaimThreshold = 4096

// reclaimIdleEvery is how many reads pass between opportunistic sweeps while
// the graph holds any version at all.
//
// It bounds the sweep's share of the read path: at 64, a workload that keeps
// versions permanently alive pays one 64-shard pass per 64 reads, and one that
// merely churns pays a pass shortly after each write and then nothing, because
// the sweep drives the count to zero and the gate above stops taking the tick.
const reclaimIdleEvery = 64

// reclaimIfDue sweeps the version chains when enough churn has accumulated
// since the last sweep.
//
// The caller must hold the visibility barrier in write mode; see the file
// comment for why that is a requirement and not merely where it happens to be
// called from.
func (g *Graph[N, W]) reclaimIfDue() {
	live := g.VersionCount()
	if live == 0 {
		g.reclaimDebt.Store(0)
		return
	}
	if g.reclaimDebt.Load() < reclaimThreshold {
		return
	}
	g.reclaimDebt.Store(0)
	if !g.sweeping.CompareAndSwap(false, true) {
		return
	}
	defer g.sweeping.Store(false)
	g.ReclaimNow()
	g.publishMVCCMetrics()
}

// reclaimAfterDirectWrite sweeps when a DIRECT Go-API mutation — one made
// outside any transaction — has pushed the reclamation debt past the
// threshold.
//
// # Why this exists at all
//
// [Graph.endWrite] drives reclamation for a barrier-held transaction, which is
// every write the Cypher engine and the durable store make. It does NOT cover
// the public Go-API mutators, which a caller may drive without ever opening a
// transaction — and that is a supported, documented use of this package.
// Without this, such a caller leaks ONE version record per modification for the
// life of the process, and every subsequent read of the affected shard pays a
// side-map probe it can never stop paying. Measured before the fix: a 60 000
// node build through the direct API left 120 000 live versions and made
// BenchmarkEngReadProjectLargeSerial 58 % slower, forever.
//
// # Why it can take the barrier here and endWrite cannot
//
// The reclaimers need writer exclusion, which the barrier supplies. A direct
// mutator does not hold it, so this can acquire it — but only when no
// transaction is open, because inside one the barrier is ALREADY held by this
// goroutine and re-acquiring it is the re-entrancy violation the guard exists
// to catch. [mvcc.WriteStamp.Armed] is exactly that question, and it is one
// atomic load.
//
// The caller must NOT hold any shard lock: this takes the barrier and then
// every shard lock in turn.
func (g *Graph[N, W]) reclaimAfterDirectWrite(tx *writeCtx) {
	if !g.mvccArmed {
		return
	}
	if tx != nil {
		// This write CARRIES a transaction, so it is not a direct write at all
		// and its bracket's endWrite owns the sweep. Tested before the ambient
		// slot because it is the exact answer where the slot is only a proxy: once
		// two brackets overlap the slot reports "a transaction is open" for every
		// writer, the caller's own or not (rmp #2320).
		return
	}
	if g.stamp.Armed() {
		// Inside a transaction: this goroutine already holds the barrier, so it
		// must not try to take it again, and endWrite owns the sweep anyway.
		// Checked FIRST: an earlier version charged the untracked count before
		// testing this, so a graph with any untracked history could fall
		// through to ApplyAtomically from inside the barrier.
		return
	}
	if n := g.stamp.TakeUntracked(); n > 0 {
		g.reclaimDebt.Add(n)
	}
	if g.reclaimDebt.Load() < reclaimThreshold {
		return
	}
	g.reclaimDebt.Store(0)
	_ = g.ApplyAtomically(func() error {
		g.ReclaimNow()
		return nil
	})
}

// ReclaimIdle sweeps from the READ path, which is what makes the substrate
// settle back to zero when writing stops.
//
// # The residue this exists to remove
//
// [Graph.reclaimIfDue] sweeps once the debt passes a threshold, so whatever the
// last sweep did not reach stays for as long as no further write arrives. On a
// build-then-read workload that is the whole point of the graph, and the cost
// is not the memory — it is that EVERY subsequent read of an affected shard
// pays a side-map probe for history nothing can reach. Measured: a 60 000 node
// build left 3 136 unreachable versions and made
// BenchmarkEngReadProjectLargeSerial 42 % slower than the pre-MVCC baseline,
// permanently. Sweeping them costs one pass and the cost goes away.
//
// # Why a READER may do this
//
// The reclaimers need three things, and a reader supplies all of them:
//
//   - No concurrent WRITER. Each reclaimer takes the same per-shard write lock
//     the write path takes, so they are mutually excluded shard by shard. This
//     does not depend on the visibility barrier, which is why it will still
//     hold once P4c retires it.
//   - No concurrent SWEEP. [Graph.sweeping] admits exactly one.
//   - A watermark no active reader is older than. A reader is REGISTERED with
//     the horizon before it gets here, so the watermark it computes is at or
//     below its own start timestamp — which makes sweeping from a reader
//     strictly safer than sweeping from a writer, where the readers are the
//     ones that have to be accounted for.
//
// # Why it does not spin
//
// A long-lived reader legitimately holds versions back, and re-sweeping for
// them on every query would be pure waste. So a sweep that changes nothing
// records the count it left behind, and the next read skips until a write moves
// it.
//
// Safe for concurrent use.
func (g *Graph[N, W]) ReclaimIdle() {
	if !g.mvccArmed || g.VersionCount() == 0 {
		return
	}
	// Throttled by a TICK, not by "has the count changed since I last swept".
	// That first shape latched: a sweep that freed nothing — which happens
	// whenever a reader's start timestamp lands just before a commit — recorded
	// the unchanged count and every later read then skipped forever, leaving
	// the residue in place for the life of the graph. Measured: three live
	// versions were enough to keep the bitmap filter armed and hold short-read
	// throughput at 1 104 op/s against a 215 232 op/s baseline, with the writer
	// long since stopped.
	//
	// A tick cannot latch. It also costs nothing in the state that matters: the
	// count is zero on a graph nobody is writing, so the guard above returns
	// before the tick is even taken.
	if g.idleTicks.Add(1)%reclaimIdleEvery != 0 {
		return
	}
	if !g.sweeping.CompareAndSwap(false, true) {
		return
	}
	defer g.sweeping.Store(false)
	g.ReclaimNow()
	g.publishMVCCMetrics()
}

// ReclaimNow frees every version no active reader can reach, and returns how
// many records were released.
//
// The watermark is the oldest start timestamp among the readers registered with
// this graph's horizon, falling back to the clock's current value when none is
// registered — at which point every version a completed write superseded is
// unreachable and all of it goes. A reader that could not be registered
// suspends reclamation entirely rather than risk it; see [mvcc.Horizon.Oldest].
//
// It is exported so a caller that knows it has just finished a large mutation
// can settle the debt immediately instead of waiting for the next write, and so
// a test can assert that the substrate returns to zero.
//
// The caller must hold the visibility barrier in write mode, or otherwise
// exclude concurrent writers. Safe against concurrent readers.
func (g *Graph[N, W]) ReclaimNow() int {
	watermark := g.horizon.Oldest(g.mvccClock.ReadTS())
	if watermark == 0 {
		return 0
	}
	return g.ReclaimVersions(watermark) +
		g.adj.Reclaim(watermark) +
		g.reclaimNodeLife(watermark) +
		// The adjacency conflict stamps are bounded here and nowhere else. They
		// are pure write-side bookkeeping — one pair of timestamps per node a
		// transaction has touched, carrying no pre-image and taking part in no
		// rollback — so nothing else has a reason to remove them, and without this
		// sweep the map would grow to one entry per node ever written
		// transactionally and stay there for the life of the process. A stamp at or
		// below the watermark can no longer refuse anything, because
		// [mvcc.Conflicts] is false for a head below every live transaction's
		// start. See [adjVersions.truncate].
		g.adjVer.truncate(watermark) +
		g.applyDeferredIndexRemovals(watermark)
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
