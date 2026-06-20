package lpg

// edge_property_column.go — the columnar edge-property tier (sprint 222,
// design D1 in docs/columnar-edge-properties-design.md).
//
// # What this replaces
//
// Edge properties were stored as a per-shard map[edgeKey]propBag: one boxed
// property bag per (src,dst) endpoint pair, keyed by a redundant (src,dst) pair
// the adjacency already holds. A measurement audit found this the dominant
// resident-memory consumer at scale (~128 B/edge for one date property, of
// which only ~16 B was the value).
//
// # The columnar block
//
// [edgePropCols] is an immutable, lpg-owned block carried verbatim inside each
// adjacency [adjlist.adjEntry] as its opaque [adjlist.AuxColumn]. One block
// lives per source node and holds a small ordered set of typed columns, one per
// (PropertyKeyID, PropertyKind) present on that node's out-edges, each aligned
// 1:1 to the entry's neighbours array. The scalar kinds are stored DE-BOXED
// ([]int64, []float64, bit-packed bool, []int32 epoch-day for dates) so the
// per-edge value cost collapses to the raw width; bytes/list and the rare
// same-key-type-collision spill keep a boxed []PropertyValue fallback.
//
// # Presence (validity)
//
// A property absent on a slot is null. Presence is an Arrow-style validity
// bitmap, one bit per slot, OMITTED entirely when a column has no nulls (the
// dense case pays zero validity overhead). A bitmap is NOT replaceable by a
// sentinel: 0 is a legal int, NaN a legal float. IS NULL / IS NOT NULL read
// only the bitmap (popcount), never the values.
//
// # Per-pair contract via per-slot columns (load-bearing)
//
// The public [Graph.EdgeProperties] contract is PER-PAIR: one coalesced,
// latest-wins map per (src,dst), folding parallel edges. The columns are
// PER-SLOT. The two are reconciled by a LOCKSTEP write rule, exactly as the
// relationship-label column does: a per-pair SetEdgeProperty writes the value
// to EVERY dst-matching slot; a read takes the LAST dst-matching slot (all
// dst-matching slots carry the identical set, so the choice is conventional);
// a delete clears the key on EVERY dst-matching slot; and appending a new
// parallel (src,dst) slot copies the pair's current set onto the new slot. This
// keeps every dst-matching slot identical at all observable points, so the
// derived per-pair view is byte-identical to the old single-bag-per-pair map.
//
// # Immutability and concurrency
//
// A published [edgePropCols] is immutable; every mutation ([edgePropCols.set],
// [edgePropCols.del], [edgePropCols.GrowSlot], [edgePropCols.CompactSlot])
// returns a NEW block copy-on-write, sharing nothing mutable with the receiver.
// adjlist publishes the new block as part of an immutable adjEntry via a single
// atomic.StorePointer, so a lock-free reader observes either the old block or
// the new one, never a half-built column. The mutating methods are called only
// under the adjacency shard write lock (via [adjlist.AdjList.UpdateEntryAux]),
// so they never race each other for one entry.

