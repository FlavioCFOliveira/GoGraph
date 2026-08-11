package lpg

import (
	"encoding/binary"
	"math"
	"unsafe"
)

// propbag.go — the per-node property bag's compact representation.
//
// # History
//
// Node properties were originally a nested map[graph.NodeID]map[PropertyKeyID]
// PropertyValue. Sprint 207 (#1587) replaced the INNER map with a small
// unsorted slice of (key, value) pairs, which removed a ~300 B Go map per node.
// Sprint 339 (#2408) replaced that slice with the byte stream below, which is
// what this file now documents; the pair-slice tier is gone.
//
// # Motivation for the byte stream
//
// The memory audit of 2026-08-11 measured GoGraph spending 146.66 B on one
// 9-character node property against Memgraph's 19.45 B — a 7.54x gap, the
// worst ratio in that audit — and 297.50 B against 36.18 B for a realistic
// three-property node. A heap profile attributed GoGraph's share to three
// things, none of which is the data: the pair slice, the interface box inside
// every [PropertyValue], and the string header. Of 375 B/node, 17 B was the
// string the caller asked to store.
//
// # Design, and where it comes from
//
// The encoding is adapted from Memgraph's PropertyStore, read at v3.9.0
// (src/storage/v2/property_store.cpp:99-205). The structural idea taken from it
// is a single self-describing byte per record:
//
//	metadata: 0bTTTT_IIPP  — 4-bit type, 2-bit id width, 2-bit payload width
//
// followed by the property id in the narrowest of 1/2/4/8 bytes that holds it,
// followed by a payload whose shape the type determines. A boolean's value
// rides in the payload-width field and occupies no payload bytes at all. Their
// source comment claims "approximately 10 times less memory" against a
// std::map; the figure that justified adopting it here is the one measured in
// this project, in BenchmarkPropBagRepresentation: 96.11 -> 40.00 B for one
// string property and 224.0 -> 56.00 B for three properties.
//
// What was NOT taken: Memgraph sorts its records by property id and relies on
// that for a merge-style rewrite. Here the records are unsorted, because
// iteration order is not observable — the public accessors return a
// map[string]PropertyValue and the snapshot serializer emits self-describing
// records whose on-disk order never depended on bag order — and an unsorted
// stream makes an append O(1) in the number of properties.
//
// # Tiers
//
//	stream — the byte encoding, for the four SCALAR kinds (PropString,
//	         PropInt64, PropFloat64, PropBool). This is the overwhelmingly
//	         common case and the one the audit measured.
//	map    — a promoted map[PropertyKeyID]PropertyValue. Reached when a set
//	         would store a kind the stream does not model (PropTime, PropBytes,
//	         PropList, all of which are variable-shaped or hold references), or
//	         would grow the bag past smallBagMax records. Promotion is ONE-WAY,
//	         mirroring [index.NodeSet] and the tiers that preceded this one.
//
// # The immutability invariant, and why it lets reads avoid a copy
//
// Every mutation allocates a NEW buffer sized EXACTLY to its contents; a buffer
// that has been published is never written again, and no buffer ever carries
// spare capacity. That is the whole safety argument for [unsafe.String] in
// [propBag.get]: a string handed to a caller aliases bytes that nothing will
// ever modify, so it is immutable in fact as well as in contract, and reading a
// string property allocates nothing — which the pair-slice tier also managed
// and a copying decoder would have lost.
//
// The invariant also disposes of a hazard that spare capacity would create.
// propBag is stored BY VALUE and therefore copied freely (a caller reads a bag
// out of a shard map, mutates it, writes it back; the MVCC path clones one for
// a pre-image). Two copies sharing a backing array with room to grow could each
// append into the same bytes and disagree about what is there. With exact-size
// buffers there is no room to grow into, so every append necessarily allocates
// and the two copies necessarily diverge into separate arrays.
//
// # Concurrency
//
// [propBag] is NOT safe for concurrent use on its own. The per-shard RWMutex of
// [nodePropShard] guards every read and write, exactly as it guarded the nested
// maps before. A propBag value is mutated only under the shard write lock and
// read only under the matching read lock, and — because it is held by value —
// every mutation must be written back into the shard map under that same lock.

