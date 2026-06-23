package index

// nodeset.go — the small-set tier shared by the btree and hash property
// indexes and the label inverted index (sprint 206, tasks #1584/#1585;
// 16-byte packing #1596).
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
// [NodeSet] is a four-state tagged union packed into TWO machine words —
// {ptr unsafe.Pointer; meta uint64} = 16 B — and stored BY VALUE in the
// index maps so a singleton key costs no separate heap object. The 48-byte
// safe union it replaces (#1584/#1585) is cut to 16 B, dropping the per-
// singleton index entry from ~134 B to ~56 B resident (~5.1x lighter than
// the original per-key roaring object, #1596). The four states:
//
//	empty      — no nodes.        ptr == nil, meta == 0 (the zero value).
//	singleton  — exactly one id.  ptr == nil, id held in meta's high bits;
//	             no allocation.
//	small      — 2..smallSetMax ids, held in a sorted-ascending *[8]uint64
//	             backing array. ptr points at the backing; meta carries the
//	             length. One allocation, 64 B fixed.
//	bitmap     — a promoted *roaring64.Bitmap held through ptr. Reached when
//	             the set grows past smallSetMax, or when a range is added
//	             wholesale ([NodeSet.AddRange]). Promotion is ONE-WAY: a set
//	             that becomes a bitmap never demotes, which keeps a dense
//	             label permanently on the run-container-optimal roaring path
//	             (graph-theory-expert, #1584/#1585).
//
// The promotion threshold smallSetMax = 8 keeps the backing array within
// one cache line (8x8 = 64 B) while the singleton state captures the
// dominant high-cardinality win; the slice tier wins on resident bytes
// over roaring up to n ≈ 17 (graph-theory-expert, #1584).
//
// # GC-safety and unsafe-pointer contract (#1596)
//
// The packed union is correct only under a strict set of invariants. They
// are the load-bearing safety contract; a violation is undefined behaviour
// and corrupts index membership (an ACID Consistency violation):
//
//  1. The ptr field is GC-SCANNED. It is ALWAYS nil or a real Go pointer
//     (the small-state *[8]uint64 backing, or the *roaring64.Bitmap).
//     It NEVER holds a tagged or fake pointer; all tag and length bits
//     live in meta (a plain uint64 the GC ignores). State is resolved by
//     meta's tag bits, never by inspecting ptr arithmetically.
//  2. The tag occupies meta's LOW two bits, so the zero value
//     (ptr==nil, meta==0) is unambiguously the empty state; a singleton
//     of id 0 is meta == stateSingleton (non-zero), never confused with it.
//  3. The small backing is a *[8]uint64: the capacity is a compile-time
//     constant, so reads reconstruct the live slice with
//     unsafe.Slice((*uint64)(ptr), n) where n <= 8 <= cap by construction —
//     never reading past the allocation. The backing's base address is the
//     start of the allocation (no interior-pointer arithmetic), and Go's
//     allocator guarantees its 8-byte alignment for uint64.
//  4. COPY-ON-WRITE backing. Because a NodeSet is copied BY VALUE (B+ tree
//     leaf splits, map assignment), two copies may transiently share one
//     *[8]uint64. The backing is therefore treated as IMMUTABLE once
//     published: every mutation allocates a fresh backing and repoints ptr,
//     so a write is never observed through an aliasing copy. This is both a
//     correctness invariant and an ACID Isolation guarantee. The current
//     consumers all re-store their value-copy under the index lock, so an
//     in-place small-tier mutation would also be safe today; COW is kept
//     deliberately so the integrity invariant does not depend on that
//     caller-held write-back discipline (storage-engine-auditor, #1596).
//  5. The *roaring64.Bitmap is stored and recovered through the blessed
//     same-type unsafe.Pointer round-trip ((*roaring64.Bitmap)(ptr)), which
//     the GC tracks exactly as a typed *roaring64.Bitmap field would.
//  6. No unsafe.Pointer<->uintptr round-trips exist anywhere in this file;
//     all tag math is on meta. This keeps the code clean under the race
//     build's checkptr instrumentation.
//
// # Query invariants
//
// Membership ([NodeSet.Contains]), cardinality ([NodeSet.Cardinality]),
// and the STRICTLY-ASCENDING iteration order every consumer relies on
// ([NodeSet.ToArray], [NodeSet.OrInto]) are identical for all four states
// and all cardinalities. The on-disk serialization of every index is
// representation-INDEPENDENT: it writes the logical sorted NodeID list
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
// the matching read lock; the promotion swap (small -> *roaring64.Bitmap)
// and every small-tier COW repoint are single field writes completed
// entirely within one locked operation, so no reader can observe a
// half-promoted set.

