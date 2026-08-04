package lpg

// mvcc_index.go — the candidate-set discipline (rmp #2290, MVCC P4c).
//
// # Superset good, missing entry fatal
//
// The versioned stores answer what an object CONTAINS. The label bitmap index
// answers which objects a scan should CONSIDER, and it is not versioned. The
// two failure directions are not symmetrical:
//
//   - a label ADDED after the reader started leaves an EXTRA member in the
//     bitmap. Harmless, provided the scan re-checks the versioned label bag —
//     which [Graph.LabelBitmapAsOf] makes it do;
//   - a label REMOVED after the reader started takes a member OUT. Nothing can
//     recover it, and the reader silently loses a row.
//
// So removals are DEFERRED: recorded when they happen, applied to the bitmap
// only once the reclamation watermark has passed them, at which point no reader
// can still want the entry. PostgreSQL defers exactly this to VACUUM and
// Memgraph to its index GC; neither removes an index entry at the instant the
// value changes, and this is the reason.
//
// # What the deferral costs
//
// Between the removal and the sweep the bitmap over-reports. Two consequences,
// both handled here rather than left to the caller:
//
//   - a SCAN must filter, which [Graph.LabelBitmapAsOf] does, at O(members)
//     once per scan rather than per row;
//   - a COUNT cannot be served from the bitmap at all, because there is no
//     object to re-check a number against. [Graph.LabelCountExact] reports when
//     the O(1) answer is still exact — which is every read-only workload — and
//     the caller falls back to counting the filtered scan otherwise.