// smallBagMax is the largest number of records the byte-stream tier holds
// before a propBag promotes to a map. It matches the [index.NodeSet] small-set
// threshold. Beyond it a linear scan of the stream stops being competitive with
// a map probe, and a property-heavy node is better served by O(1) lookup than
// by the stream's compactness.
const smallBagMax = 8

// Metadata-byte layout, mirroring the scheme described above.
const (
	bagMaskType    = 0xf0 // 4 bits: the PropertyKind
	bagMaskIDSize  = 0x0c // 2 bits: 1/2/4/8-byte property id
	bagMaskPaySize = 0x03 // 2 bits: payload width, or the value itself for a bool
	bagShiftType   = 4
	bagShiftIDSize = 2

	// bagBoolTrue is the payload-width value a true boolean carries. False is
	// 0. No payload bytes follow either.
	bagBoolTrue = 0x03
)

// bagWidth returns the narrowest of 1/2/4/8 bytes that holds v, with its 2-bit
// selector.
func bagWidth(v uint64) (n int, sel byte) {
	switch {
	case v <= math.MaxUint8:
		return 1, 0
	case v <= math.MaxUint16:
		return 2, 1
	case v <= math.MaxUint32:
		return 4, 2
	default:
		return 8, 3
	}
}

// bagSizeOf returns the byte width a 2-bit selector denotes.
func bagSizeOf(sel byte) int { return 1 << sel }

// bagPutUint appends the low n bytes of v, little-endian.
func bagPutUint(dst []byte, v uint64, n int) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	return append(dst, tmp[:n]...)
}

// bagUint reads an n-byte little-endian unsigned integer at off.
func bagUint(buf []byte, off, n int) uint64 {
	var tmp [8]byte
	copy(tmp[:], buf[off:off+n])
	return binary.LittleEndian.Uint64(tmp[:])
}

// bagStreamable reports whether kind is one the byte stream models. Every other
// kind forces promotion to the map tier.
func bagStreamable(kind PropertyKind) bool {
	switch kind {
	case PropString, PropInt64, PropFloat64, PropBool:
		return true
	}
	return false
}

// bagEncodedLen returns the exact number of bytes the record for (key, val)
// occupies. The caller must have checked bagStreamable(val.Kind()).
func bagEncodedLen(key PropertyKeyID, val PropertyValue) int {
	idN, _ := bagWidth(uint64(key))
	switch val.Kind() {
	case PropBool:
		return 1 + idN
	case PropInt64:
		iv, _ := val.Int64()
		vN, _ := bagWidth(bagZigZag(iv))
		return 1 + idN + vN
	case PropFloat64:
		return 1 + idN + 8
	default: // PropString
		sv, _ := val.String()
		lN, _ := bagWidth(uint64(len(sv)))
		return 1 + idN + lN + len(sv)
	}
}

// bagZigZag maps a signed integer onto an unsigned one that keeps small
// magnitudes narrow in both directions, so that -1 costs one byte rather than
// eight. Memgraph reaches the same end by storing a signed type at each width.
func bagZigZag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

// bagUnZigZag inverts [bagZigZag].
func bagUnZigZag(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }

