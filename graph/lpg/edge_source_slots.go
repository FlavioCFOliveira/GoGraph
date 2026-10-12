package lpg

// edge_source_slots.go — the per-view slot index behind the per-pair
// relationship reads of a [ReadView] (rmp #3089).
//
// # What it removes
//
// The per-pair surfaces — the derived type union and the coalesced property
// reads — find the pair's slots by scanning every slot of the source. A query
// that reads one relationship per row over a source of degree d therefore paid
// O(d) per row and O(d²) per source. The scan cannot be replaced by reading the
// bound edge's own slot: the per-pair contract unions types and coalesces
// properties over EVERY parallel slot of the pair, and a Go-API pair can carry
// different types or properties on its parallel slots. So the index keeps the
// per-pair answer exactly and removes only the search: one neighbour<<32 |
// position key per slot, sorted, so a pair's slots are one contiguous run found
// by binary search, in slot order.
//
// # Exactness
//
// An index describes ONE immutable adjacency-entry version and holds that
// version's columns. Every read first verifies that the entry the view resolves
// now is still that version (pointer identity of the published columns; the
// index keeps them alive, so identity cannot be reused) and otherwise falls
// back to the scan. Every answer is therefore the scan's answer over the same
// entry, in the same order.
//
// # Bounds and cost
//
// A view caches ONE index — the last source it indexed — swapped atomically.
// A source is indexed only after [slotsBuildAfterReads] consecutive requests,
// so a read stream that alternates sources, or reads a source only a few times,
// keeps the plain scan instead of paying for an index it cannot amortise; a source below [minSourceSlotsDegree] slots is never
// indexed. An index costs eight bytes per slot plus a fixed header, and lives as
// long as the view or until another source replaces it. Nothing is persisted and
// no writer maintains anything.

