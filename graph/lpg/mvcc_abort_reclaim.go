package lpg

// mvcc_abort_reclaim.go — MVCC (rmp #2318): the vacuum withdraws an aborted
// transaction's writes and releases its versions.
//
// # The two defects this closes, both measured
//
// **1. The versions leaked, permanently.** [mvcc.AbortedTS] is `^uint64(0)`, the
// MAXIMUM uint64, and every reclaimer truncates on `stamp <= watermark`, which
// AbortedTS can never satisfy. Measured at b3e1aa0b: seed 50 nodes with a label,
// reclaim to zero, open a transaction, write a label on each of the 50, abort,
// reclaim again with NO live reader — `freed=0` and `VersionCount()=50`, for the
// life of the process. Live since d8847ce7 made a write-write conflict abort the
// transaction, so every serialization failure leaked its versions.
//
// **2. A later commit made the aborted writes VISIBLE.** This is the worse half
// and it is NOT in the ticket; it was found while implementing the first. The
// stored value keeps the aborted transaction's writes, and its deltas are the only
// thing masking them:
//
//	T_abort adds label L to n, aborts.  Stored bag = {…, L}; the head delta is aborted.
//	A reader now correctly does NOT see L: the head is invisible, so its undo applies.
//	T2 adds label M to n and COMMITS — Conflicts() exempted the aborted head — and
//	  builds its value from the DIRTY stored bag, so the bag becomes {…, L, M}.
//	A reader after T2 walks the chain, finds T2's delta VISIBLE, and BREAKS,
//	  never reaching the aborted delta behind it.
//	The reader sees L.
//
// Measured exactly that: `after T2 commit: reader sees L=true M=true`. A committed
// read observing work from a transaction that was told it failed is an ATOMICITY
// violation, not a leak, and it is why this task's severity is not the ticket's.
//
// The chain walk's early break is sound only while a chain is TIMESTAMP-MONOTONE —
// reach a visible delta and everything older is visible too. An aborted delta
// breaks that: AbortedTS is the maximum uint64 yet invisible to everyone, so it can
// sit behind a newer commit while still needing to be undone.
//
// # Why "make the walk continue" is not the fix, and the adjacency proves it
//
// Continuing past a visible delta would fix the label, property and edge-side
// stores, whose deltas are UNDO ACTIONS that compose in any order. It cannot fix
// the ADJACENCY, whose chain is of immutable ENTRY SNAPSHOTS: T2 built its entry
// from the dirty base, so T2's entry itself CONTAINS the aborted edge. No amount of
// walking recovers a value that was never recorded.
//
// So the dirty base must never be built on. That is [mvcc.Conflicts]' job, and it
// is why this task changes it.
//
// # The two halves, and why neither works alone
//
//  1. **A writer may not build on a dirty base.** [mvcc.Conflicts] now treats an
//     aborted head as a conflict, so the interleaving above is unreachable and an
//     aborted delta is ALWAYS at its chain's head — which is what makes the walks'
//     early break sound again with no change to any read path.
//  2. **The vacuum cleans promptly.** Half 1 alone is a LIVENESS bug, and it was
//     measured as one when the exemption was introduced (graph/mvcc/conflict.go):
//     with nothing cleaning the aborted version, "the FIRST transaction to abort on
//     an object made that object permanently unwritable" and
//     examples/27_concurrent_txn's writers exhausted a nine-attempt retry chain on
//     their first aborted account. The exemption's own doc calls itself a
//     placeholder — unlinking "is rmp #2318's, and when it lands this branch
//     becomes unreachable rather than wrong." This is that landing, and the
//     cleaner is the vacuum rmp #2308 built.
//
// So an abort wakes the vacuum UNCONDITIONALLY rather than through the debt
// threshold: aborted records are not ordinary garbage, they hold a write lock on
// the object in all but name, and waiting for 4096 versions of churn to accumulate
// before clearing them would make the retry window unbounded.
//
// # Where the undo comes from
//
// Each store's clean value is computed by its OWN `asOf` walk — the same code a
// reader runs — so the withdrawn value cannot disagree with what readers have been
// seeing. Nothing here re-implements an undo.
//
// # Prior art, and what the ticket got wrong about it
//
// Memgraph does this AT ABORT rather than in its GC, and says so in its own source:
// "Abort will modify objects to restore state to how they were before this txn"
// (`InMemoryStorage::InMemoryAccessor::Abort`,
// src/storage/v2/inmemory/storage.cpp:1482-1790, read 2026-08-04 at commit
// 0e8aa326). It restores each object under that object's lock, unlinks the deltas,
// and only then hands them to `garbage_undo_buffers_` with a `mark_timestamp` so the
// GC frees the MEMORY once `mark_timestamp <= oldest_active_start_timestamp`
// (:1792-1808, and storage.cpp:3084-3100 for the GC side). **Its GC never applies an
// undo**, so the ticket's premise that "Memgraph's GC … walks an aborted
// transaction's deltas" to undo them is not what the source does.
//
// PostgreSQL is also cited by the ticket and is not a model for this at all: it has
// no undo log. An aborted transaction's tuple version is simply never visible (its
// xmin is an aborted xid per the CLOG), and the previous version was never
// overwritten because PostgreSQL appends a new tuple version rather than mutating in
// place; VACUUM reclaims the dead tuple and undoes nothing. GoGraph mutates the
// stored value in place with undo records beside it, which puts it in Memgraph's
// family and not PostgreSQL's.
//
// # Why the vacuum and not abort, given Memgraph aborts
//
// Doing it at abort would need the transaction's own write set — Memgraph's
// `transaction_.deltas` — which GoGraph does not keep, and adding it means an append
// on EVERY write to serve the rare abort path, on a sprint whose objective is write
// throughput. The vacuum already scans every chain, so it finds aborted heads for
// free. Half 1 above is what buys the deferral its safety: with the dirty base
// unwritable, the only cost of cleaning late is a retry, and a retriable
// serialization failure is already this sprint's contract.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// withdrawAbortedNow withdraws every aborted version in the graph, synchronously,
// and returns how many records it released.
//
// # Why an abort cannot merely SIGNAL the vacuum
//
// Deferring the withdrawal leaves the stored value dirty until the sweep runs, and
// a PRESENT-TIME read takes the stored value directly — [Graph.ReadAt](nil) is
// documented as "the current stored value", which is what every plain public getter
// resolves through. Measured: immediately after an abort, `HasNodeLabel` returned
// true for the aborted transaction's own label. Read committed at the present
// instant may not include work that was never committed, so the withdrawal has to
// have happened by the time abort returns.
//
// So this runs ON the aborting goroutine. It is the one thing rmp #2308 put back on
// a caller's path, and the justification is the one the decision framework gives:
// correctness outranks speed, and there is no correct asynchronous answer here.
//
// # The cost, stated
//
// It scans the objects carrying history rather than the objects THIS transaction
// touched, because the substrate keeps no per-transaction write set — Memgraph's
// `transaction_.deltas` — and adding one taxes every write to serve the rare abort
// path. The scan is therefore O(objects carrying history), which is bounded by the
// retained version count and so by [reclaimDebtCeiling] plus whatever a live reader
// holds back. Abort is the rare path and this is its price; making it O(this
// transaction's writes) needs the write set and is a separate change.
func (g *Graph[N, W]) withdrawAbortedNow() int {
	if !g.mvccArmed {
		return 0
	}
	g.vac.acquireSweeper()
	defer g.vac.releaseSweeper()
	freed := g.withdrawAbortedLabels() + g.withdrawAbortedProps() +
		g.withdrawAbortedSides() + g.reclaimAbortedLife() +
		g.withdrawAbortedIndexRemovals()
	g.adjVer.clearAborted()
	return freed
}

