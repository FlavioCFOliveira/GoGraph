package lpg

// mvcc_edge_side.go — MVCC P3c (rmp #2291): the read and write halves of the
// three per-edge side stores' version chains.
//
// The generic chain lives in mvcc_sidemap.go; this file is the wiring: where a
// pre-image is recorded, and how a reader resolves one. Each store keeps its
// OWN lock-free counter so a read that touches one of them is not pushed off
// its fast path by churn in another — the same separation the node-label and
// node-property pairs already have, and for the same reason.

import "github.com/FlavioCFOliveira/GoGraph/graph/mvcc"

// ── overflow relationship types ──────────────────────────────────────────────

// addOverflowVersioned records the pre-image and then adds lid to k's overflow
// list, reporting whether the list changed. The caller must hold the shard's
// write lock.
//
// The presence check comes FIRST so a re-assertion records no version. That is
// not a micro-optimisation: MERGE's match branch re-asserts a relationship type
// on every idempotent match, so without the guard one statement would leave one
// record per match instead of none — and, since only a write that RECORDS a
// version can conflict, it is also what keeps an idempotent re-assertion from
// aborting a transaction that changed nothing.
func (g *Graph[N, W]) addOverflowVersioned(sh *edgeLabelShard, k edgeKey, lid LabelID, tx *writeCtx) bool {
	if sh.hasOverflow(k, lid) {
		return false
	}
	if !g.pushOverflowVersion(sh, k, tx) {
		return false
	}
	return sh.addOverflow(k, lid)
}

// removeOverflowVersioned records the pre-image and then detaches lid from k's
// overflow list, reporting whether lid was present. The caller must hold the
// shard's write lock.
func (g *Graph[N, W]) removeOverflowVersioned(sh *edgeLabelShard, k edgeKey, lid LabelID, tx *writeCtx) bool {
	if !sh.hasOverflow(k, lid) {
		return false
	}
	if !g.pushOverflowVersion(sh, k, tx) {
		return false
	}
	return sh.removeOverflow(k, lid)
}

// clearOverflowVersioned records the pre-image and then drops every overflow
// label on k, returning how many were dropped. The caller must hold the shard's
// write lock.
func (g *Graph[N, W]) clearOverflowVersioned(sh *edgeLabelShard, k edgeKey, tx *writeCtx) int {
	if len(sh.overflow[k]) == 0 {
		return 0
	}
	if !g.pushOverflowVersion(sh, k, tx) {
		return 0
	}
	return sh.clearOverflow(k)
}

// pushOverflowVersion records the overflow list of k before a change, and
// reports whether the change may proceed. The caller must hold the shard's
// write lock.
//
// It returns FALSE when tx may not displace the newest version — a write-write
// conflict (rmp #2300) — and in that case records nothing and mutates nothing.
// The caller must abandon its mutation too: the transaction is doomed, and
// applying the change to the stored value while recording no pre-image would
// leave the store holding a write that no reader can ever undo.
//
// The pre-image is COPIED, because the stored slice is appended to in place by
// [edgeLabelShard.addOverflow] and truncated in place by removeOverflow, so
// retaining the header alone would hand a reader a window onto a mutated array.
// The copy is one or two elements: overflow exists only for a pair that already
// carries a second relationship type.
func (g *Graph[N, W]) pushOverflowVersion(sh *edgeLabelShard, k edgeKey, tx *writeCtx) bool {
	if !g.mvccArmed {
		return true
	}
	if head := sh.v.headStamp(k); tx.conflicts(head) {
		_ = tx.conflictErr(mvcc.StoreEdgeTypes, head)
		return false
	}
	cur, had := sh.overflow[k]
	var pre []LabelID
	if had && len(cur) > 0 {
		pre = make([]LabelID, len(cur))
		copy(pre, cur)
	}
	info, ts := g.deltaStamp(tx.record())
	sh.v.push(k, pre, had, info, ts, &g.edgeLabelVersionActive)
	return true
}

// overflowLabelsAsOf returns the overflow relationship types of k as they were
// at s, or the current list when s is nil.
//
// The returned slice MUST NOT be mutated: on the fast path it aliases the
// stored list, exactly as the direct map read it replaces did. The caller holds
// the shard read lock, which is what makes that alias safe for the duration of
// the read.
func (g *Graph[N, W]) overflowLabelsAsOf(sh *edgeLabelShard, k edgeKey, s *Snapshot) []LabelID {
	cur := sh.overflow[k]
	if s == nil || sh.v.empty() {
		return cur
	}
	out, had := sh.v.asOf(k, cur, cur != nil, s.startTS, s.txID)
	if !had {
		return nil
	}
	return out
}