import (
	"math"
	"math/bits"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// auxColumn aliases [adjlist.AuxColumn], the opaque per-entry side-column
// interface [edgePropCols] implements. The alias keeps the lifecycle-method
// signatures in this file short.
type auxColumn = adjlist.AuxColumn

// edgePropPayload is the opaque value the fused property-carrying append path
// ([Graph.AddEdgeLabeledWithProperty]) threads through adjlist's append fast path
// (as the [adjlist] edgeExtra aux payload) to the new slot. adjlist treats it as
// an opaque interface word and never inspects it; only the two lpg-owned
// consumers — [edgePropCols.GrowSlotWithValue] (existing entry) and
// [newEdgePropColsAux] (fresh entry, via the registered aux factory) — read it,
// folding the value into the per-slot columnar block.
//
// It is always handed across the boundary as a *edgePropPayload, so wrapping it
// in adjlist's any-typed field stores the pointer directly in the interface word
// and adds no boxing allocation beyond the single struct itself; lpg allocates
// exactly one per fused edge. keyID/value are the interned key and the value to
// record on the new slot.
type edgePropPayload struct {
	value PropertyValue
	keyID PropertyKeyID
}

// edgePropColumn is one typed value column inside an [edgePropCols] block. It is
// keyed by the interned property key and the property kind, holds one logical
// value per adjacency slot in a de-boxed representation when possible, and
// stores presence in one of two physical representations selected by fill ratio
// (the [edgePropColumn.sparse] flag):
//
//	DENSE  (sparse == false): the typed backing slice has exactly [length]
//	  elements, one per slot, indexed directly by slot. Presence is the
//	  Arrow-style validity bitmap [edgePropColumn.valid], which is nil ⇔ every
//	  slot is present (a fully-dense column pays zero validity overhead). A slot
//	  whose validity bit is clear reads as null regardless of its value cell.
//
//	SPARSE (sparse == true): a COO (coordinate-list) representation. The typed
//	  backing slice holds only the P present values in ascending-slot order, and
//	  [edgePropColumn.idx] holds the present slot indices, strictly ascending and
//	  in [0, length). Presence is membership in idx; the validity bitmap is nil.
//	  Sparse wins on bytes when fill P/length is below the per-kind break-even
//	  (see [denseFillThreshold]); for a sparse-within-a-high-degree-node key it
//	  drops the (length-P) absent value cells the dense backing would allocate.
//
// Exactly one of the typed backing slices is non-nil, selected by kind. The
// physical representation is re-evaluated with hysteresis at each mutation (see
// [edgePropColumn.reshaped]) so it tracks the actual current fill without
// thrashing at the boundary.
//
// A column is immutable once it is part of a published block; the mutation
// helpers return a fresh column.
type edgePropColumn struct {
	key  PropertyKeyID
	kind PropertyKind

	// De-boxed scalar backings. Exactly one is used per column, by kind:
	//   PropInt64   -> i64
	//   PropFloat64 -> f64
	//   PropBool    -> packed bits in boolBits (1 bit/slot)
	//   date        -> days (int32 epoch-day); see dateKind below
	//   PropString  -> str
	//   PropBytes / PropList / collision spill -> boxed
	//
	// DENSE: length elements, indexed by slot. SPARSE: P elements, indexed by
	// position in idx (the k-th element is the value of slot idx[k]). Bit-packed
	// bool is never sparse (its break-even fill is ~0.06, see reshaped), so
	// boolBits always has the dense, slot-indexed meaning.
	i64      []int64
	f64      []float64
	boolBits []uint64 // bit i = bool value of slot i (DENSE only; bool is never sparse)
	days     []int32
	str      []string
	boxed    []PropertyValue

	// valid is the Arrow-style validity bitmap used in the DENSE representation:
	// bit i set ⇔ slot i carries a value. nil ⇔ the dense column is fully present
	// (zero validity overhead). Always nil in the SPARSE representation, where
	// presence is membership in idx.
	valid []uint64

	// idx holds the present slot indices of the SPARSE representation, strictly
	// ascending and each in [0, length). len(idx) == the present count P, and the
	// k-th typed backing element is the value of slot idx[k]. nil ⇔ DENSE.
	idx []int32

	// sparse selects the physical representation: true ⇔ COO (idx + compacted
	// backing), false ⇔ dense (slot-indexed backing + optional validity bitmap).
	sparse bool

	// length is the number of slots this column spans; it equals the adjacency
	// entry's neighbour count at every observable point. It is an explicit stored
	// scalar (NOT derivable from idx): a grow-without-set leaves absent trailing
	// slots, so length can exceed max(idx)+1.
	length int
}

// dateKind is the internal PropertyKind sentinel for the int32 epoch-day date
// column. A Cypher date is delivered to the property layer as a PropString
// whose first byte is the SOH Date tag (see cypher/exec/temporal_literal.go and
// the round-trip note in edge_property.go); the column recognises that shape and
// stores the epoch-day in []int32, reconstituting the identical tagged string on
// read so cypher/api.go's lpgPropToExpr round-trips it to a native Date. The
// sentinel is internal to this file: it never appears in a PropertyValue.Kind()
// nor on disk.
//
// It is set well above the public PropertyKind enum so it can never collide
// with a real kind value carried by a PropertyValue.
const dateKind PropertyKind = 200

// epochDayTag is the SOH Date tag byte (matches cypher/exec/temporal_literal.go
// tempPrefixDate). A PropString value beginning with this byte is a Cypher Date
// in canonical YYYY-MM-DD text after the tag.
const epochDayTag = 0x01

// edgePropCols is the immutable per-source-node columnar property block. It
// implements [adjlist.AuxColumn]. The zero value is an empty block of length 0.
//
// cols is an ordered slice of typed columns, one per (key, kind) pair present
// on the node's out-edges. For the overwhelmingly common case of an edge with
// one or two property keys the slice is tiny and a linear scan to locate a
// column beats a map probe while avoiding the per-block map allocation.
type edgePropCols struct {
	cols   []edgePropColumn
	length int
}

// --- adjlist.AuxColumn implementation -------------------------------------

// GrowSlot returns a new block of length oldLen+1 whose existing slots are
// unchanged and whose new slot at index oldLen is ABSENT in every column. This
// is the highest-risk correctness point in the design: the new slot's value
// cell is don't-care, so its presence MUST come from the validity bitmap being
// clear, never from a reused backing-array cell. Because GrowSlot allocates a
// fresh validity bitmap sized to the new length (and a dense column gains its
// first bitmap here, with the new bit clear and all prior bits set), the new
// slot is unconditionally absent.
func (c *edgePropCols) GrowSlot(oldLen int) auxColumn {
	if c == nil {
		return nil
	}
	out := &edgePropCols{length: oldLen + 1}
	if len(c.cols) == 0 {
		return out
	}
	out.cols = make([]edgePropColumn, len(c.cols))
	for i := range c.cols {
		out.cols[i] = c.cols[i].grown(oldLen)
	}
	return out
}

// GrowSlotWithValue is the value-carrying analogue of [edgePropCols.GrowSlot]: it
// returns a new block of length oldLen+1 whose existing slots are unchanged and
// whose new slot at index oldLen is PRESENT in the (key, kind) column named by
// the opaque payload, and ABSENT in every other column. It implements
// [adjlist.AuxColumn] and is the per-slot half of the fused build fast path:
// adjlist calls it during the same O(1)-amortised append that grows the
// adjacency entry, so a degree-d source stamping one property per edge stays
// O(d) total rather than the O(d²) a per-edge SetEdgeProperty column rebuild
// would cost.
//
// Two design rules from the amortised analysis (graph-theory-expert + the
// columnar-db survey) make the O(d) guarantee real and are load-bearing:
//
//  1. The target column's value lands at the TAIL: oldLen is strictly greater
//     than every slot already present (length == neighbour count at every
//     observable point, and every prior slot index is < oldLen), so the append
//     is a coordinate-list tail push — append the value and append oldLen to the
//     strictly-ascending idx — in O(1) amortised, never the O(P) ordered insert
//     [edgePropColumn.setSlot] would do. This path forces/keeps the target column
//     SPARSE for exactly that reason; the dense slot-indexed backing has cap ==
//     length (no spare), so a dense append would reallocate-and-copy O(oldLen)
//     each time and reintroduce O(d²).
//  2. It does NOT call [edgePropColumn.reshaped]. Re-evaluating the sparse↔dense
//     representation per append would run an O(P) toDense rebuild repeatedly as a
//     fill→1.0 build crosses the promote threshold, again O(d²). Representation
//     re-evaluation is deferred to [edgePropCols.Compact] (the freeze/trim pass)
//     and to the next ordinary set/del/grow, exactly as an Arrow array builder
//     picks its physical encoding at Finish, not per Append.
//
// payload must be a *edgePropPayload (lpg is the only caller); a payload of any
// other dynamic type is ignored and the new slot is left absent (defensive — it
// cannot happen through the public API).
func (c *edgePropCols) GrowSlotWithValue(oldLen int, payload any) auxColumn {
	p, ok := payload.(*edgePropPayload)
	if !ok || p == nil {
		// Defensive: an unexpected payload degrades to an absent grow rather than
		// corrupting the block. Unreachable through the public API.
		return c.GrowSlot(oldLen)
	}
	newLen := oldLen + 1
	if c == nil {
		// No prior block (the entry's first aux value arrives via the grow path
		// rather than the factory, e.g. earlier edges added without a property):
		// build a fresh single-present-slot block spanning newLen.
		return newEdgePropColsWithValue(newLen, oldLen, p)
	}
	kind, days, _ := classify(p.value)
	out := &edgePropCols{length: newLen}
	if len(c.cols) == 0 {
		// Block existed but carried no columns yet: just the new column.
		out.cols = []edgePropColumn{newSparseSingleSlot(p.keyID, kind, newLen, oldLen, p.value, days)}
		return out
	}
	out.cols = make([]edgePropColumn, len(c.cols))
	target := -1
	for i := range c.cols {
		if c.cols[i].key == p.keyID && c.cols[i].kind == kind {
			target = i
			out.cols[i] = c.cols[i].grownWithValue(oldLen, p.value, days)
			continue
		}
		// Every NON-target column grows the new slot ABSENT. This MUST stay O(1)
		// amortised: a source node that carries two keys (e.g. a USER with both
		// FRIEND.since and LIKE.when out-edges) appends to each key's column while
		// the OTHER key's column is the non-target on every such append, so a
		// dense full-column copy here (grown) would reintroduce O(degree²). The
		// absent shared-extend bumps the length while sharing the immutable
		// backing — O(1) for a sparse column (the new slot is simply not in idx)
		// and a copy-on-write-safe bitmap extend for a dense one.
		out.cols[i] = c.cols[i].grownAbsentShared(oldLen)
	}
	if target < 0 {
		// The (key, kind) column does not exist yet on this node: append a fresh
		// sparse column whose single present slot is the new one.
		out.cols = append(out.cols, newSparseSingleSlot(p.keyID, kind, newLen, oldLen, p.value, days))
	}
	return out
}

// newEdgePropColsAux is the [adjlist.AdjList] aux factory (registered in
// [New]) for the fused property-carrying append. adjlist invokes it for the
// FIRST edge of a node — where there is no aux block to grow — handing the block
// length the single present slot must span (1 for a brand-new entry; oldLen+1
// when earlier edges were added without a property) and the opaque payload. It
// returns a block whose only present slot is the last one (index length-1).
//
// The signature matches adjlist's registered factory type exactly; payload must
// be a *edgePropPayload.
func newEdgePropColsAux(length int, payload any) auxColumn {
	p, ok := payload.(*edgePropPayload)
	if !ok || p == nil {
		return nil // defensive: unreachable through the public API
	}
	if length < 1 {
		length = 1
	}
	return newEdgePropColsWithValue(length, length-1, p)
}

// newEdgePropColsWithValue builds a fresh block of the given length carrying the
// payload's value on the single present slot `slot` (all other slots absent),
// with the column in the SPARSE representation so a subsequent fused append is an
// O(1) coordinate-list tail push (see [edgePropCols.GrowSlotWithValue] rule 1).
func newEdgePropColsWithValue(length, slot int, p *edgePropPayload) *edgePropCols {
	kind, days, _ := classify(p.value)
	return &edgePropCols{
		length: length,
		cols:   []edgePropColumn{newSparseSingleSlot(p.keyID, kind, length, slot, p.value, days)},
	}
}

// CompactSlot returns a new block of length n-1 with the slot at idx excised
// from every column. The validity bitmap (when present) is compacted by the
// SAME index transform via a bit splice. idx must be a valid index in [0, n).
func (c *edgePropCols) CompactSlot(idx int) auxColumn {
	if c == nil {
		return nil
	}
	if idx < 0 || idx >= c.length {
		// Defensive: an out-of-range index would corrupt alignment. adjlist only
		// ever passes a valid index, so this is unreachable in practice; return
		// the block unchanged rather than panic inside the lock-free publish path.
		return c
	}
	out := &edgePropCols{length: c.length - 1}
	if len(c.cols) == 0 {
		return out
	}
	out.cols = make([]edgePropColumn, len(c.cols))
	for i := range c.cols {
		out.cols[i] = c.cols[i].compacted(idx)
	}
	return out
}

// Compact returns a block whose columns hold no backing slack, or the receiver
// when every column is already exactly sized. A sparse (COO) column built by
// amortised-growth inserts can carry up to ~2x slack in its idx and value
// backings; Compact re-allocates each such backing at exact length. Dense
// columns are already exactly length-sized, so a property-free or dense-only
// block returns unchanged. Implements [adjlist.AuxColumn].
func (c *edgePropCols) Compact() auxColumn {
	if c == nil || len(c.cols) == 0 {
		return c
	}
	var out *edgePropCols
	for i := range c.cols {
		if !c.cols[i].hasSlack() {
			continue
		}
		if out == nil {
			out = &edgePropCols{length: c.length, cols: make([]edgePropColumn, len(c.cols))}
			copy(out.cols, c.cols)
		}
		out.cols[i] = c.cols[i].compactBacking()
	}
	if out == nil {
		return c // no slack anywhere
	}
	return out
}

// --- block-level mutation (copy-on-write) ---------------------------------

// withLength returns the block's length, treating a nil block as length 0.
func (c *edgePropCols) lenOrZero() int {
	if c == nil {
		return 0
	}
	return c.length
}

// set returns a new block equal to c but with value v recorded for keyID on the
// given slot. The receiver is never mutated. length is the slot count the block
// must span (the entry's neighbour count); a nil/short receiver is grown to it.
// A date-shaped string value is folded into the int32 epoch-day column.
//
// One live value per (slot, key): a key may carry different kinds across
// different slots (openCypher allows this), so the block stores at most one
// column per (key, kind). But on a SINGLE slot a key has at most one value —
// last write wins regardless of kind. So a set that lands on a slot already
// carrying keyID under a DIFFERENT kind-column first clears keyID's presence on
// that slot in every other column, then writes the new value. This reproduces
// the old single-PropertyValue-per-key bag semantics exactly.
func (c *edgePropCols) set(keyID PropertyKeyID, slot, length int, v PropertyValue) *edgePropCols {
	kind, days, _ := classify(v)
	out := c.clone(length)
	// Clear keyID on this slot in every column of a DIFFERENT kind, so the slot
	// ends up carrying the new value under exactly one column (last-write-wins).
	// In the common single-kind-per-key case nothing is cleared.
	clearedOther := false
	for i := range out.cols {
		if out.cols[i].key != keyID || out.cols[i].kind == kind {
			continue
		}
		if out.cols[i].slotValid(slot) {
			out.cols[i] = out.cols[i].cloneCol()
			out.cols[i].clearSlot(slot)
			out.cols[i] = out.cols[i].reshaped() // fill dropped: may demote to sparse
			clearedOther = true
		}
	}
	ci := out.columnIndex(keyID, kind)
	if ci < 0 {
		// New (key, kind) column: newColumn picks the representation for a
		// single-slot fill, then setSlot writes the first value.
		col := newColumn(keyID, kind, length)
		col.setSlot(slot, v, days)
		out.cols = append(out.cols, col.reshaped())
	} else {
		out.cols[ci] = out.cols[ci].cloneCol()
		out.cols[ci].setSlot(slot, v, days)
		out.cols[ci] = out.cols[ci].reshaped() // fill rose: may promote to dense
	}
	// Clearing the old-kind cell above may have emptied that column; drop any
	// column with no present slot so a key that changed kind does not leave an
	// empty husk behind (which would also break the cardinality oracle). Only
	// needed when a cross-kind clear actually happened.
	if clearedOther {
		out.dropEmptyColumns()
	}
	return out
}

// del returns a new block equal to c but with the value for keyID cleared on
// the given slot (its validity bit reset). The receiver is never mutated. When
// the slot was the only carrier of its column the column may become entirely
// empty; an empty column is dropped so a key that no longer appears on any slot
// costs nothing. Returns the (possibly unchanged) block and whether anything
// changed.
func (c *edgePropCols) del(keyID PropertyKeyID, slot int) (*edgePropCols, bool) {
	if c == nil || len(c.cols) == 0 {
		return c, false
	}
	// A delete may target either the canonical kind column or the date column
	// for the same key, and (rarely) a boxed-spill column. Clear the slot in
	// every column carrying keyID, since at most one of them actually has the
	// slot present.
	changed := false
	var out *edgePropCols
	for i := range c.cols {
		if c.cols[i].key != keyID {
			continue
		}
		if !c.cols[i].slotValid(slot) {
			continue
		}
		if out == nil {
			out = c.clone(c.length)
		}
		out.cols[i] = out.cols[i].cloneCol()
		out.cols[i].clearSlot(slot)
		out.cols[i] = out.cols[i].reshaped() // fill dropped: may demote to sparse
		changed = true
	}
	if !changed {
		return c, false
	}
	out.dropEmptyColumns()
	return out, true
}

// get returns the value recorded for keyID on the given slot and whether it is
// present. A null (validity bit clear) reports false.
func (c *edgePropCols) get(keyID PropertyKeyID, slot int) (PropertyValue, bool) {
	if c == nil {
		return PropertyValue{}, false
	}
	for i := range c.cols {
		if c.cols[i].key != keyID {
			continue
		}
		if v, ok := c.cols[i].slotValue(slot); ok {
			return v, true
		}
	}
	return PropertyValue{}, false
}

// forEachAt invokes fn once per (key, value) present on the given slot. The
// iteration order is the column order, which is unobservable (the public
// accessors return a map). fn is never called for an absent value.
func (c *edgePropCols) forEachAt(slot int, fn func(key PropertyKeyID, v PropertyValue)) {
	if c == nil {
		return
	}
	for i := range c.cols {
		if v, ok := c.cols[i].slotValue(slot); ok {
			fn(c.cols[i].key, v)
		}
	}
}

// keyPresentAt reports whether keyID is present on the given slot, reading only
// the validity bitmap (never the value). This is the IS NULL / IS NOT NULL
// popcount-style fast path the design calls for.
func (c *edgePropCols) keyPresentAt(keyID PropertyKeyID, slot int) bool {
	if c == nil {
		return false
	}
	for i := range c.cols {
		if c.cols[i].key == keyID && c.cols[i].slotValid(slot) {
			return true
		}
	}
	return false
}

// clone returns a shallow copy of the block whose cols SLICE is fresh (so the
// caller may replace an element) but whose column elements still alias the
// receiver's immutable backings until cloneCol is called on the one being
// mutated. length grows the clone to at least the requested slot count.
func (c *edgePropCols) clone(length int) *edgePropCols {
	out := &edgePropCols{length: length}
	if c != nil && c.length > length {
		out.length = c.length
	}
	if c == nil || len(c.cols) == 0 {
		return out
	}
	out.cols = make([]edgePropColumn, len(c.cols))
	copy(out.cols, c.cols)
	// If the block grew, every aliased column must be conceptually extended to
	// the new length. We defer the physical grow to setSlot/slotValid, which
	// bound by length; but a column's own length field must reflect the block
	// length so reads past the old end report absent. Re-stamp lengths.
	if out.length != c.length {
		for i := range out.cols {
			out.cols[i] = out.cols[i].grownTo(out.length)
		}
	}
	return out
}

// columnIndex returns the index of the column matching (key, kind), or -1.
func (c *edgePropCols) columnIndex(key PropertyKeyID, kind PropertyKind) int {
	for i := range c.cols {
		if c.cols[i].key == key && c.cols[i].kind == kind {
			return i
		}
	}
	return -1
}

// dropEmptyColumns removes any column with no present slot, in place on the
// freshly-cloned block.
func (c *edgePropCols) dropEmptyColumns() {
	w := 0
	for i := range c.cols {
		if c.cols[i].nonEmpty() {
			c.cols[w] = c.cols[i]
			w++
		}
	}
	c.cols = c.cols[:w]
}

// --- dense <-> sparse representation policy --------------------------------
//
// A column is stored dense (slot-indexed backing + optional validity bitmap) or
// sparse (COO: ascending idx + compacted backing). The choice is a pure bytes
// trade-off — both representations read identically (see [graph-theory] note in
// reshaped) — driven by the fill ratio P/length against the per-kind break-even.
//
// Break-even derivation (bytes per logical slot, 4-byte int32 COO indices):
//
//	dense+validity:  v + 1/8        (value cell + one validity bit)
//	sparse (COO):    (v + 4) * f    (value + index, only for present slots)
//
// where v is the per-slot value width in bytes and f = P/length the fill ratio.
// Sparse wins on bytes when (v+4)*f < v + 1/8, i.e. f < (v + 1/8)/(v + 4). This
// is [breakevenFill]. Hysteresis (see reshaped) keeps a column from thrashing
// when its fill drifts across the break-even.

// slotValueWidthBytes returns the per-slot value width in bytes used by the
// dense/sparse bytes model, by storage kind. Bytes/list use the boxed
// representation, whose per-slot cost is the PropertyValue header.
func slotValueWidthBytes(kind PropertyKind) float64 {
	switch kind {
	case PropInt64, PropFloat64:
		return 8
	case dateKind:
		return 4 // int32 epoch-day
	case PropBool:
		return 0.125 // one bit, packed
	case PropString:
		return 16 // string header (ptr + len); the body is shared either way
	default: // PropBytes, PropList, collision spill: boxed PropertyValue
		return 24
	}
}

// breakevenFill returns the fill ratio f below which the sparse (COO)
// representation costs fewer bytes than dense+validity for a column of the given
// kind: f = (v + 1/8)/(v + 4) with v = [slotValueWidthBytes]. For bit-packed
// bool (v = 0.125) this is ~0.06, so bool is effectively never sparse.
func breakevenFill(kind PropertyKind) float64 {
	v := slotValueWidthBytes(kind)
	return (v + 0.125) / (v + 4)
}

// reshapeBand is the hysteresis width applied below the break-even fill for the
// scalar kinds: a dense column demotes to sparse only when its fill drops at
// least this far below break-even, and a sparse column promotes to dense at the
// break-even. The gap prevents thrashing when a node's fill oscillates around
// the boundary. The string kind (v = 16) uses a tighter band — its break-even
// (~0.806) sits just above the dominant ~0.5 workload fill, so a wide band would
// leave a region paying up to ~16% extra dense bytes; see reshaped.
const (
	reshapeBand       = 0.10 // scalar kinds (v in [4,8])
	reshapeBandString = 0.05 // string kind (v = 16), tighter near a high break-even
)

// neverSparseWidth is the value-width ceiling at or below which a column is
// always kept dense: the 4-byte COO index dwarfs the value, so sparse never pays
// (bit-packed bool, v = 0.125, is the only kind in this regime today).
const neverSparseWidth = 1.0

// promoteThreshold / demoteThreshold return the fill ratios at which a column
// changes representation. A column promotes (sparse -> dense) at fill >=
// promoteThreshold; it demotes (dense -> sparse) at fill <= demoteThreshold.
// demoteThreshold is clamped at 0, so a kind whose (break-even - band) is
// non-positive never demotes (it is effectively always dense).
func promoteThreshold(kind PropertyKind) float64 {
	if slotValueWidthBytes(kind) <= neverSparseWidth {
		return 0 // promote at any fill: always dense
	}
	return breakevenFill(kind)
}

func demoteThreshold(kind PropertyKind) float64 {
	if slotValueWidthBytes(kind) <= neverSparseWidth {
		return -1 // demote never fires (fill is always >= 0)
	}
	band := reshapeBand
	if kind == PropString {
		band = reshapeBandString
	}
	d := breakevenFill(kind) - band
	if d < 0 {
		d = 0
	}
	return d
}

// reshaped returns col reshaped to the physical representation its CURRENT fill
// dictates under hysteresis, or col unchanged when no change is warranted. The
// receiver must already be cloned for writing (reshaped mutates in place and
// returns the receiver value). The decision is taken on the ACTUAL current
// physical type (not a predicted post-op fill): a dense column at fill <=
// [demoteThreshold] becomes sparse; a sparse column at fill >= [promoteThreshold]
// becomes dense; otherwise the representation is left as-is, which is always
// correct because the two representations are observationally identical
// (confirmed by the dense-differential property oracle).
//
// A length-0 or single-slot column is left dense: there is nothing to compress
// and the dense fast path the spike measured must never grow a COO sidecar.
func (col *edgePropColumn) reshaped() edgePropColumn {
	if col.length <= 1 {
		if col.sparse {
			return col.toDense()
		}
		return *col
	}
	fill := float64(col.popcountValid()) / float64(col.length)
	switch {
	case col.sparse && fill >= promoteThreshold(col.kind):
		return col.toDense()
	case !col.sparse && fill <= demoteThreshold(col.kind):
		return col.toSparse()
	default:
		return *col
	}
}

// toSparse converts a dense column to the COO representation in place and returns
// it. It walks the dense slots once, copying every present slot's value into a
// freshly-sized compacted backing and recording its index in idx (ascending by
// construction). The validity bitmap is dropped. Idempotent on an already-sparse
// column.
func (col *edgePropColumn) toSparse() edgePropColumn {
	if col.sparse {
		return *col
	}
	p := col.popcountValid()
	out := edgePropColumn{key: col.key, kind: col.kind, length: col.length, sparse: true}
	out.idx = make([]int32, 0, p)
	allocSparseBacking(&out, col.kind, p)
	for slot := 0; slot < col.length; slot++ {
		if !col.slotValid(slot) {
			continue
		}
		out.idx = append(out.idx, int32(slot))
		out.appendSparseValueFromDense(col, slot)
	}
	return out
}

// toDense converts a sparse column to the dense representation in place and
// returns it. It allocates a full-length backing, scatters each present value to
// its slot index, and materialises a validity bitmap unless every slot is
// present (in which case the column is fully dense and the bitmap stays nil).
// Idempotent on an already-dense column.
func (col *edgePropColumn) toDense() edgePropColumn {
	if !col.sparse {
		return *col
	}
	out := edgePropColumn{key: col.key, kind: col.kind, length: col.length}
	allocDenseBacking(&out, col.kind, col.length)
	full := len(col.idx) == col.length
	if !full {
		out.valid = make([]uint64, words(col.length))
	}
	for k, slot := range col.idx {
		s := int(slot)
		out.scatterDenseValueFromSparse(col, k, s)
		if !full {
			out.valid[s>>6] |= 1 << (uint(s) & 63)
		}
	}
	return out
}

// allocSparseBacking allocates the single typed backing slice the sparse
// representation uses, sized to the present count p. bool is never sparse, so it
// is unreachable here; we allocate a slot-indexed bit word defensively in case a
// future kind change routes through.
func allocSparseBacking(col *edgePropColumn, kind PropertyKind, p int) {
	switch kind {
	case PropInt64:
		col.i64 = make([]int64, 0, p)
	case PropFloat64:
		col.f64 = make([]float64, 0, p)
	case dateKind:
		col.days = make([]int32, 0, p)
	case PropString:
		col.str = make([]string, 0, p)
	case PropBool:
		// Unreachable: bool never goes sparse. A bit-packed bool has no compact
		// COO value array; fall back to a per-present-slot bit word.
		col.boolBits = make([]uint64, words(p))
	default:
		col.boxed = make([]PropertyValue, 0, p)
	}
}

// allocDenseBacking allocates the slot-indexed typed backing of length n.
func allocDenseBacking(col *edgePropColumn, kind PropertyKind, n int) {
	switch kind {
	case PropInt64:
		col.i64 = make([]int64, n)
	case PropFloat64:
		col.f64 = make([]float64, n)
	case PropBool:
		col.boolBits = make([]uint64, words(n))
	case dateKind:
		col.days = make([]int32, n)
	case PropString:
		col.str = make([]string, n)
	default:
		col.boxed = make([]PropertyValue, n)
	}
}

// appendSparseValueFromDense appends the value of dense slot `slot` of src to the
// receiver's compacted sparse backing (the receiver being built by toSparse).
func (col *edgePropColumn) appendSparseValueFromDense(src *edgePropColumn, slot int) {
	switch col.kind {
	case PropInt64:
		col.i64 = append(col.i64, src.i64[slot])
	case PropFloat64:
		col.f64 = append(col.f64, src.f64[slot])
	case dateKind:
		col.days = append(col.days, src.days[slot])
	case PropString:
		col.str = append(col.str, src.str[slot])
	default:
		col.boxed = append(col.boxed, src.boxed[slot])
	}
}

// scatterDenseValueFromSparse writes the k-th sparse value of src to dense slot s
// of the receiver (the receiver being built by toDense).
func (col *edgePropColumn) scatterDenseValueFromSparse(src *edgePropColumn, k, s int) {
	switch col.kind {
	case PropInt64:
		col.i64[s] = src.i64[k]
	case PropFloat64:
		col.f64[s] = src.f64[k]
	case dateKind:
		col.days[s] = src.days[k]
	case PropString:
		col.str[s] = src.str[k]
	default:
		col.boxed[s] = src.boxed[k]
	}
}

// sparsePos returns the position in idx of slot `slot` (so the typed backing at
// that position is the slot's value), and whether the slot is present. idx is
// ascending, so this is a binary search.
func (col *edgePropColumn) sparsePos(slot int) (int, bool) {
	lo, hi := 0, len(col.idx)
	target := int32(slot)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		switch {
		case col.idx[mid] < target:
			lo = mid + 1
		case col.idx[mid] > target:
			hi = mid
		default:
			return mid, true
		}
	}
	return lo, false // lo is the insertion point that keeps idx ascending
}