// bagAppend writes the record for (key, val) at the end of dst.
func bagAppend(dst []byte, key PropertyKeyID, val PropertyValue) []byte {
	idN, idSel := bagWidth(uint64(key))
	switch val.Kind() {
	case PropBool:
		bv, _ := val.Bool()
		pay := byte(0)
		if bv {
			pay = bagBoolTrue
		}
		dst = append(dst, byte(PropBool)<<bagShiftType|idSel<<bagShiftIDSize|pay)
		return bagPutUint(dst, uint64(key), idN)
	case PropInt64:
		iv, _ := val.Int64()
		zz := bagZigZag(iv)
		vN, vSel := bagWidth(zz)
		dst = append(dst, byte(PropInt64)<<bagShiftType|idSel<<bagShiftIDSize|vSel)
		dst = bagPutUint(dst, uint64(key), idN)
		return bagPutUint(dst, zz, vN)
	case PropFloat64:
		fv, _ := val.Float64()
		dst = append(dst, byte(PropFloat64)<<bagShiftType|idSel<<bagShiftIDSize)
		dst = bagPutUint(dst, uint64(key), idN)
		return bagPutUint(dst, math.Float64bits(fv), 8)
	default: // PropString
		sv, _ := val.String()
		lN, lSel := bagWidth(uint64(len(sv)))
		dst = append(dst, byte(PropString)<<bagShiftType|idSel<<bagShiftIDSize|lSel)
		dst = bagPutUint(dst, uint64(key), idN)
		dst = bagPutUint(dst, uint64(len(sv)), lN)
		return append(dst, sv...)
	}
}

// bagRecord describes one decoded record: its key, its value, and where the
// next record starts.
type bagRecord struct {
	key  PropertyKeyID
	val  PropertyValue
	next int
}

// bagDecodeAt decodes the record beginning at off.
//
// The returned string, when the record holds one, ALIASES buf rather than
// copying it. That is sound because a published buffer is never written again;
// see the immutability invariant in this file's header.
func bagDecodeAt(buf []byte, off int) bagRecord {
	meta := buf[off]
	kind := PropertyKind(meta & bagMaskType >> bagShiftType)
	idN := bagSizeOf(meta & bagMaskIDSize >> bagShiftIDSize)
	paySel := meta & bagMaskPaySize
	p := off + 1
	key := PropertyKeyID(bagUint(buf, p, idN))
	p += idN

	switch kind {
	case PropBool:
		return bagRecord{key: key, val: BoolValue(paySel == bagBoolTrue), next: p}
	case PropInt64:
		vN := bagSizeOf(paySel)
		v := bagUnZigZag(bagUint(buf, p, vN))
		return bagRecord{key: key, val: Int64Value(v), next: p + vN}
	case PropFloat64:
		v := math.Float64frombits(bagUint(buf, p, 8))
		return bagRecord{key: key, val: Float64Value(v), next: p + 8}
	default: // PropString
		lN := bagSizeOf(paySel)
		n := int(bagUint(buf, p, lN))
		p += lN
		var s string
		if n > 0 {
			// SAFETY: buf[p:p+n] belongs to a published buffer, which this
			// file's invariant guarantees is never written again, so the
			// string is immutable for its whole lifetime. Avoiding the copy is
			// the point: the tier this replaced returned the stored header
			// without allocating, and a copying decoder would have made every
			// string property read allocate.
			//
			//nolint:gosec // G103: audited; buf is produced only by bagAppend, which
			// wrote this record's own length field, so p+n == the record end <= len(buf)
			// by construction, and the bytes are immutable (see the file header).
			s = unsafe.String(&buf[p], n)
		}
		return bagRecord{key: key, val: StringValue(s), next: p + n}
	}
}

// propBag is the per-node property bag. The zero value is a valid empty bag.
// The fields form a tagged union resolved by which is non-nil:
//   - m != nil -> map state (promoted; never demotes).
//   - m == nil -> stream state, the (possibly empty) encoded buffer.
type propBag struct {
	buf []byte                          // stream state; exact-size, never rewritten
	m   map[PropertyKeyID]PropertyValue // non-nil iff promoted to map (one-way)
}

// get returns the value stored under key and whether it is present.
func (b *propBag) get(key PropertyKeyID) (PropertyValue, bool) {
	if b.m != nil {
		v, ok := b.m[key]
		return v, ok
	}
	for off := 0; off < len(b.buf); {
		r := bagDecodeAt(b.buf, off)
		if r.key == key {
			return r.val, true
		}
		off = r.next
	}
	return PropertyValue{}, false
}

