package index

// nodeset.go — the small-set tier shared by the btree and hash property
// indexes and the label inverted index (sprint 206, tasks #1584/#1585).
//
// # Motivation
//
// Every secondary index maps a key (a property value, or a label
// identifier) to the SET of NodeIDs that carry it. The original
// representation stored each such set as a full *roaring64.Bitmap. A
// roaring64 holding a SINGLE NodeID costs ~168 B across ~9 heap
// allocations — about 18x the ~16 B logical minimum. That overhead
// dominates every high-cardinality index, where most keys map to one or
// a handful of nodes: the unique {id} column of a node, an email index,
// the sparse tail of a label index. A dense label (one label covering a
// contiguous band of millions of NodeIDs), by contrast, is already
// optimal as a roaring run-container (~0.01 bit/node) and must stay so.
//
// # Design
//
// [NodeSet] is a three-state tagged union, stored BY VALUE in the index
// maps so a singleton key costs no separate heap object:
//
//	empty      — count 0, no allocation.
//	singleton  — exactly one id, held inline in the `single` field; no
//	             allocation.
//	small      — 2..smallSetMax ids, held in a sorted-ascending []uint64.
//	             One slice allocation, 8 B per element.
//	bitmap     — a promoted *roaring64.Bitmap. Reached when the set grows
//	             past smallSetMax, or when a range is added wholesale
//	             ([NodeSet.AddRange]). Promotion is ONE-WAY: a set that
//	             becomes a bitmap never demotes, which keeps a dense label
//	             permanently on the run-container-optimal roaring path
//	             (graph-theory-expert, #1584/#1585).
//
// The promotion threshold smallSetMax = 8 keeps the backing array within
// one cache line (8x8 = 64 B) while the singleton state captures the
// dominant high-cardinality win; the slice tier wins on resident bytes
// over roaring up to n ≈ 17 (graph-theory-expert, #1584).
//
// # Query invariants
//
// Membership ([NodeSet.Contains]), cardinality ([NodeSet.Cardinality]),
// and the STRICTLY-ASCENDING iteration order every consumer relies on
// ([NodeSet.ToArray], [NodeSet.OrInto]) are identical for all three
// states and all cardinalities. The on-disk serialization of every index
// is representation-INDEPENDENT: it writes the logical sorted NodeID list
// (btree, hash) or a roaring image materialised from that same list
// (label), so a NodeSet produces byte-identical output to the original
// per-key bitmap (storage-engine-auditor, #1584/#1585).
//
// # Concurrency
//
// [NodeSet] is NOT safe for concurrent use on its own. Every index that
// embeds it (btree, hash, label) guards the whole map/tree operation with
// the index's own RWMutex, exactly as it guarded the bitmaps before. A
// NodeSet value is mutated only under that write lock and read only under
// the matching read lock; the promotion swap (slice -> *roaring64.Bitmap)
// is a single field write completed entirely within one locked operation,
// so no reader can observe a half-promoted set.

import "github.com/RoaringBitmap/roaring/v2/roaring64"

// smallSetMax is the largest cardinality held in the inline sorted-slice
// tier before a NodeSet promotes to a *roaring64.Bitmap. Chosen so the
// backing array stays within one 64-byte cache line (8 x uint64) and the
// dominant singleton/handful case never touches roaring's ~168 B fixed
// overhead (graph-theory-expert, #1584). A set built only from individual
// [NodeSet.Add] calls cannot reach a dense contiguous band before crossing
// this threshold, so a dense label is never mis-tiered as a small set.
const smallSetMax = 8