// --- column-level helpers --------------------------------------------------

// classify maps a PropertyValue to its storage kind. A date-shaped string (SOH
// Date tag + canonical text) is folded into the epoch-day date column; its
// int32 epoch-day is returned in days. For every other kind days is 0 and the
// returned kind equals v.Kind().
func classify(v PropertyValue) (kind PropertyKind, days int32, dateOK bool) {
	if v.kind == PropString {
		if s, ok := v.v.(string); ok {
			if d, ok := stringToEpochDay(s); ok {
				return dateKind, d, true
			}
		}
	}
	return v.kind, 0, false
}

// newColumn allocates a dense (validity-free) column of the given length for
// (key, kind). All value cells start zero; the caller sets the live slot via
// setSlot. Because the column is created dense, the validity bitmap is
// materialised lazily on the first absent slot (the first GrowSlot or the first
// del), so a dense column carries no bitmap.
//
// A freshly-created column of length L with a single live slot is conceptually
// "L-1 absent slots", so it is NOT dense: newColumn therefore initialises the
// validity bitmap with only the live slot set. The exception is L==1 with the
// slot live, which IS dense and needs no bitmap; setSlot handles that by leaving
// valid nil and the single bit implicitly set.
func newColumn(key PropertyKeyID, kind PropertyKind, length int) edgePropColumn {
	// Build-sparse-by-default for a multi-slot column whose single live slot
	// leaves the fill at or below the demote threshold: a fresh column with one
	// value has fill 1/length, so for any kind whose demote threshold * length >=
	// 1 the column starts sparse and never allocates the length-sized dense
	// backing on a high-degree, low-fill node — the exact regression this guards.
	// A length-1 column (fill 1.0) and any kind that is always dense (bool) start
	// dense, preserving the dense fast path the spike measured.
	if length > 1 && slotValueWidthBytes(kind) > neverSparseWidth &&
		1.0/float64(length) <= demoteThreshold(kind) {
		col := edgePropColumn{key: key, kind: kind, length: length, sparse: true}
		col.idx = make([]int32, 0, 1)
		allocSparseBacking(&col, kind, 1)
		return col
	}
	col := edgePropColumn{key: key, kind: kind, length: length}
	switch kind {
	case PropInt64:
		col.i64 = make([]int64, length)
	case PropFloat64:
		col.f64 = make([]float64, length)
	case PropBool:
		col.boolBits = make([]uint64, words(length))
	case dateKind:
		col.days = make([]int32, length)
	case PropString:
		col.str = make([]string, length)
	default: // PropBytes, PropList, and any collision spill.
		col.boxed = make([]PropertyValue, length)
	}
	// A multi-slot DENSE column created for a single live slot starts mostly
	// absent: allocate the validity bitmap so the other slots read null. A
	// length-1 column is dense and keeps valid nil.
	if length > 1 {
		col.valid = make([]uint64, words(length))
	}
	return col
}