// withdrawAbortedIndexRemovals cancels the deferred label-index removals an
// aborted transaction recorded, and reports how many it cancelled.
//
// A removal the transaction never committed must not fire, and cancellation is the
// mechanism the ROLLBACK path already uses: the entry is still in the bitmap, so
// cancelling leaves the bitmap a SUPERSET of the truth, which is the direction the
// candidate-set discipline tolerates. Letting it fire would delete an index entry
// for a label the node still carries — the unrecoverable direction.
//
// They are version memory too ([MVCCStats.IndexRemovalBacklog] counts them), and
// [Graph.applyDeferredIndexRemovals] can no more reach an [mvcc.AbortedTS] stamp
// than any other reclaimer can.
func (g *Graph[N, W]) withdrawAbortedIndexRemovals() int {
	if g.idxPendingActive.Load() == 0 {
		return 0
	}
	g.idxDeferred.mu.Lock()
	cancelled := 0
	for k, st := range g.idxDeferred.pending {
		if st.at() == mvcc.AbortedTS {
			delete(g.idxDeferred.pending, k)
			cancelled++
		}
	}
	if len(g.idxDeferred.pending) == 0 {
		g.idxDeferred.pending = nil
	}
	g.idxDeferred.mu.Unlock()
	if cancelled > 0 {
		g.idxPendingActive.Add(-int64(cancelled))
	}
	return cancelled
}