import (
	"slices"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// minSourceSlotsDegree is the smallest source degree a view indexes. Below it
// a scan of the source's slots is cheaper than building and searching an index.
const minSourceSlotsDegree = 32

// sourceSlots is an immutable index over one version of one source node's
// adjacency entry. It is never mutated after construction, so it is safe for
// concurrent use.
type sourceSlots struct {
	src   graph.NodeID
	nbs   []graph.NodeID
	labs  []uint32
	aux   adjlist.AuxColumn
	block *edgePropCols
	// keys holds one key per slot, neighbour<<32 | position, sorted: a pair's
	// slots are one contiguous run in ascending position, found by a binary
	// search over this one contiguous array.
	keys []uint64
}

// pairRun returns the keys of dstID's slots, in ascending slot position; the
// position of a key k is uint32(k).
func (s *sourceSlots) pairRun(dstID graph.NodeID) []uint64 {
	lo, hi := 0, len(s.keys)
	if uint64(dstID) > maxIndexedNeighbour {
		return nil
	}
	want := uint64(dstID) << 32
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if s.keys[mid] < want {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	end := lo
	for end < len(s.keys) && s.keys[end]>>32 == uint64(dstID) {
		end++
	}
	return s.keys[lo:end]
}

// maxIndexedNeighbour is the largest neighbour NodeID a key can hold.
const maxIndexedNeighbour = 1<<32 - 1

// buildSourceSlots indexes srcID's entry as it stands at this view's instant,
// or returns nil when the source is below [minSourceSlotsDegree] slots, has
// more slots than a 32-bit position can name, or has a neighbour whose NodeID
// does not fit a key's upper 32 bits; the scan serves those.
func (v *ReadView[N, W]) buildSourceSlots(srcID graph.NodeID) *sourceSlots {
	ev := v.g.EntryViewAsOf(srcID, v.snap)
	n := len(ev.Neighbours)
	if n < minSourceSlotsDegree || n > maxIndexedNeighbour {
		return nil
	}
	s := &sourceSlots{
		src:   srcID,
		nbs:   ev.Neighbours,
		labs:  ev.Labels,
		aux:   ev.Aux,
		block: asEdgePropCols(ev.Aux),
		keys:  make([]uint64, n),
	}
	for i, nb := range s.nbs {
		if uint64(nb) > maxIndexedNeighbour {
			return nil
		}
		s.keys[i] = uint64(nb)<<32 | uint64(i)
	}
	slices.Sort(s.keys)
	return s
}

// slotsBuildAfterReads is how many consecutive per-pair reads of one source a
// view serves by scanning before it indexes the source. A build costs about
// d·log2(d) comparisons against d per scanned read, so it amortises after a
// handful of reads; indexing on the second read rebuilt the index for sources
// read only a few times (measured on examples/26_social_scale_bench).
const slotsBuildAfterReads = 8

// indexedSlots returns the view's index for srcID when one is already cached,
// without counting the read toward a build. The property reads use it: they
// reuse an index the source's type reads built, and never build one themselves,
// so a build is paid once per source by the read that every relationship row
// performs first — its type — and never by a property read (measured on
// examples/26_social_scale_bench: builds triggered by property reads moved their
// cost onto those sites).
func (v *ReadView[N, W]) indexedSlots(srcID graph.NodeID) *sourceSlots {
	if s := v.slots.Load(); s != nil && s.src == srcID {
		return s
	}
	return nil
}

// cachedSlots returns the view's index for srcID, building it on srcID's
// [slotsBuildAfterReads]-th consecutive request, or nil when the view holds none
// for srcID.
func (v *ReadView[N, W]) cachedSlots(srcID graph.NodeID) *sourceSlots {
	if s := v.slots.Load(); s != nil && s.src == srcID {
		return s
	}
	key := uint64(srcID) + 1
	if v.smallSrc.Load() == key {
		return nil
	}
	if v.lastSrc.Load() != key {
		v.lastSrc.Store(key) // first sighting: scan, remember the source
		v.lastHits.Store(1)
		return nil
	}
	if v.lastHits.Add(1) < slotsBuildAfterReads {
		return nil // too few reads yet to amortise an O(d log d) build
	}
	s := v.buildSourceSlots(srcID)
	if s == nil {
		v.smallSrc.Store(key)
		return nil
	}
	v.slots.Store(s)
	return s
}

// sameSlice reports whether a and b are the same published column: the same
// backing array and the same length.
func sameSlice[T any](a, b []T) bool {
	return len(a) == len(b) && unsafe.SliceData(a) == unsafe.SliceData(b) //nolint:gosec // G103: audited — compares backing addresses for identity only, never dereferences them.
}

// propsCurrent reports whether s still indexes the entry its source has at this
// view's instant, as the property surfaces resolve it ([Graph.EntryViewAsOf]).
func (v *ReadView[N, W]) propsCurrent(s *sourceSlots) bool {
	ev := v.g.EntryViewAsOf(s.src, v.snap)
	return sameSlice(ev.Neighbours, s.nbs) && ev.Aux == s.aux
}

// edgeLabelsBySlots is [Graph.EdgeLabelsByIDAsOf] for (s.src, dstID) answered
// through s: the same label ids in the same order. ok is false when s no longer
// describes the entry the view resolves.
func (v *ReadView[N, W]) edgeLabelsBySlots(s *sourceSlots, dstID graph.NodeID) (labels []string, ok bool) {
	var idBuf [4]LabelID
	ids, ok := v.edgeLabelIDsBySlots(s, dstID, idBuf[:0])
	if !ok || len(ids) == 0 {
		return nil, ok
	}
	g := v.g
	out := make([]string, 0, len(ids))
	for _, lid := range ids {
		if name, has := g.reg.Resolve(lid); has {
			out = append(out, name)
		}
	}
	return out, true
}

// forEachEdgeLabelBySlots is [Graph.ForEachEdgeLabelByIDAsOf] for (s.src,
// dstID) answered through s: the same names in the same order, with no result
// slice (rmp #3106). It returns false, visiting nothing, when s is stale.
func (v *ReadView[N, W]) forEachEdgeLabelBySlots(s *sourceSlots, dstID graph.NodeID, visit func(name string)) bool {
	var idBuf [4]LabelID
	ids, ok := v.edgeLabelIDsBySlots(s, dstID, idBuf[:0])
	if !ok {
		return false
	}
	for _, lid := range ids {
		if name, has := v.g.reg.Resolve(lid); has {
			visit(name)
		}
	}
	return true
}

// edgeLabelIDsBySlots appends to ids the distinct label ids of (s.src, dstID)
// in the order [Graph.EdgeLabelsByIDAsOf] collects them: the pair's slots in
// ascending position, then the overflow. ok is false when s no longer describes
// the entry the view resolves. The caller's buffer keeps the common case off the
// heap (rmp #3106).
func (v *ReadView[N, W]) edgeLabelIDsBySlots(s *sourceSlots, dstID graph.NodeID, ids []LabelID) (_ []LabelID, ok bool) {
	g := v.g
	k := edgeKey{src: s.src, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.RLock()
	// The accessor [Graph.slotLabelsForPair] reads, so the index answers only
	// when it describes exactly the columns the scan would see.
	nbs, labs := g.entrySlotLabels(s.src, v.snap)
	if !sameSlice(nbs, s.nbs) || !sameSlice(labs, s.labs) {
		sh.mu.RUnlock()
		return ids, false
	}
	seen := func(lid LabelID) bool {
		for _, x := range ids {
			if x == lid {
				return true
			}
		}
		return false
	}
	if labs != nil {
		n := min(len(nbs), len(labs))
		for _, key := range s.pairRun(dstID) {
			p := int(uint32(key))
			if p >= n {
				break
			}
			if lid, has := decodeSlotLabel(labs[p]); has && !seen(lid) {
				ids = append(ids, lid)
			}
		}
	}
	for _, lid := range g.overflowLabelsAsOf(sh, k, v.snap) {
		if !seen(lid) {
			ids = append(ids, lid)
		}
	}
	sh.mu.RUnlock()
	return ids, true
}

// forEachEdgePropertyBySlots is [Graph.ForEachEdgePropertyByIDAsOf] for
// (s.src, dstID) answered through s: the same emissions in the same slot order.
// It returns false, visiting nothing, when s is stale.
func (v *ReadView[N, W]) forEachEdgePropertyBySlots(s *sourceSlots, dstID graph.NodeID, visit func(name string, pv PropertyValue)) bool {
	if !v.propsCurrent(s) {
		return false
	}
	block := s.block
	if block == nil {
		return true
	}
	n := minInt(len(s.nbs), block.lenOrZero())
	for _, key := range s.pairRun(dstID) {
		p := int(uint32(key))
		if p >= n {
			break
		}
		block.forEachAt(p, func(kk PropertyKeyID, pv PropertyValue) {
			if name, ok := v.g.pkeys.Resolve(kk); ok {
				visit(name, pv) // latest dst-matching slot wins (caller dedups)
			}
		})
	}
	return true
}

// edgePropertyBySlots is [Graph.GetEdgePropertyAsOf] for (s.src, dstID) and an
// interned key, answered through s. ok is false when s is stale.
func (v *ReadView[N, W]) edgePropertyBySlots(s *sourceSlots, dstID graph.NodeID, keyID PropertyKeyID) (pv PropertyValue, found, ok bool) {
	if !v.propsCurrent(s) {
		return PropertyValue{}, false, false
	}
	if s.block == nil {
		return PropertyValue{}, false, true
	}
	n := minInt(len(s.nbs), s.block.lenOrZero())
	for _, key := range s.pairRun(dstID) {
		p := int(uint32(key))
		if p >= n {
			break
		}
		if val, present := s.block.get(keyID, p); present {
			pv, found = val, true // latest dst-matching slot wins
		}
	}
	return pv, found, true
}

// edgeHasPropertyBySlots is [Graph.EdgeHasPropertyAsOf] for (s.src, dstID) and
// an interned key, answered through s with the same latest-slot kind gate. ok
// is false when s is stale.
func (v *ReadView[N, W]) edgeHasPropertyBySlots(s *sourceSlots, dstID graph.NodeID, keyID PropertyKeyID) (has, ok bool) {
	if !v.propsCurrent(s) {
		return false, false
	}
	if s.block == nil {
		return false, true
	}
	n := minInt(len(s.nbs), s.block.lenOrZero())
	found := false
	var winning PropertyKind
	for _, key := range s.pairRun(dstID) {
		p := int(uint32(key))
		if p >= n {
			break
		}
		if k, present := s.block.slotKind(keyID, p); present {
			winning, found = k, true // latest dst-matching slot wins
		}
	}
	if !found {
		return false, true
	}
	return kindMapsToNonNullCypher(winning), true
}

// pairIDs resolves both endpoint keys, as every key-keyed per-pair read does
// first.
func (v *ReadView[N, W]) pairIDs(src, dst N) (srcID, dstID graph.NodeID, ok bool) {
	m := v.g.adj.Mapper()
	if srcID, ok = m.Lookup(src); !ok {
		return 0, 0, false
	}
	if dstID, ok = m.Lookup(dst); !ok {
		return 0, 0, false
	}
	return srcID, dstID, true
}