import (
	"unsafe"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
)

// smallSetMax is the largest cardinality held in the inline sorted-array
// tier before a NodeSet promotes to a *roaring64.Bitmap. Chosen so the
// backing array stays within one 64-byte cache line (8 x uint64) and the
// dominant singleton/handful case never touches roaring's ~168 B fixed
// overhead (graph-theory-expert, #1584). A set built only from individual
// [NodeSet.Add] calls cannot reach a dense contiguous band before crossing
// this threshold, so a dense label is never mis-tiered as a small set.
const smallSetMax = 8

// State tags occupy meta's low two bits (#1596). The zero value of meta
// (and the whole struct) is therefore stateEmpty, the cheap map-miss /
// slice-grow default.
const (
	stateEmpty     uint64 = 0b00 // ptr == nil, meta == 0
	stateSingleton uint64 = 0b01 // ptr == nil, id in meta>>tagShift
	stateSmall     uint64 = 0b10 // ptr == *[8]uint64, n in meta>>tagShift
	stateBitmap    uint64 = 0b11 // ptr == *roaring64.Bitmap

	tagMask  uint64 = 0b11
	tagShift uint64 = 2

	// maxSingletonID is the largest NodeID storable inline in the singleton
	// state: the id occupies meta's high 62 bits. NodeIDs come from a
	// monotonic counter that cannot approach 2^62 ≈ 4.6e18 in any realistic
	// workload, so a singleton id never overflows this cap; a value that
	// somehow exceeds it is held in a 1-element backing array instead, never
	// truncated.
	maxSingletonID uint64 = (uint64(1) << 62) - 1
)

// NodeSet is the per-key node-set representation shared by the btree,
// hash, and label indexes. The zero value is a valid empty set. See the
// package-level nodeset.go documentation for the state machine and the
// query/serialization/GC-safety invariants.
//
// Concurrency: a NodeSet is not safe for concurrent use on its own. It is
// embedded by value in an index (a btree leaf slot, a hash shard map, or
// the label-index map), and every read and mutation is serialised by that
// owning index's lock; a NodeSet is never shared across goroutines outside
// that discipline. Lookup paths that hand a set's contents to a caller copy
// out under the read lock, so the returned data is safe for concurrent use.
//
// The two fields form a tagged union resolved solely by meta's low two
// bits (see the state* constants). ptr is GC-scanned and is always nil or
// a real Go pointer; it never carries tag bits.
type NodeSet struct {
	ptr  unsafe.Pointer // nil (empty/singleton) | *[8]uint64 (small) | *roaring64.Bitmap (bitmap)
	meta uint64         // low 2 bits = state tag; high bits = id (singleton) or len (small)
}

// smallBacking is the fixed-capacity backing array for the small state.
// Its constant capacity makes every read's unsafe.Slice provably in-bounds.
type smallBacking = [smallSetMax]uint64

// tag returns the set's state tag.
func (s *NodeSet) tag() uint64 { return s.meta & tagMask }

// smallSlice reconstructs the live sorted-ascending id slice for a set in
// the small state. The caller guarantees the small state; the returned
// slice borrows the backing and must not outlive the next mutation.
func (s *NodeSet) smallSlice() []uint64 {
	n := s.meta >> tagShift
	// ptr is a real *[8]uint64 base; n <= smallSetMax = cap, so the span is
	// within one allocation (GC-safe, checkptr-clean).
	return unsafe.Slice((*uint64)(s.ptr), n) //nolint:gosec // G103: audited; ptr is a *[8]uint64 base, n <= 8 = cap (in-bounds, contract invariant 3)
}

// bitmapRef returns the live *roaring64.Bitmap for a set in the bitmap
// state via the blessed same-type unsafe.Pointer round-trip.
func (s *NodeSet) bitmapRef() *roaring64.Bitmap {
	return (*roaring64.Bitmap)(s.ptr)
}

// setSmall publishes ids (sorted ascending, len in [2, smallSetMax]) as the
// set's small state, copying into a freshly allocated immutable backing
// (copy-on-write invariant 4). It clears any prior pointer.
func (s *NodeSet) setSmall(ids []uint64) {
	backing := new(smallBacking)
	n := copy(backing[:], ids)
	s.ptr = unsafe.Pointer(backing) //nolint:gosec // G103: audited; stores a real *[8]uint64 (nil-or-valid, contract invariant 1)
	s.meta = (uint64(n) << tagShift) | stateSmall
}

// setSingleton publishes id as the singleton state.
func (s *NodeSet) setSingleton(id uint64) {
	if id > maxSingletonID {
		// Out-of-cap id: hold it in a 1-element backing rather than truncate.
		s.setSmall([]uint64{id})
		return
	}
	s.ptr = nil
	s.meta = (id << tagShift) | stateSingleton
}

