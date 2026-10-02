package adjlist

// mvcc_adj.go — MVCC P3 (rmp #2281): version chains for the adjacency, so a
// reader can reconstruct which edges existed at its own start timestamp.
//
// # Why this is much cheaper here than in the reference implementation
//
// Memgraph mutates a vertex's edge lists IN PLACE and records an undo record
// per modification (ADD_OUT_EDGE, REMOVE_IN_EDGE, …) so a reader can rebuild the
// older list. GoGraph does not need to: [adjEntry] is already "an immutable
// snapshot of a node's outgoing adjacency", replaced rather than mutated on
// every topology change and published with a single atomic store. **The older
// version already exists as a by-product of every write.** All that was missing
// was a way to reach it.
//
// Measured before building anything: an edge append costs 143 ns and 3
// allocations at 10 000 nodes against 163 ns and 3 allocations at 1 000 000 —
// flat, because the shard's slot array is cloned only on GROW. So retaining the
// prior entry adds one small record per topology change and copies nothing.
//
// # Why the version pointer lives INSIDE the entry
//
// The first design put version heads in a side array published beside the entry
// array. It is racy in BOTH orderings, and the race is a wrong answer rather
// than a crash:
//
//   - publish version then entry: a reader that loads the OLD entry may still
//     load the NEW head and undo a change the entry never had;
//   - publish entry then version: a reader that loads the NEW entry may still
//     load the OLD head and MISS an undo it needed.
//
// Two independent atomics cannot be read consistently without a seqlock or a
// lock, and a lock on this path would undo the lock-free read contract the
// adjacency exists to provide.
//
// Putting the pointer in the entry removes the problem by construction: the
// entry is immutable and published with ONE atomic store, so a reader that sees
// an entry sees exactly the version chain that belonged to it. Everything after
// that first load is immutable pointer chasing with no synchronisation at all.
// This is also what Memgraph does — the delta pointer is a field of the Vertex,
// not a side table — and the reason is the same.
//
// # The chain IS the entry history
//
// A version record holds the entry it replaced, and that entry holds its own
// version record. So walking back through versions walks back through entries,
// and no edge-level undo record is ever built.

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// adjVersion records that one entry replaced another, and when.
//
// 24 bytes: two pointers and a timestamp. One per topology change on a node,
// and nothing is copied — prev is the entry the write already produced.
type adjVersion[W any] struct {
	// prev is the entry this version superseded. It carries its own ver field,
	// so the chain of versions is the chain of entries.
	prev *adjEntry[W]
	// info is the commit record shared with every other change the same
	// transaction made, in this store and in the others. Nil for an autocommit
	// write, in which case ts carries the commit timestamp directly — the same
	// union lpg's deltas use, for the same reason: an autocommit write is
	// already committed when it is made and needs no shared mutable record.
	info *mvcc.CommitInfo
	// ts is the commit timestamp of an autocommit write when info is nil. When
	// info is set, no reader consults it, and it carries [adjOverPreImage] or
	// zero instead: whether this version is its transaction's FIRST write over a
	// committed pre-image, the only kind [AdjList.WithdrawTx] restores
	// (rmp #2965). Keeping the mark here costs no memory.
	ts uint64
}

// adjOverPreImage marks, in a transactional version's ts, that prev was a
// committed entry — or no entry — when the version was linked, so prev is the
// transaction's exact pre-image. A version over another transaction's
// uncommitted entry does not carry it, and its abort leaves the entry alone.
const adjOverPreImage = 1

// supersededAt returns the timestamp at which prev was replaced.
func (v *adjVersion[W]) supersededAt() uint64 {
	if v.info != nil {
		return v.info.TS()
	}
	return v.ts
}

// EnableVersioning arms adjacency versioning.
//
// Off by default and armed by nothing in the module: the phase lands the
// mechanism and its measurements, not a behaviour change. Must be called before
// any edge is written and never concurrently with another operation.
//
// Not safe for concurrent use.
func (a *AdjList[N, W]) EnableVersioning() { a.versioning = true }

// DisableVersioning disarms adjacency versioning, so writes record no versions
// and reads take the current entry with no walk.
//
// It exists so both arms can be compared in ONE process rather than across two
// builds. Must be called before any edge is written and never concurrently with
// another operation.
//
// Not safe for concurrent use.
func (a *AdjList[N, W]) DisableVersioning() { a.versioning = false }