// NodeSet is the per-key node-set representation shared by the btree,
// hash, and label indexes. The zero value is a valid empty set. See the
// package-level nodeset.go documentation for the state machine and the
// query/serialization invariants.
//
// Concurrency: a NodeSet is not safe for concurrent use on its own. It is
// embedded by value in an index (a btree leaf slot, a hash shard map, or
// the label-index map), and every read and mutation is serialised by that
// owning index's lock; a NodeSet is never shared across goroutines outside
// that discipline. Lookup paths that hand a set's contents to a caller copy
// out under the read lock, so the returned data is safe for concurrent use.
//
// The fields form a tagged union resolved by which is non-zero:
//   - bm != nil           -> bitmap state (promoted; never demotes).
//   - bm == nil, ids != nil -> small state (len(ids) in 1..smallSetMax,
//     sorted ascending).
//   - bm == nil, ids == nil, count == 1 -> singleton state (single).
//   - bm == nil, ids == nil, count == 0 -> empty.
type NodeSet struct {
	bm     *roaring64.Bitmap // non-nil iff promoted to bitmap (one-way)
	ids    []uint64          // sorted ascending; len in [1, smallSetMax] in small state
	single uint64            // the lone id in singleton state
	count  uint8             // 0 = empty, 1 = singleton, else len(ids); ignored once bm != nil
}

// Add inserts node into the set, preserving ascending order and
// promoting to a bitmap when the small tier would overflow. Adding a
// node already present is a no-op (set semantics). Returns true when the
// set was previously empty (so a caller maintaining a distinct-key count
// can detect a brand-new entry).
func (s *NodeSet) Add(node uint64) (wasEmpty bool) {
	if s.bm != nil {
		s.bm.Add(node)
		return false
	}
	switch s.count {
	case 0:
		s.single = node
		s.count = 1
		return true
	case 1:
		if node == s.single {
			return false
		}
		// Grow from singleton to a two-element sorted slice.
		lo, hi := s.single, node
		if lo > hi {
			lo, hi = hi, lo
		}
		s.ids = []uint64{lo, hi}
		s.single = 0
		s.count = 2
		return false
	default:
		s.addSmall(node)
		return false
	}
}

// addSmall inserts node into the sorted small slice, promoting to a
// bitmap when the insert would exceed smallSetMax. The caller guarantees
// the set is in the small state (s.ids has 2..smallSetMax elements).
func (s *NodeSet) addSmall(node uint64) {
	i := lowerBoundU64(s.ids, node)
	if i < len(s.ids) && s.ids[i] == node {
		return // already present
	}
	if len(s.ids) >= smallSetMax {
		// Promote: build a bitmap from the existing sorted ids plus node.
		bm := roaring64.New()
		bm.AddMany(s.ids)
		bm.Add(node)
		s.bm = bm
		s.ids = nil
		s.count = 0
		return
	}
	// Insert at i, shifting the (in-cache-line) tail right by one.
	s.ids = append(s.ids, 0)
	copy(s.ids[i+1:], s.ids[i:])
	s.ids[i] = node
	s.count = uint8(len(s.ids))
}

// Remove deletes node from the set. No-op when absent. A NodeSet never
// demotes: removing from a bitmap leaves it a bitmap even if its
// cardinality drops to one (promote-and-never-demote, #1584). Returns
// true when the set became EMPTY as a result (so a caller maintaining a
// distinct-key count can drop the key).
func (s *NodeSet) Remove(node uint64) (nowEmpty bool) {
	if s.bm != nil {
		s.bm.Remove(node)
		return s.bm.IsEmpty()
	}
	switch s.count {
	case 0:
		return false
	case 1:
		if s.single != node {
			return false
		}
		s.single = 0
		s.count = 0
		return true
	default:
		i := lowerBoundU64(s.ids, node)
		if i >= len(s.ids) || s.ids[i] != node {
			return false
		}
		s.ids = append(s.ids[:i], s.ids[i+1:]...)
		if len(s.ids) == 1 {
			// Collapse back to the singleton state so a key that churned
			// down to one node is as cheap as a fresh singleton.
			s.single = s.ids[0]
			s.ids = nil
			s.count = 1
			return false
		}
		s.count = uint8(len(s.ids))
		return false
	}
}

// AddRange adds every id in [from, to] (inclusive) to the set. It always
// promotes to (or stays) a bitmap and uses roaring's run-container
// AddRange, so a contiguous band of NodeIDs is stored in O(1) space. This
// is the bulk-ingest fast path the label index relies on for dense
// labels; it is intentionally the ONLY entry point that can create a
// bitmap without first crossing smallSetMax, and a set that takes an
// AddRange is permanently a bitmap (#1585).
func (s *NodeSet) AddRange(from, to uint64) {
	if s.bm == nil {
		bm := roaring64.New()
		// Fold any existing inline state into the new bitmap so the range
		// add is additive (set union), not a replacement.
		switch s.count {
		case 1:
			bm.Add(s.single)
		default:
			if s.ids != nil {
				bm.AddMany(s.ids)
			}
		}
		s.bm = bm
		s.ids = nil
		s.single = 0
		s.count = 0
	}
	s.bm.AddRange(from, to+1)
}

