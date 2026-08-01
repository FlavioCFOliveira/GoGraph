package lpg

// mvcc_sidemap.go — MVCC P3c (rmp #2291): version chains for the per-edge side
// stores.
//
// # The three stores, and why one mechanism serves all of them
//
// Three structures a read touches were left unversioned by P1 to P3b, and all
// three have the same shape — a sparse map behind a shard lock, holding a small
// value per edge:
//
//   - [edgeLabelShard].overflow — a pair's SECOND and later relationship types,
//     and any type orphaned by RemoveEdgeLabel on an absent edge;
//   - [edgeHandleLabelShard].m — the per-CREATE relationship type of one
//     parallel edge instance, keyed by its stable handle;
//   - [edgeHandlePropShard].m — that instance's properties.
//
// Their own documentation already admitted the gap: EdgeLabelsByHandle says it
// is "only per-operation atomic" and "NOT cross-store consistent … outside a
// transaction barrier". That barrier is precisely what P4c removes.
//
// # Pre-images, not undo actions — and why the two differ
//
// The node-label and node-property chains record an ACTION (add this label
// back, restore this value) because their value is a whole bag and a delta must
// stay small: a bag copy per modification would be the dominant cost on the
// hottest write path the module has.
//
// These stores are the opposite case. Their values are already small — a one or
// two element []LabelID, a single-instance labelBag, a single-instance propBag —
// and they change rarely, only when a relationship type or an edge property is
// written. So the record holds the PRE-IMAGE of the whole value, which is
// simpler and impossible to get subtly wrong: there is no action enum, no apply
// order, and no way for an undo to be inverted.
//
// The walk is correspondingly trivial, and worth stating because it is not the
// obvious one. Reading newest-first, each record's pre-image is the value BEFORE
// that record's change. A reader keeps overwriting its answer with the
// pre-image of every record it must undo and stops at the first it must not:
//
//	val := current
//	for d := head; d != nil && d.mustUndo(...); d = d.next {
//	    val = d.pre
//	}
//
// which lands on the value as of the last change the reader can see.
//
// PostgreSQL's heap works the same way at a coarser grain: an UPDATE writes a
// whole new tuple version rather than a description of what changed, and a
// reader walks back to the version its snapshot admits.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// edgeHandleKey identifies one parallel edge instance: the endpoint pair plus
// the stable handle allocated when it was created.
//
// The per-handle chains are keyed by it rather than by the pair, so a write to
// one instance copies THAT instance's small value rather than the whole
// pair's inner map. On a pair with many parallel edges the difference is O(1)
// against O(instances) per write.
type edgeHandleKey struct {
	pair   edgeKey
	handle uint64
}

// edgeInstanceKey identifies one parallel edge instance by its ORDINAL within
// the pair, which is how the by-index stores address it.
//
// It is a second key type rather than a reuse of [edgeHandleKey] because the
// two stores address the same instance by different identities — a handle
// survives sibling deletion, an ordinal does not — and conflating them would
// let a version chain answer for the wrong instance after a compaction.
type edgeInstanceKey struct {
	pair edgeKey
	idx  int64
}

// preimageDelta is one record in a side store's version chain: the value the
// key held before the change that created the record.
type preimageDelta[V any] struct {
	// next is the older record, or nil when this is the oldest retained.
	next *preimageDelta[V]
	// info is the commit record SHARED with every other change the same
	// transaction made, in this store and in the others, so the transaction
	// publishes with one atomic store. Nil for a write made outside any
	// transaction, in which case ts carries its commit timestamp directly —
	// the same union the node-label and node-property deltas use, for the same
	// reason.
	info *commitInfo
	// ts is the commit timestamp of an untransacted write; read only when info
	// is nil.
	ts uint64
	// pre is the value before the change. Meaningful only when had is true.
	pre V
	// had records whether the key existed at all before the change, so a
	// reader from before a key's FIRST write sees it absent rather than
	// seeing a zero value.
	had bool
}

// mustUndo reports whether a reader at (startTS, txID) must step back over this
// record — the same three cases, in the same order, as [nodeLabelDelta.mustUndo].
func (d *preimageDelta[V]) mustUndo(startTS, txID uint64) bool {
	ts := d.ts
	if d.info != nil {
		ts = d.info.TS()
	}
	return !mvcc.Visible(ts, startTS, txID)
}