// cloneCol returns a deep-enough copy of the column for copy-on-write mutation:
// the typed backing slice in use AND the validity bitmap are copied so the
// caller may write into them without disturbing the shared immutable original.
// The pointer receiver only reads *col; every backing slice in the returned
// column is a fresh copy, so the result aliases none of the receiver's storage.
func (col *edgePropColumn) cloneCol() edgePropColumn {
	out := *col
	switch col.kind {
	case PropInt64:
		out.i64 = cloneI64(col.i64)
	case PropFloat64:
		out.f64 = cloneF64(col.f64)
	case PropBool:
		out.boolBits = cloneU64(col.boolBits)
	case dateKind:
		out.days = cloneI32(col.days)
	case PropString:
		out.str = cloneStr(col.str)
	default:
		out.boxed = cloneBoxed(col.boxed)
	}
	out.valid = cloneU64(col.valid)
	out.idx = cloneI32Idx(col.idx)
	return out
}

// hasSlack reports whether the column's in-use backing slices have spare
// capacity that Compact should reclaim. Only the sparse representation (built by
// amortised-growth inserts) can carry slack; dense backings are exactly
// length-sized. idx and the value backing are checked together.
func (col *edgePropColumn) hasSlack() bool {
	if !col.sparse {
		return false
	}
	if cap(col.idx) > len(col.idx) {
		return true
	}
	switch col.kind {
	case PropInt64:
		return cap(col.i64) > len(col.i64)
	case PropFloat64:
		return cap(col.f64) > len(col.f64)
	case dateKind:
		return cap(col.days) > len(col.days)
	case PropString:
		return cap(col.str) > len(col.str)
	default:
		return cap(col.boxed) > len(col.boxed)
	}
}