// VersionCount returns the number of live adjacency version records.
//
// The lock-free gate a reader consults before considering a walk, and the
// memory a reclamation phase owes: nothing reclaims these yet, so under
// sustained topology churn this grows without bound.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) VersionCount() int64 { return a.versionActive.Load() }

// linkVersion attaches to next a record of the entry it replaces, so a reader
// that must not see the replacement can step back to prev.
//
// Called from storeEntry with s.mu held, BEFORE the entry is published — which
// is safe precisely because next is not yet reachable by any reader.
//
// prev MAY BE NIL, and that case is load-bearing rather than defensive: it is
// the node's FIRST entry, and without a record for it the creation itself is
// unversioned, so a reader from before the node had any edge would see the edge
// anyway. A version with a nil prev means "nothing was here", and the walk
// stepping onto it yields no neighbours. Memgraph makes the same point in the
// opposite direction — an Edge "must be created with an initial DELETE_OBJECT
// delta" — because in both designs EXISTENCE has to be versioned, not just
// mutation. Two of this file's tests failed on exactly this before it was
// added.
//
// A replacement by the SAME transaction that produced prev is not recorded: the
// transaction sees its own writes anyway, so an extra link would only lengthen
// the chain. That matters because a multi-edge write to one node replaces its
// entry once per edge, and without this a single statement would leave one
// record per edge instead of one per node.
//
// It reports whether it linked a NEW version carrying info, which is when the
// transaction's write set gains this entry. A transactional version over a
// committed pre-image is marked [adjOverPreImage].
func (a *AdjList[N, W]) linkVersion(next, prev *adjEntry[W], info *mvcc.CommitInfo, ts uint64) bool {
	if next == nil {
		return false
	}
	if info != nil && prev != nil {
		if pv := prev.ver.Load(); pv != nil && pv.info == info {
			// Same transaction, already recorded for this node: keep prev's
			// chain and drop the intermediate entry, which no reader can need.
			next.ver.Store(pv)
			return false
		}
	}
	if info != nil {
		ts = 0
		if overPreImage(prev) {
			ts = adjOverPreImage
		}
	}
	next.ver.Store(&adjVersion[W]{prev: prev, info: info, ts: ts})
	a.versionActive.Add(1)
	return info != nil
}

// overPreImage reports whether e is a committed entry, or no entry at all, so
// that a transaction's first write over it supersedes its exact pre-image. An
// aborted head is stepped back over, as [AdjList.WithdrawTx] steps back over
// it; another transaction's in-flight entry is not a pre-image.
func overPreImage[W any](e *adjEntry[W]) bool {
	for e != nil {
		v := e.ver.Load()
		if v == nil {
			return true
		}
		at := v.supersededAt()
		if at != mvcc.AbortedTS {
			return at < mvcc.TxIDBase
		}
		e = v.prev
	}
	return true
}

// entryAsOf returns the adjacency entry of intraIdx as it was at startTS for a
// reader running as txID.
//
// The fast path is one atomic load plus one uncontended atomic gate read, which
// is what a non-versioned read already costs. When the graph holds no live
// version — the whole of a read-only workload — nothing else runs.
func (a *AdjList[N, W]) entryAsOf(s *adjShard[W], intraIdx, startTS, txID uint64) *adjEntry[W] {
	return a.entryAsOfLoaded(loadEntry(s, intraIdx), startTS, txID)
}

// entryAsOfLoaded is [AdjList.entryAsOf] for a caller that has already loaded
// the current entry, so a bulk scan does not load the slot twice.
//
// e must be the CURRENT entry of the slot: the walk starts there and follows the
// version chain backwards, so starting from an older entry would resolve against
// a truncated chain and could return a version this reader must not see.
// entryAsOfLoadedVisible resolves through the caller's pinned verdict, and does
// NOT short-circuit on the global versionActive counter: that counter answers a
// whole-adjacency question and cannot gate a per-entry one (rmp #2378).
func (a *AdjList[N, W]) entryAsOfLoadedVisible(e *adjEntry[W], visible func(*mvcc.CommitInfo, uint64) bool) *adjEntry[W] {
	for e != nil {
		v := e.ver.Load()
		if v == nil {
			break
		}
		if visible(v.info, v.ts) {
			break
		}
		e = v.prev
	}
	return e
}