// promote moves the stream's records into a map and switches tier. extra is a
// capacity hint for the records the caller is about to add.
func (b *propBag) promote(extra int) {
	m := make(map[PropertyKeyID]PropertyValue, b.count()+extra)
	for off := 0; off < len(b.buf); {
		r := bagDecodeAt(b.buf, off)
		// The value may alias b.buf; the buffer is immutable, so carrying the
		// alias into the map is safe and avoids copying every string on
		// promotion.
		m[r.key] = r.val
		off = r.next
	}
	b.m = m
	b.buf = nil
}

// count returns how many records the stream holds.
func (b *propBag) count() int {
	n := 0
	for off := 0; off < len(b.buf); {
		n++
		off = bagDecodeAt(b.buf, off).next
	}
	return n
}

// set inserts or overwrites the value stored under key, promoting to the map
// tier when the value's kind is not one the stream models or when an insert
// would grow the bag past smallBagMax.
//
// Every mutation of the stream allocates a new exact-size buffer; see the
// immutability invariant in this file's header.
func (b *propBag) set(key PropertyKeyID, val PropertyValue) {
	if b.m != nil {
		b.m[key] = val
		return
	}
	if !bagStreamable(val.Kind()) {
		b.promote(1)
		b.m[key] = val
		return
	}

	// Locate an existing record for key, and measure the stream while doing so.
	found, foundEnd, n := -1, 0, 0
	for off := 0; off < len(b.buf); {
		r := bagDecodeAt(b.buf, off)
		n++
		if r.key == key {
			found, foundEnd = off, r.next
		}
		off = r.next
	}

	if found < 0 {
		if n >= smallBagMax {
			b.promote(1)
			b.m[key] = val
			return
		}
		next := make([]byte, 0, len(b.buf)+bagEncodedLen(key, val))
		next = append(next, b.buf...)
		b.buf = bagAppend(next, key, val)
		return
	}

	// Overwrite: rebuild without the old record, then append the new one. The
	// old buffer is left untouched for anything that aliases it.
	next := make([]byte, 0, len(b.buf)-(foundEnd-found)+bagEncodedLen(key, val))
	next = append(next, b.buf[:found]...)
	next = append(next, b.buf[foundEnd:]...)
	b.buf = bagAppend(next, key, val)
}

// del removes the value stored under key. It reports whether the bag became
// empty as a result, so the caller can drop the node's bag entry. A bag in the
// map tier never demotes (promote-and-never-demote, mirroring [index.NodeSet]).
func (b *propBag) del(key PropertyKeyID) (nowEmpty bool) {
	if b.m != nil {
		delete(b.m, key)
		return len(b.m) == 0
	}
	for off := 0; off < len(b.buf); {
		r := bagDecodeAt(b.buf, off)
		if r.key != key {
			off = r.next
			continue
		}
		if len(b.buf)-(r.next-off) == 0 {
			b.buf = nil
			return true
		}
		next := make([]byte, 0, len(b.buf)-(r.next-off))
		next = append(next, b.buf[:off]...)
		next = append(next, b.buf[r.next:]...)
		b.buf = next
		return false
	}
	return len(b.buf) == 0
}

// len returns the number of properties in the bag.
func (b *propBag) len() int {
	if b.m != nil {
		return len(b.m)
	}
	return b.count()
}

// forEach invokes fn once per (keyID, value) pair in the bag. The iteration
// order is unspecified, matching the prior map-backed behaviour. fn must not
// mutate the bag.
func (b *propBag) forEach(fn func(key PropertyKeyID, val PropertyValue)) {
	if b.m != nil {
		for k, v := range b.m {
			fn(k, v)
		}
		return
	}
	for off := 0; off < len(b.buf); {
		r := bagDecodeAt(b.buf, off)
		fn(r.key, r.val)
		off = r.next
	}
}
