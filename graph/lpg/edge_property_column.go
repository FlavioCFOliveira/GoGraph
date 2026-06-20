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

// edgePropColumn is one typed value column inside an [edgePropCols] block. It is
// keyed by the interned property key and the property kind, holds one logical
// value per adjacency slot in a de-boxed representation when possible, and
// carries an optional validity bitmap that is nil when the column is dense
// (every slot present). Exactly one of the typed backing slices is non-nil,
// selected by kind.
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
	i64      []int64
	f64      []float64
	boolBits []uint64 // bit i = bool value of slot i (only meaningful where valid)
	days     []int32
	str      []string
	boxed    []PropertyValue

	// valid is the Arrow-style validity bitmap: bit i set ⇔ slot i carries a
	// value for this column. nil ⇔ the column is dense (every slot present), so
	// a dense column pays zero validity overhead. A slot whose bit is clear
	// reads as null regardless of the (don't-care) value cell.
	valid []uint64

	// length is the number of slots this column spans; it equals the adjacency
	// entry's neighbour count at every observable point.
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
			clearedOther = true
		}
	}
	ci := out.columnIndex(keyID, kind)
	if ci < 0 {
		// New (key, kind) column: allocate dense-shaped then mark the slot.
		col := newColumn(keyID, kind, length)
		col.setSlot(slot, v, days)
		out.cols = append(out.cols, col)
	} else {
		out.cols[ci] = out.cols[ci].cloneCol()
		out.cols[ci].setSlot(slot, v, days)
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
	// A multi-slot column created for a single live slot starts mostly absent:
	// allocate the validity bitmap so the other slots read null. A length-1
	// column is dense and keeps valid nil.
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
	return out
}

// grown returns a copy of the column extended by one slot at index oldLen, with
// the new slot ABSENT. The validity bitmap is always present in the result (a
// previously-dense column materialises one here, with every prior bit set and
// the new bit clear), which is what makes the freshly-appended slot read null.
func (col *edgePropColumn) grown(oldLen int) edgePropColumn {
	newLen := oldLen + 1
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
	return out
}

// grownTo returns a copy of the column whose length is exactly newLen (>=
// current length), with any added trailing slots ABSENT. Used when a block is
// cloned at a larger length than the column currently spans.
func (col *edgePropColumn) grownTo(newLen int) edgePropColumn {
	if newLen <= col.length {
		return *col
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
	return out
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

// compacted returns a copy of the column with the slot at idx excised: result
// slots [0,idx) equal the receiver's, result slots [idx,n-1) equal the
// receiver's [idx+1,n). The validity bitmap is compacted by the same transform.
func (col *edgePropColumn) compacted(idx int) edgePropColumn {
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

// setSlot records v on the slot in place (the column must already be cloned for
// writing). It sets the value cell and marks the validity bit. days carries the
// pre-classified epoch-day for the date column; it is ignored for other kinds.
func (col *edgePropColumn) setSlot(slot int, v PropertyValue, days int32) {
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

// clearSlot resets the validity bit for the slot in place (the column must
// already be cloned for writing). The value cell is left as don't-care.
func (col *edgePropColumn) clearSlot(slot int) {
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

// markValid sets the slot's validity bit, allocating the bitmap if the column
// was dense. A dense column whose only newly-set slot keeps it dense (no bitmap
// is needed when EVERY slot is present), but because setSlot is only ever
// called on an already-allocated-or-grown column the dense invariant is
// preserved by construction: a length-1 single-slot column stays dense (valid
// nil); any longer column already carries a bitmap from newColumn/grown.
func (col *edgePropColumn) markValid(slot int) {
	if col.valid == nil {
		// Dense column: the only dense case is length 1 with the single slot
		// present, which needs no bitmap. Setting that one slot keeps it dense.
		if col.length == 1 && slot == 0 {
			return
		}
		col.valid = make([]uint64, words(col.length))
	}
	col.valid[slot>>6] |= 1 << (uint(slot) & 63)
}

// slotValid reports whether the slot carries a value (validity bit set, or the
// column is dense). Bounds-checked: a slot past the column length is absent.
func (col *edgePropColumn) slotValid(slot int) bool {
	if slot < 0 || slot >= col.length {
		return false
	}
	if col.valid == nil {
		return true // dense: every in-range slot is present
	}
	return col.valid[slot>>6]&(1<<(uint(slot)&63)) != 0
}

// slotValue returns the value on the slot and whether it is present.
func (col *edgePropColumn) slotValue(slot int) (PropertyValue, bool) {
	if !col.slotValid(slot) {
		return PropertyValue{}, false
	}
	switch col.kind {
	case PropInt64:
		return Int64Value(col.i64[slot]), true
	case PropFloat64:
		return Float64Value(col.f64[slot]), true
	case PropBool:
		return BoolValue(col.boolBits[slot>>6]&(1<<(uint(slot)&63)) != 0), true
	case dateKind:
		return StringValue(epochDayToString(col.days[slot])), true
	case PropString:
		return StringValue(col.str[slot]), true
	default:
		return col.boxed[slot], true
	}
}

// nonEmpty reports whether any slot of the column is present.
func (col *edgePropColumn) nonEmpty() bool {
	if col.length == 0 {
		return false
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
	if col.valid == nil {
		return col.length // dense
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