// EntryViewAsOfVisible is [AdjList.EntryViewAsOf] through a pinned verdict.
func (a *AdjList[N, W]) EntryViewAsOfVisible(id graph.NodeID, visible func(*mvcc.CommitInfo, uint64) bool) EntryView[W] {
	s := &a.shards[id&shardMask]
	return viewOf(a.entryAsOfLoadedVisible(loadEntry(s, uint64(id)>>shardBits), visible))
}

func (a *AdjList[N, W]) entryAsOfLoaded(e *adjEntry[W], startTS, txID uint64) *adjEntry[W] {
	if a.versionActive.Load() == 0 || e == nil {
		return e
	}
	// Immutable pointer chasing from here: no synchronisation, and no way to
	// observe a torn pair, because the chain was fixed when e was published.
	for e != nil {
		v := e.ver.Load()
		if v == nil {
			break
		}
		if mvcc.Visible(v.supersededAt(), startTS, txID) {
			break // the change that produced e is visible: e is this reader's version
		}
		e = v.prev
	}
	return e
}

// EntryNeighboursAsOf returns the out-neighbours of id as they were at startTS
// for a reader running as txID, or nil when the node had none.
//
// The returned slice aliases an immutable entry and MUST NOT be mutated. It is
// the versioned counterpart of the ordinary neighbour read, and exists so the
// layer above can be tested against a reconstructed past before any operator
// depends on it.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) EntryNeighboursAsOf(id graph.NodeID, startTS, txID uint64) []graph.NodeID {
	s := &a.shards[id&shardMask]
	e := a.entryAsOf(s, uint64(id)>>shardBits, startTS, txID)
	if e == nil {
		return nil
	}
	return e.neighbours
}

// SetWriteStamp supplies the [mvcc.WriteStamp] this AdjList's version records
// draw from, or nil to draw from none.
//
// It is SHARED rather than owned: the higher layer passes the same stamp to
// every versioned store it has, so one transaction's topology, node labels and
// node properties all take one commit record and become visible together. A
// transaction reaches the adjacency through [AdjList.Writer], which carries it to
// every version the write creates.
//
// # A write that carries no transaction (rmp #2967)
//
// A write made through the AdjList's own methods, or through a [Writer] built
// from the zero [mvcc.Tx], is its own single-operation transaction. It never
// joins a transaction the stamp's slot names, even while an exclusive bracket
// of the higher layer holds that slot: the module cannot tell the bracket's own
// goroutine from an unrelated one, so a write that joined the bracket was
// acknowledged and then lost when the bracket aborted. Instead the write
//
//   - refuses, with a [*mvcc.Conflict] for [mvcc.StoreAdjacency] and no change,
//     an entry another transaction published and has not committed — the
//     bracket's included ([AdjList.directConflictLocked]). The refusal is
//     retryable: it clears once that transaction commits or aborts;
//   - otherwise commits at once, under a fresh timestamp of its own
//     ([mvcc.WriteStamp.UntransactedStamp]), so a later abort of any
//     transaction leaves it in place.
//
// A write that belongs to a transaction says so by writing through
// [AdjList.Writer] over it.
//
// Must be called before any edge is written and never concurrently with another
// operation.
//
// Not safe for concurrent use.
func (a *AdjList[N, W]) SetWriteStamp(s *mvcc.WriteStamp) { a.stamp = s }

// versionStamp resolves how the version record of the write in progress is
// timestamped, from the transaction the write CARRIES.
//
// Called from storeEntry with the shard lock held and only when versioning is
// armed AND this write actually supersedes something, so its cost is paid per
// topology change and never on a read.
//
// # The three cases
//
// A write that carries a transaction takes that transaction's shared record, so
// every version of one transaction points at one record and publishing it is one
// atomic store — whatever else is writing concurrently (rmp #2320).
//
// A write that carries a transaction whose window has already been RETRACTED
// takes a fresh untransacted timestamp. Adopting the record the stamp's slot
// names instead would publish this version at a concurrent transaction's
// commit instant. A timestamp later than the write actually happened is the
// safe direction; see the [mvcc.WriteStamp] file comment.
//
// A write that carries NO transaction takes a fresh untransacted timestamp too,
// and never the slot's record (rmp #2967): it is its own single-operation
// transaction, committed the instant it is made. See [AdjList.SetWriteStamp].
// The slot is not consulted on this path at all.
func (a *AdjList[N, W]) versionStamp(tx mvcc.Tx) (*mvcc.CommitInfo, uint64) {
	if a.stamp == nil {
		return nil, 0
	}
	if tx.Valid() {
		if info := tx.Record(); info != nil {
			return info, 0
		}
	}
	return a.stamp.UntransactedStamp()
}

