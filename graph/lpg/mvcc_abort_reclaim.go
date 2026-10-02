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
// # The withdrawal now runs at abort, by the transaction's write set
//
// This file first deferred the withdrawal to the vacuum, because doing it at
// abort needs the transaction's own write set — Memgraph's
// `transaction_.deltas` — and GoGraph kept none. [Graph.withdrawAbortedNow] later
// moved it onto the aborting goroutine for correctness, and scanned every store
// to find the aborted heads. Since ACID audit round 6 (finding M1) a transaction
// records each object it versions in its [mvcc.TxState] write set, one entry per
// object written, and the abort visits exactly those, so a conflict-and-rerun
// workload no longer pays a whole-graph scan per refused attempt. A statement
// that writes no side-store object records nothing, and one that writes a few
// records them in an inline buffer without allocating. Half 1 above still holds:
// the dirty base is unwritable until the withdrawal has run.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// withdrawAbortedNow withdraws every version the aborted transaction whose state
// is st wrote, synchronously, and returns how many records it released.
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
// # The cost: the transaction's own writes (ACID audit round 6, finding M1)
//
// It visits the objects the transaction wrote and nothing else. Every version a
// transaction creates in a node-label, node-property, node-life, adjacency-claim or
// per-edge side store is entered in its [mvcc.TxState] write set as it is created
// ([writeCtx.noteSide]), exactly as its adjacency entries are entered for
// [Graph.abortRecord], so an abort is proportional to what the transaction did.
// It used to scan every object in the graph that carried history, which a
// conflict-and-rerun workload paid on every refused attempt.
//
// Two stores are still swept rather than visited, and only when the transaction
// touched them: the deferred label-index removals and the constraint stamps,
// which a transaction reaches far more rarely than the stores above.
//
// One shape has no write set to visit: a version an untransacted writer stamped
// with st's record through the ambient slot ([mvcc.TouchedAmbient]). Such an
// abort falls back to the full sweep, [Graph.withdrawAbortedAll], so no version
// is ever missed.
func (g *Graph[N, W]) withdrawAbortedNow(st *mvcc.TxState) int {
	if !g.mvccArmed {
		return 0
	}
	g.vac.acquireSweeper()
	defer g.vac.releaseSweeper()
	touched := st.Touched()
	if touched&mvcc.TouchedAmbient != 0 {
		return g.withdrawAbortedAllLocked()
	}
	freed := g.withdrawAbortedWrites(st.SideWrites())
	if touched&touchedIdxRemoval != 0 {
		freed += g.withdrawAbortedIndexRemovals()
	}
	if touched&touchedConstraint != 0 {
		// The constraint stamps an aborted transaction set (rmp #2353): a stamp at
		// [mvcc.AbortedTS] refuses every later writer forever, and the watermark
		// sweep cannot reach it because AbortedTS is above every watermark there
		// can be.
		g.conVer.clearAborted()
	}
	return freed
}

// withdrawAbortedAll withdraws every aborted version in the graph, whichever
// transaction wrote it, and returns how many records it released. It is the
// fallback [Graph.withdrawAbortedNow] takes for a transaction whose write set is
// incomplete, and costs O(objects carrying history).
func (g *Graph[N, W]) withdrawAbortedAll() int {
	if !g.mvccArmed {
		return 0
	}
	g.vac.acquireSweeper()
	defer g.vac.releaseSweeper()
	return g.withdrawAbortedAllLocked()
}

// withdrawAbortedAllLocked is [Graph.withdrawAbortedAll] for a caller that
// holds the sweeper slot.
func (g *Graph[N, W]) withdrawAbortedAllLocked() int {
	freed := g.withdrawAbortedLabels() + g.withdrawAbortedProps() +
		g.withdrawAbortedSides() + g.reclaimAbortedLife() +
		g.withdrawAbortedIndexRemovals()
	g.adjVer.clearAborted()
	g.conVer.clearAborted()
	return freed
}