// setBitmap publishes bm as the (one-way) bitmap state.
func (s *NodeSet) setBitmap(bm *roaring64.Bitmap) {
	s.ptr = unsafe.Pointer(bm) //nolint:gosec // G103: audited; same-type *roaring64.Bitmap round-trip (contract invariant 5)
	s.meta = stateBitmap
}

// setEmpty resets the set to the empty state.
func (s *NodeSet) setEmpty() {
	s.ptr = nil
	s.meta = stateEmpty
}

// Add inserts node into the set, preserving ascending order and
// promoting to a bitmap when the small tier would overflow. Adding a
// node already present is a no-op (set semantics). Returns true when the
// set was previously empty (so a caller maintaining a distinct-key count
// can detect a brand-new entry).
func (s *NodeSet) Add(node uint64) (wasEmpty bool) {
	switch s.tag() {
	case stateEmpty:
		s.setSingleton(node)
		return true
	case stateSingleton:
		cur := s.meta >> tagShift
		if node == cur {
			return false
		}
		lo, hi := cur, node
		if lo > hi {
			lo, hi = hi, lo
		}
		s.setSmall([]uint64{lo, hi})
		return false
	case stateSmall:
		s.addSmall(node)
		return false
	default: // stateBitmap
		s.bitmapRef().Add(node)
		return false
	}
}

// addSmall inserts node into the sorted small backing, promoting to a
// bitmap when the insert would exceed smallSetMax. The caller guarantees
// the set is in the small state. The insert is copy-on-write: a fresh
// backing is allocated rather than mutating the published one (invariant 4).
func (s *NodeSet) addSmall(node uint64) {
	cur := s.smallSlice()
	i := lowerBoundU64(cur, node)
	if i < len(cur) && cur[i] == node {
		return // already present
	}
	if len(cur) >= smallSetMax {
		// Promote: build a bitmap from the existing sorted ids plus node.
		bm := roaring64.New()
		bm.AddMany(cur)
		bm.Add(node)
		s.setBitmap(bm)
		return
	}
	// Build the new sorted backing (copy-on-write): elements before i, the
	// new node, then the tail.
	next := make([]uint64, 0, len(cur)+1)
	next = append(next, cur[:i]...)
	next = append(next, node)
	next = append(next, cur[i:]...)
	s.setSmall(next)
}