// Reclaim frees every adjacency version that no reader can reach any more, and
// returns how many records were released.
//
// watermark is the oldest start timestamp among active readers, from
// [mvcc.Horizon.Oldest]. A version superseded at or before it is unreachable:
// every reader began at or after that instant, so every reader resolves to the
// current entry rather than stepping back through it. A watermark of zero means
// "reclaim nothing", which is what the horizon reports while a reader could not
// be registered.
//
// # Why this severs rather than unlinks record by record
//
// A chain is ordered newest-first, so the FIRST record reachable from the
// current entry whose supersede timestamp is at or before the watermark makes
// every record behind it unreachable too. Storing nil at that point releases
// the whole tail in one atomic store, and the Go collector frees the records
// and the old entries they pinned. There is no need to walk to the end, and no
// window in which a reader sees a chain with a hole in it.
//
// # Why it is safe against a concurrent reader
//
// The store is atomic, so a reader traversing the chain sees either the record
// or nil. Both answers are correct: the watermark says no active reader has a
// start timestamp old enough to need what is behind that point, so a reader
// that sees nil stops at the current entry, which is the version it should get
// anyway.
//
// # Why it IS safe against a concurrent writer (rmp #2308)
//
// This said "not safe to run concurrently with itself or with writers", and the
// second half was pessimistic rather than true. It takes `s.mu.Lock()` on each
// shard, and [AdjList.storeEntry] — the only writer of an entry's version chain,
// through [AdjList.linkVersion] — is called under that same lock from every one
// of its call sites. A version chain never leaves the shard that owns its slot,
// so severing one under the shard lock excludes the only writer that could be
// touching it. The barrier the caller used to hold added nothing here.
//
// Verified by reading the call sites rather than by trusting this comment, which
// is how the correction was found; the sweep that relies on it is
// [lpg.Graph.sweepUnit].
//
// # The depth histogram
//
// hist accumulates the RETAINED depth of every chain this sweep leaves behind — the
// number of version records a reader arriving now may still step through — or is nil
// for a caller that does not measure. The count is the sever walk's own loop
// counter, so measuring it costs a register increment on the sweeper and no second
// traversal (rmp #2312). The caller resets it; this function only fills it, because
// a caller that sweeps several stores decides when a distribution begins.
//
// Safe for concurrent use with readers and with writers. NOT safe to run
// concurrently with itself: two sweeps would walk and sever the same chain.
func (a *AdjList[N, W]) Reclaim(watermark uint64, hist *mvcc.DepthHist) int {
	// The reverse index's ghosts are retired on the same watermark as the forward
	// versions they shadow (rmp #2884), and BEFORE the gate below: a sweep that
	// finds no live version must still retire the ghosts the previous one left.
	// They are not counted in the return, which reports version records only.
	if a.versioning {
		a.rev.sweepGhosts(watermark)
	}
	if watermark == 0 || a.versionActive.Load() == 0 {
		return 0
	}
	freed := 0
	for si := range a.shards {
		s := &a.shards[si]
		s.mu.Lock()
		if len(s.versioned) == 0 {
			s.mu.Unlock()
			continue
		}
		for intraIdx := range s.versioned {
			e := loadEntry(s, intraIdx)
			if e == nil {
				delete(s.versioned, intraIdx)
				continue
			}
			n, retained, stillVersioned := severChain(e, watermark)
			freed += n
			if hist != nil {
				hist.Observe(retained)
			}
			if !stillVersioned {
				delete(s.versioned, intraIdx)
			}
		}
		s.mu.Unlock()
	}
	if freed > 0 {
		a.versionActive.Add(-int64(freed))
	}
	return freed
}