// compactBacking returns a copy of the sparse column with idx and the value
// backing re-allocated at exact length (cap == len), reclaiming amortised-growth
// slack. The contents are unchanged, so the result reads identically.
func (col *edgePropColumn) compactBacking() edgePropColumn {
	out := *col
	out.idx = exactI32(col.idx)
	switch col.kind {
	case PropInt64:
		out.i64 = exactI64(col.i64)
	case PropFloat64:
		out.f64 = exactF64(col.f64)
	case dateKind:
		out.days = exactI32Vals(col.days)
	case PropString:
		out.str = exactStr(col.str)
	default:
		out.boxed = exactBoxed(col.boxed)
	}
	return out
}

// grown returns a copy of the column extended by one slot at index oldLen, with
// the new slot ABSENT.
//
// SPARSE: appending an absent trailing slot is a pure no-op on idx/val — the new
// top slot is simply not in idx, so it reads null — and only the stored length
// increments. The result is then re-evaluated: appending an absent slot lowers
// the fill, which can only push toward sparse, so no promotion is possible, but
// reshaped is called for uniformity (it is a no-op here).
//
// DENSE: the value backing grows by one and the validity bitmap is always
// present in the result (a previously-fully-dense column materialises one here,
// with every prior bit set and the new bit clear), which is what makes the
// freshly-appended slot read null. The grown dense column is then re-evaluated:
// the lower fill may demote it to sparse.
func (col *edgePropColumn) grown(oldLen int) edgePropColumn {
	newLen := oldLen + 1
	if col.sparse {
		out := col.cloneCol()
		out.length = newLen
		// idx/val unchanged: the new slot at oldLen is absent (not in idx).
		return out.reshaped()
	}
	out := edgePropColumn{key: col.key, kind: col.kind, length: newLen}
	switch col.kind {
	case PropInt64:
		out.i64 = growI64(col.i64, newLen)
	case PropFloat64:
		out.f64 = growF64(col.f64, newLen)
	case PropBool:
		out.boolBits = growU64(col.boolBits, words(newLen))
	case dateKind:
		out.days = growI32(col.days, newLen)
	case PropString:
		out.str = growStr(col.str, newLen)
	default:
		out.boxed = growBoxed(col.boxed, newLen)
	}
	out.valid = col.materialiseValidity(oldLen, newLen)
	return out.reshaped()
}

// grownTo returns a copy of the column whose length is exactly newLen (>=
// current length), with any added trailing slots ABSENT. Used when a block is
// cloned at a larger length than the column currently spans.
func (col *edgePropColumn) grownTo(newLen int) edgePropColumn {
	if newLen <= col.length {
		return *col
	}
	if col.sparse {
		// Added slots [col.length, newLen) are absent (not in idx); only length
		// changes. The lower fill may demote, never promote.
		out := col.cloneCol()
		out.length = newLen
		return out.reshaped()
	}
	out := edgePropColumn{key: col.key, kind: col.kind, length: newLen}
	switch col.kind {
	case PropInt64:
		out.i64 = growI64(col.i64, newLen)
	case PropFloat64:
		out.f64 = growF64(col.f64, newLen)
	case PropBool:
		out.boolBits = growU64(col.boolBits, words(newLen))
	case dateKind:
		out.days = growI32(col.days, newLen)
	case PropString:
		out.str = growStr(col.str, newLen)
	default:
		out.boxed = growBoxed(col.boxed, newLen)
	}
	// Every added slot in [col.length, newLen) is absent. Materialise a bitmap
	// that preserves the existing presence and clears the added slots.
	v := make([]uint64, words(newLen))
	if col.valid != nil {
		copy(v, col.valid)
	} else {
		// Was dense: every existing slot was present.
		for i := 0; i < col.length; i++ {
			v[i>>6] |= 1 << (uint(i) & 63)
		}
	}
	// Added slots stay clear (absent) — no action needed.
	out.valid = v
	return out.reshaped()
}

// materialiseValidity returns the validity bitmap for a column grown from oldLen
// to newLen (= oldLen+1). The new slot at oldLen is clear; the prior oldLen
// slots keep their presence: when the source was dense (valid == nil) every
// prior slot was present and is set here; otherwise the source bitmap is copied.
func (col *edgePropColumn) materialiseValidity(oldLen, newLen int) []uint64 {
	v := make([]uint64, words(newLen))
	if col.valid != nil {
		copy(v, col.valid)
		// Defensive: clear any stale high bit at/after oldLen so the new slot is
		// unconditionally absent even if the source bitmap had don't-care bits.
		clearBitsFrom(v, oldLen)
		return v
	}
	// Dense source: prior [0,oldLen) slots are all present.
	for i := 0; i < oldLen; i++ {
		v[i>>6] |= 1 << (uint(i) & 63)
	}
	return v
}

// --- fused build fast path (sparse coordinate-list tail append) ------------

// newSparseSingleSlot builds a column of the given length whose ONLY present
// slot is `slot`, carrying value v (days is its pre-classified epoch-day for the
// date kind). It is always the SPARSE representation: a single (slot) entry in
// idx and one value in the compacted backing, no validity bitmap. This is the
// shape the fused build path starts every property column in, so the next fused
// append is an O(1) coordinate-list tail push rather than a dense
// reallocate-and-copy (see [edgePropCols.GrowSlotWithValue]).
//
// bit-packed bool has no compact COO value array, so a bool single-slot column
// is built dense (length-indexed bits + a validity bitmap unless length 1); this
// is unreachable from the date-property build path and exists only for
// robustness if a future caller fuses a bool.
func newSparseSingleSlot(key PropertyKeyID, kind PropertyKind, length, slot int, v PropertyValue, days int32) edgePropColumn {
	if kind == PropBool {
		col := newColumn(key, kind, length)
		col.setSlot(slot, v, days)
		return col
	}
	col := edgePropColumn{key: key, kind: kind, length: length, sparse: true}
	col.idx = make([]int32, 0, 1)
	allocSparseBacking(&col, kind, 1)
	col.tailAppendSparse(int32(slot), v, days)
	return col
}

// grownWithValue returns a copy of the column grown by one slot at oldLen whose
// new slot at oldLen is PRESENT, carrying value v. It is the column-level half of
// the fused build fast path and is the value-carrying analogue of
// [edgePropColumn.grown].
//
// The new slot index oldLen is strictly greater than every present slot (the
// column spans the entry, whose neighbour count was oldLen before the append), so
// the append is a tail push that preserves the strictly-ascending idx invariant:
//
//   - SPARSE: append oldLen to idx and the value to the compacted backing, both
//     O(1) amortised. The append SHARES the receiver's backing arrays rather than
//     copying them, exactly as the adjacency neighbour fast path does: a
//     lock-free reader holding the prior column reads only [0:len(idx)], which is
//     never mutated; the append either writes at index len(idx) — a slot no
//     current reader observes and that becomes visible only via the freshly
//     published longer header — or, when the backing is full, reallocates and
//     copies geometrically (so total copy work is O(d), not O(d) per append).
//     Sharing is what keeps a fused build O(d) per source; cloning the whole
//     backing on each append would reintroduce O(d²).
//   - DENSE: convert the column to sparse ONCE (toSparse is O(P)), then tail
//     push. A pure fused build never hits this branch because the column starts
//     and stays sparse; it only fires when a fused append follows an earlier
//     dense general-path mutation on the same column, so the O(P) conversion is a
//     one-off, leaving subsequent appends O(1) — amortised O(1).
//
// reshaped() is deliberately NOT called: representation re-evaluation is deferred
// to Compact (see [edgePropCols.GrowSlotWithValue] rule 2).
//
// Concurrency: adjlist holds the source's shard write lock across the whole
// append, so two fused appends never race on the shared backing; the published
// header is installed via a single atomic store. This is the same copy-on-write
// contract the neighbour/handle/label columns rely on.
func (col *edgePropColumn) grownWithValue(oldLen int, v PropertyValue, days int32) edgePropColumn {
	if !col.sparse {
		s := col.toSparse() // fresh, exactly-sized backing; safe to extend in place
		s.length = oldLen + 1
		s.tailAppendSparse(int32(oldLen), v, days)
		return s
	}
	// Share the receiver's immutable backings and extend at the tail. out aliases
	// col's idx/value slices; appending to out's (aliased) headers writes only at
	// the tail — a slot no reader bounded by the old length observes — or
	// reallocates geometrically, so the prior immutable column's [0:len) prefix is
	// never disturbed. This is the same copy-on-write-safe extend the adjacency
	// neighbour array uses.
	out := *col
	out.length = oldLen + 1
	out.tailAppendSparse(int32(oldLen), v, days)
	return out
}

