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
	// ts is the commit timestamp of an autocommit write; read only when info is
	// nil.
	ts uint64
}

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
func (a *AdjList[N, W]) linkVersion(next, prev *adjEntry[W], info *mvcc.CommitInfo, ts uint64) {
	if next == nil {
		return
	}
	if info != nil && prev != nil {
		if pv := prev.ver.Load(); pv != nil && pv.info == info {
			// Same transaction, already recorded for this node: keep prev's
			// chain and drop the intermediate entry, which no reader can need.
			next.ver.Store(pv)
			return
		}
	}
	next.ver.Store(&adjVersion[W]{prev: prev, info: info, ts: ts})
	a.versionActive.Add(1)
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
// node properties all take one commit record and become visible together. It is
// a field rather than a parameter because storeEntry has a dozen callers and
// every one of them would otherwise have to thread it.
//
// # What a concurrent unstamped writer inherits, and why that is sound
//
// The public Go-API mutators are documented as per-operation atomic, not
// transactional, so one may run on another goroutine while a transaction holds
// the barrier and has the stamp armed. That write then joins the transaction's
// visibility group: it becomes visible when the transaction publishes rather
// than the instant it is made. That is not a regression — under the barrier
// such a write is ALREADY invisible to every barrier reader until the barrier
// is released, which is the same instant. It cannot be lost, because a
// transaction's record is published on rollback as well as on commit (the
// in-memory undo log restores the stored value physically, so the chain nets
// out; see lpg's endWrite).
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
// # The three cases, and why the middle one may not fall back to the slot
//
// A write that carries a transaction takes that transaction's shared record, so
// every version of one transaction points at one record and publishing it is one
// atomic store — whatever else is writing concurrently (rmp #2320).
//
// A write that carries a transaction whose window has already been RETRACTED
// takes a fresh untransacted timestamp instead of consulting the ambient slot.
// Consulting it would be the defect this threading removes, arrived at from the
// far side: the slot may name a live concurrent transaction, and adopting that
// record would publish this version at that transaction's commit instant. A
// timestamp later than the write actually happened is the safe direction; see
// the [mvcc.WriteStamp] file comment.
//
// A write that carries NO transaction resolves through the ambient slot, which
// is the correct reading for adjlist's remaining untransacted callers — the
// exclusive bulk builds, WAL replay, snapshot apply and the direct Go API.
func (a *AdjList[N, W]) versionStamp(tx mvcc.Tx) (*mvcc.CommitInfo, uint64) {
	if a.stamp == nil {
		return nil, 0
	}
	if tx.Valid() {
		if info := tx.Record(); info != nil {
			return info, 0
		}
		return a.stamp.UntransactedStamp()
	}
	return a.stamp.Stamp()
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
