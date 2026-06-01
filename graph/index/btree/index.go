// Package btree provides an order-preserving property index over a
// constraints.Ordered value type, answering range predicates against
// the NodeIDs that carry each value.
//
// The v1 implementation is a sorted-array index — sufficient to meet
// the Sprint 2 acceptance criteria (sub-microsecond Range first-
// result on a 10^7-element index, multi-million-entry bulk-load in
// seconds). The public API is designed so that a future replacement
// with a fan-out-tuned B+ tree (or skip list) can land without
// breaking callers; per-key Insert is O(n) today and should be
// avoided on the hot path in favour of [Index.BulkLoad].
//
// All operations are safe for concurrent use; a single [sync.RWMutex]
// guards the array.
package btree

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"sort"
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// ErrMismatchedLengths is returned by [Index.BulkLoad] when the
// values and nodes slices supplied to it do not share a common
// length. Before sprint 21 this condition panicked; the error
// returned here lets callers handle it as a recoverable input
// validation failure.
var ErrMismatchedLengths = errors.New("btree: values and nodes slices must have the same length")

// entry is one (value, set-of-nodes) record in the sorted array.
type entry[V cmp.Ordered] struct {
	value V
	bm    *roaring64.Bitmap
}

// Index is an order-preserving property index keyed by V.
type Index[V cmp.Ordered] struct {
	mu      sync.RWMutex
	entries []entry[V]
}

// New returns an empty index.
func New[V cmp.Ordered]() *Index[V] { return &Index[V]{} }

// BulkLoad replaces the contents of the index with the given
// (value, node) pairs in O(n log n) time. The pairs slice is left
// untouched. Calling BulkLoad on a non-empty index discards previous
// data. Returns [ErrMismatchedLengths] when len(values) != len(nodes).
func (i *Index[V]) BulkLoad(values []V, nodes []graph.NodeID) error {
	if len(values) != len(nodes) {
		return ErrMismatchedLengths
	}
	type pair struct {
		v V
		n graph.NodeID
	}
	pairs := make([]pair, len(values))
	for k := range values {
		pairs[k] = pair{v: values[k], n: nodes[k]}
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].v < pairs[b].v })
	out := make([]entry[V], 0, len(pairs))
	for k := 0; k < len(pairs); {
		j := k
		bm := roaring64.New()
		for j < len(pairs) && pairs[j].v == pairs[k].v {
			bm.Add(uint64(pairs[j].n))
			j++
		}
		out = append(out, entry[V]{value: pairs[k].v, bm: bm})
		k = j
	}
	i.mu.Lock()
	i.entries = out
	i.mu.Unlock()
	return nil
}

// Insert records that node carries value.
func (i *Index[V]) Insert(value V, node graph.NodeID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	idx := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= value })
	if idx < len(i.entries) && i.entries[idx].value == value {
		i.entries[idx].bm.Add(uint64(node))
		return
	}
	bm := roaring64.New()
	bm.Add(uint64(node))
	i.entries = append(i.entries, entry[V]{})
	copy(i.entries[idx+1:], i.entries[idx:len(i.entries)-1])
	i.entries[idx] = entry[V]{value: value, bm: bm}
}

// Delete removes node from the set associated with value. No-op when
// absent. The (value, bitmap) entry is removed when its bitmap
// becomes empty.
func (i *Index[V]) Delete(value V, node graph.NodeID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	idx := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= value })
	if idx >= len(i.entries) || i.entries[idx].value != value {
		return
	}
	i.entries[idx].bm.Remove(uint64(node))
	if i.entries[idx].bm.IsEmpty() {
		i.entries = append(i.entries[:idx], i.entries[idx+1:]...)
	}
}

// RangeFirst returns the first NodeID in the smallest indexed value
// not less than lo and not greater than hi, plus that value. The
// second return value reports whether any match exists. It is the
// allocation-free way to peek the first row of a range scan; the
// full union of matches is available via [Index.Range].
func (i *Index[V]) RangeFirst(lo, hi V) (V, graph.NodeID, bool) {
	var zeroV V
	if hi < lo {
		return zeroV, 0, false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	start := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= lo })
	if start >= len(i.entries) || i.entries[start].value > hi {
		return zeroV, 0, false
	}
	first := i.entries[start].bm.Minimum()
	return i.entries[start].value, graph.NodeID(first), true
}

// Range returns a Roaring bitmap that is the union of the per-value
// bitmaps for every key v with lo <= v <= hi. The returned bitmap is
// freshly allocated; the caller owns it.
func (i *Index[V]) Range(lo, hi V) *roaring64.Bitmap {
	out := roaring64.New()
	if hi < lo {
		return out
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	start := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= lo })
	for k := start; k < len(i.entries) && i.entries[k].value <= hi; k++ {
		out.Or(i.entries[k].bm)
	}
	return out
}