// sideVersions is the sparse chain index for one shard of a side store.
//
// The map is allocated on the first write, so a shard nothing has written keeps
// no side map and a read-only workload pays only the lock-free counter check
// its owner makes before consulting this at all.
//
// The caller must hold the owning shard's lock; sideVersions adds no
// synchronisation of its own, exactly as the node-label side map does not.
type sideVersions[K comparable, V any] struct {
	d map[K]*preimageDelta[V]
}

// empty reports whether this shard holds no version at all, which is the gate a
// read consults before considering a walk.
//
// It is a PLAIN pointer comparison on a field in the same cache line as the
// store's own map, and that placement is the whole point. The first version
// gated on a graph-level atomic counter, exactly as the node-label and
// adjacency readers do — and it cost 9.2 ns per read, a third of the whole
// by-handle label read, because nothing else in the read touches the tail of
// the Graph struct so every gate check was a cache miss. Measured by removing
// the load: 40.2 ns with it, 31.0 ns without, against a 30.5 ns baseline.
//
// The graph-level counters still exist, for [Graph.EdgeSideVersionCount] and
// for the reclaimer to skip a store nothing has written. They are read on the
// WRITE path and by observability, never per read.
//
// The caller must hold the owning shard's lock, which every reader here does.
func (sv *sideVersions[K, V]) empty() bool { return sv.d == nil }

// push records that k is about to change, retaining the value it held.
//
// active is the store's lock-free counter, kept exact so a reader can skip the
// whole mechanism with one atomic load and a reclaimer can tell there is
// nothing to do.
func (sv *sideVersions[K, V]) push(k K, pre V, had bool, info *commitInfo, ts uint64, active *atomic.Int64) {
	if sv.d == nil {
		sv.d = make(map[K]*preimageDelta[V], 8)
	}
	head := sv.d[k]
	if info != nil && head != nil && head.info == info {
		// The same transaction already recorded this key, and that record holds
		// the value from BEFORE the transaction — which is the only value any
		// other reader can want. A second record would only lengthen the chain,
		// and a statement that writes one edge's type repeatedly (MERGE
		// re-asserting on every match) would otherwise leave one record per
		// write instead of one per transaction. The adjacency's linkVersion
		// makes the same elision for the same reason.
		return
	}
	sv.d[k] = &preimageDelta[V]{next: head, info: info, ts: ts, pre: pre, had: had}
	active.Add(1)
}

// asOf returns the value k held at startTS for a reader running as txID, given
// its current value.
//
// curHad distinguishes "absent" from "present and zero", which matters for a
// store whose zero value is a legitimate empty set.
func (sv *sideVersions[K, V]) asOf(k K, cur V, curHad bool, startTS, txID uint64) (V, bool) {
	if sv.d == nil {
		return cur, curHad
	}
	d := sv.d[k]
	if d == nil {
		return cur, curHad
	}
	val, had := cur, curHad
	for ; d != nil; d = d.next {
		if !d.mustUndo(startTS, txID) {
			break
		}
		val, had = d.pre, d.had
	}
	return val, had
}

// reclaim truncates every chain in this shard at the watermark and returns how
// many records it released.
//
// A record stamped at or before the watermark is one no active reader must undo
// — every reader began at or after that instant — so it and everything behind
// it are unreachable, and one assignment releases the whole tail. That is the
// identical argument the node-label reclaimer makes; see mvcc_reclaim.go.
func (sv *sideVersions[K, V]) reclaim(watermark uint64) int {
	if sv.d == nil {
		return 0
	}
	freed := 0
	for k, head := range sv.d {
		if head == nil {
			delete(sv.d, k)
			continue
		}
		if head.stamp() <= watermark {
			// Even the NEWEST record is unreachable, so the whole chain goes
			// and the key leaves the index.
			freed += chainLen(head)
			delete(sv.d, k)
			continue
		}
		prev := head
		for d := head.next; d != nil; d = d.next {
			if d.stamp() <= watermark {
				freed += chainLen(d)
				prev.next = nil
				break
			}
			prev = d
		}
	}
	if len(sv.d) == 0 {
		sv.d = nil
	}
	return freed
}

// stamp returns the commit timestamp of the change this record undoes.
func (d *preimageDelta[V]) stamp() uint64 {
	if d.info != nil {
		return d.info.TS()
	}
	return d.ts
}

// chainLen counts d and every record behind it.
func chainLen[V any](d *preimageDelta[V]) int {
	n := 0
	for ; d != nil; d = d.next {
		n++
	}
	return n
}