// grownAbsentShared returns a copy of the column grown by one slot at oldLen
// whose new slot is ABSENT, in O(1) amortised — the absent-grow counterpart of
// [edgePropColumn.grownWithValue], used for the NON-target columns on a fused
// append so a node carrying several property keys stays O(degree) per source.
//
//   - SPARSE: the new slot oldLen is simply not a member of idx, so presence is
//     already correct; only length changes. The idx and value backings are SHARED
//     with the receiver (immutable, untouched) — no copy. The result reads
//     identically; reshape is deferred to Compact. O(1).
//   - DENSE: convert to sparse ONCE (O(P)) then bump the length. A pure fused
//     build keeps every column sparse, so this fires at most once per column if a
//     prior dense general-path mutation left it dense, leaving subsequent
//     absent-grows O(1) — amortised O(1). (The dense in-place absent-grow would
//     need an O(length/64) bitmap copy each call, which is quadratic over a build;
//     the one-off sparse conversion avoids that.)
//
// The shared SPARSE extend is copy-on-write-safe under the same rule as
// [edgePropColumn.grownWithValue]: the receiver's [0:len) prefix is never mutated
// and adjlist holds the shard lock across the publish.
func (col *edgePropColumn) grownAbsentShared(oldLen int) edgePropColumn {
	if !col.sparse {
		s := col.toSparse()
		s.length = oldLen + 1
		return s
	}
	out := *col // share idx + value backing; only length changes
	out.length = oldLen + 1
	return out
}

// tailAppendSparse pushes one (slotIdx, value) entry onto the tail of the
// receiver's sparse (COO) idx and value backings, in O(1) amortised. slotIdx must
// be strictly greater than every index already in idx (the caller guarantees this
// — it is the new last slot of a growing column), so the append keeps idx
// strictly ascending without a shift. days carries the pre-classified epoch-day
// for the date column and is ignored for other kinds; bit-packed bool never
// reaches the sparse path.
//
// The receiver's idx/value slices may ALIAS a prior immutable column's backings
// (the copy-on-write-safe shared-extend in [edgePropColumn.grownWithValue]).
// append assigns back to the same headers and writes only at the tail or
// reallocates, so a concurrent reader bounded by the prior column's length never
// observes the new cell — the prior [0:len) prefix is untouched.
func (col *edgePropColumn) tailAppendSparse(slotIdx int32, v PropertyValue, days int32) {
	col.idx = append(col.idx, slotIdx)
	switch col.kind {
	case PropInt64:
		i, _ := v.Int64()
		col.i64 = append(col.i64, i)
	case PropFloat64:
		f, _ := v.Float64()
		col.f64 = append(col.f64, f)
	case dateKind:
		col.days = append(col.days, days)
	case PropString:
		s, _ := v.String()
		col.str = append(col.str, s)
	default:
		col.boxed = append(col.boxed, v)
	}
}

// compacted returns a copy of the column with the slot at idx excised, keeping
// it aligned 1:1 with the neighbour array that adjlist compacts by the SAME
// stable shift-down (see adjlist.compactEntry). It dispatches on the ACTUAL
// current physical representation — never a predicted post-op fill — so the
// transform always matches the storage it operates on (the dispatch-bug hazard
// the design review flagged).
//
// DENSE: result slots [0,idx) equal the receiver's, result slots [idx,n-1) equal
// the receiver's [idx+1,n). The value backing and the validity bitmap are
// spliced by the identical index transform.
//
// SPARSE (COO): the transform is NOT the dense copy-down splice. It is
// delete-if-present-then-decrement: the entry whose stored index == idx (if any)
// is dropped, and every stored index strictly greater than idx is decremented by
// one. Stored indices below idx are unchanged. This is the unique transform that
// keeps idx aligned with the shifted neighbours while preserving the
// strict-ascending no-duplicate invariant.
//
// The compacted column keeps the SAME physical representation it had (correct
// because the two representations read identically); re-evaluation is deferred to
// the next set/grow, which is a pure bytes decision.
func (col *edgePropColumn) compacted(idx int) edgePropColumn {
	if col.sparse {
		return col.compactedSparse(idx)
	}
	n := col.length
	out := edgePropColumn{key: col.key, kind: col.kind, length: n - 1}
	switch col.kind {
	case PropInt64:
		out.i64 = spliceI64(col.i64, idx)
	case PropFloat64:
		out.f64 = spliceF64(col.f64, idx)
	case PropBool:
		out.boolBits = spliceBits(col.boolBits, idx, n)
	case dateKind:
		out.days = spliceI32(col.days, idx)
	case PropString:
		out.str = spliceStr(col.str, idx)
	default:
		out.boxed = spliceBoxed(col.boxed, idx)
	}
	if col.valid != nil {
		out.valid = spliceBits(col.valid, idx, n)
	}
	return out
}

// compactedSparse applies the COO compaction transform for slot idx:
// delete-if-present, then decrement every stored index > idx. Both idx and the
// typed value backing are rebuilt in lockstep; the value of a dropped slot is
// elided. The result stays strictly ascending: dropping one element and
// decrementing a strictly-greater suffix cannot collide (the straddle a<idx<b
// gives a < b-1 on integers; the only way to manufacture {idx,idx} is to
// decrement idx+1 WITHOUT dropping idx — which this never does).
func (col *edgePropColumn) compactedSparse(idx int) edgePropColumn {
	out := edgePropColumn{key: col.key, kind: col.kind, length: col.length - 1, sparse: true}
	// Worst case (idx absent) keeps every present entry.
	out.idx = make([]int32, 0, len(col.idx))
	allocSparseBacking(&out, col.kind, len(col.idx))
	target := int32(idx)
	for k, slot := range col.idx {
		switch {
		case slot == target:
			continue // drop the excised slot's entry
		case slot > target:
			out.idx = append(out.idx, slot-1) // shift down
		default:
			out.idx = append(out.idx, slot) // below idx: unchanged
		}
		out.appendSparseValueFromSparse(col, k)
	}
	return out
}

// appendSparseValueFromSparse appends src's k-th sparse value to the receiver's
// sparse backing (used by compactedSparse, which copies surviving entries in
// order).
func (col *edgePropColumn) appendSparseValueFromSparse(src *edgePropColumn, k int) {
	switch col.kind {
	case PropInt64:
		col.i64 = append(col.i64, src.i64[k])
	case PropFloat64:
		col.f64 = append(col.f64, src.f64[k])
	case dateKind:
		col.days = append(col.days, src.days[k])
	case PropString:
		col.str = append(col.str, src.str[k])
	default:
		col.boxed = append(col.boxed, src.boxed[k])
	}
}

// setSlot records v on the slot in place (the column must already be cloned for
// writing). days carries the pre-classified epoch-day for the date column; it is
// ignored for other kinds.
//
// DENSE: writes the value cell at index slot and marks the validity bit.
//
// SPARSE: locates slot in idx (binary search). If present, overwrites the value
// at that position; if absent, inserts the index (keeping idx ascending) and the
// value at the matching position. Bit-packed bool never reaches the sparse path.
func (col *edgePropColumn) setSlot(slot int, v PropertyValue, days int32) {
	if col.sparse {
		col.setSlotSparse(slot, v, days)
		return
	}
	switch col.kind {
	case PropInt64:
		i, _ := v.Int64()
		col.i64[slot] = i
	case PropFloat64:
		f, _ := v.Float64()
		col.f64[slot] = f
	case PropBool:
		b, _ := v.Bool()
		if b {
			col.boolBits[slot>>6] |= 1 << (uint(slot) & 63)
		} else {
			col.boolBits[slot>>6] &^= 1 << (uint(slot) & 63)
		}
	case dateKind:
		col.days[slot] = days
	case PropString:
		s, _ := v.String()
		col.str[slot] = s
	default:
		col.boxed[slot] = v
	}
	col.markValid(slot)
}

// setSlotSparse records v on the slot in the COO representation, inserting a new
// (idx, value) entry in ascending order or overwriting an existing one.
func (col *edgePropColumn) setSlotSparse(slot int, v PropertyValue, days int32) {
	pos, present := col.sparsePos(slot)
	if present {
		col.overwriteSparseValue(pos, v, days)
		return
	}
	col.idx = insertI32(col.idx, pos, int32(slot))
	col.insertSparseValue(pos, v, days)
}

// overwriteSparseValue replaces the value at position pos of the sparse backing.
func (col *edgePropColumn) overwriteSparseValue(pos int, v PropertyValue, days int32) {
	switch col.kind {
	case PropInt64:
		i, _ := v.Int64()
		col.i64[pos] = i
	case PropFloat64:
		f, _ := v.Float64()
		col.f64[pos] = f
	case dateKind:
		col.days[pos] = days
	case PropString:
		s, _ := v.String()
		col.str[pos] = s
	default:
		col.boxed[pos] = v
	}
}

// insertSparseValue inserts v at position pos of the sparse backing, shifting the
// tail up by one to stay aligned with idx.
func (col *edgePropColumn) insertSparseValue(pos int, v PropertyValue, days int32) {
	switch col.kind {
	case PropInt64:
		i, _ := v.Int64()
		col.i64 = insertI64(col.i64, pos, i)
	case PropFloat64:
		f, _ := v.Float64()
		col.f64 = insertF64(col.f64, pos, f)
	case dateKind:
		col.days = insertI32(col.days, pos, days)
	case PropString:
		s, _ := v.String()
		col.str = insertStr(col.str, pos, s)
	default:
		col.boxed = insertBoxed(col.boxed, pos, v)
	}
}

// clearSlot makes the slot absent in place (the column must already be cloned for
// writing).
//
// DENSE: resets the validity bit (materialising the bitmap first if the column
// was fully dense) and drops any string/boxed reference so the dead value can be
// GC'd.
//
// SPARSE: removes the (idx, value) entry for the slot, if present.
func (col *edgePropColumn) clearSlot(slot int) {
	if col.sparse {
		col.clearSlotSparse(slot)
		return
	}
	if col.valid == nil {
		// Was dense (every slot present). Materialise the bitmap with every slot
		// set, then clear the target.
		col.valid = make([]uint64, words(col.length))
		for i := 0; i < col.length; i++ {
			col.valid[i>>6] |= 1 << (uint(i) & 63)
		}
	}
	col.valid[slot>>6] &^= 1 << (uint(slot) & 63)
	// Drop the slot's string reference so it can be GC'd; the value is now
	// don't-care and keeping a dead string pointer would defeat the memory win.
	if col.kind == PropString {
		col.str[slot] = ""
	} else if col.boxed != nil {
		col.boxed[slot] = PropertyValue{}
	}
}