// severChain drops the unreachable tail of e's version chain and reports how
// many records it released, how many it left reachable, and whether any remain.
//
// The chain runs newest-first, so the FIRST record whose supersede timestamp is
// at or before the watermark makes everything behind it unreachable as well:
// every active reader began at or after that instant, so none of them steps
// back past it. Storing nil there releases the whole tail at once.
//
// retained is the length of the prefix the walk did NOT sever: the records a reader
// starting now may still have to step through to reach its own version. It is the
// walk's existing loop, counted (rmp #2312).
func severChain[W any](e *adjEntry[W], watermark uint64) (freed, retained int, remaining bool) {
	for cur := e; cur != nil; {
		v := cur.ver.Load()
		if v == nil {
			break
		}
		if v.supersededAt() <= watermark {
			cur.ver.Store(nil)
			for w := v; w != nil; {
				freed++
				if w.prev == nil {
					break
				}
				w = w.prev.ver.Load()
			}
			break
		}
		retained++
		cur = v.prev
	}
	return freed, retained, e.ver.Load() != nil
}

// WithdrawTx restores every adjacency entry the transaction whose commit
// record is info published to the entry it superseded, and returns how many
// version records it released (rmp #2965). ids is the transaction's adjacency
// write set ([mvcc.TxState.AdjacencyWrites]); only those entries are visited,
// each under its own shard's lock, so the cost is O(the transaction's own
// adjacency writes) and nothing at all when it wrote none. The caller calls it
// while the transaction is still in flight and marks info aborted only after it
// returns.
//
// # Why the adjacency needs a withdrawal of its own
//
// Rollback of an adjacency write used to be physical only: an inverse write made
// by the engine's undo log, or by the write itself when it refuses after
// inserting. A bracket aborted without either — a caller of lpg's ApplyVersioned
// that has no undo log, such as the durable store's in-memory apply — left its
// entries as the stored value. Its version records made a snapshot reader step
// back over them, but a present-time read took the aborted entry as it was, and
// the next write built on it, so an aborted edge removal or append became
// permanent.
//
// # What it restores, and from where
//
// A transaction's writes to one node's entry share ONE version record. When that
// record was linked over a committed entry it is marked [adjOverPreImage], and
// its prev is the exact pre-image; only such a head is restored. No
// transaction can build on another's uncommitted entry, because
// [AdjList.directConflictLocked] refuses every write over one whatever claims
// the writing path took (the ACID audit of rmp #2965 found a path that took
// none), so the mark is the defence in depth, not the guarantee. A pre-image whose own head
// is aborted is stepped back over. The restored entry is a copy whose columns
// are clipped to their length: the aborted entry may have extended the
// pre-image's backing arrays in place, and a lock-free reader may still hold it,
// so the next append must allocate rather than write into that memory. The
// reverse index and the edge count are corrected from the difference between
// the two entries' neighbour multisets. The ghost a withdrawn removal left in
// the reverse index is retired by [AdjList.Reclaim], which retires every
// aborted ghost.
//
// # Why before the record is marked aborted
//
// While the record is in flight its entries are guarded as they were for the
// transaction's whole life: the claims refuse every claiming writer, and
// [AdjList.directConflictLocked] refuses a direct write. Restoring them first
// means no aborted entry is ever the stored value.
//
// Safe for concurrent use with readers, writers and [AdjList.Reclaim]: every
// chain it changes is changed under that chain's shard lock, which Reclaim
// takes too, and Reclaim never severs a record that is still in flight.
//
// For use by graph/lpg and graph/adjlist; not part of the stable API.
func (a *AdjList[N, W]) WithdrawTx(info *mvcc.CommitInfo, ids []uint64) (freed int) {
	if info == nil || len(ids) == 0 || !a.versioning {
		return 0
	}
	var size int64
	for _, id := range ids {
		si := id & shardMask
		intraIdx := id >> shardBits
		s := &a.shards[si]
		s.mu.Lock()
		cur := loadEntry(s, intraIdx)
		var v *adjVersion[W]
		if cur != nil {
			v = cur.ver.Load()
		}
		if v == nil || v.info != info || v.ts != adjOverPreImage {
			s.mu.Unlock()
			continue
		}
		pre, n := v.prev, 1
		for pre != nil {
			pv := pre.ver.Load()
			if pv == nil || pv.supersededAt() != mvcc.AbortedTS {
				break
			}
			pre, n = pv.prev, n+1
		}
		size += a.reindexWithdrawnLocked(graph.NodeID(id), cur, pre)
		restoreSlotLocked(s, intraIdx, clipEntry(pre))
		if pre == nil || pre.ver.Load() == nil {
			delete(s.versioned, intraIdx)
		}
		freed += n
		s.mu.Unlock()
	}
	if freed > 0 {
		a.versionActive.Add(-int64(freed))
	}
	if size != 0 {
		a.size.Add(uint64(size))
	}
	return freed
}