// The stores a [mvcc.SideWrite] names, as [writeCtx.noteSide] records them.
const (
	sideNodeLabels     uint8 = iota + 1 // A: node id
	sideNodeProps                       // A: node id
	sideNodeLife                        // A: node id
	sideAdjClaim                        // A: node id
	sideEdgeOverflow                    // A, B: pair
	sideHandleLabels                    // A, B: pair; C: handle
	sideHandleProps                     // A, B: pair; C: handle
	sideInstanceLabels                  // A, B: pair; C: ordinal
	sideInstanceProps                   // A, B: pair; C: ordinal
)

// noteSide enters one versioned object in the transaction's write set, so its
// abort withdraws that object without scanning the store. A nil receiver — a
// write on a disarmed graph — has nothing to withdraw.
func (w *writeCtx) noteSide(store uint8, a, b, c uint64) {
	if w == nil {
		return
	}
	w.tx.NoteSide(mvcc.SideWrite{Store: store, A: a, B: b, C: c})
}

// withdrawAbortedWrites withdraws the aborted head of every object in writes,
// a transaction's side write set, and returns how many records it released.
// An object entered twice is withdrawn by its first visit and found clean by
// the second. The caller holds the sweeper slot, and the transaction's record
// is already marked aborted.
func (g *Graph[N, W]) withdrawAbortedWrites(writes []mvcc.SideWrite) int {
	if len(writes) == 0 {
		return 0
	}
	var (
		labels, props, life, overflow int
		hLabels, hProps, iLabels      int
		iProps                        int
		lifeIDs, claimIDs             []graph.NodeID
	)
	for _, w := range writes {
		pair := edgeKey{src: graph.NodeID(w.A), dst: graph.NodeID(w.B)}
		switch w.Store {
		case sideNodeLabels:
			id := graph.NodeID(w.A)
			sh := g.nodeLabelShardFor(id)
			sh.mu.Lock()
			labels += g.reclaimAbortedLabelsLocked(sh, id)
			if len(sh.d) == 0 {
				sh.d = nil
			}
			sh.mu.Unlock()
		case sideNodeProps:
			id := graph.NodeID(w.A)
			sh := g.nodePropShardFor(id)
			sh.mu.Lock()
			props += g.reclaimAbortedPropsLocked(sh, id)
			if len(sh.d) == 0 {
				sh.d = nil
			}
			sh.mu.Unlock()
		case sideNodeLife:
			lifeIDs = append(lifeIDs, graph.NodeID(w.A))
		case sideAdjClaim:
			claimIDs = append(claimIDs, graph.NodeID(w.A))
		case sideEdgeOverflow:
			sh := g.edgeLabelShardFor(pair)
			sh.mu.Lock()
			overflow += g.withdrawAbortedEdgeLabelLocked(sh, pair)
			sh.mu.Unlock()
		case sideHandleLabels:
			sh := g.edgeHandleLabelShardFor(pair)
			sh.mu.Lock()
			hLabels += g.withdrawAbortedHandleLabelLocked(sh, edgeHandleKey{pair: pair, handle: w.C})
			sh.mu.Unlock()
		case sideHandleProps:
			sh := g.edgeHandlePropShardFor(pair)
			sh.mu.Lock()
			hProps += g.withdrawAbortedHandlePropLocked(sh, edgeHandleKey{pair: pair, handle: w.C})
			sh.mu.Unlock()
		case sideInstanceLabels:
			sh := g.edgeInstanceLabelShardFor(pair)
			sh.mu.Lock()
			iLabels += g.withdrawAbortedInstanceLabelLocked(sh, edgeInstanceKey{pair: pair, idx: int64(w.C)})
			sh.mu.Unlock()
		case sideInstanceProps:
			sh := g.edgeInstancePropShardFor(pair)
			sh.mu.Lock()
			iProps += g.withdrawAbortedInstancePropLocked(sh, edgeInstanceKey{pair: pair, idx: int64(w.C)})
			sh.mu.Unlock()
		}
	}
	if labels > 0 {
		g.labelDeltaActive.Add(-int64(labels))
	}
	if props > 0 {
		g.propDeltaActive.Add(-int64(props))
	}
	if overflow > 0 {
		g.edgeLabelVersionActive.Add(-int64(overflow))
	}
	if hLabels > 0 {
		g.edgeHandleLabelVersionActive.Add(-int64(hLabels))
	}
	if hProps > 0 {
		g.edgeHandlePropVersionActive.Add(-int64(hProps))
	}
	if iLabels > 0 {
		g.edgeInstanceLabelVersionActive.Add(-int64(iLabels))
	}
	if iProps > 0 {
		g.edgeInstancePropVersionActive.Add(-int64(iProps))
	}
	if len(lifeIDs) > 0 {
		life = g.reclaimAbortedLifeOf(lifeIDs)
	}
	if len(claimIDs) > 0 {
		g.adjVer.clearAbortedOf(claimIDs)
	}
	return labels + props + life + overflow + hLabels + hProps + iLabels + iProps
}

