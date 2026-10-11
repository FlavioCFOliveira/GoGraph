package lpg

// edge_property_shared.go — the shared append buffer behind the fused
// property-carrying edge append (rmp #3090).
//
// # Why
//
// Every fused append ([edgePropCols.GrowSlotWithValue]) publishes a new
// immutable block. Before this file each block carried its own copy of every
// column header (176 B per column), so a source with two property keys paid
// 32 + 352 B of fresh headers per appended edge although only ~12 B of value
// was new: 4.39 GiB flat at that site in example 26's load. The value backings
// themselves were already amortised; the header array was not.
//
// # Design
//
// A column may be held in SHARED mode (bit i of [edgePropCols.shr]). Its header
// in cols[i] is then an append buffer shared by every block version derived
// from it by appends: the header's idx and value slices span the buffer's whole
// capacity (len == cap), and the per-version logical content — idx[:np[i]] and
// values[:np[i]] over [edgePropCols.length] slots — is fixed by the version's
// own np[i] and length. An append writes ONE new cell at index np and
// publishes a new 48 B block that reuses the same header array; the header
// array is replaced only when a buffer is reallocated (geometric, so O(log d)
// times per source) or a column first enters shared mode.
//
// # Why a reader of an earlier version never sees the appended slot or value
//
//  1. Bounded view. Every read goes through [edgePropCols.colAt], which bounds
//     the column to the version's np and length. A version with np = k reads
//     cells [0, k) only, and its slots are [0, length).
//  2. Write-once cells. A cell is written only at index tail.n, and tail.n is
//     incremented after the write, so each cell of a buffer is written at most
//     once in its lifetime. A version is created with np <= tail.n, so every
//     cell a version can read was written before the version existed and is
//     never written again.
//  3. High-water check. The in-place append requires np == tail.n. A version
//     whose np is behind the high-water mark — a pre-image restored by a
//     rollback ([adjlist.AdjList.WithdrawTx] carries the aux column verbatim),
//     or an older version a later write builds on — cannot write the next cell
//     (some newer version, still held by a reader, may own it); it reallocates
//     into a fresh buffer instead.
//  4. Publication. The cell is written before the new block is published by
//     the adjacency entry's atomic store; a reader that acquires the new entry
//     therefore observes the cell (Go memory model, release/acquire).
//  5. Serialised writers. tail is read and written only by the fused append,
//     which adjlist calls under the source's shard write lock; a buffer is
//     reachable from one source's entries in one adjacency list only. Readers
//     never dereference tail.
//
// Every other mutation path (set, del, CompactSlot, the GrowSlot fallback,
// Compact) first materialises the block ([edgePropCols.unshared]) into ordinary
// columns whose slices are capacity-clipped, so it can never write into a
// shared buffer. A rolled-back append therefore leaves no value: the restored
// pre-image's np excludes the cell, and the next append on it reallocates.

import (
	"math"
	"unsafe"
)

// maxSharedCols is the number of columns a block can hold in shared mode. The
// per-version present counts live inline in [edgePropCols.np] so a fused
// append allocates only the block; a block with more columns takes the
// per-append header copy.
const maxSharedCols = 2

// edgePropTail is the write cursor of a shared append buffer: n cells of its
// idx and value backings have been written. Writer-only, under the source's
// adjacency shard lock.
//
// On an ordinary (non-shared) sparse column extended by the fallback append
// ([edgePropColumn.appendOwned]) the cursor also names the backings it belongs
// to, idx and val, so a header holding other backings never matches it. Shared
// headers leave both nil.
type edgePropTail struct {
	idx unsafe.Pointer
	val unsafe.Pointer
	n   int32
}

// valBase returns the address and capacity of the sparse value backing in use
// for col's kind.
func (col *edgePropColumn) valBase() (unsafe.Pointer, int) {
	switch col.kind {
	case PropInt64, PropFloat64:
		return unsafe.Pointer(unsafe.SliceData(col.nums)), cap(col.nums) //nolint:gosec // G103: audited — the backing address is only compared for identity, never dereferenced.
	case dateKind:
		return unsafe.Pointer(unsafe.SliceData(col.days)), cap(col.days) //nolint:gosec // G103: audited — the backing address is only compared for identity, never dereferenced.
	case PropString:
		return unsafe.Pointer(unsafe.SliceData(col.str)), cap(col.str) //nolint:gosec // G103: audited — the backing address is only compared for identity, never dereferenced.
	default:
		return unsafe.Pointer(unsafe.SliceData(col.boxed)), cap(col.boxed) //nolint:gosec // G103: audited — the backing address is only compared for identity, never dereferenced.
	}
}