// withdrawAbortedLabels withdraws every aborted label chain head in the graph.
func (g *Graph[N, W]) withdrawAbortedLabels() int {
	if g.labelDeltaActive.Load() == 0 {
		return 0
	}
	freed := 0
	for i := range g.nodeLabelShards {
		sh := &g.nodeLabelShards[i]
		sh.mu.Lock()
		for id := range sh.d {
			freed += g.reclaimAbortedLabelsLocked(sh, id)
		}
		if len(sh.d) == 0 {
			sh.d = nil
		}
		sh.mu.Unlock()
	}
	if freed > 0 {
		g.labelDeltaActive.Add(-int64(freed))
	}
	return freed
}

// withdrawAbortedProps is the property-chain counterpart.
func (g *Graph[N, W]) withdrawAbortedProps() int {
	if g.propDeltaActive.Load() == 0 {
		return 0
	}
	freed := 0
	for i := range g.nodePropShards {
		sh := &g.nodePropShards[i]
		sh.mu.Lock()
		for id := range sh.d {
			freed += g.reclaimAbortedPropsLocked(sh, id)
		}
		if len(sh.d) == 0 {
			sh.d = nil
		}
		sh.mu.Unlock()
	}
	if freed > 0 {
		g.propDeltaActive.Add(-int64(freed))
	}
	return freed
}

// withdrawAbortedSides withdraws every aborted head in the five per-edge side
// stores.
func (g *Graph[N, W]) withdrawAbortedSides() int {
	freed := 0
	if g.edgeLabelVersionActive.Load() != 0 {
		n := 0
		for i := range g.edgeLabelShards {
			sh := &g.edgeLabelShards[i]
			sh.mu.Lock()
			n += g.withdrawAbortedEdgeLabelsLocked(sh)
			sh.mu.Unlock()
		}
		g.edgeLabelVersionActive.Add(-int64(n))
		freed += n
	}
	if g.edgeHandleLabelVersionActive.Load() != 0 {
		n := 0
		for i := range g.edgeHandleLabelShards {
			sh := &g.edgeHandleLabelShards[i]
			sh.mu.Lock()
			n += g.withdrawAbortedHandleLabelsLocked(sh)
			sh.mu.Unlock()
		}
		g.edgeHandleLabelVersionActive.Add(-int64(n))
		freed += n
	}
	if g.edgeHandlePropVersionActive.Load() != 0 {
		n := 0
		for i := range g.edgeHandlePropShards {
			sh := &g.edgeHandlePropShards[i]
			sh.mu.Lock()
			n += g.withdrawAbortedHandlePropsLocked(sh)
			sh.mu.Unlock()
		}
		g.edgeHandlePropVersionActive.Add(-int64(n))
		freed += n
	}
	if g.edgeInstanceLabelVersionActive.Load() != 0 {
		n := 0
		for i := range g.edgeInstanceLabelShards {
			sh := &g.edgeInstanceLabelShards[i]
			sh.mu.Lock()
			n += g.withdrawAbortedInstanceLabelsLocked(sh)
			sh.mu.Unlock()
		}
		g.edgeInstanceLabelVersionActive.Add(-int64(n))
		freed += n
	}
	if g.edgeInstancePropVersionActive.Load() != 0 {
		n := 0
		for i := range g.edgeInstancePropShards {
			sh := &g.edgeInstancePropShards[i]
			sh.mu.Lock()
			n += g.withdrawAbortedInstancePropsLocked(sh)
			sh.mu.Unlock()
		}
		g.edgeInstancePropVersionActive.Add(-int64(n))
		freed += n
	}
	return freed
}

