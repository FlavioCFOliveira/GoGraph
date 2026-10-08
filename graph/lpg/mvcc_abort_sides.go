package lpg

// mvcc_abort_sides.go — MVCC (rmp #2318): withdrawing an aborted transaction's
// writes from the five per-edge side stores, the node-existence records and the
// adjacency conflict stamps.
//
// The reasoning, the measurements and the prior art live in
// mvcc_abort_reclaim.go. What is here is the per-store mechanics, which differ
// only because each store keeps its current value in a differently-shaped map:
// the version chains are all [sideVersions] and all withdraw through
// [sideVersions.withdrawAborted].
//
// Every function here must be called with its shard's write lock held, and must
// apply the restored value before releasing it — a chain that no longer masks a
// value the store has not yet corrected is exactly the exposure this task exists
// to prevent.

import (
	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// withdrawAbortedEdgeLabels restores the overflow relationship-type store.
//
// The overflow store's value is a flat []LabelID per pair, so the restore is a
// plain assignment or a deletion.
func (g *Graph[N, W]) withdrawAbortedEdgeLabelsLocked(sh *edgeLabelShard) int {
	if sh.v.d == nil {
		return 0
	}
	freed := 0
	for k := range sh.v.d {
		freed += g.withdrawAbortedEdgeLabelLocked(sh, k)
	}
	return freed
}

// withdrawAbortedEdgeLabelLocked is [Graph.withdrawAbortedEdgeLabelsLocked] for
// the one pair k.
func (g *Graph[N, W]) withdrawAbortedEdgeLabelLocked(sh *edgeLabelShard, k edgeKey) int {
	pre, had, n, withdrew := sh.v.withdrawAborted(k)
	if !withdrew {
		return 0
	}
	// The gate counts the overflow labels the map HOLDS, so restoring a list
	// moves it by the difference (rmp #2947). An aborted clear restores
	// labels the clear had subtracted, and an aborted add takes away one it
	// had added; leaving the gate where the aborted write put it let a later
	// read of a pair that still carries overflow labels skip them at zero.
	before := len(sh.overflow[k])
	if had {
		if sh.overflow == nil {
			sh.overflow = make(map[edgeKey][]LabelID, 1)
		}
		sh.overflow[k] = pre
	} else {
		delete(sh.overflow, k)
	}
	if d := len(sh.overflow[k]) - before; d != 0 {
		g.edgeLabelOverflowActive.Add(int64(d))
	}
	return n
}

// withdrawAbortedHandleLabelsLocked restores the per-handle relationship-type
// store, whose value is nested: pair -> handle -> bag.
func (g *Graph[N, W]) withdrawAbortedHandleLabelsLocked(sh *edgeHandleLabelShard) int {
	if sh.v.d == nil {
		return 0
	}
	freed := 0
	for k := range sh.v.d {
		freed += g.withdrawAbortedHandleLabelLocked(sh, k)
	}
	return freed
}

// withdrawAbortedHandleLabelLocked is [Graph.withdrawAbortedHandleLabelsLocked] for the one key k.
func (g *Graph[N, W]) withdrawAbortedHandleLabelLocked(sh *edgeHandleLabelShard, k edgeHandleKey) int {
	pre, had, n, withdrew := sh.v.withdrawAborted(k)
	if !withdrew {
		return 0
	}
	// The zero instMap is a valid empty one, so an absent pair needs no
	// special case: set materialises it and the write-back installs it.
	im := sh.m[k.pair]
	if had {
		im.set(k.handle, pre)
		sh.m[k.pair] = im
		return n
	}
	im.del(k.handle)
	if im.len() == 0 {
		delete(sh.m, k.pair)
	} else {
		sh.m[k.pair] = im
	}
	return n
}

// withdrawAbortedHandlePropsLocked is the per-handle property counterpart.
func (g *Graph[N, W]) withdrawAbortedHandlePropsLocked(sh *edgeHandlePropShard) int {
	if sh.v.d == nil {
		return 0
	}
	freed := 0
	for k := range sh.v.d {
		freed += g.withdrawAbortedHandlePropLocked(sh, k)
	}
	return freed
}

// withdrawAbortedHandlePropLocked is [Graph.withdrawAbortedHandlePropsLocked] for the one key k.
func (g *Graph[N, W]) withdrawAbortedHandlePropLocked(sh *edgeHandlePropShard, k edgeHandleKey) int {
	pre, had, n, withdrew := sh.v.withdrawAborted(k)
	if !withdrew {
		return 0
	}
	// The zero instMap is a valid empty one, so an absent pair needs no
	// special case: set materialises it and the write-back installs it.
	im := sh.m[k.pair]
	if had {
		im.set(k.handle, pre)
		sh.m[k.pair] = im
		return n
	}
	im.del(k.handle)
	if im.len() == 0 {
		delete(sh.m, k.pair)
	} else {
		sh.m[k.pair] = im
	}
	return n
}

// withdrawAbortedInstanceLabelsLocked restores the by-ordinal relationship-type
// store, whose value is nested: pair -> ordinal -> bag.
func (g *Graph[N, W]) withdrawAbortedInstanceLabelsLocked(sh *edgeInstanceLabelShard) int {
	if sh.v.d == nil {
		return 0
	}
	freed := 0
	for k := range sh.v.d {
		freed += g.withdrawAbortedInstanceLabelLocked(sh, k)
	}
	return freed
}

// withdrawAbortedInstanceLabelLocked is [Graph.withdrawAbortedInstanceLabelsLocked] for the one key k.
func (g *Graph[N, W]) withdrawAbortedInstanceLabelLocked(sh *edgeInstanceLabelShard, k edgeInstanceKey) int {
	pre, had, n, withdrew := sh.v.withdrawAborted(k)
	if !withdrew {
		return 0
	}
	// The zero instMap is a valid empty one, so an absent pair needs no
	// special case: set materialises it and the write-back installs it.
	im := sh.m[k.pair]
	if had {
		im.set(k.idx, pre)
		sh.m[k.pair] = im
		return n
	}
	im.del(k.idx)
	if im.len() == 0 {
		delete(sh.m, k.pair)
	} else {
		sh.m[k.pair] = im
	}
	return n
}

// withdrawAbortedInstancePropsLocked is the by-ordinal property counterpart.
func (g *Graph[N, W]) withdrawAbortedInstancePropsLocked(sh *edgeInstancePropShard) int {
	if sh.v.d == nil {
		return 0
	}
	freed := 0
	for k := range sh.v.d {
		freed += g.withdrawAbortedInstancePropLocked(sh, k)
	}
	return freed
}

// withdrawAbortedInstancePropLocked is [Graph.withdrawAbortedInstancePropsLocked] for the one key k.
func (g *Graph[N, W]) withdrawAbortedInstancePropLocked(sh *edgeInstancePropShard, k edgeInstanceKey) int {
	pre, had, n, withdrew := sh.v.withdrawAborted(k)
	if !withdrew {
		return 0
	}
	// The zero instMap is a valid empty one, so an absent pair needs no
	// special case: set materialises it and the write-back installs it.
	im := sh.m[k.pair]
	if had {
		im.set(k.idx, pre)
		sh.m[k.pair] = im
		return n
	}
	im.del(k.idx)
	if im.len() == 0 {
		delete(sh.m, k.pair)
	} else {
		sh.m[k.pair] = im
	}
	return n
}

// reclaimAbortedLife drops the birth and death records an aborted transaction
// wrote, and reconciles the tombstone bitmap with them.
//
// A life record is a single instant per direction rather than a chain, so its undo
// is its removal — and the removal alone is not enough. A transaction that created
// a node left it ABSENT from the tombstone set and one that removed a node left it
// PRESENT, so dropping the record without correcting the bitmap would let the
// aborted transaction's answer stand as the fallback.
//
// The bitmap is corrected OUTSIDE the life shard lock, because the tombstone
// mutators take a lock of their own and taking it under a shard lock inverts the
// order [Graph.reviveNode] documents.
//
// It sweeps every shard, for [Graph.withdrawAbortedAll]; an abort withdraws its
// own records through [Graph.reclaimAbortedLifeOf].
func (g *Graph[N, W]) reclaimAbortedLife() int {
	if g.nodeLifeActive.Load() == 0 {
		return 0
	}
	lw := lifeWithdrawal{markUnborn: g.markUnborn}
	for i := range g.nodeLifeShards {
		sh := &g.nodeLifeShards[i]
		sh.mu.Lock()
		for id, st := range sh.born {
			if st.at() == mvcc.AbortedTS {
				lw.withdrawLocked(sh, id)
			}
		}
		if h := g.reclaimAbortedLifeHookForTest; h != nil {
			h(sh)
		}
		// THE PAIR IS DECIDED HERE TOO (rmp #2949). The abort that makes a
		// record's timestamp read AbortedTS is an atomic store by the
		// transaction's own goroutine, taken under no shard lock, so it can land
		// after the loop above passed over this node's birth as not yet aborted
		// and before this loop reads its death. Both records carry the same
		// commit record, so the birth reads AbortedTS by now as well, and
		// [lifeWithdrawal.withdrawLocked] reads both. Withdrawing the death alone
		// split the pair: the birth, left for a later pass, was taken for a
		// create and tombstoned a node that was alive before the transaction —
		// the node of a rolled-back DETACH DELETE vanished for every reader,
		// measured at about one in 20000 concurrent runs of the straddler
		// enumeration's case.
		for id, st := range sh.died {
			if st.at() == mvcc.AbortedTS {
				lw.withdrawLocked(sh, id)
			}
		}
		for id, c := range sh.claim {
			if c.at() == mvcc.AbortedTS {
				lw.withdrawLocked(sh, id)
			}
		}
		if len(sh.claim) == 0 {
			sh.claim = nil
		}
		if len(sh.born) == 0 {
			sh.born = nil
		}
		if len(sh.died) == 0 {
			sh.died = nil
		}
		sh.mu.Unlock()
	}
	return g.finishLifeWithdrawal(&lw)
}

// reclaimAbortedLifeOf is [Graph.reclaimAbortedLife] for the nodes ids alone:
// the node-life write set of one aborted transaction, whose record is already
// marked aborted (ACID audit round 6, finding M1). A node entered more than once
// is withdrawn by its first visit.
func (g *Graph[N, W]) reclaimAbortedLifeOf(ids []graph.NodeID) int {
	lw := lifeWithdrawal{markUnborn: g.markUnborn}
	for _, id := range ids {
		sh := g.nodeLifeShardFor(id)
		sh.mu.Lock()
		lw.withdrawLocked(sh, id)
		if len(sh.born) == 0 {
			sh.born = nil
		}
		if len(sh.died) == 0 {
			sh.died = nil
		}
		if len(sh.claim) == 0 {
			sh.claim = nil
		}
		sh.mu.Unlock()
	}
	return g.finishLifeWithdrawal(&lw)
}

// lifeWithdrawal accumulates what withdrawing aborted life records owes the
// tombstone bitmap, the unborn set and the churn gate, for
// [Graph.finishLifeWithdrawal] to settle outside the shard locks.
type lifeWithdrawal struct {
	// markUnborn marks an id unborn. [lifeWithdrawal.withdrawLocked] calls it
	// UNDER the life-shard lock, before it deletes the aborted birth record, so
	// [Graph.NodeBornAsOf] never sees an aborted first creation as neither
	// recorded nor unborn (WAL v2 step 1, design risk 6). The lock order is
	// life shard, then unbornMu, which is a leaf lock.
	markUnborn  func(graph.NodeID)
	toTombstone []lifeTombstone
	toRevive    []graph.NodeID
	released    []LabelID
	freed       int
}

// lifeTombstone is one node an aborted birth leaves dead, and whether it had
// never existed before the aborted transaction ([lifeStamp.unbornBefore]).
type lifeTombstone struct {
	id     graph.NodeID
	unborn bool
}

// withdrawLocked withdraws id's aborted birth and death records, and records
// the state the node must return to: the state immediately before the aborted
// transaction first touched it. The caller holds sh's write lock.
//
//   - An aborted birth AND death are one transaction's whole work on the node
//     and are withdrawn together; the pair's write order encodes the state
//     before them ([aliveBefore]). Deciding each direction independently
//     tombstoned the node and then REVIVED it, so an aborted create+delete left
//     a bare phantom node visible to every reader (rmp #2443). died-then-born —
//     an applied delete the rollback's undo replay revived — means the node was
//     ALIVE when the transaction first touched it (rmp #2445).
//   - A lone aborted birth is a creation or a revival, which leaves the node
//     dead — unless it is the undo replay of a rolled-back DELETE whose death
//     record is already gone ([lifeStamp.wasAlive]), which leaves it alive.
//   - A lone aborted death was a removal of a living node, which leaves it
//     alive: a removal of a node already dead records nothing
//     ([Graph.removeNodeInfo]).
//
// A node left dead is marked unborn ONLY when the aborted birth was its first
// existence ([lifeStamp.unbornBefore]); the revival of a committed-dead node
// returns it to dead-and-born (ACID audit round 6, finding C2).
//
// An aborted existence claim ([Graph.noteNodeClaim]) is dropped and changes
// nothing else: it recorded no event.
func (lw *lifeWithdrawal) withdrawLocked(sh *nodeLifeShard, id graph.NodeID) {
	if c, ok := sh.claim[id]; ok && c.at() == mvcc.AbortedTS {
		delete(sh.claim, id)
		lw.freed++
	}
	born, hasBorn := sh.born[id]
	died, hasDied := sh.died[id]
	bornAborted := hasBorn && born.at() == mvcc.AbortedTS
	diedAborted := hasDied && died.at() == mvcc.AbortedTS
	switch {
	case bornAborted && diedAborted:
		revive := aliveBefore(born, died)
		if !revive && born.unbornBefore {
			lw.markUnborn(id)
		}
		lw.withdrawRecordLocked(sh, true, id, born)
		lw.withdrawRecordLocked(sh, false, id, died)
		if revive {
			lw.toRevive = append(lw.toRevive, id)
		} else {
			lw.toTombstone = append(lw.toTombstone, lifeTombstone{id: id, unborn: born.unbornBefore})
		}
	case bornAborted:
		if !born.wasAlive && born.unbornBefore {
			lw.markUnborn(id)
		}
		lw.withdrawRecordLocked(sh, true, id, born)
		if born.wasAlive {
			lw.toRevive = append(lw.toRevive, id)
			return
		}
		lw.toTombstone = append(lw.toTombstone, lifeTombstone{id: id, unborn: born.unbornBefore})
	case diedAborted:
		lw.withdrawRecordLocked(sh, false, id, died)
		lw.toRevive = append(lw.toRevive, id)
	}
}

// withdrawRecordLocked withdraws st, id's aborted birth (alive) or death record,
// by putting back the record it displaced (rmp #3001), and deletes the slot only
// when it displaced nothing. A displaced record that is itself aborted is
// stepped over.
//
// Deleting unconditionally lost the displaced commit: a reader older than it fell
// back to the present tombstone bitmap, so a node created after a read
// transaction began reappeared in it once a DETACH DELETE of that node rolled
// back (the undo's revival is a birth that displaces the committed one).
//
// A restored record keeps the churn holds the withdrawn one took, which name the
// node's labels as the restored record's own did: a hold outliving its reason
// only over-counts, the safe direction. The caller holds sh's write lock.
func (lw *lifeWithdrawal) withdrawRecordLocked(sh *nodeLifeShard, alive bool, id graph.NodeID, st lifeStamp) {
	m := sh.died
	if alive {
		m = sh.born
	}
	r := st.displaced
	for r != nil && r.at() == mvcc.AbortedTS {
		r = r.displaced
	}
	if r != nil {
		m[id] = *r
		return
	}
	delete(m, id)
	lw.released = append(lw.released, sh.takeChurnHeld(alive, id)...)
	lw.freed++
}

// finishLifeWithdrawal applies what lw accumulated — the bitmap flips, the
// unborn marks and the churn holds — and returns the records it freed. It runs
// with no life shard lock held.
func (g *Graph[N, W]) finishLifeWithdrawal(lw *lifeWithdrawal) int {
	if lw.freed > 0 {
		g.nodeLifeActive.Add(-int64(lw.freed))
	}
	for _, t := range lw.toTombstone {
		g.tombstoneAborted(t.id)
		if t.unborn {
			// The node existed only by the aborted transaction's creation:
			// a first birth, or a create-then-delete pair (rmp #2947, see
			// [Graph.unborn]). withdrawLocked already marked it under the life-
			// shard lock (WAL v2 step 1); marking is idempotent, and repeating it
			// here keeps the set right for any withdrawal built without the hook.
			g.markUnborn(t.id)
		}
	}
	for _, id := range lw.toRevive {
		g.reviveAborted(id)
	}
	// THE CHURN HOLDS GO LAST, after both flips (rmp #2686). The records are
	// already gone, so the holds are over-counting from the moment they were
	// deleted — but the flips are the instant at which a reader's answer
	// actually changes, and dropping the holds before them would let a reader
	// take the fast path across exactly that transition.
	g.labelChurn.releaseAll(lw.released)
	return lw.freed
}

// tombstoneAborted marks id dead in the bitmap without recording a death
// instant, which is what withdrawing an aborted CREATE means: there is no
// transaction whose removal it would be.
func (g *Graph[N, W]) tombstoneAborted(id graph.NodeID) {
	if !g.IsTombstonedStored(id) {
		// The node is about to stop existing with NO life record to say so, and
		// this call strips no label bitmaps. Any index entry it still carries is
		// therefore a permanent disagreement that nothing will ever revisit, so
		// the churn gate is pinned for those labels before the flip that creates
		// it. See [Graph.pinChurnForDivergentBag] for why the probe is exact
		// rather than blind, and why the ordinary aborted CREATE pins nothing.
		g.pinChurnForDivergentBag(id, false)
	}
	g.tombstoneMu.Lock()
	cur := g.tombstones.Load()
	if cur != nil && cur.Contains(uint64(id)) {
		g.tombstoneMu.Unlock()
		return
	}
	next := roaring64.New()
	if cur != nil {
		next = cur.Clone()
	}
	next.Add(uint64(id))
	// Counter first, bitmap second — see [Graph.removeNodeInfo] (rmp #2687).
	g.tombstoneActive.Add(1)
	g.tombstones.Store(next)
	g.tombstoneMu.Unlock()
	g.BumpTopoGeneration()
}

// reviveAborted clears id from the bitmap without recording a birth instant,
// which is what withdrawing an aborted DELETE means.
func (g *Graph[N, W]) reviveAborted(id graph.NodeID) {
	if g.IsTombstonedStored(id) {
		// The mirror image of [Graph.tombstoneAborted]: the node is about to
		// exist again with no life record, and this call restores no label
		// bitmaps, so a label the bag carries but the index has lost is a
		// permanent disagreement in the LOSING direction — a silently absent
		// row. Pinned before the flip, for the same reason.
		g.pinChurnForDivergentBag(id, true)
	}
	g.tombstoneMu.Lock()
	cur := g.tombstones.Load()
	if cur == nil || !cur.Contains(uint64(id)) {
		g.tombstoneMu.Unlock()
		return
	}
	next := cur.Clone()
	next.Remove(uint64(id))
	// Bitmap first, counter second on the way DOWN, so the count never falls
	// short of the published set — see [Graph.removeNodeInfo] (rmp #2687).
	g.tombstones.Store(next)
	g.tombstoneActive.Add(-1)
	g.tombstoneMu.Unlock()
	g.clearUnborn(id)
	g.BumpTopoGeneration()
}

// clearAbortedOf is [adjVersions.clearAborted] for the nodes ids alone: the
// adjacency-claim write set of one aborted transaction (ACID audit round 6,
// finding M1).
func (av *adjVersions) clearAbortedOf(ids []graph.NodeID) (freed int) {
	for _, id := range ids {
		sh := av.shard(id)
		sh.mu.Lock()
		freed += sh.clearAbortedLocked(id)
		sh.releaseIfEmptyLocked()
		sh.mu.Unlock()
	}
	return freed
}

// clearAbortedLocked clears the aborted sides of id's stamps, dropping the entry
// when both are aborted and it keeps no displaced commit, and reports whether it
// dropped it. The caller holds the shard lock.
//
// A cleared side does not take the commit it displaced with it: that commit was
// folded into floorTS when the aborted write overwrote it ([adjStamps.set]), so
// the entry stays, floor alone, until the watermark passes it (rmp #2997).
func (sh *adjVersionShard) clearAbortedLocked(id graph.NodeID) int {
	e := sh.d[id]
	if e == nil {
		return 0
	}
	a := stampTS(e.appendInfo)
	x := stampTS(e.exclusiveInfo)
	if a == mvcc.AbortedTS && x == mvcc.AbortedTS && e.floorTS == 0 {
		delete(sh.d, id)
		return 1
	}
	// One side aborted and the other live: clear only the aborted side, so the
	// live one keeps refusing what it must.
	if a == mvcc.AbortedTS {
		e.appendInfo = nil
	}
	if x == mvcc.AbortedTS {
		e.exclusiveInfo = nil
	}
	return 0
}

// clearAborted drops every adjacency conflict stamp an aborted transaction set,
// and reports how many entries it removed.
//
// The stamps carry no pre-image and take no part in a reader's decision, so their
// undo is their removal. [adjVersions.truncate] cannot reach them: it compares
// against the watermark and [mvcc.AbortedTS] is above every watermark there can
// be.
func (av *adjVersions) clearAborted() (freed int) {
	for i := range av.shards {
		sh := &av.shards[i]
		sh.mu.Lock()
		for id := range sh.d {
			freed += sh.clearAbortedLocked(id)
		}
		sh.releaseIfEmptyLocked()
		sh.mu.Unlock()
	}
	return freed
}