// ── per-handle relationship types ────────────────────────────────────────────

// pushHandleLabelVersion records the label bag of one edge instance before a
// change. The caller must hold the shard's lock.
//
// labelBag is stored BY VALUE and its small tier aliases a backing array the
// mutator may grow in place, so the pre-image is deep-copied by
// [cloneLabelBag] — the same reason the node-label reader copies before
// applying an undo.
// It returns FALSE when tx may not displace the newest version; see
// [Graph.pushOverflowVersion] for why the caller must then abandon its mutation.
func (g *Graph[N, W]) pushHandleLabelVersion(sh *edgeHandleLabelShard, k edgeKey, handle uint64, tx *writeCtx) bool {
	if !g.mvccArmed {
		return true
	}
	key := edgeHandleKey{pair: k, handle: handle}
	if head := sh.v.headStamp(key); tx.conflicts(head) {
		_ = tx.conflictErr(mvcc.StoreEdgeTypesHandle, head)
		return false
	}
	var pre labelBag
	had := false
	if im, ok := sh.m[k]; ok {
		var bag labelBag
		bag, had = im.get(handle)
		if had {
			pre = cloneLabelBag(bag)
		}
	}
	info, ts := g.deltaStamp(tx.record())
	sh.v.push(key, pre, had, info, ts, &g.edgeHandleLabelVersionActive)
	return true
}

// handleLabelBagAsOf returns the label bag of one edge instance as it was at s,
// or the current bag when s is nil. had reports whether a record existed at
// all, which the caller must distinguish from an empty bag: a slot with NO
// record is column-typed and its type comes from the adjacency instead.
func (g *Graph[N, W]) handleLabelBagAsOf(sh *edgeHandleLabelShard, k edgeKey, handle uint64, s *Snapshot) (labelBag, bool) {
	// Shaped, not merely written, and for the reason mvcc_labels.go records for
	// the node-label reader: the gate is checked BEFORE anything else and the
	// walk lives in a separate function, so the fast path stays small enough to
	// inline. A first version that folded the walk in here cost 6.4 ns on
	// BenchmarkEdgeSideRead_LabelsByHandle — a fifth of the whole read, paid by
	// workloads that never write. Indexing the nil OUTER map is deliberate: Go
	// returns the zero instMap, whose get reports "no record".
	im := sh.m[k]
	bag, had := im.get(handle)
	if s == nil || sh.v.empty() {
		return bag, had
	}
	return sh.v.asOf(edgeHandleKey{pair: k, handle: handle}, bag, had, s.startTS, s.txID)
}

// ── per-handle properties ────────────────────────────────────────────────────

// pushHandlePropVersion records the property bag of one edge instance before a
// change. The caller must hold the shard's lock.
// It returns FALSE when tx may not displace the newest version; see
// [Graph.pushOverflowVersion].
func (g *Graph[N, W]) pushHandlePropVersion(sh *edgeHandlePropShard, k edgeKey, handle uint64, tx *writeCtx) bool {
	if !g.mvccArmed {
		return true
	}
	key := edgeHandleKey{pair: k, handle: handle}
	if head := sh.v.headStamp(key); tx.conflicts(head) {
		_ = tx.conflictErr(mvcc.StoreEdgePropsHandle, head)
		return false
	}
	var pre propBag
	had := false
	if im, ok := sh.m[k]; ok {
		var bag propBag
		bag, had = im.get(handle)
		if had {
			pre = clonePropBag(bag)
		}
	}
	info, ts := g.deltaStamp(tx.record())
	sh.v.push(key, pre, had, info, ts, &g.edgeHandlePropVersionActive)
	return true
}

// ── per-instance relationship types and properties (keyed by ordinal) ────────

// pushInstanceLabelVersion records the label bag of the (pair, ordinal)
// instance before a change. The caller must hold the shard's write lock.
// It returns FALSE when tx may not displace the newest version; see
// [Graph.pushOverflowVersion].
func (g *Graph[N, W]) pushInstanceLabelVersion(sh *edgeInstanceLabelShard, k edgeKey, idx int64, tx *writeCtx) bool {
	if !g.mvccArmed {
		return true
	}
	key := edgeInstanceKey{pair: k, idx: idx}
	if head := sh.v.headStamp(key); tx.conflicts(head) {
		_ = tx.conflictErr(mvcc.StoreEdgeTypesOrd, head)
		return false
	}
	var pre labelBag
	had := false
	if im, ok := sh.m[k]; ok {
		var bag labelBag
		bag, had = im.get(idx)
		if had {
			pre = cloneLabelBag(bag)
		}
	}
	info, ts := g.deltaStamp(tx.record())
	sh.v.push(key, pre, had, info, ts, &g.edgeInstanceLabelVersionActive)
	return true
}