import (
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// idxEntry identifies one label-index entry.
type idxEntry struct {
	id  graph.NodeID
	lid uint32
}

// deferredIdx holds the removals not yet applied.
//
// KEYED, not a list, and that is load-bearing: a removal must be CANCELLABLE.
// A rolled-back statement strips a node's labels and then puts them back, and
// if the strip's deferred removal survived, the sweep would later delete an
// entry the undo had legitimately restored — the node would vanish from every
// label scan. TestRunInTx_DeleteReturn caught exactly that.
//
// Lazily allocated, so a graph that has never removed a label holds nothing.
type deferredIdx struct {
	pending map[idxEntry]lifeStamp
	mu      sync.Mutex
}

// deferLabelIndexRemoval records that id should leave lid's bitmap, without
// doing it yet.
//
// Returns false when versioning is disarmed, in which case the caller removes
// the entry immediately as it always did.
//
// # Whose instant the removal is stamped with (rmp #2303, MVCC B1)
//
// tx is the transaction performing the removal, and the stamp is taken FROM IT
// rather than from the graph's ambient [mvcc.WriteStamp] slot. That distinction is
// the whole of this structure's ordering basis.
//
// The ambient slot names whichever transaction currently holds the write
// visibility barrier. While the barrier admits one writer that is always the right
// answer, which is why reading it was correct until now. With concurrent writers
// it is the WRONG answer in both directions: writer A's deferred removal could be
// stamped with writer B's record, so it becomes reclaimable when B commits — before
// A's own readers are done with the entry, which silently loses a row — or, if B is
// still in flight, it carries an id no record will ever publish and the removal is
// never swept at all, so the bitmap over-reports for the life of the process.
//
// This is the same defect class as audit finding E3, which rmp #2301 closed for the
// commit record and the version count by moving them onto [writeCtx]; the deferred
// index removal was the one remaining reader of the ambient slot on this path.
//
// A nil tx — a direct Go-API mutation outside any transaction — falls back to the
// ambient stamp, which is correct for it: such a write is committed the instant it
// is made, so there is no transaction whose instant could differ.
func (g *Graph[N, W]) deferLabelIndexRemoval(lid uint32, id graph.NodeID, tx *writeCtx) bool {
	if !g.mvccArmed {
		return false
	}
	info, ts := g.deferralStamp(tx)
	k := idxEntry{id: id, lid: lid}
	g.idxDeferred.mu.Lock()
	if g.idxDeferred.pending == nil {
		g.idxDeferred.pending = make(map[idxEntry]lifeStamp, 8)
	}
	_, existed := g.idxDeferred.pending[k]
	g.idxDeferred.pending[k] = lifeStamp{info: info, ts: ts}
	g.idxDeferred.mu.Unlock()
	if !existed {
		g.idxPendingActive.Add(1)
	}
	return true
}

// cancelDeferredIndexRemoval withdraws a pending removal because the entry has
// been put back.
//
// It is what makes a ROLLBACK safe: the undo log re-attaches the labels a
// failed statement stripped, and without this the strip's deferred removal
// would still fire at the next sweep and delete an entry that is legitimately
// present again.
func (g *Graph[N, W]) cancelDeferredIndexRemoval(lid uint32, id graph.NodeID) {
	if g.idxPendingActive.Load() == 0 {
		return
	}
	k := idxEntry{id: id, lid: lid}
	g.idxDeferred.mu.Lock()
	_, existed := g.idxDeferred.pending[k]
	if existed {
		delete(g.idxDeferred.pending, k)
		if len(g.idxDeferred.pending) == 0 {
			g.idxDeferred.pending = nil
		}
	}
	g.idxDeferred.mu.Unlock()
	if existed {
		g.idxPendingActive.Add(-1)
	}
}

// applyDeferredIndexRemovals removes the entries the watermark has passed, and
// returns how many it applied.
//
// Called from the reclamation sweep ([Graph.sweepUnit]) and from
// [Graph.ReclaimNow].
//
// # Why the bitmap removal happens UNDER the lock (rmp #2308)
//
// This is the one reclaimer that did not exclude a concurrent writer, and the
// consequence was the exact failure the candidate-set discipline says nothing can
// recover from — a node silently absent from a label scan, forever:
//
//	T1 removes label L from n, commits at 10; the removal is deferred, stamped 10
//	the watermark reaches 10
//	the sweep collects {L,n} as ready, deletes it from pending, RELEASES the lock
//	T2 adds L back to n:  nodeIdx.Add(L,n) succeeds, then its cancel finds
//	                      nothing pending to withdraw — the sweep already took it
//	the sweep calls nodeIdx.Remove(L,n)
//	n now carries L and is NOT in L's bitmap. Every later MATCH (n:L) misses it.
//
// It could not surface while the sweep ran under the visibility barrier, which
// excluded T2 by construction. The background vacuum has no barrier, so the
// exclusion has to be real: the bitmap removals now happen while
// `idxDeferred.mu` is still held, and [Graph.setNodeLabelInfo] cancels BEFORE it
// adds. That makes the two paths mutually exclusive in both interleavings —
// a writer that cancels first removes the key so the sweep never collects it, and
// a writer that arrives second blocks on the lock and re-adds the entry after the
// sweep has removed it. Either way the bitmap ends up a SUPERSET of the truth,
// which is the direction the discipline tolerates.
//
// Holding the lock across the removals costs nothing that matters: the work is
// bounded by the ready set, the only other holders of this lock are the deferral
// and cancellation paths, and neither is on a scan.
//
// Pinned by TestDeferredIndexRemoval_ConcurrentReaddIsNotLost.
func (g *Graph[N, W]) applyDeferredIndexRemovals(watermark uint64) int {
	if g.idxPendingActive.Load() == 0 {
		return 0
	}
	g.idxDeferred.mu.Lock()
	ready := make([]idxEntry, 0, len(g.idxDeferred.pending))
	for k, st := range g.idxDeferred.pending {
		if st.at() <= watermark {
			ready = append(ready, k)
		}
	}
	for _, k := range ready {
		delete(g.idxDeferred.pending, k)
	}
	if len(g.idxDeferred.pending) == 0 {
		g.idxDeferred.pending = nil
	}
	// Under the lock. See the comment above for the lost row this closes; the
	// lock order is idxDeferred.mu then the index's own lock, which is the order
	// the label-add path takes them in too.
	for _, k := range ready {
		g.nodeIdx.Remove(k.lid, k.id)
	}
	g.idxDeferred.mu.Unlock()

	if n := len(ready); n > 0 {
		g.idxPendingActive.Add(-int64(n))
		return n
	}
	return 0
}

// IndexRemovalBacklog returns how many label-index removals are waiting for the
// watermark.
//
// It is exported because a deferred removal is memory the substrate is
// responsible for, and the bounded-resources mandate asks that such things be
// observable rather than merely bounded.
//
// Safe for concurrent use.
func (g *Graph[N, W]) IndexRemovalBacklog() int64 { return g.idxPendingActive.Load() }

// LabelBitmapAsOf returns the members of lid's bitmap that carried the label at
// s, as a bitmap the caller owns.
//
// A nil snapshot, or a graph with nothing deferred and no live label or node
// history, returns the index's own answer untouched — which is what every
// read-only workload gets, and it costs one bitmap clone exactly as it did
// before.
//
// Otherwise every member is re-checked against the versioned label bag and the
// versioned existence record. That is O(members) once per SCAN, not per row.
//
// Safe for concurrent use.
func (g *Graph[N, W]) LabelBitmapAsOf(lid LabelID, s *Snapshot) *roaring64.Bitmap {
	bm := g.nodeIdx.Intersect(uint32(lid))
	if !g.labelBitmapNeedsFilter(s) {
		return bm
	}
	g.correctBitmap(bm, s, func(bag labelBag) bool { return bag.has(lid) })
	return bm
}

// correctBitmap adjusts bm in place so it describes s rather than the present.
//
// # Why it visits the SUSPECTS and not the bitmap
//
// The obvious implementation re-checks every member. It is also unusable: the
// fairness harness reads a 20 000-node label through an index seek, and
// re-checking all 20 000 cost 180 µs on a 4.7 µs query — a 39× collapse that
// appeared the moment ANY version was live anywhere in the graph, and stayed
// after the writer stopped, with THREE live versions.
//
// The set that can possibly differ is not the bitmap; it is the nodes a writer
// has touched recently enough for a reader to disagree about — and that set is
// already materialised, as the keys of the sparse side maps the version chains
// hang off. So this walks those, which is bounded by the churn the reclaimer
// has not yet caught up with, and adjusts the handful of members that are
// actually wrong. Everything else in the bitmap is correct by construction,
// because a node with no live history looks the same at every instant.
//
// bm is mutated in place: [label.Index.Intersect] returns a bitmap the caller
// owns, so there is nothing to clone.
func (g *Graph[N, W]) correctBitmap(bm *roaring64.Bitmap, s *Snapshot, want func(labelBag) bool) {
	// Every shard lock is RELEASED before the first check runs; see
	// [Graph.suspectNodes].
	for _, id := range g.suspectNodes() {
		present := bm.Contains(uint64(id))
		// NodeExistsAsOf takes the LIFE shard lock and labelBagTest takes the
		// LABEL shard lock, in that order and never nested — the two are
		// different locks, and the bag is consumed inside its own.
		should := g.NodeExistsAsOf(id, s) && g.labelBagTest(id, s, want)
		switch {
		case present && !should:
			bm.Remove(uint64(id))
		case !present && should:
			// Reachable only if an index entry was dropped despite the
			// deferral. Adding it back is the safe direction: a missing member
			// is a silently lost row.
			bm.Add(uint64(id))
		}
	}
}

// suspectNodes returns every node a reader might disagree with the present
// about: one with a live label version, a live existence record, or a deferred
// index removal.
//
// A node may appear more than once; the caller's work is idempotent.
//
// # It COLLECTS and returns rather than taking a callback, and that is a
// deadlock fix, not a style choice
//
// The callback form held a shard's read lock across the visit, and the visit
// resolves the node's label bag — which read-locks THE SAME SHARD. Go's
// sync.RWMutex is not re-entrant: a writer arriving between the outer RLock and
// the inner one is queued ahead of the inner acquire, and the inner acquire
// waits for a writer that waits for the outer reader. Every reader in the
// fairness soak's eight-reader cell wedged there, at zero CPU, for eighteen
// minutes.
//
// The slice is small by construction — it is exactly the churn the reclaimer
// has not yet caught up with — and it is allocated only when there IS churn,
// because all three gates are checked first.
func (g *Graph[N, W]) suspectNodes() []graph.NodeID {
	var out []graph.NodeID
	if g.labelDeltaActive.Load() != 0 {
		for i := range g.nodeLabelShards {
			sh := &g.nodeLabelShards[i]
			sh.mu.RLock()
			for id := range sh.d {
				out = append(out, id)
			}
			sh.mu.RUnlock()
		}
	}
	if g.nodeLifeActive.Load() != 0 {
		for i := range g.nodeLifeShards {
			sh := &g.nodeLifeShards[i]
			sh.mu.RLock()
			for id := range sh.born {
				out = append(out, id)
			}
			for id := range sh.died {
				out = append(out, id)
			}
			sh.mu.RUnlock()
		}
	}
	if g.idxPendingActive.Load() != 0 {
		g.idxDeferred.mu.Lock()
		for k := range g.idxDeferred.pending {
			out = append(out, k.id)
		}
		g.idxDeferred.mu.Unlock()
	}
	return out
}

// LabelsBitmapAsOf is [Graph.LabelBitmapAsOf] for a conjunction of labels.
//
// Safe for concurrent use.
func (g *Graph[N, W]) LabelsBitmapAsOf(lids []LabelID, s *Snapshot) *roaring64.Bitmap {
	raw := make([]uint32, len(lids))
	for i, l := range lids {
		raw[i] = uint32(l)
	}
	bm := g.nodeIdx.Intersect(raw...)
	if !g.labelBitmapNeedsFilter(s) {
		return bm
	}
	g.correctBitmap(bm, s, func(bag labelBag) bool {
		for _, l := range lids {
			if !bag.has(l) {
				return false
			}
		}
		return true
	})
	return bm
}

// labelBitmapNeedsFilter reports whether the raw bitmap could disagree with
// what s should see.
//
// It cannot when there is no snapshot, when nothing has been deferred, and when
// no label or existence history is live — the three together mean the index and
// the versioned stores describe the same instant.
func (g *Graph[N, W]) labelBitmapNeedsFilter(s *Snapshot) bool {
	// A deferred removal makes the raw bitmap over-report for EVERY reader,
	// snapshot or not: the entry is still there and the label is already gone.
	// So this branch is deliberately outside the nil check — filtering only for
	// snapshot readers would hand a current-value reader a node whose label was
	// removed, which is a wrong answer rather than a stale one.
	if g.idxPendingActive.Load() != 0 {
		return true
	}
	if s == nil {
		return false
	}
	return g.labelDeltaActive.Load() != 0 || g.nodeLifeActive.Load() != 0
}

// LabelCountExact returns the number of nodes carrying lid and whether that
// number is EXACT for s.
//
// It is not exact whenever [Graph.LabelBitmapAsOf] would have to filter, for
// the reason given in the file comment: a count has no object to be re-checked
// against, so the only sound answer is to decline and let the caller count the
// filtered scan.
//
// Safe for concurrent use.
func (g *Graph[N, W]) LabelCountExact(lid LabelID, s *Snapshot) (int64, bool) {
	if g.labelBitmapNeedsFilter(s) {
		return 0, false
	}
	return int64(g.nodeIdx.Count(uint32(lid))), true
}

// deferralStamp resolves the instant a deferred index removal is charged to.
//
// A transaction's own record and id, when there is one; the graph's ambient stamp
// otherwise. See [Graph.deferLabelIndexRemoval] for why the difference matters
// once writers overlap.
func (g *Graph[N, W]) deferralStamp(tx *writeCtx) (*commitInfo, uint64) {
	if tx != nil {
		return tx.record(), tx.txID
	}
	return g.stamp.Stamp()
}