// Lookup returns a clone of the bitmap associated with value, or an
// empty bitmap when value is unknown.
func (i *Index[V]) Lookup(value V) *roaring64.Bitmap {
	i.mu.RLock()
	defer i.mu.RUnlock()
	idx := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= value })
	if idx >= len(i.entries) || i.entries[idx].value != value {
		return roaring64.New()
	}
	return i.entries[idx].bm.Clone()
}

// Cardinality returns the number of NodeIDs associated with value.
func (i *Index[V]) Cardinality(value V) uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	idx := sort.Search(len(i.entries), func(k int) bool { return i.entries[k].value >= value })
	if idx >= len(i.entries) || i.entries[idx].value != value {
		return 0
	}
	return i.entries[idx].bm.GetCardinality()
}

// DistinctValues returns the number of distinct values currently
// indexed.
func (i *Index[V]) DistinctValues() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.entries)
}

// Kind returns "btree" — satisfies [index.Subscriber].
func (*Index[V]) Kind() string { return "btree" }

// Apply is a no-op for the generic B+ tree index. See the matching
// note on [hash.Index.Apply] — the index cannot auto-project arbitrary
// [index.Change] values without caller-supplied bindings.
func (*Index[V]) Apply(index.Change) {}

// btreeMagic is the four-byte magic at the head of a serialised
// btree index ('SBTR' little-endian — 0x52544253).
const btreeMagic uint32 = 0x52544253

// btreeFormatVersion is the on-disk format version of a serialised
// btree index.
const btreeFormatVersion uint32 = 1

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// encodeOrdered serialises one cmp.Ordered value to bytes. The
// supported set matches [hash.Index] for the types that are both
// comparable and ordered.
//
//nolint:gocyclo // type switch over supported ordered kinds
func encodeOrdered[V cmp.Ordered](v V) ([]byte, error) {
	switch x := any(v).(type) {
	case string:
		return []byte(x), nil
	case int64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(x))
		return buf[:], nil
	case int32:
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], uint32(x))
		return buf[:], nil
	case int:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(x))
		return buf[:], nil
	case uint64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], x)
		return buf[:], nil
	case uint32:
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], x)
		return buf[:], nil
	case uint:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(x))
		return buf[:], nil
	case float64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(x))
		return buf[:], nil
	}
	return nil, fmt.Errorf("%w: %T", index.ErrIndexValueTypeUnsupported, v)
}

// decodeOrdered is the inverse of [encodeOrdered].
//
//nolint:gocyclo // type switch over supported ordered kinds
func decodeOrdered[V cmp.Ordered](b []byte) (V, error) {
	var zero V
	switch any(zero).(type) {
	case string:
		var out V
		assignAny(&out, string(b))
		return out, nil
	case int64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: int64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, int64(binary.LittleEndian.Uint64(b)))
		return out, nil
	case int32:
		if len(b) != 4 {
			return zero, fmt.Errorf("%w: int32 wants 4 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, int32(binary.LittleEndian.Uint32(b)))
		return out, nil
	case int:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: int wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, int(int64(binary.LittleEndian.Uint64(b))))
		return out, nil
	case uint64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: uint64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, binary.LittleEndian.Uint64(b))
		return out, nil
	case uint32:
		if len(b) != 4 {
			return zero, fmt.Errorf("%w: uint32 wants 4 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, binary.LittleEndian.Uint32(b))
		return out, nil
	case uint:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: uint wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, uint(binary.LittleEndian.Uint64(b)))
		return out, nil
	case float64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: float64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, math.Float64frombits(binary.LittleEndian.Uint64(b)))
		return out, nil
	}
	return zero, fmt.Errorf("%w: %T", index.ErrIndexValueTypeUnsupported, zero)
}

// assignAny copies src into *dst, treating dst as an any.
func assignAny[V any](dst *V, src any) {
	*dst = src.(V)
}