// clearSlotSparse removes the (idx, value) entry for slot from the COO
// representation, if present. The value is elided entirely (no dead reference to
// retain), so a string/boxed column needs no separate GC-hygiene step.
func (col *edgePropColumn) clearSlotSparse(slot int) {
	pos, present := col.sparsePos(slot)
	if !present {
		return
	}
	col.idx = removeI32(col.idx, pos)
	switch col.kind {
	case PropInt64:
		col.i64 = removeI64(col.i64, pos)
	case PropFloat64:
		col.f64 = removeF64(col.f64, pos)
	case dateKind:
		col.days = removeI32(col.days, pos)
	case PropString:
		col.str = removeStr(col.str, pos)
	default:
		col.boxed = removeBoxed(col.boxed, pos)
	}
}

// markValid sets the slot's validity bit on a DENSE column.
//
// A nil validity bitmap means the column is FULLY dense — every slot present —
// in two cases: a length-1 single-slot column, and a multi-slot column promoted
// from sparse at 100 % fill (see toDense). In BOTH cases setting a slot keeps the
// column fully dense, so the bitmap stays nil. This is the corrected invariant:
// a nil bitmap is the fully-present sentinel, NOT "single slot only". Allocating
// a zeroed bitmap here and setting one bit would WRONGLY drop the other present
// slots' presence — the bug that arose once promotion could leave a multi-slot
// column with nil validity.
//
// A non-nil bitmap means some slot is (or was) absent; the target bit is set
// directly. setSlot only ever marks a slot it has just written, so this records
// the presence of a previously-absent slot or re-affirms a present one.
func (col *edgePropColumn) markValid(slot int) {
	if col.valid == nil {
		// Fully dense (length-1 or 100 %-fill promoted): stays fully dense.
		return
	}
	col.valid[slot>>6] |= 1 << (uint(slot) & 63)
}

// slotValid reports whether the slot carries a value. Bounds-checked: a slot
// past the column length is absent. DENSE: reads the validity bit (or true when
// the bitmap is omitted). SPARSE: tests membership in idx.
func (col *edgePropColumn) slotValid(slot int) bool {
	if slot < 0 || slot >= col.length {
		return false
	}
	if col.sparse {
		_, ok := col.sparsePos(slot)
		return ok
	}
	if col.valid == nil {
		return true // dense: every in-range slot is present
	}
	return col.valid[slot>>6]&(1<<(uint(slot)&63)) != 0
}

// backingIndex returns the index into the typed value backing for the slot and
// whether the slot is present. DENSE: the index is the slot itself (or absent
// when its validity bit is clear). SPARSE: the index is the slot's position in
// idx (or absent when the slot is not in idx). Presence reuses slotValid, so the
// bounds and bitmap reasoning live in exactly one place.
func (col *edgePropColumn) backingIndex(slot int) (int, bool) {
	if !col.slotValid(slot) {
		return 0, false
	}
	if col.sparse {
		pos, _ := col.sparsePos(slot) // present (slotValid true) ⇒ found
		return pos, true
	}
	return slot, true
}

// slotValue returns the value on the slot and whether it is present. DENSE
// indexes the backing by slot; SPARSE indexes by the slot's position in idx.
func (col *edgePropColumn) slotValue(slot int) (PropertyValue, bool) {
	i, ok := col.backingIndex(slot)
	if !ok {
		return PropertyValue{}, false
	}
	switch col.kind {
	case PropInt64:
		return Int64Value(col.i64[i]), true
	case PropFloat64:
		return Float64Value(col.f64[i]), true
	case PropBool:
		// bool is never sparse, so i == slot here.
		return BoolValue(col.boolBits[i>>6]&(1<<(uint(i)&63)) != 0), true
	case dateKind:
		return StringValue(epochDayToString(col.days[i])), true
	case PropString:
		return StringValue(col.str[i]), true
	default:
		return col.boxed[i], true
	}
}

// nonEmpty reports whether any slot of the column is present.
func (col *edgePropColumn) nonEmpty() bool {
	if col.length == 0 {
		return false
	}
	if col.sparse {
		return len(col.idx) > 0
	}
	if col.valid == nil {
		return true // dense and length>0
	}
	for _, w := range col.valid {
		if w != 0 {
			return true
		}
	}
	return false
}

// --- bit / slice utilities -------------------------------------------------

// words returns the number of uint64 words needed to hold n bits.
func words(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + 63) >> 6
}

// clearBitsFrom clears every bit at index >= from in the packed bitmap v.
func clearBitsFrom(v []uint64, from int) {
	if from < 0 {
		from = 0
	}
	w := from >> 6
	if w >= len(v) {
		return
	}
	// Clear the partial first word's high bits at/after `from`.
	v[w] &= (1 << (uint(from) & 63)) - 1
	for i := w + 1; i < len(v); i++ {
		v[i] = 0
	}
}

// spliceBits removes the bit at index idx from a packed little-endian bitmap of
// n bits, returning a fresh bitmap of n-1 bits. Bits [0,idx) are preserved; bits
// (idx,n) move down by one position. The implementation copies bit-by-bit for
// the moved tail (no word-spanning shift), which is simple and correct; the high
// bits past n-1 in the result are guaranteed zero. Returns nil when the result
// has zero bits (n==1).
func spliceBits(src []uint64, idx, n int) []uint64 {
	if n-1 <= 0 {
		return nil
	}
	out := make([]uint64, words(n-1))
	// Preserve bits [0, idx).
	for i := 0; i < idx; i++ {
		if src[i>>6]&(1<<(uint(i)&63)) != 0 {
			out[i>>6] |= 1 << (uint(i) & 63)
		}
	}
	// Move bits (idx, n) down to [idx, n-1).
	for i := idx + 1; i < n; i++ {
		if src[i>>6]&(1<<(uint(i)&63)) != 0 {
			j := i - 1
			out[j>>6] |= 1 << (uint(j) & 63)
		}
	}
	return out
}

// popcountValid returns the number of present slots recorded in a column,
// reading only the validity bitmap (or the length for a dense column). It is the
// popcount the IS NOT NULL aggregate path can use to count present values
// without touching the value cells.
//
// This is the storage-layer primitive for the deferred IS NOT NULL engine fast
// path (sprint 222 #1638; the engine-integration half remains open). The Cypher
// relationship read path currently materialises the whole property map in
// buildRelationshipValueFromRow, so a presence-only / lazy read path must be
// added before this primitive can be wired in — that work is out of scope here.
func (col *edgePropColumn) popcountValid() int {
	if col.sparse {
		return len(col.idx) // COO: one stored entry per present slot
	}
	if col.valid == nil {
		return col.length // dense, fully present
	}
	total := 0
	for _, w := range col.valid {
		total += bits.OnesCount64(w)
	}
	return total
}

func cloneI64(s []int64) []int64 {
	if s == nil {
		return nil
	}
	out := make([]int64, len(s))
	copy(out, s)
	return out
}

func cloneF64(s []float64) []float64 {
	if s == nil {
		return nil
	}
	out := make([]float64, len(s))
	copy(out, s)
	return out
}

func cloneI32(s []int32) []int32 {
	if s == nil {
		return nil
	}
	out := make([]int32, len(s))
	copy(out, s)
	return out
}