// clipEntry returns a copy of e whose columns have no spare capacity, carrying
// e's version chain, or nil for nil. See [AdjList.WithdrawTx] for why a restored
// entry may not share spare capacity with the entry it replaces.
func clipEntry[W any](e *adjEntry[W]) *adjEntry[W] {
	if e == nil {
		return nil
	}
	c := &adjEntry[W]{
		aux:        e.aux,
		neighbours: e.neighbours[:len(e.neighbours):len(e.neighbours)],
		weights:    e.weights[:len(e.weights):len(e.weights)],
		handles:    e.handles[:len(e.handles):len(e.handles)],
		labels:     e.labels[:len(e.labels):len(e.labels)],
	}
	c.ver.Store(e.ver.Load())
	return c
}

// reindexWithdrawnLocked corrects the reverse index for src's entry going back
// from cur to pre, and returns the change in the edge count. An undirected edge
// lives in both endpoints' entries and is counted once, from the entry of the
// lower node id; a self-loop has one slot and is counted from it. The caller
// holds src's shard lock; the reverse index is a leaf below it.
func (a *AdjList[N, W]) reindexWithdrawnLocked(src graph.NodeID, cur, pre *adjEntry[W]) int64 {
	var delta map[graph.NodeID]int
	note := func(e *adjEntry[W], d int) {
		if e == nil {
			return
		}
		for _, nb := range e.neighbours {
			if delta == nil {
				delta = make(map[graph.NodeID]int, len(e.neighbours))
			}
			delta[nb] += d
		}
	}
	note(pre, 1)
	note(cur, -1)
	var size int64
	for nb, d := range delta {
		counted := a.cfg.Directed || src <= nb
		for ; d > 0; d-- {
			a.rev.add(nb, src)
			if counted {
				size++
			}
		}
		for ; d < 0; d++ {
			a.rev.remove(nb, src, nil, 0)
			if counted {
				size--
			}
		}
	}
	return size
}

// restoreSlotLocked publishes e as the entry of slot intraIdx of s without
// versioning it: in place when no [Snapshot] has pinned the published slot
// array, and otherwise into a clone, exactly as [AdjList.storeEntry] decides. A
// clone also drops the shard's window builder, so its owner's next write starts
// from the published array instead of mutating one no reader can reach. The
// caller holds s.mu, so no in-place store can be in progress.
func restoreSlotLocked[W any](s *adjShard[W], intraIdx uint64, e *adjEntry[W]) {
	base := s.slotsRef.Load()
	if base.state.CompareAndSwap(slotsWritable, slotsStoring) {
		atomic.StorePointer(&base.slots[intraIdx], unsafe.Pointer(e)) //nolint:gosec // atomic publication of *adjEntry[W] into an unpinned published array
		base.state.Store(slotsWritable)
		return
	}
	next := &shardSlots{slots: make([]unsafe.Pointer, len(base.slots))}
	copy(next.slots, base.slots)
	next.slots[intraIdx] = unsafe.Pointer(e) //nolint:gosec // typed publication of *adjEntry[W] into a fresh clone
	s.building, s.buildingOwner = nil, 0
	s.slotsRef.Store(next)
}