// appendOwned pushes (slotIdx, value) onto the tail of a non-shared sparse
// column, writing into the backings' spare capacity only when this header owns
// their write cursor: the cursor names these backings and equals the header's
// length, so the next cell has never been written and no other version can
// read it. Otherwise the backings are clipped first, so the append reallocates
// and the new backings get a fresh cursor. This is the high-water discipline of
// the shared buffer applied to the fallback path (blocks with more than
// maxSharedCols columns, or a bit-packed bool column): a version a rollback
// restores ([adjlist.AdjList.WithdrawTx] carries the aux column verbatim) is
// behind the cursor and cannot rewrite the cell the aborted version owns.
// Writer-only, under the source's adjacency shard lock.
func (col *edgePropColumn) appendOwned(slotIdx int32, v PropertyValue, days int32) {
	n := len(col.idx)
	vb, vcap := col.valBase()
	t := col.tail
	owned := t != nil && int(t.n) == n && cap(col.idx) > n && vcap > n &&
		t.idx == unsafe.Pointer(unsafe.SliceData(col.idx)) && t.val == vb //nolint:gosec // G103: audited — the backing address is only compared for identity, never dereferenced.
	if !owned {
		col.idx = col.idx[:n:n]
		switch col.kind {
		case PropInt64, PropFloat64:
			col.nums = col.nums[:n:n]
		case dateKind:
			col.days = col.days[:n:n]
		case PropString:
			col.str = col.str[:n:n]
		default:
			col.boxed = col.boxed[:n:n]
		}
	}
	col.tailAppendSparse(slotIdx, v, days)
	if owned {
		t.n++
		return
	}
	vb, _ = col.valBase()
	col.tail = &edgePropTail{idx: unsafe.Pointer(unsafe.SliceData(col.idx)), val: vb, n: int32(n + 1)} //nolint:gosec // G103: audited — the backing address is only compared for identity, never dereferenced.
}

// colAt returns column i of the block as a reader must see it: the column
// itself when it is not shared, otherwise its view bounded to this version,
// built in *tmp. The result must not be retained past tmp's lifetime.
func (c *edgePropCols) colAt(i int, tmp *edgePropColumn) *edgePropColumn {
	if i >= maxSharedCols || c.shr&(1<<uint(i)) == 0 {
		return &c.cols[i]
	}
	*tmp = c.cols[i].sharedView(int(c.np[i]), c.length)
	return tmp
}

// sharedView returns the ordinary sparse column a shared header denotes for a
// version with np present entries spanning length slots. Its slices are
// capacity-clipped, so an append through the view reallocates instead of
// writing into the shared buffer, and it carries no tail.
func (col *edgePropColumn) sharedView(np, length int) edgePropColumn {
	v := *col
	v.tail = nil
	v.length = length
	v.idx = col.idx[:np:np]
	switch col.kind {
	case PropInt64, PropFloat64:
		v.nums = col.nums[:np:np]
	case dateKind:
		v.days = col.days[:np:np]
	case PropString:
		v.str = col.str[:np:np]
	default:
		v.boxed = col.boxed[:np:np]
	}
	return v
}

// unshared returns the block with every shared column replaced by its bounded
// view, or the receiver when it holds none. exact copies each shared column's
// live cells into exactly sized backings, releasing the buffer's slack (the
// freeze pass); otherwise the views alias the buffer, capacity-clipped.
func (c *edgePropCols) unshared(exact bool) *edgePropCols {
	if c == nil || c.shr == 0 {
		return c
	}
	out := &edgePropCols{length: c.length, cols: make([]edgePropColumn, len(c.cols))}
	copy(out.cols, c.cols)
	for i := 0; i < maxSharedCols && i < len(c.cols); i++ {
		if c.shr&(1<<uint(i)) == 0 {
			continue
		}
		v := c.cols[i].sharedView(int(c.np[i]), c.length)
		if exact {
			v = v.cloneCol()
		}
		out.cols[i] = v
	}
	return out
}

// shareable reports whether a non-shared column can enter shared mode. A
// bit-packed bool is never sparse and a FOR-packed date is read-only; both keep
// the per-append header copy.
func (col *edgePropColumn) shareable() bool {
	return col.kind != PropBool && !col.packedDate
}

// mintShared returns col as a shared header and its present count. A dense
// column is converted to sparse once (O(P)); a sparse one is adopted without a
// copy, its slices clipped to their length so the first append reallocates
// rather than writing spare capacity another version may also reach.
func mintShared(col *edgePropColumn) (edgePropColumn, int32) {
	var s edgePropColumn
	if col.sparse {
		s = *col
		np := len(col.idx)
		s.idx = col.idx[:np:np]
		switch col.kind {
		case PropInt64, PropFloat64:
			s.nums = col.nums[:np:np]
		case dateKind:
			s.days = col.days[:np:np]
		case PropString:
			s.str = col.str[:np:np]
		default:
			s.boxed = col.boxed[:np:np]
		}
	} else {
		s = col.toSparse()
	}
	n := int32(len(s.idx))
	s.valid = nil
	s.tail = &edgePropTail{n: n}
	return s, n
}

// newSharedSingle returns a shared header whose buffer holds the single cell
// (slot, v); its version present count is 1.
func newSharedSingle(key PropertyKeyID, kind PropertyKind, slot int, v PropertyValue, days int32) edgePropColumn {
	col := edgePropColumn{key: key, kind: kind, sparse: true}
	allocSharedBuffer(&col, 1)
	col.writeSharedCell(0, int32(slot), v, days)
	col.tail = &edgePropTail{n: 1}
	return col
}

