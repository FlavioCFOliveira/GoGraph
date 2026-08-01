package lpg

// mvcc_props.go — MVCC P2 (rmp #2279): delta chains for node PROPERTIES.
//
// The same design as mvcc_labels.go, and deliberately the same shape, but this
// is the first structure whose undo record carries a VALUE rather than an
// identifier. That is why P2 re-measures the cost model instead of assuming it
// carries over from P0: a label delta is 32 bytes because a LabelID is a
// uint32, and a property delta cannot be.
//
// # What an undo record has to say about a property
//
// Three transitions are possible and they need different undo information:
//
//	absent  -> value   undo is "delete the key"
//	value   -> value'  undo is "restore the old value"
//	value   -> absent  undo is "restore the old value"
//
// So one action flag plus the previous value covers all three: [undoDelProp]
// when the key was absent before, [undoSetProp] with the pre-image otherwise.
// Memgraph's SET_PROPERTY delta carries exactly this — a PropertyId and the old
// value — for the same reason.
//
// # Why the value is stored inline rather than behind a pointer
//
// [PropertyValue] is a small struct, and putting it behind a pointer would swap
// a larger delta for a second allocation per modification. The whole cost model
// this programme was authorised on is "one allocation, constant size, per
// modification", so the inline form is the one that preserves it. The size is
// pinned by test for the same reason the label delta's is.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// Property-delta actions. As with labels, a delta records the UNDO of the
// change that created it.
const (
	// undoDelProp undoes the creation of a key that was previously absent.
	undoDelProp uint8 = iota
	// undoSetProp undoes an overwrite or a deletion by restoring the pre-image.
	undoSetProp
)

// nodePropDelta is one undo record in a node's property-version chain.
type nodePropDelta struct {
	// next is the older version's delta, or nil at the end of the chain.
	next *nodePropDelta
	// info is the commit record shared with every other delta the same
	// transaction wrote, or nil for an autocommit write — in which case ts
	// carries the commit timestamp. See mvcc_txn.go.
	info *commitInfo
	// prev is the value to restore, meaningful only for [undoSetProp].
	prev PropertyValue
	// ts is the commit timestamp of an autocommit write; read only when info
	// is nil.
	ts uint64
	// key identifies the property this record restores or deletes.
	key PropertyKeyID
	// action is [undoSetProp] or [undoDelProp].
	action uint8
}

// mustUndo applies the same visibility rule as [nodeLabelDelta.mustUndo]; see
// there for the three cases and mvcc_txn.go for the timestamp encoding.
func (d *nodePropDelta) mustUndo(startTS, txID uint64) bool {
	ts := d.ts
	if d.info != nil {
		ts = d.info.TS()
	}
	return !mvcc.Visible(ts, startTS, txID)
}

// EnablePropDeltas arms the node-property half of the MVCC substrate.
//
// Separate from [Graph.EnableLabelDeltas] on purpose: the phases land one
// structure at a time, and each must be measurable on its own before the next
// is wired. Must be called before any property is written and never
// concurrently with another operation on g.
//
// Not safe for concurrent use.
func (g *Graph[N, W]) EnablePropDeltas() { g.propDeltas = true }

// propDeltasEnabled reports whether the property half is armed.
func (g *Graph[N, W]) propDeltasEnabled() bool { return g.propDeltas }

// PropDeltaCount returns the number of live node-property delta records.
//
// The lock-free gate a property reader consults, and the memory bound the
// design owes a garbage-collection phase.
//
// Safe for concurrent use.
func (g *Graph[N, W]) PropDeltaCount() int64 { return g.propDeltaActive.Load() }

// pushPropDelta links an undo record at the head of id's property chain. The
// caller must hold the shard's write lock.
//
// The counters are kept separate from the label ones so a property write cannot
// push a label reader off its fast path, and vice versa: each structure's gate
// answers only for itself.
func (s *nodePropShard) pushPropDelta(id graph.NodeID, action uint8, key PropertyKeyID, prev PropertyValue, info *commitInfo, ts uint64, active *atomic.Int64) {
	if s.d == nil {
		s.d = make(map[graph.NodeID]*nodePropDelta, 8)
	}
	s.d[id] = &nodePropDelta{
		next:   s.d[id],
		info:   info,
		prev:   prev,
		ts:     ts,
		key:    key,
		action: action,
	}
	active.Add(1)
}