func cloneStr(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneU64(s []uint64) []uint64 {
	if s == nil {
		return nil
	}
	out := make([]uint64, len(s))
	copy(out, s)
	return out
}

func cloneBoxed(s []PropertyValue) []PropertyValue {
	if s == nil {
		return nil
	}
	out := make([]PropertyValue, len(s))
	copy(out, s)
	return out
}

func growI64(s []int64, n int) []int64     { out := make([]int64, n); copy(out, s); return out }
func growF64(s []float64, n int) []float64 { out := make([]float64, n); copy(out, s); return out }
func growI32(s []int32, n int) []int32     { out := make([]int32, n); copy(out, s); return out }
func growStr(s []string, n int) []string   { out := make([]string, n); copy(out, s); return out }
func growU64(s []uint64, n int) []uint64   { out := make([]uint64, n); copy(out, s); return out }

func growBoxed(s []PropertyValue, n int) []PropertyValue {
	out := make([]PropertyValue, n)
	copy(out, s)
	return out
}

func spliceI64(s []int64, idx int) []int64 {
	out := make([]int64, len(s)-1)
	copy(out, s[:idx])
	copy(out[idx:], s[idx+1:])
	return out
}

func spliceF64(s []float64, idx int) []float64 {
	out := make([]float64, len(s)-1)
	copy(out, s[:idx])
	copy(out[idx:], s[idx+1:])
	return out
}

func spliceI32(s []int32, idx int) []int32 {
	out := make([]int32, len(s)-1)
	copy(out, s[:idx])
	copy(out[idx:], s[idx+1:])
	return out
}

func spliceStr(s []string, idx int) []string {
	out := make([]string, len(s)-1)
	copy(out, s[:idx])
	copy(out[idx:], s[idx+1:])
	return out
}

func spliceBoxed(s []PropertyValue, idx int) []PropertyValue {
	out := make([]PropertyValue, len(s)-1)
	copy(out, s[:idx])
	copy(out[idx:], s[idx+1:])
	return out
}

// --- sparse (COO) backing edits (insert/remove at a position) --------------
//
// These edit the COO backing slices in place on an already-cloned column. insert
// grows by one at pos (shifting the tail up); remove shrinks by one at pos
// (shifting the tail down). They keep the value backing aligned with idx.

func insertI32(s []int32, pos int, v int32) []int32 {
	s = append(s, 0)
	copy(s[pos+1:], s[pos:])
	s[pos] = v
	return s
}

func insertI64(s []int64, pos int, v int64) []int64 {
	s = append(s, 0)
	copy(s[pos+1:], s[pos:])
	s[pos] = v
	return s
}

func insertF64(s []float64, pos int, v float64) []float64 {
	s = append(s, 0)
	copy(s[pos+1:], s[pos:])
	s[pos] = v
	return s
}

func insertStr(s []string, pos int, v string) []string {
	s = append(s, "")
	copy(s[pos+1:], s[pos:])
	s[pos] = v
	return s
}

func insertBoxed(s []PropertyValue, pos int, v PropertyValue) []PropertyValue {
	s = append(s, PropertyValue{})
	copy(s[pos+1:], s[pos:])
	s[pos] = v
	return s
}

func removeI32(s []int32, pos int) []int32 {
	copy(s[pos:], s[pos+1:])
	return s[:len(s)-1]
}

func removeI64(s []int64, pos int) []int64 {
	copy(s[pos:], s[pos+1:])
	return s[:len(s)-1]
}

func removeF64(s []float64, pos int) []float64 {
	copy(s[pos:], s[pos+1:])
	return s[:len(s)-1]
}

func removeStr(s []string, pos int) []string {
	copy(s[pos:], s[pos+1:])
	s[len(s)-1] = "" // release the dropped string reference for GC
	return s[:len(s)-1]
}

func removeBoxed(s []PropertyValue, pos int) []PropertyValue {
	copy(s[pos:], s[pos+1:])
	s[len(s)-1] = PropertyValue{} // release the dropped value for GC
	return s[:len(s)-1]
}

// cloneI32Idx deep-copies the sparse index slice (nil stays nil so a dense column
// keeps idx nil).
func cloneI32Idx(s []int32) []int32 {
	if s == nil {
		return nil
	}
	out := make([]int32, len(s))
	copy(out, s)
	return out
}

// exact* return a copy of the slice sized exactly to its length (cap == len),
// reclaiming amortised-growth slack. nil stays nil. len==cap returns the slice
// unchanged (no allocation when there is nothing to reclaim).

func exactI32(s []int32) []int32 {
	if s == nil || cap(s) == len(s) {
		return s
	}
	out := make([]int32, len(s))
	copy(out, s)
	return out
}

func exactI32Vals(s []int32) []int32 { return exactI32(s) }

func exactI64(s []int64) []int64 {
	if s == nil || cap(s) == len(s) {
		return s
	}
	out := make([]int64, len(s))
	copy(out, s)
	return out
}

func exactF64(s []float64) []float64 {
	if s == nil || cap(s) == len(s) {
		return s
	}
	out := make([]float64, len(s))
	copy(out, s)
	return out
}

func exactStr(s []string) []string {
	if s == nil || cap(s) == len(s) {
		return s
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func exactBoxed(s []PropertyValue) []PropertyValue {
	if s == nil || cap(s) == len(s) {
		return s
	}
	out := make([]PropertyValue, len(s))
	copy(out, s)
	return out
}

// --- date <-> epoch-day codec ----------------------------------------------
//
// The date column stores the canonical openCypher Date as an int32 count of
// days since the Unix epoch (1970-01-01). The on-the-wire form a Cypher write
// delivers is a PropString whose first byte is [epochDayTag] (SOH) followed by
// the ISO-8601 YYYY-MM-DD text. stringToEpochDay parses that shape; an
// arbitrary string (or a tagged non-date temporal) is rejected so only true
// dates fold into the column. epochDayToString reconstitutes the identical
// tagged string on read so the cypher read-back round-trips it to a native Date.

const (
	// minEpochDay / maxEpochDay bound the int32 epoch-day so a malformed or
	// out-of-range date never folds into the column (it stays a boxed string).
	minEpochDay = math.MinInt32
	maxEpochDay = math.MaxInt32
)

// stringToEpochDay recognises an SOH-tagged canonical Date string and returns
// its int32 epoch-day. Returns ok=false for any string that is not a tagged
// date in canonical YYYY-MM-DD form, or whose epoch-day would overflow int32.
func stringToEpochDay(s string) (int32, bool) {
	if len(s) < 2 || s[0] != epochDayTag {
		return 0, false
	}
	body := s[1:]
	y, mo, d, ok := parseCanonicalDate(body)
	if !ok {
		return 0, false
	}
	ed := daysFromCivil(y, mo, d)
	if ed < minEpochDay || ed > maxEpochDay {
		return 0, false
	}
	// Round-trip guard: only fold dates whose canonical text reproduces exactly,
	// so reconstitution is byte-identical to what was stored.
	if epochDayToBody(int32(ed)) != body {
		return 0, false
	}
	return int32(ed), true
}

// epochDayToString reconstitutes the SOH-tagged canonical Date string for an
// int32 epoch-day, the exact inverse of [stringToEpochDay].
func epochDayToString(ed int32) string {
	return string(rune(epochDayTag)) + epochDayToBody(ed)
}

// epochDayToBody renders an epoch-day as canonical YYYY-MM-DD text (no tag).
func epochDayToBody(ed int32) string {
	y, m, d := civilFromDays(int64(ed))
	return formatCivil(y, m, d)
}

// parseCanonicalDate parses strictly "YYYY-MM-DD" (year may be more than four
// digits or negative-signed to match openCypher's extended-year dates). Returns
// ok=false on any deviation from the canonical form so non-date strings and
// non-canonical spellings never fold into the column.
func parseCanonicalDate(s string) (year, month, day int, ok bool) {
	// Split into year / month / day on the two ASCII hyphens, allowing a leading
	// sign on the year. We scan manually to avoid a strings/strconv import-heavy
	// path and to reject any non-canonical layout.
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	yStart := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == yStart || i >= len(s) || s[i] != '-' {
		return 0, 0, 0, false
	}
	year, ok = atoiRange(s[yStart:i])
	if !ok {
		return 0, 0, 0, false
	}
	if neg {
		year = -year
	}
	i++ // skip '-'
	// Month: exactly two digits.
	if i+2 > len(s) {
		return 0, 0, 0, false
	}
	month, ok = atoiRange(s[i : i+2])
	if !ok || month < 1 || month > 12 {
		return 0, 0, 0, false
	}
	i += 2
	if i >= len(s) || s[i] != '-' {
		return 0, 0, 0, false
	}
	i++ // skip '-'
	// Day: exactly two digits, and that must be the end of the string.
	if i+2 != len(s) {
		return 0, 0, 0, false
	}
	day, ok = atoiRange(s[i : i+2])
	if !ok || day < 1 || day > daysInMonth(year, month) {
		return 0, 0, 0, false
	}
	return year, month, day, true
}

// atoiRange parses a non-negative decimal integer from s (already known to be
// all digits by the caller for the fixed-width fields; the year field is
// validated here). Returns ok=false on overflow or a non-digit.
func atoiRange(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 { // generous bound; real years are tiny
			return 0, false
		}
	}
	return n, true
}

// daysInMonth returns the number of days in the given month of the given
// proleptic-Gregorian year.
func daysInMonth(year, month int) int {
	switch month {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	case 2:
		if isLeap(year) {
			return 29
		}
		return 28
	}
	return 0
}

func isLeap(y int) bool {
	return (y%4 == 0 && y%100 != 0) || y%400 == 0
}

// daysFromCivil converts a proleptic-Gregorian date to a day count since the
// Unix epoch (1970-01-01 == 0). Uses Howard Hinnant's well-known algorithm,
// which is exact for the full int range. Returns an int64 to detect int32
// overflow at the call site.
func daysFromCivil(y, m, d int) int64 {
	yy := int64(y)
	if m <= 2 {
		yy--
	}
	era := yy
	if era < 0 {
		era -= 399
	}
	era /= 400
	yoe := yy - era*400
	mp := int64((m + 9) % 12)
	doy := (153*mp+2)/5 + int64(d) - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// civilFromDays is the inverse of [daysFromCivil]: it converts a day count
// since the Unix epoch back to a proleptic-Gregorian (year, month, day).
func civilFromDays(z int64) (year, month, day int) {
	z += 719468
	era := z
	if era < 0 {
		era -= 146096
	}
	era /= 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return int(y), int(m), int(d)
}

// formatCivil renders a (year, month, day) as canonical YYYY-MM-DD, padding the
// year to at least four digits and prefixing a '-' for negative years. This
// reproduces [expr.DateValue.String] for the year range openCypher uses.
func formatCivil(y, m, d int) string {
	var buf [16]byte
	i := len(buf)

	// day, two digits
	i -= 2
	buf[i] = byte('0' + d/10)
	buf[i+1] = byte('0' + d%10)
	i--
	buf[i] = '-'
	// month, two digits
	i -= 2
	buf[i] = byte('0' + m/10)
	buf[i+1] = byte('0' + m%10)
	i--
	buf[i] = '-'
	// year, at least four digits, optional leading '-'
	yy := y
	neg := false
	if yy < 0 {
		neg = true
		yy = -yy
	}
	digits := 0
	for yy > 0 || digits < 4 {
		i--
		buf[i] = byte('0' + yy%10)
		yy /= 10
		digits++
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