// allocSharedBuffer gives col fresh idx and value backings of length (and
// capacity) n.
func allocSharedBuffer(col *edgePropColumn, n int) {
	col.idx = make([]int32, n)
	switch col.kind {
	case PropInt64, PropFloat64:
		col.nums = make([]uint64, n)
	case dateKind:
		col.days = make([]int32, n)
	case PropString:
		col.str = make([]string, n)
	default:
		col.boxed = make([]PropertyValue, n)
	}
}

// reallocShared returns a header for a fresh buffer of capacity newCap holding
// a copy of col's first np cells, with its own write cursor at np.
func (col *edgePropColumn) reallocShared(np, newCap int) edgePropColumn {
	out := edgePropColumn{key: col.key, kind: col.kind, sparse: true}
	allocSharedBuffer(&out, newCap)
	copy(out.idx, col.idx[:np])
	switch col.kind {
	case PropInt64, PropFloat64:
		copy(out.nums, col.nums[:np])
	case dateKind:
		copy(out.days, col.days[:np])
	case PropString:
		copy(out.str, col.str[:np])
	default:
		copy(out.boxed, col.boxed[:np])
	}
	out.tail = &edgePropTail{n: int32(np)}
	return out
}

// writeSharedCell writes the (slotIdx, value) entry into cell k of the shared
// buffer. Only the cell is written; the header is not modified.
func (col *edgePropColumn) writeSharedCell(k int, slotIdx int32, v PropertyValue, days int32) {
	col.idx[k] = slotIdx
	switch col.kind {
	case PropInt64:
		i, _ := v.Int64()
		col.nums[k] = uint64(i)
	case PropFloat64:
		f, _ := v.Float64()
		col.nums[k] = math.Float64bits(f)
	case dateKind:
		col.days[k] = days
	case PropString:
		s, _ := v.String()
		col.str[k] = s
	default:
		col.boxed[k] = v
	}
}

// growSharedAbsent is the shared-mode [edgePropCols.GrowSlot]: when every
// column is shared, the new slot is absent from all of them by construction (it
// is not in any idx prefix), so only the version's length changes and the
// header array is reused. It returns nil when the block is not wholly shared.
func (c *edgePropCols) growSharedAbsent(oldLen int) *edgePropCols {
	n := len(c.cols)
	if n == 0 || n > maxSharedCols || c.shr != uint8(1<<uint(n))-1 {
		return nil
	}
	return &edgePropCols{cols: c.cols, length: oldLen + 1, np: c.np, shr: c.shr}
}

// growSharedWithValue is the shared-mode [edgePropCols.GrowSlotWithValue]. It
// returns nil, leaving the receiver untouched, when the block cannot be held in
// shared mode (more than maxSharedCols columns after the append, a bool value,
// or a bit-packed bool / FOR-packed date column).
func (c *edgePropCols) growSharedWithValue(oldLen int, p *edgePropPayload, kind PropertyKind, days int32) *edgePropCols {
	n := len(c.cols)
	if kind == PropBool || n > maxSharedCols || c.length != oldLen {
		return nil
	}
	t := -1
	for i := range c.cols {
		col := &c.cols[i]
		if col.key == p.keyID && col.kind == kind {
			t = i
		}
		if c.shr&(1<<uint(i)) == 0 && !col.shareable() {
			return nil
		}
	}
	if t < 0 && n == maxSharedCols {
		return nil
	}
	out := &edgePropCols{cols: c.cols, length: oldLen + 1, np: c.np, shr: c.shr}
	fresh := false
	ensureFresh := func(extra int) {
		if !fresh {
			cols := make([]edgePropColumn, n, n+extra)
			copy(cols, c.cols)
			out.cols, fresh = cols, true
		}
	}
	extra := 0
	if t < 0 {
		extra = 1
	}
	for i := range c.cols {
		if c.shr&(1<<uint(i)) != 0 {
			continue
		}
		ensureFresh(extra)
		out.cols[i], out.np[i] = mintShared(&c.cols[i])
		out.shr |= 1 << uint(i)
	}
	if t < 0 {
		ensureFresh(extra)
		out.cols = append(out.cols, newSharedSingle(p.keyID, kind, oldLen, p.value, days))
		out.np[n] = 1
		out.shr |= 1 << uint(n)
		return out
	}
	np := int(out.np[t])
	if col := &out.cols[t]; col.tail.n == int32(np) && np < len(col.idx) {
		// The cell at np is unwritten and beyond every version's view: write it
		// in place and advance the cursor.
		col.writeSharedCell(np, int32(oldLen), p.value, days)
		col.tail.n++
		out.np[t] = int32(np + 1)
		return out
	}
	ensureFresh(extra)
	newCap := 2 * np
	if newCap < 1 {
		newCap = 1
	}
	out.cols[t] = out.cols[t].reallocShared(np, newCap)
	out.cols[t].writeSharedCell(np, int32(oldLen), p.value, days)
	out.cols[t].tail.n++
	out.np[t] = int32(np + 1)
	return out
}