// abortRecord aborts the transaction whose state is st and whose commit record
// is info: it restores the adjacency entries the transaction published to their
// pre-images, then marks info aborted, and returns how many adjacency version
// records the restoration released, which the caller's [Graph.abortWake] charge
// excludes. It visits only the transaction's own adjacency write set, so an
// abort that wrote no adjacency touches no adjacency shard.
//
// The adjacency is withdrawn HERE, while the record is still in flight, and not
// by [Graph.withdrawAbortedNow] like every other store (rmp #2965). Its rollback
// used to be physical only, by an undo log or a withdrawal the write itself
// makes, so a bracket aborted without one — [Graph.ApplyVersioned] as the
// durable store's apply runs it, [Graph.ApplyAtomicallyTx], an explicit
// transaction ended without a Cypher undo — left its adjacency writes applied. An
// adjacency entry is an immutable snapshot, so a write built on an aborted one
// would embed the aborted change for good; restoring the entries before the
// record is marked means none is ever the stored value. Until then the
// transaction's claims still refuse every topology writer, exactly as they did
// for its whole life. See [adjlist.AdjList.WithdrawTx]. A restored entry changes
// topology, so the generation every topology-keyed cache checks moves on.
func (g *Graph[N, W]) abortRecord(st *mvcc.TxState, info *mvcc.CommitInfo) int64 {
	freed := g.adj.WithdrawTx(info, st.AdjacencyWrites())
	if freed > 0 {
		g.topoGeneration.Add(1)
	}
	info.Abort()
	return int64(freed)
}