// Serialize writes every (value, NodeID-set) pair in key order to w.
// The on-disk layout is:
//
//	uint32 magic ('SBTR')
//	uint32 formatVersion
//	uint64 entryCount
//	repeat entryCount times:
//	  uint32 keyLen
//	  [keyLen]byte key (kind-specific encoding)
//	  uint64 idCount
//	  [idCount]uint64 NodeIDs (sorted ascending)
//	uint32 crc32c (little-endian)
//
// Writing in key order lets [Deserialize] use [Index.BulkLoad]
// indirectly: the reader appends one entry at a time and the sorted
// order is preserved.
func (i *Index[V]) Serialize(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, btreeMagic); err != nil {
		return err
	}
	if err := binary.Write(tee, binary.LittleEndian, btreeFormatVersion); err != nil {
		return err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	if err := binary.Write(tee, binary.LittleEndian, uint64(len(i.entries))); err != nil {
		return err
	}
	for k := range i.entries {
		key, err := encodeOrdered(i.entries[k].value)
		if err != nil {
			return err
		}
		if uint64(len(key)) > uint64(^uint32(0)) {
			return fmt.Errorf("btree: key too long to serialize: %d", len(key))
		}
		if err := binary.Write(tee, binary.LittleEndian, uint32(len(key))); err != nil {
			return err
		}
		if _, err := tee.Write(key); err != nil {
			return err
		}
		ids := i.entries[k].bm.ToArray()
		if err := binary.Write(tee, binary.LittleEndian, uint64(len(ids))); err != nil {
			return err
		}
		if err := binary.Write(tee, binary.LittleEndian, ids); err != nil {
			return err
		}
	}

	if err := binary.Write(bw, binary.LittleEndian, hasher.Sum32()); err != nil {
		return err
	}
	return bw.Flush()
}

// Deserialize replaces the receiver's state with the contents of r.
// Because the writer dumps entries in ascending key order, the
// reader can build the sorted entries slice directly without an
// extra sort pass; the loader is therefore O(n) instead of
// [Index.BulkLoad]'s O(n log n).
//
//nolint:gocyclo // index deserialize: header + per-entry decode + per-step bounds checks
func (i *Index[V]) Deserialize(r io.Reader) error {
	all, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("%w: read: %w", index.ErrIndexCorrupted, err)
	}
	if len(all) < 4 {
		return fmt.Errorf("%w: short payload", index.ErrIndexCorrupted)
	}
	body := all[:len(all)-4]
	trailer := binary.LittleEndian.Uint32(all[len(all)-4:])
	if got := crc32.Checksum(body, castagnoli); got != trailer {
		return fmt.Errorf("%w: crc32c mismatch: got %d, want %d",
			index.ErrIndexCorrupted, got, trailer)
	}

	br := bufio.NewReader(bytes.NewReader(body))
	var magic, version uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		return fmt.Errorf("%w: magic: %w", index.ErrIndexCorrupted, err)
	}
	if magic != btreeMagic {
		return fmt.Errorf("%w: bad magic %#x", index.ErrIndexCorrupted, magic)
	}
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		return fmt.Errorf("%w: version: %w", index.ErrIndexCorrupted, err)
	}
	if version != btreeFormatVersion {
		return fmt.Errorf("%w: unsupported format version %d",
			index.ErrIndexCorrupted, version)
	}
	var entryCount uint64
	if err := binary.Read(br, binary.LittleEndian, &entryCount); err != nil {
		return fmt.Errorf("%w: entryCount: %w", index.ErrIndexCorrupted, err)
	}
	if entryCount > 1<<40 {
		return fmt.Errorf("%w: implausible entryCount %d",
			index.ErrIndexCorrupted, entryCount)
	}

	out := make([]entry[V], 0, entryCount)
	var prev V
	hasPrev := false
	for e := uint64(0); e < entryCount; e++ {
		var keyLen uint32
		if err := binary.Read(br, binary.LittleEndian, &keyLen); err != nil {
			return fmt.Errorf("%w: keyLen: %w", index.ErrIndexCorrupted, err)
		}
		if uint64(keyLen) > uint64(len(body)) {
			return fmt.Errorf("%w: implausible keyLen %d",
				index.ErrIndexCorrupted, keyLen)
		}
		kbuf := make([]byte, keyLen)
		if _, err := io.ReadFull(br, kbuf); err != nil {
			return fmt.Errorf("%w: key bytes: %w", index.ErrIndexCorrupted, err)
		}
		v, derr := decodeOrdered[V](kbuf)
		if derr != nil {
			return derr
		}
		if hasPrev && v < prev {
			return fmt.Errorf("%w: keys not in ascending order",
				index.ErrIndexCorrupted)
		}
		prev = v
		hasPrev = true
		var idCount uint64
		if err := binary.Read(br, binary.LittleEndian, &idCount); err != nil {
			return fmt.Errorf("%w: idCount: %w", index.ErrIndexCorrupted, err)
		}
		if idCount > uint64(len(body)) {
			return fmt.Errorf("%w: implausible idCount %d",
				index.ErrIndexCorrupted, idCount)
		}
		ids := make([]uint64, idCount)
		if err := binary.Read(br, binary.LittleEndian, ids); err != nil {
			return fmt.Errorf("%w: ids: %w", index.ErrIndexCorrupted, err)
		}
		bm := roaring64.New()
		bm.AddMany(ids)
		out = append(out, entry[V]{value: v, bm: bm})
	}

	i.mu.Lock()
	i.entries = out
	i.mu.Unlock()
	return nil
}