// RemoveRange removes every id in [from, to] (inclusive). On an inline
// (non-bitmap) set it removes the few covered ids individually; on a
// bitmap it uses roaring's RemoveRange. A NodeSet never demotes, so a
// bitmap stays a bitmap. Returns true when the set became EMPTY.
func (s *NodeSet) RemoveRange(from, to uint64) (nowEmpty bool) {
	if s.bm != nil {
		s.bm.RemoveRange(from, to+1)
		return s.bm.IsEmpty()
	}
	switch s.count {
	case 0:
		return false
	case 1:
		if s.single < from || s.single > to {
			return false
		}
		s.single = 0
		s.count = 0
		return true
	default:
		out := s.ids[:0]
		for _, v := range s.ids {
			if v < from || v > to {
				out = append(out, v)
			}
		}
		s.ids = out
		switch len(s.ids) {
		case 0:
			s.ids = nil
			s.count = 0
			return true
		case 1:
			s.single = s.ids[0]
			s.ids = nil
			s.count = 1
			return false
		default:
			s.count = uint8(len(s.ids))
			return false
		}
	}
}

// Contains reports whether node is in the set. O(1) for the singleton
// state, O(log n) for the small slice, and roaring's container probe for
// the bitmap state.
func (s *NodeSet) Contains(node uint64) bool {
	if s.bm != nil {
		return s.bm.Contains(node)
	}
	switch s.count {
	case 0:
		return false
	case 1:
		return s.single == node
	default:
		i := lowerBoundU64(s.ids, node)
		return i < len(s.ids) && s.ids[i] == node
	}
}

// Cardinality returns the number of NodeIDs in the set.
func (s *NodeSet) Cardinality() uint64 {
	if s.bm != nil {
		return s.bm.GetCardinality()
	}
	if s.count == 1 {
		return 1
	}
	return uint64(len(s.ids))
}

// IsEmpty reports whether the set holds no NodeIDs.
func (s *NodeSet) IsEmpty() bool {
	if s.bm != nil {
		return s.bm.IsEmpty()
	}
	return s.count == 0
}

// Minimum returns the smallest NodeID in the set. The caller must ensure
// the set is non-empty; on an empty set it returns 0.
func (s *NodeSet) Minimum() uint64 {
	if s.bm != nil {
		return s.bm.Minimum()
	}
	switch s.count {
	case 0:
		return 0
	case 1:
		return s.single
	default:
		return s.ids[0] // sorted ascending
	}
}

// ToArray returns the NodeIDs in strictly ascending order as a freshly
// allocated slice the caller owns. This is the canonical iteration order
// every index consumer relies on, and the exact sorted list the btree and
// hash on-disk formats serialize — so it is representation-independent.
func (s *NodeSet) ToArray() []uint64 {
	if s.bm != nil {
		return s.bm.ToArray()
	}
	switch s.count {
	case 0:
		return nil
	case 1:
		return []uint64{s.single}
	default:
		out := make([]uint64, len(s.ids))
		copy(out, s.ids)
		return out
	}
}

// OrInto adds every NodeID in the set to dst (set union into dst),
// preserving dst's ascending order. It is the allocation-light way to
// fold a small set into a destination bitmap during a range scan: a
// singleton becomes a single Add, a small set an AddMany of the sorted
// ids (which hits roaring's batch-by-high-bits fast path), and a bitmap a
// roaring Or — never materialising a throwaway bitmap for the inline
// states (graph-theory-expert, #1584).
func (s *NodeSet) OrInto(dst *roaring64.Bitmap) {
	if s.bm != nil {
		dst.Or(s.bm)
		return
	}
	switch s.count {
	case 0:
	case 1:
		dst.Add(s.single)
	default:
		dst.AddMany(s.ids)
	}
}

