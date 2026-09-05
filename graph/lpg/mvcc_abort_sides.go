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
		pre, had, n, withdrew := sh.v.withdrawAborted(k)
		if !withdrew {
			continue
		}
		freed += n
		if had {
			if sh.overflow == nil {
				sh.overflow = make(map[edgeKey][]LabelID, 1)
			}
			sh.overflow[k] = pre
		} else {
			delete(sh.overflow, k)
		}
	}
	return freed
}

// withdrawAbortedHandleLabelsLocked restores the per-handle relationship-type
// store, whose value is nested: pair -> handle -> bag.
func (g *Graph[N, W]) withdrawAbortedHandleLabelsLocked(sh *edgeHandleLabelShard) int {
	if sh.v.d == nil {
		return 0
	}
	freed := 0
	for k := range sh.v.d {
		pre, had, n, withdrew := sh.v.withdrawAborted(k)
		if !withdrew {
			continue
		}
		freed += n
		// The zero instMap is a valid empty one, so an absent pair needs no
		// special case: set materialises it and the write-back installs it.
		im := sh.m[k.pair]
		if had {
			im.set(k.handle, pre)
			sh.m[k.pair] = im
			continue
		}
		im.del(k.handle)
		if im.len() == 0 {
			delete(sh.m, k.pair)
		} else {
			sh.m[k.pair] = im
		}
	}
	return freed
}

// withdrawAbortedHandlePropsLocked is the per-handle property counterpart.
func (g *Graph[N, W]) withdrawAbortedHandlePropsLocked(sh *edgeHandlePropShard) int {
	if sh.v.d == nil {
		return 0
	}
	freed := 0
	for k := range sh.v.d {
		pre, had, n, withdrew := sh.v.withdrawAborted(k)
		if !withdrew {
			continue
		}
		freed += n
		// The zero instMap is a valid empty one, so an absent pair needs no
		// special case: set materialises it and the write-back installs it.
		im := sh.m[k.pair]
		if had {
			im.set(k.handle, pre)
			sh.m[k.pair] = im
			continue
		}
		im.del(k.handle)
		if im.len() == 0 {
			delete(sh.m, k.pair)
		} else {
			sh.m[k.pair] = im
		}
	}
	return freed
}

// withdrawAbortedInstanceLabelsLocked restores the by-ordinal relationship-type
// store, whose value is nested: pair -> ordinal -> bag.
func (g *Graph[N, W]) withdrawAbortedInstanceLabelsLocked(sh *edgeInstanceLabelShard) int {
	if sh.v.d == nil {
		return 0
	}
	freed := 0
	for k := range sh.v.d {
		pre, had, n, withdrew := sh.v.withdrawAborted(k)
		if !withdrew {
			continue
		}
		freed += n
		// The zero instMap is a valid empty one, so an absent pair needs no
		// special case: set materialises it and the write-back installs it.
		im := sh.m[k.pair]
		if had {
			im.set(k.idx, pre)
			sh.m[k.pair] = im
			continue
		}
		im.del(k.idx)
		if im.len() == 0 {
			delete(sh.m, k.pair)
		} else {
			sh.m[k.pair] = im
		}
	}
	return freed
}