// pushInstancePropVersion records the property bag of the (pair, ordinal)
// instance before a change. The caller must hold the shard's write lock.
// It returns FALSE when tx may not displace the newest version; see
// [Graph.pushOverflowVersion].
func (g *Graph[N, W]) pushInstancePropVersion(sh *edgeInstancePropShard, k edgeKey, idx int64, tx *writeCtx) bool {
	if !g.mvccArmed {
		return true
	}
	key := edgeInstanceKey{pair: k, idx: idx}
	if head := sh.v.headStamp(key); tx.conflicts(head) {
		_ = tx.conflictErr(mvcc.StoreEdgePropsOrd, head)
		return false
	}
	var pre propBag
	had := false
	if im, ok := sh.m[k]; ok {
		var bag propBag
		bag, had = im.get(idx)
		if had {
			pre = clonePropBag(bag)
		}
	}
	info, ts := g.deltaStamp(tx.record())
	sh.v.push(key, pre, had, info, ts, &g.edgeInstancePropVersionActive)
	return true
}

// ── whole-pair drops ─────────────────────────────────────────────────────────
//
// clearEdgePairState drops a whole pair's per-instance metadata in one map
// delete once the last edge between the endpoints is gone. A reader from before
// that must still see all of it, so every instance the pair held needs its own
// pre-image recorded. The walks are over the pair's own [instMap], so their
// cost is the number of parallel edges the pair had, and they run only on the
// path that is already deleting them.
//
// Each reports whether every instance could be recorded. A conflict on ANY
// instance refuses the whole pair drop: the pair is one unit as far as the
// caller is concerned, and dropping part of it would leave the store in a state
// no serial schedule produces. The loop stops at the first refusal rather than
// recording pre-images the transaction can never publish.

func (g *Graph[N, W]) pushHandleLabelVersionsForPair(sh *edgeHandleLabelShard, k edgeKey, tx *writeCtx) bool {
	if !g.mvccArmed {
		return true
	}
	im := sh.m[k]
	ok := true
	im.forEachKey(func(handle uint64) bool {
		ok = g.pushHandleLabelVersion(sh, k, handle, tx)
		return ok
	})
	return ok
}

func (g *Graph[N, W]) pushHandlePropVersionsForPair(sh *edgeHandlePropShard, k edgeKey, tx *writeCtx) bool {
	if !g.mvccArmed {
		return true
	}
	im := sh.m[k]
	ok := true
	im.forEachKey(func(handle uint64) bool {
		ok = g.pushHandlePropVersion(sh, k, handle, tx)
		return ok
	})
	return ok
}

func (g *Graph[N, W]) pushInstanceLabelVersionsForPair(sh *edgeInstanceLabelShard, k edgeKey, tx *writeCtx) bool {
	if !g.mvccArmed {
		return true
	}
	im := sh.m[k]
	ok := true
	im.forEachKey(func(idx int64) bool {
		ok = g.pushInstanceLabelVersion(sh, k, idx, tx)
		return ok
	})
	return ok
}

func (g *Graph[N, W]) pushInstancePropVersionsForPair(sh *edgeInstancePropShard, k edgeKey, tx *writeCtx) bool {
	if !g.mvccArmed {
		return true
	}
	im := sh.m[k]
	ok := true
	im.forEachKey(func(idx int64) bool {
		ok = g.pushInstancePropVersion(sh, k, idx, tx)
		return ok
	})
	return ok
}

// ── reclamation ──────────────────────────────────────────────────────────────

