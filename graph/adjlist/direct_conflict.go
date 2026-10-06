package adjlist

// direct_conflict.go — which adjacency writes may publish over an uncommitted
// entry (rmp #2947).
//
// # The defect
//
// Every adjacency write builds a new immutable entry from the CURRENT one and
// publishes it. When the current entry was published by a transaction that has
// not committed, the new entry EMBEDS that transaction's uncommitted change: a
// reader sees an arc, a slot label or an edge property no committed transaction
// wrote, and when that transaction rolls back its undo is computed against an
// entry that already carries the second write, which it then loses. The
// conflict unit is therefore the whole entry — every edge out of one node — not
// the one slot the write changes.
//
// # The rule
//
// The test runs under the entry's shard lock, before the write changes
// anything, and refuses with a [*mvcc.Conflict] for [mvcc.StoreAdjacency]
// EVERY write — explicit, implicit, bounded, or one that carries no transaction
// — over an entry published by a transaction other than its own that is still
// in flight. The refusal is structural: it does not depend on whether the
// writing path took the layer above's adjacency claims, so a path that forgets
// one cannot stack an entry on another transaction's uncommitted one.
//
// A write with no transaction has the id zero, which no version head carries,
// so every in-flight head refuses it (rmp #2967): it does not adopt the
// exclusive bracket that holds the stamp's slot, so that bracket's uncommitted
// entries refuse it like any other transaction's. See [AdjList.SetWriteStamp].
//
// Until the ACID audit of rmp #2965 an EXPLICIT transaction's write was refused
// only by an implicit transaction's entry, on the premise that the layer
// above's claims arbitrate explicit transactions against each other. One path
// took no claim — the durable store's edge apply,
// [github.com/FlavioCFOliveira/GoGraph/graph/lpg.Graph.AddEdgeHIfAbsent] inside
// a store commit — so its entry embedded another explicit transaction's
// uncommitted arc, and that transaction's abort then restored the stacked entry
// with the arc still in it; on an undirected graph it left a half edge. The
// claims remain, because they also order writes against versions committed
// after a writer's snapshot, which this test does not see; this test is what
// makes "no entry is built on an uncommitted one" hold whatever the claims do.
//
// An ABORTED entry is not refused. A transaction's abort restores the entries it
// published to their pre-images BEFORE it marks its record aborted
// ([AdjList.WithdrawTx], rmp #2965), so no aborted entry is left as the stored
// value for a write to build on; the aborted record is severed only when a later
// write supersedes the entry and is reclaimed. Refusing it would make the node
// unwritable by direct writes until some other write happened to land.
//
// # Several entries, one decision
//
// An undirected edge lives in two entries. A write that changes both takes both
// shard locks in ascending shard order — a shard shared by both is locked once
// — tests both entries, and only then writes either, so a refusal on the second
// entry leaves the first untouched. A bulk removal does the same over its whole
// locked set (see [AdjList.RemoveAllEdgesFrom]). Every path that holds more than
// one adjacency shard lock takes them in ascending shard order, which is what
// makes the order deadlock-free; the reverse index stays a leaf below all of
// them.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// directConflictLocked returns the conflict a write carrying tx would hit on the
// entry at intraIdx of shard s, or nil, by the rule in this file's comment. It
// is nil whenever versioning is off. The caller holds s.mu.
func (a *AdjList[N, W]) directConflictLocked(tx mvcc.Tx, s *adjShard[W], intraIdx uint64) error {
	if !a.versioning || a.stamp == nil {
		return nil
	}
	cur := loadEntry[W](s, intraIdx)
	if cur == nil {
		return nil
	}
	v := cur.ver.Load()
	if v == nil {
		return nil
	}
	head := v.supersededAt()
	if head < mvcc.TxIDBase || head == mvcc.AbortedTS {
		return nil
	}
	// A write with no transaction has the id zero, which no version head
	// carries, so every in-flight head refuses it (rmp #2967).
	own := tx.ID()
	if own != 0 && head == own {
		return nil
	}
	return mvcc.NewConflict(mvcc.StoreAdjacency, head, 0, own)
}

// directConflictLockedID is [AdjList.directConflictLocked] addressed by node id.
// The caller holds the lock of id's shard.
func (a *AdjList[N, W]) directConflictLockedID(tx mvcc.Tx, id graph.NodeID) error {
	return a.directConflictLocked(tx, &a.shards[id&shardMask], uint64(id)>>shardBits)
}

// lockPair locks the shards of x and y in ascending shard order — once when
// they share a shard — and returns them for [AdjList.unlockPair]. It is the
// two-entry case every undirected edge write takes, without a slice or a
// returned closure: either would allocate on the hot path.
func (a *AdjList[N, W]) lockPair(x, y graph.NodeID) (lo, hi uint64) {
	lo, hi = uint64(x)&shardMask, uint64(y)&shardMask
	if lo > hi {
		lo, hi = hi, lo
	}
	a.shards[lo].mu.Lock()
	if hi != lo {
		a.shards[hi].mu.Lock()
	}
	return lo, hi
}

// unlockPair releases the shards [AdjList.lockPair] locked.
func (a *AdjList[N, W]) unlockPair(lo, hi uint64) {
	if hi != lo {
		a.shards[hi].mu.Unlock()
	}
	a.shards[lo].mu.Unlock()
}