// withdrawAbortedInstancePropsLocked is the by-ordinal property counterpart.
func (g *Graph[N, W]) withdrawAbortedInstancePropsLocked(sh *edgeInstancePropShard) int {
	if sh.v.d == nil {
		return 0
	}
	freed := 0
	for k := range sh.v.d {
		pre, had, n, withdrew := sh.v.withdrawAborted(k)
		if !withdrew {
			continue
		}
		freed += n
		// The zero instMap is a valid empty one, so an absent pair needs no
		// special case: set materialises it and the write-back installs it.
		im := sh.m[k.pair]
		if had {
			im.set(k.idx, pre)
			sh.m[k.pair] = im
			continue
		}
		im.del(k.idx)
		if im.len() == 0 {
			delete(sh.m, k.pair)
		} else {
			sh.m[k.pair] = im
		}
	}
	return freed
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
func (g *Graph[N, W]) reclaimAbortedLife() int {
	if g.nodeLifeActive.Load() == 0 {
		return 0
	}
	var toTombstone, toRevive []graph.NodeID
	var released []LabelID
	freed := 0
	for i := range g.nodeLifeShards {
		sh := &g.nodeLifeShards[i]
		sh.mu.Lock()
		for id, st := range sh.born {
			if st.at() != mvcc.AbortedTS {
				continue
			}
			delete(sh.born, id)
			released = append(released, sh.takeChurnHeld(true, id)...)
			freed++
			if d, ok := sh.died[id]; ok && d.at() == mvcc.AbortedTS {
				// BOTH events belong to the aborted transaction, so the state
				// to restore is the one before the transaction's EARLIEST
				// event, which the pair's write order encodes (see
				// [aliveBefore]). Deciding each direction independently
				// tombstoned the node here and then REVIVED it in the loop
				// below, so an aborted create+delete left a bare phantom node
				// visible to every reader (rmp #2443, found by the DST
				// multi-session mode). died-then-born — an applied delete the
				// rollback's undo replay revived before the abort was
				// processed — means the node was ALIVE when the transaction
				// first touched it: restore alive, never re-tombstone the
				// node the undo just repaired (rmp #2445, same finder).
				delete(sh.died, id)
				released = append(released, sh.takeChurnHeld(false, id)...)
				freed++
				if aliveBefore(st, d) {
					toRevive = append(toRevive, id)
				} else {
					toTombstone = append(toTombstone, id)
				}
				continue
			}
			// Created by a transaction that never committed, so as far as
			// every reader is concerned the node never existed.
			toTombstone = append(toTombstone, id)
		}
		for id, st := range sh.died {
			if st.at() == mvcc.AbortedTS {
				delete(sh.died, id)
				released = append(released, sh.takeChurnHeld(false, id)...)
				freed++
				// Removed by a transaction that never committed, so it is alive.
				toRevive = append(toRevive, id)
			}
		}
		if len(sh.born) == 0 {
			sh.born = nil
		}
		if len(sh.died) == 0 {
			sh.died = nil
		}
		sh.mu.Unlock()
	}
	if freed > 0 {
		g.nodeLifeActive.Add(-int64(freed))
	}
	for _, id := range toTombstone {
		g.tombstoneAborted(id)
	}
	for _, id := range toRevive {
		g.reviveAborted(id)
	}
	// THE CHURN HOLDS GO LAST, after both flips (rmp #2686). The records are
	// already gone, so the holds are over-counting from the moment the loops
	// above deleted them — but the flips are the instant at which a reader's
	// answer actually changes, and dropping the holds before them would let a
	// reader take the fast path across exactly that transition.
	g.labelChurn.releaseAll(released)
	return freed
}

// tombstoneAborted marks id dead in the bitmap without recording a death
// instant, which is what withdrawing an aborted CREATE means: there is no
// transaction whose removal it would be.
func (g *Graph[N, W]) tombstoneAborted(id graph.NodeID) {
	if !g.IsTombstoned(id) {
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
	if g.IsTombstoned(id) {
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
	g.BumpTopoGeneration()
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
		for id, e := range sh.d {
			a := adjEffective(e.appendInfo, e.appendTS)
			x := adjEffective(e.exclusiveInfo, e.exclusiveTS)
			if a == mvcc.AbortedTS && x == mvcc.AbortedTS {
				delete(sh.d, id)
				freed++
				continue
			}
			// One side aborted and the other live: clear only the aborted side,
			// so the live one keeps refusing what it must.
			if a == mvcc.AbortedTS {
				e.appendInfo, e.appendTS = nil, 0
			}
			if x == mvcc.AbortedTS {
				e.exclusiveInfo, e.exclusiveTS = nil, 0
			}
		}
		if len(sh.d) == 0 {
			sh.d = nil
		}
		sh.mu.Unlock()
	}
	return freed
}