// CheckInvariants verifies the adjacency's derived structures against its
// forward entries and returns a description of every disagreement, or nil: the
// reverse index must be exactly the multiset transpose of the forward entries,
// the edge count must equal the arcs the entries hold (an undirected edge
// counted once), and an undirected graph's entries must be symmetric.
//
// It is a diagnostic for tests and for an operator who suspects corruption. It
// takes every shard's lock in turn, so the answer is meaningful only while no
// writer runs; with writers running it may report a transient disagreement.
func (a *AdjList[N, W]) CheckInvariants() error {
	fwd := map[[2]graph.NodeID]int{}
	var arcs int64
	for si := range a.shards {
		s := &a.shards[si]
		s.mu.Lock()
		if base := s.slotsRef.Load(); base != nil {
			for intra := range base.slots {
				e := loadEntry[W](s, uint64(intra))
				if e == nil {
					continue
				}
				src := graph.NodeID(uint64(intra)<<shardBits | uint64(si))
				for _, nb := range e.neighbours {
					fwd[[2]graph.NodeID{src, nb}]++
					if a.cfg.Directed || src <= nb {
						arcs++
					}
				}
			}
		}
		s.mu.Unlock()
	}
	rev := map[[2]graph.NodeID]int{}
	for si := range a.rev.shards {
		sh := &a.rev.shards[si]
		sh.mu.RLock()
		for intra, list := range sh.srcs {
			dst := graph.NodeID(uint64(intra)<<shardBits | uint64(si))
			for _, src := range list {
				rev[[2]graph.NodeID{src, dst}]++
			}
		}
		sh.mu.RUnlock()
	}
	var errs []string
	for k, n := range fwd {
		if rev[k] != n {
			errs = append(errs, fmt.Sprintf("arc %v: forward %d, reverse %d", k, n, rev[k]))
		}
		if !a.cfg.Directed {
			if m := fwd[[2]graph.NodeID{k[1], k[0]}]; m != n {
				errs = append(errs, fmt.Sprintf("asymmetric %v: %d against %d", k, n, m))
			}
		}
	}
	for k, n := range rev {
		if fwd[k] == 0 {
			errs = append(errs, fmt.Sprintf("arc %v: reverse %d, no forward", k, n))
		}
	}
	if got := a.size.Load(); got != uint64(arcs) {
		errs = append(errs, fmt.Sprintf("edge count %d, entries hold %d", got, arcs))
	}
	if len(errs) == 0 {
		return nil
	}
	slices.Sort(errs)
	if len(errs) > 10 {
		errs = append(errs[:10], fmt.Sprintf("and %d more", len(errs)-10))
	}
	return fmt.Errorf("adjlist: invariants violated: %s", strings.Join(errs, "; "))
}

// committedStartTS is the read position of the present-state readers: every
// committed version is visible and every uncommitted one is not (rmp #2965,
// round 5). It is the start timestamp the graph layer's implicit transactions
// read at.
const committedStartTS = mvcc.TxIDBase - 1

// committedEntry returns the newest COMMITTED version of node id's entry: the
// stored entry when the change that produced it has committed, and otherwise
// the entry it replaced, repeated until a committed one is reached.
//
// # Why the present-state readers step back (finding R5-F3)
//
// [AdjList.HasEdge], [AdjList.Neighbours], the out-degree family and the
// in-neighbour readers used to read the stored entry, which a transaction
// replaces as soon as it writes: a durable commit applied but not yet fsynced
// was visible through them, and withdrawn again if the fsync failed. A version
// written by a transaction that has not published is invisible to every other
// reader; a transaction reads its own writes through its own snapshot
// ([AdjList.EntryViewAsOf] with its id, or a [Writer]).
//
// The fast path — no version on the entry, or a committed one — is the one
// atomic load the stored read already cost, and allocates nothing. A
// transaction that commits while the walk runs is seen wholly or not at all: a
// record first classified in flight is held so for the rest of the walk, even
// if its timestamp flips before the walk reaches its next version.
func (a *AdjList[N, W]) committedEntry(id graph.NodeID) *adjEntry[W] {
	e := loadEntry[W](&a.shards[id&shardMask], uint64(id)>>shardBits)
	if !a.versioning {
		return e
	}
	var held *mvcc.CommitInfo
	for e != nil {
		v := e.ver.Load()
		if v == nil {
			return e
		}
		if v.info == nil || v.info != held {
			if mvcc.Visible(v.supersededAt(), committedStartTS, 0) {
				return e
			}
			held = v.info
		}
		e = v.prev
	}
	return nil
}