// The stores a transaction marks on its [mvcc.TxState] when it writes to them.
// The withdrawal of an aborted transaction visits its write sets, and sweeps the
// two stores that keep none only when these bits say it wrote them.
const (
	touchedLife       uint32 = 1 << iota // a node-life record ([Graph.noteNodeLife])
	touchedAdjClaims                     // an adjacency conflict stamp ([adjVersions])
	touchedIdxRemoval                    // a deferred label-index removal ([Graph.deferLabelIndexRemoval])
	touchedConstraint                    // a constraint stamp ([constraintVersions.note])
)

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
	var released []LabelID
	var aborted []idxEntry
	for k, st := range g.idxDeferred.pending {
		if st.at() == mvcc.AbortedTS {
			aborted = append(aborted, k)
		}
	}
	for k, sh := range g.idxDeferred.shadow {
		// A replaced stamp that aborted itself has nothing left to reinstate.
		if sh.st.at() == mvcc.AbortedTS {
			g.idxDeferred.dropShadowLocked(k)
		}
	}
	for _, k := range aborted {
		// ANOTHER TRANSACTION STILL OWES THIS REMOVAL when the aborted stamp
		// replaced its own (rmp #2947, audit F4): reinstate that stamp, and the
		// retirement mark it carried, instead of dropping the key. The key stays
		// pending, so neither the count nor the per-label hold changes.
		if sh, ok := g.idxDeferred.shadow[k]; ok {
			g.idxDeferred.pending[k] = sh.st
			if sh.retiring {
				if g.idxDeferred.retiring == nil {
					g.idxDeferred.retiring = make(map[idxEntry]struct{}, 1)
				}
				g.idxDeferred.retiring[k] = struct{}{}
			} else if g.idxDeferred.retiring != nil {
				delete(g.idxDeferred.retiring, k)
				if len(g.idxDeferred.retiring) == 0 {
					g.idxDeferred.retiring = nil
				}
			}
			g.idxDeferred.dropShadowLocked(k)
			continue
		}
		// Every map, so a withdrawn retirement removal leaves no retiring mark
		// behind (rmp #2964).
		g.idxDeferred.dropLocked(k)
		cancelled++
		released = append(released, LabelID(k.lid))
	}
	g.idxDeferred.mu.Unlock()
	if cancelled > 0 {
		g.idxPendingActive.Add(-int64(cancelled))
		// The per-label holds these entries owned (rmp #2686). A cancelled
		// removal leaves the bitmap entry in place beside a bag that still
		// carries the label — the two agree again — so the suspect has stopped
		// being one and the hold goes with it.
		g.labelChurn.releaseAll(released)
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
	// UNDO ONLY THE ABORTED HEAD DELTAS (rmp #2326).
	//
	// This used to recompute the bag with [Graph.labelBagAsOfLocked] at the present
	// instant, on the reasoning that the reader's own walk cannot disagree with the
	// reader. It can, and it LOST WRITES: that walk undoes every delta invisible at
	// the given instant, which includes a CONCURRENT IN-FLIGHT transaction's, and the
	// result was stored back as the bag — discarding work the other transaction had
	// already applied. Bisected with a powered probe (`go test -race
	// ./internal/sim/ -run TestSchemaMutation_OracleModelsMutations -count=200`):
	// "REMOVE n:Vip … engine=1, want=0 (SET/REMOVE label did not round-trip)" at
	// 4/200, against 0/200 at the sprint's entry head.
	//
	// Each delta records what to do to reverse ITSELF, so applying only the aborted
	// ones is both narrower and exact.
	clean := cloneLabelBag(sh.m[id])
	before := cloneLabelBag(sh.m[id])
	freed := 0
	var held []LabelID
	for d := sh.d[id]; d != nil && abortedHead(d.stampTS()); d = sh.d[id] {
		switch d.action {
		case undoAddLabel:
			clean.add(d.lid)
		case undoRemoveLabel:
			clean.del(d.lid)
		}
		sh.d[id] = d.next
		// The per-label hold this delta owned is released at the END of this
		// function, after the bag AND the bitmap have both been corrected — see
		// there for the window that releasing here would open.
		held = append(held, d.lid)
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
	// THE PER-LABEL HOLDS GO LAST (rmp #2686). Each withdrawn delta owned one,
	// taken by [nodeLabelShard.pushLabelDelta], and it may only be dropped once
	// the bag and the bitmap agree again. Dropping it inside the unlink loop
	// above left a window in which the bag had already lost the label, the
	// bitmap still carried it, and the gate said the label was quiet — which is
	// the under-count the whole mechanism exists to prevent.
	g.labelChurn.releaseAll(held)
	return freed
}

// reclaimAbortedPropsLocked is the property-chain counterpart.
func (g *Graph[N, W]) reclaimAbortedPropsLocked(sh *nodePropShard, id graph.NodeID) int {
	head := sh.d[id]
	if head == nil || !abortedHead(head.stampTS()) {
		return 0
	}
	// Undo ONLY the aborted head deltas; see [Graph.reclaimAbortedLabelsLocked] for
	// the concurrent writes the general as-of walk lost when it was used here.
	clean := clonePropBag(sh.m[id])
	freed := 0
	for d := sh.d[id]; d != nil && abortedHead(d.stampTS()); d = sh.d[id] {
		switch d.action {
		case undoSetProp:
			clean.set(d.key, d.prev)
		case undoDelProp:
			clean.del(d.key)
		}
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