// abortedHead reports whether a delta stamp names an aborted transaction.
//
// A single comparison, and the reason it can be one is that [mvcc.AbortedTS] is a
// reserved value no live transaction id or commit timestamp can take.
func abortedHead(stamp uint64) bool { return stamp == mvcc.AbortedTS }

// reclaimAbortedLabels withdraws every aborted label delta at a chain head,
// restoring the stored bag, and returns how many records it released.
//
// The stored bag is recomputed by [Graph.labelBagAsOfLocked] at the present
// instant, which is the reader's own walk: it applies the undo of every delta it
// must, and after this the aborted ones are gone so it will not apply them again.
//
// The caller must hold the shard's write lock.
func (g *Graph[N, W]) reclaimAbortedLabelsLocked(sh *nodeLabelShard, id graph.NodeID) int {
	head := sh.d[id]
	if head == nil || !abortedHead(head.stampTS()) {
		return 0
	}
	clean := cloneLabelBag(g.labelBagAsOfLocked(sh, id, g.mvccClock.ReadTS(), 0))
	before := sh.m[id]
	freed := 0
	for d := sh.d[id]; d != nil && abortedHead(d.stampTS()); d = sh.d[id] {
		sh.d[id] = d.next
		freed++
	}
	if sh.d[id] == nil {
		delete(sh.d, id)
	}
	if clean.len() == 0 {
		delete(sh.m, id)
	} else {
		sh.m[id] = clean
	}
	// THE LABEL INDEX. A label the aborted transaction ADDED went into the bitmap
	// immediately — only REMOVALS are deferred — so withdrawing it from the bag
	// while leaving the entry makes the bitmap over-report. That is normally the
	// harmless direction, and here it is not: [Graph.LabelCountExact] serves
	// `count(*)` from the bitmap whenever nothing is deferred, and this withdrawal
	// has just cleared the aborted transaction's deferrals. Measured before this
	// correction: `MATCH (n:Person:Admin) RETURN count(*)` answered 1 against a
	// hand-computed oracle of 0 (TestMVCCSnapshotRead_AbsoluteOracle).
	//
	// Only the labels the withdrawal actually took away are removed, and only when
	// the clean bag no longer carries them — a label the node still holds from an
	// earlier COMMITTED add must keep its entry, and dropping it would be the
	// unrecoverable direction.
	before.forEach(func(lid LabelID) {
		if !clean.has(lid) {
			g.nodeIdx.Remove(uint32(lid), id)
		}
	})
	return freed
}

// reclaimAbortedPropsLocked is the property-chain counterpart.
func (g *Graph[N, W]) reclaimAbortedPropsLocked(sh *nodePropShard, id graph.NodeID) int {
	head := sh.d[id]
	if head == nil || !abortedHead(head.stampTS()) {
		return 0
	}
	clean := clonePropBag(g.propBagAsOfLocked(sh, id, g.mvccClock.ReadTS(), 0))
	freed := 0
	for d := sh.d[id]; d != nil && abortedHead(d.stampTS()); d = sh.d[id] {
		sh.d[id] = d.next
		freed++
	}
	if sh.d[id] == nil {
		delete(sh.d, id)
	}
	if clean.len() == 0 {
		delete(sh.m, id)
	} else {
		sh.m[id] = clean
	}
	return freed
}