// propBagAsOf reconstructs the property set of id as it was at startTS.
//
// # The gate is checked UNDER the lock, and that is a correctness fix
//
// It used to be a graph-level atomic counter read BEFORE the lock, which is
// fast and wrong. Between that read and the actual read a writer can create the
// first delta and apply its change, so the reader takes the RAW CURRENT value
// for this node — and the pre-write value for the next one, whose gate it
// samples after. The result is a state that never existed. Example 27's
// bank-transfer invariant reported it as "readers observed a torn total 40
// time(s)", and it only became frequent once reclamation started returning the
// counter to zero between transactions (rmp #2290).
//
// The shard's own side map is the gate now: nil means this shard holds no
// history at all, it is a plain field read in the same cache line as the map
// the read has already touched, and it is sampled ATOMICALLY WITH THE VALUE.
// A window that cannot be observed cannot tear.
func (g *Graph[N, W]) propBagAsOf(id graph.NodeID, startTS, txID uint64) propBag {
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	cur := s.m[id]
	if s.d == nil {
		s.mu.RUnlock()
		return cur
	}
	out := g.propBagAsOfLocked(s, id, startTS, txID)
	s.mu.RUnlock()
	return out
}

// propBagAsOfLocked is [Graph.propBagAsOfSlow] with the shard read lock already
// held. See [Graph.labelBagAsOfLocked] for why the distinction matters.
func (g *Graph[N, W]) propBagAsOfLocked(s *nodePropShard, id graph.NodeID, startTS, txID uint64) propBag {
	cur := s.m[id]
	if s.d == nil {
		return cur
	}
	d := s.d[id]
	if d == nil {
		return cur
	}
	var out propBag
	copied := false
	for ; d != nil; d = d.next {
		if !d.mustUndo(startTS, txID) {
			break
		}
		if !copied {
			out = clonePropBag(cur)
			copied = true
		}
		switch d.action {
		case undoSetProp:
			out.set(d.key, d.prev)
		case undoDelProp:
			out.del(d.key)
		}
	}
	if !copied {
		return cur
	}
	return out
}

// clonePropBag returns a copy that can be mutated without disturbing the stored
// version. Called only when a chain walk has an undo to apply.
func clonePropBag(b propBag) propBag {
	out := b
	switch {
	case b.m != nil:
		out.m = make(map[PropertyKeyID]PropertyValue, len(b.m))
		for k, v := range b.m {
			out.m[k] = v
		}
	case len(b.pairs) > 0:
		out.pairs = make([]kv, len(b.pairs))
		copy(out.pairs, b.pairs)
	}
	return out
}

// propBagPlain is the non-MVCC read, with the same call and return shape as
// [Graph.propBagAsOf] so the two can be compared fairly. See
// [Graph.labelBagPlain] for why a fair control matters here.
func (g *Graph[N, W]) propBagPlain(id graph.NodeID) propBag {
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	cur := s.m[id]
	s.mu.RUnlock()
	return cur
}

// propValuesDefinitelyEqual reports whether a and b are certainly the same
// value. It is deliberately CONSERVATIVE: false means "not known to be equal",
// not "different".
//
// It exists because the obvious spelling is a runtime panic. [PropertyValue]
// holds its payload in an `any`, and two of its kinds — PropBytes ([]byte) and
// PropList ([]PropertyValue) — are slices, which are UNCOMPARABLE. Writing
// `prev != value` to decide whether a write changed anything therefore panics
// with "comparing uncomparable type" the first time a byte or list property is
// overwritten. That is a crash on an ordinary write path, reachable from any
// SET of a list-valued property.
//
// The conservative direction is the safe one. A false negative records a delta
// for a write that changed nothing: wasteful, and reclaimed by garbage
// collection. A false positive would SKIP a delta for a write that did change
// something, leaving a reader unable to reconstruct the older version — a wrong
// answer. So only the scalar kinds, whose payloads are comparable by
// construction, are compared at all.
func propValuesDefinitelyEqual(a, b PropertyValue) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case PropString, PropInt64, PropFloat64, PropBool, PropTime:
		return a.v == b.v
	default:
		// PropBytes and PropList carry slices; anything unrecognised is treated
		// the same way. Assume changed.
		return false
	}
}