// Remove deletes node from the set. No-op when absent. A NodeSet never
// demotes: removing from a bitmap leaves it a bitmap even if its
// cardinality drops to one (promote-and-never-demote, #1584). Returns
// true when the set became EMPTY as a result (so a caller maintaining a
// distinct-key count can drop the key).
func (s *NodeSet) Remove(node uint64) (nowEmpty bool) {
	switch s.tag() {
	case stateEmpty:
		return false
	case stateSingleton:
		if s.meta>>tagShift != node {
			return false
		}
		s.setEmpty()
		return true
	case stateSmall:
		cur := s.smallSlice()
		i := lowerBoundU64(cur, node)
		if i >= len(cur) || cur[i] != node {
			return false
		}
		if len(cur)-1 == 1 {
			// Collapse to the singleton state so a key that churned down to
			// one node is as cheap as a fresh singleton.
			if i == 0 {
				s.setSingleton(cur[1])
			} else {
				s.setSingleton(cur[0])
			}
			return false
		}
		// Copy-on-write removal into a fresh backing.
		next := make([]uint64, 0, len(cur)-1)
		next = append(next, cur[:i]...)
		next = append(next, cur[i+1:]...)
		s.setSmall(next)
		return false
	default: // stateBitmap
		bm := s.bitmapRef()
		bm.Remove(node)
		return bm.IsEmpty()
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
	if s.tag() != stateBitmap {
		bm := roaring64.New()
		// Fold any existing inline state into the new bitmap so the range
		// add is additive (set union), not a replacement.
		switch s.tag() {
		case stateSingleton:
			bm.Add(s.meta >> tagShift)
		case stateSmall:
			bm.AddMany(s.smallSlice())
		}
		s.setBitmap(bm)
	}
	s.bitmapRef().AddRange(from, to+1)
}

// RemoveRange removes every id in [from, to] (inclusive). On an inline
// (non-bitmap) set it removes the few covered ids individually; on a
// bitmap it uses roaring's RemoveRange. A NodeSet never demotes, so a
// bitmap stays a bitmap. Returns true when the set became EMPTY.
func (s *NodeSet) RemoveRange(from, to uint64) (nowEmpty bool) {
	switch s.tag() {
	case stateEmpty:
		return false
	case stateSingleton:
		id := s.meta >> tagShift
		if id < from || id > to {
			return false
		}
		s.setEmpty()
		return true
	case stateSmall:
		cur := s.smallSlice()
		kept := make([]uint64, 0, len(cur))
		for _, v := range cur {
			if v < from || v > to {
				kept = append(kept, v)
			}
		}
		switch len(kept) {
		case 0:
			s.setEmpty()
			return true
		case 1:
			s.setSingleton(kept[0])
			return false
		default:
			s.setSmall(kept)
			return false
		}
	default: // stateBitmap
		bm := s.bitmapRef()
		bm.RemoveRange(from, to+1)
		return bm.IsEmpty()
	}
}

// Contains reports whether node is in the set. O(1) for the singleton
// state, O(log n) for the small array, and roaring's container probe for
// the bitmap state.
func (s *NodeSet) Contains(node uint64) bool {
	switch s.tag() {
	case stateEmpty:
		return false
	case stateSingleton:
		return s.meta>>tagShift == node
	case stateSmall:
		cur := s.smallSlice()
		i := lowerBoundU64(cur, node)
		return i < len(cur) && cur[i] == node
	default: // stateBitmap
		return s.bitmapRef().Contains(node)
	}
}

// Cardinality returns the number of NodeIDs in the set.
func (s *NodeSet) Cardinality() uint64 {
	switch s.tag() {
	case stateEmpty:
		return 0
	case stateSingleton:
		return 1
	case stateSmall:
		return s.meta >> tagShift
	default: // stateBitmap
		return s.bitmapRef().GetCardinality()
	}
}

// IsEmpty reports whether the set holds no NodeIDs.
func (s *NodeSet) IsEmpty() bool {
	switch s.tag() {
	case stateEmpty:
		return true
	case stateBitmap:
		return s.bitmapRef().IsEmpty()
	default: // singleton or small are never empty
		return false
	}
}

// Minimum returns the smallest NodeID in the set. The caller must ensure
// the set is non-empty; on an empty set it returns 0.
func (s *NodeSet) Minimum() uint64 {
	switch s.tag() {
	case stateEmpty:
		return 0
	case stateSingleton:
		return s.meta >> tagShift
	case stateSmall:
		return s.smallSlice()[0] // sorted ascending
	default: // stateBitmap
		return s.bitmapRef().Minimum()
	}
}

// ToArray returns the NodeIDs in strictly ascending order as a freshly
// allocated slice the caller owns. This is the canonical iteration order
// every index consumer relies on, and the exact sorted list the btree and
// hash on-disk formats serialize — so it is representation-independent.
func (s *NodeSet) ToArray() []uint64 {
	switch s.tag() {
	case stateEmpty:
		return nil
	case stateSingleton:
		return []uint64{s.meta >> tagShift}
	case stateSmall:
		cur := s.smallSlice()
		out := make([]uint64, len(cur))
		copy(out, cur)
		return out
	default: // stateBitmap
		return s.bitmapRef().ToArray()
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
	switch s.tag() {
	case stateEmpty:
	case stateSingleton:
		dst.Add(s.meta >> tagShift)
	case stateSmall:
		dst.AddMany(s.smallSlice())
	default: // stateBitmap
		dst.Or(s.bitmapRef())
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
	switch s.tag() {
	case stateEmpty:
	case stateSingleton:
		dst = append(dst, s.meta>>tagShift)
	case stateSmall:
		dst = append(dst, s.smallSlice()...)
	default: // stateBitmap
		it := s.bitmapRef().Iterator()
		for it.HasNext() {
			dst = append(dst, it.Next())
		}
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
	if s.tag() == stateBitmap {
		return s.bitmapRef(), true
	}
	out := roaring64.New()
	switch s.tag() {
	case stateSingleton:
		out.Add(s.meta >> tagShift)
	case stateSmall:
		out.AddMany(s.smallSlice())
	}
	return out, false
}

// NodeSetFromSorted builds a NodeSet from an already strictly-ascending
// id slice. It is the deserialization constructor: the btree and hash
// readers parse the logical sorted NodeID list and hand it here, getting
// the cheapest representation for that cardinality (singleton/small/
// bitmap) without re-sorting. The caller guarantees ids is sorted
// ascending with no duplicates.
func NodeSetFromSorted(ids []uint64) NodeSet {
	var s NodeSet
	switch {
	case len(ids) == 0:
		// already empty
	case len(ids) == 1:
		s.setSingleton(ids[0])
	case len(ids) <= smallSetMax:
		s.setSmall(ids)
	default:
		bm := roaring64.New()
		bm.AddMany(ids)
		s.setBitmap(bm)
	}
	return s
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
		var s NodeSet
		s.setBitmap(bm)
		return s
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
