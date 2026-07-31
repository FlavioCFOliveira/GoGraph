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
	e := loadEntry(s, intraIdx)
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

// SetWriteStamp declares how the version records of subsequent writes are
// stamped: info is the transaction's shared commit record, or nil with ts a
// commit timestamp for an autocommit write.
//
// It is a field rather than a parameter because storeEntry has a dozen callers
// and the write path is single-writer by the layer above's contract — the same
// reason commitDepth is a plain field. Set it before the write and clear it
// after; a nil info with a zero ts means "not versioned", which is what an
// unarmed AdjList always has.
//
// Not safe for concurrent use.
func (a *AdjList[N, W]) SetWriteStamp(info *mvcc.CommitInfo, ts uint64) {
	a.writeInfo, a.writeTS = info, ts
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
// Safe for concurrent use with readers. Not safe to run concurrently with
// itself or with writers on the same AdjList.
func (a *AdjList[N, W]) Reclaim(watermark uint64) int {
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
			n, stillVersioned := severChain(e, watermark)
			freed += n
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
// many records it released and whether any remain.
//
// The chain runs newest-first, so the FIRST record whose supersede timestamp is
// at or before the watermark makes everything behind it unreachable as well:
// every active reader began at or after that instant, so none of them steps
// back past it. Storing nil there releases the whole tail at once.
func severChain[W any](e *adjEntry[W], watermark uint64) (freed int, remaining bool) {
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
		cur = v.prev
	}
	return freed, e.ver.Load() != nil
}