// AppendTo appends every NodeID in strictly ascending order — the same order
// as ToArray — to dst and returns the extended slice, WITHOUT materialising a
// throwaway bitmap for the inline (singleton/small) states. It is the
// allocation-light way to drain a set into a caller-owned buffer under the
// index read lock: a singleton or small set appends straight from the inline
// fields, so a caller whose dst has spare capacity (e.g. a reused seek buffer)
// pays no heap allocation at all. Only the promoted bitmap state allocates a
// single iterator. The appended ids are an independent snapshot the caller may
// read after releasing the lock.
func (s *NodeSet) AppendTo(dst []uint64) []uint64 {
	if s.bm != nil {
		it := s.bm.Iterator()
		for it.HasNext() {
			dst = append(dst, it.Next())
		}
		return dst
	}
	switch s.count {
	case 0:
	case 1:
		dst = append(dst, s.single)
	default:
		dst = append(dst, s.ids...)
	}
	return dst
}

// Bitmap returns the set as a *roaring64.Bitmap. When the set is already
// in the bitmap state the live bitmap is returned (the caller must NOT
// mutate it); otherwise a fresh bitmap is materialised from the sorted
// ids. The materialised image is byte-identical under roaring's
// content-deterministic WriteTo to a bitmap that held the same ids all
// along, which is what keeps the label index's roaring-native on-disk
// format unchanged across this refactor (storage-engine-auditor, #1585).
//
// shared reports whether the returned bitmap aliases the set's live
// bitmap (true only in the bitmap state); callers that need an
// independent copy must Clone when shared is true.
func (s *NodeSet) Bitmap() (bm *roaring64.Bitmap, shared bool) {
	if s.bm != nil {
		return s.bm, true
	}
	out := roaring64.New()
	switch s.count {
	case 0:
	case 1:
		out.Add(s.single)
	default:
		out.AddMany(s.ids)
	}
	return out, false
}

// NodeSetFromSorted builds a NodeSet from an already strictly-ascending
// id slice. It is the deserialization constructor: the btree and hash
// readers parse the logical sorted NodeID list and hand it here, getting
// the cheapest representation for that cardinality (singleton/small/
// bitmap) without re-sorting. The caller guarantees ids is sorted
// ascending with no duplicates; ownership of ids transfers to the set.
func NodeSetFromSorted(ids []uint64) NodeSet {
	switch len(ids) {
	case 0:
		return NodeSet{}
	case 1:
		return NodeSet{single: ids[0], count: 1}
	}
	if len(ids) <= smallSetMax {
		// Defensive copy is unnecessary: ownership transfers per the
		// contract, and the slice is exactly the sorted ids we want.
		return NodeSet{ids: ids, count: uint8(len(ids))}
	}
	bm := roaring64.New()
	bm.AddMany(ids)
	return NodeSet{bm: bm}
}

// NodeSetFromBitmap returns the cheapest NodeSet representation of bm. A
// bitmap whose cardinality fits the inline small-set tier is down-converted
// (its few sorted ids extracted) so a sparse entry reloaded from a roaring
// image regains the memory win; a denser bitmap is kept on the bitmap tier
// WITHOUT extracting its (potentially huge) id array, so a dense label costs
// no transient O(cardinality) slice. Ownership of bm transfers to the set
// when it is kept; when down-converted, bm is no longer referenced.
//
// It is the label index's deserialization adaptor: that index persists the
// roaring native image, so it reads back a bitmap and calls this to recover
// the tiered in-memory shape (sprint 206, #1585).
func NodeSetFromBitmap(bm *roaring64.Bitmap) NodeSet {
	if bm.GetCardinality() > smallSetMax {
		return NodeSet{bm: bm}
	}
	// Small enough to inline: extract the (few) sorted ids and pick the
	// cheapest representation. ToArray on a <= smallSetMax bitmap allocates
	// a tiny slice.
	return NodeSetFromSorted(bm.ToArray())
}

// lowerBoundU64 returns the index of the first element of the
// sorted-ascending slice s that is >= target, in [0, len(s)].
func lowerBoundU64(s []uint64, target uint64) int {
	lo, hi := 0, len(s)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if s[mid] < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