// reclaimEdgeSideVersions truncates every per-edge side chain at the watermark
// and returns how many records it released.
//
// Called from [Graph.ReclaimVersions] under the same contract: safe against
// concurrent readers, not against a concurrent writer, which the barrier
// excludes.
func (g *Graph[N, W]) reclaimEdgeSideVersions(watermark uint64) int {
	// ONE histogram for all five per-edge side stores, reset here — at the aggregate
	// — because each of the five sub-sweeps below skips itself when its store holds
	// nothing live, and a reset inside them would clear the other four's samples.
	// See [depthStore] for why the five are not distinguished.
	hist := g.depth(depthEdgeSides)
	hist.Reset()
	freed := 0
	if g.edgeLabelVersionActive.Load() != 0 {
		for i := range g.edgeLabelShards {
			sh := &g.edgeLabelShards[i]
			sh.mu.Lock()
			// ABORTED heads first (rmp #2318): they can never satisfy reclaim's
			// watermark test, and they are the only thing masking the aborted
			// transaction's writes from the stored value.
			n := g.withdrawAbortedEdgeLabelsLocked(sh)
			n += sh.v.reclaim(watermark, hist)
			sh.mu.Unlock()
			freed += n
		}
		g.edgeLabelVersionActive.Add(-int64(freed))
	}
	if n := g.reclaimHandleLabelVersions(watermark, hist); n > 0 {
		freed += n
	}
	if n := g.reclaimHandlePropVersions(watermark, hist); n > 0 {
		freed += n
	}
	if n := g.reclaimInstanceLabelVersions(watermark, hist); n > 0 {
		freed += n
	}
	if n := g.reclaimInstancePropVersions(watermark, hist); n > 0 {
		freed += n
	}
	return freed
}

func (g *Graph[N, W]) reclaimInstanceLabelVersions(watermark uint64, hist *mvcc.DepthHist) int {
	if g.edgeInstanceLabelVersionActive.Load() == 0 {
		return 0
	}
	freed := 0
	for i := range g.edgeInstanceLabelShards {
		sh := &g.edgeInstanceLabelShards[i]
		sh.mu.Lock()
		// ABORTED heads first; see rmp #2318 and mvcc_abort_sides.go.
		freed += g.withdrawAbortedInstanceLabelsLocked(sh)
		freed += sh.v.reclaim(watermark, hist)
		sh.mu.Unlock()
	}
	g.edgeInstanceLabelVersionActive.Add(-int64(freed))
	return freed
}

func (g *Graph[N, W]) reclaimInstancePropVersions(watermark uint64, hist *mvcc.DepthHist) int {
	if g.edgeInstancePropVersionActive.Load() == 0 {
		return 0
	}
	freed := 0
	for i := range g.edgeInstancePropShards {
		sh := &g.edgeInstancePropShards[i]
		sh.mu.Lock()
		// ABORTED heads first; see rmp #2318 and mvcc_abort_sides.go.
		freed += g.withdrawAbortedInstancePropsLocked(sh)
		freed += sh.v.reclaim(watermark, hist)
		sh.mu.Unlock()
	}
	g.edgeInstancePropVersionActive.Add(-int64(freed))
	return freed
}

func (g *Graph[N, W]) reclaimHandleLabelVersions(watermark uint64, hist *mvcc.DepthHist) int {
	if g.edgeHandleLabelVersionActive.Load() == 0 {
		return 0
	}
	freed := 0
	for i := range g.edgeHandleLabelShards {
		sh := &g.edgeHandleLabelShards[i]
		sh.mu.Lock()
		// ABORTED heads first; see rmp #2318 and mvcc_abort_sides.go.
		freed += g.withdrawAbortedHandleLabelsLocked(sh)
		freed += sh.v.reclaim(watermark, hist)
		sh.mu.Unlock()
	}
	g.edgeHandleLabelVersionActive.Add(-int64(freed))
	return freed
}

func (g *Graph[N, W]) reclaimHandlePropVersions(watermark uint64, hist *mvcc.DepthHist) int {
	if g.edgeHandlePropVersionActive.Load() == 0 {
		return 0
	}
	freed := 0
	for i := range g.edgeHandlePropShards {
		sh := &g.edgeHandlePropShards[i]
		sh.mu.Lock()
		// ABORTED heads first; see rmp #2318 and mvcc_abort_sides.go.
		freed += g.withdrawAbortedHandlePropsLocked(sh)
		freed += sh.v.reclaim(watermark, hist)
		sh.mu.Unlock()
	}
	g.edgeHandlePropVersionActive.Add(-int64(freed))
	return freed
}

// EdgeSideVersionCount returns the number of live per-edge side-store version
// records: overflow relationship types plus per-handle types and properties.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgeSideVersionCount() int64 {
	return g.edgeLabelVersionActive.Load() +
		g.edgeHandleLabelVersionActive.Load() +
		g.edgeHandlePropVersionActive.Load() +
		g.edgeInstanceLabelVersionActive.Load() +
		g.edgeInstancePropVersionActive.Load()
}
