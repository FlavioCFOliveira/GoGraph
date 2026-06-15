// Package btree provides an order-preserving property index over a
// constraints.Ordered value type, answering range predicates against
// the NodeIDs that carry each value.
//
// The implementation is a cache-friendly in-memory B+ tree (task
// #1514): all (value, NodeID-set) data lives in the leaves, internal
// nodes hold separator keys + child pointers, and leaves are singly
// linked low→high for forward range scans. Insert and Delete of a
// distinct key are O(log n); point reads (Lookup, Cardinality) are
// O(log n); a range scan is O(log n + k) over the k keys it spans; and
// [Index.BulkLoad] builds the tree bottom-up in O(n) from sorted input.
// This replaces the original sorted-array index, whose per-key Insert
// and Delete were O(n) (an array shift) — the win is on write-heavy
// indexed workloads while every read path keeps its prior complexity.
// The tree internals live in bplus.go.
//
// All operations are safe for concurrent use; a single [sync.RWMutex]
// guards the tree for the whole duration of each operation. Because the
// mutex fully excludes a writer's split/unlink from any in-flight
// reader, a reader can never observe a half-applied split or a dangling
// leaf. The mutex provides index-internal isolation only; transaction
// isolation across multiple calls is the engine's responsibility.
//
// # Key ordering
//
// Keys are ordered by the TOTAL order of [cmp.Compare] / [cmp.Less],
// not by the raw < operator. The two orders agree everywhere except
// IEEE 754 NaN: under the total order a floating-point NaN key is
// less than every other value (including math.Inf(-1)), every NaN bit
// pattern compares equal to every other NaN, and ±0.0 are one key.
// Raw < is only a partial order over floats — every comparison with
// NaN is false — so a single NaN insert used to break the monotone
// predicate that [sort.Search] requires and silently corrupted the
// index for ordinary keys (task #1354). With the total order the
// sorted invariant holds for every representable input: NaN is a
// regular, deduplicated key that Lookup/Delete address and that no
// range with a non-NaN lower bound ever returns.
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

// Index is an order-preserving property index keyed by V, backed by an
// in-memory B+ tree (see bplus.go).
type Index[V cmp.Ordered] struct {
	mu   sync.RWMutex
	tree *bplus[V]

	// binding, when non-nil, ties the index to one (label, property) pair of
	// a live node graph so [Index.Apply] can translate [index.Change] events
	// into typed Insert / Delete calls. It is set once by [NewBound] before
	// the index is shared and never mutated afterwards, so Apply reads it
	// without synchronisation. See bound.go.
	binding *Binding[V]
}

// New returns an empty index.
func New[V cmp.Ordered]() *Index[V] { return &Index[V]{tree: newBplus[V]()} }

// BulkLoad replaces the contents of the index with the given
// (value, node) pairs in O(n log n) time. The pairs slice is left
// untouched. Calling BulkLoad on a non-empty index discards previous
// data. Returns [ErrMismatchedLengths] when len(values) != len(nodes).
// Values are sorted and deduplicated under the total order described
// in the package documentation, so NaN inputs collapse into one
// leading entry instead of corrupting (or, before task #1354, hanging)
// the load.
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
	// cmp.Less / cmp.Compare (not raw < / ==): the sort comparator must
	// be a total order or NaN inputs land in unspecified positions, and
	// the grouping loop below would never advance past a NaN pair
	// (NaN == NaN is false), appending empty entries forever.
	sort.Slice(pairs, func(a, b int) bool { return cmp.Less(pairs[a].v, pairs[b].v) })
	keys := make([]V, 0, len(pairs))
	bms := make([]*roaring64.Bitmap, 0, len(pairs))
	for k := 0; k < len(pairs); {
		j := k
		bm := roaring64.New()
		for j < len(pairs) && equalKeys(pairs[j].v, pairs[k].v) {
			bm.Add(uint64(pairs[j].n))
			j++
		}
		keys = append(keys, pairs[k].v)
		bms = append(bms, bm)
		k = j
	}
	tree := newBplus[V]()
	tree.bulkPack(keys, bms)
	i.mu.Lock()
	i.tree = tree
	i.mu.Unlock()
	return nil
}

// isNaN reports whether v is an IEEE 754 NaN — the only value that
// differs from itself. For non-floating-point instantiations the
// comparison is constant-false and the compiler eliminates it.
//
//nolint:gocritic // dupSubExpr: v != v is the canonical generic NaN test (mirrors stdlib cmp.isNaN).
func isNaN[V cmp.Ordered](v V) bool { return v != v }

// equalKeys reports whether two keys are equal under the total order:
// IEEE == everywhere, except that any two NaNs are equal.
func equalKeys[V cmp.Ordered](a, b V) bool {
	if isNaN(a) || isNaN(b) {
		return isNaN(a) && isNaN(b)
	}
	return a == b
}

// Lower-bound search strategy (shared by every method below): the B+
// tree is ordered under the [cmp.Compare] total order, and every
// descent and leaf search goes through that same comparator (keyCompare
// / keyLess in bplus.go). The total order places the single deduplicated
// NaN key before every other value — including -Inf — so it falls out as
// the leftmost key with no special-casing in the tree mechanics: a NaN
// probe lands on it like any other key, and any non-NaN lower bound
// excludes it because NaN < every real key. The NaN rule lives entirely
// inside the comparator (task #1354).

// Insert records that node carries value. Keys follow the total
// order described in the package documentation, so a floating-point
// NaN is a valid key: it sorts before every other value and all NaN
// bit patterns share one entry. Inserting a new distinct key is
// O(log n).
func (i *Index[V]) Insert(value V, node graph.NodeID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.tree.insert(value, uint64(node))
}

// Delete removes node from the set associated with value. No-op when
// absent. The (value, bitmap) entry is removed when its bitmap
// becomes empty, and a leaf that becomes entirely empty is unlinked
// (see the delete policy in bplus.go). Like [Index.Insert], value is
// matched under the total order, so Delete addresses a NaN-keyed entry.
func (i *Index[V]) Delete(value V, node graph.NodeID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.tree.remove(value, uint64(node))
}

// RangeFirst returns the first NodeID in the smallest indexed value
// not less than lo and not greater than hi, plus that value. The
// second return value reports whether any match exists. It is the
// allocation-free way to peek the first row of a range scan; the
// full union of matches is available via [Index.Range]. Bounds
// compare under the total order, so lo = NaN admits a NaN key while
// any non-NaN lo excludes it.
func (i *Index[V]) RangeFirst(lo, hi V) (V, graph.NodeID, bool) {
	var zeroV V
	if cmp.Less(hi, lo) {
		return zeroV, 0, false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	l, off := i.tree.lowerBound(lo)
	if l == nil || cmp.Less(hi, l.keys[off]) {
		return zeroV, 0, false
	}
	first := l.bms[off].Minimum()
	return l.keys[off], graph.NodeID(first), true
}

// Range returns a Roaring bitmap that is the union of the per-value
// bitmaps for every key v with lo <= v <= hi under the total order.
// The returned bitmap is freshly allocated; the caller owns it. A NaN
// key is below every other value, so any range with a non-NaN lo —
// including Range(math.Inf(-1), math.Inf(1)) — never returns it.
func (i *Index[V]) Range(lo, hi V) *roaring64.Bitmap {
	out := roaring64.New()
	if cmp.Less(hi, lo) {
		return out
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	l, off := i.tree.lowerBound(lo)
	for l != nil {
		for k := off; k < len(l.keys); k++ {
			if cmp.Less(hi, l.keys[k]) {
				return out
			}
			out.Or(l.bms[k])
		}
		l, off = l.next, 0
	}
	return out
}

// Lookup returns a clone of the bitmap associated with value, or an
// empty bitmap when value is unknown. Matching uses the total order,
// so Lookup(NaN) returns the NaN entry when one exists.
func (i *Index[V]) Lookup(value V) *roaring64.Bitmap {
	i.mu.RLock()
	defer i.mu.RUnlock()
	bm := i.tree.get(value)
	if bm == nil {
		return roaring64.New()
	}
	return bm.Clone()
}

// Cardinality returns the number of NodeIDs associated with value,
// matched under the total order (see [Index.Lookup]).
func (i *Index[V]) Cardinality(value V) uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	bm := i.tree.get(value)
	if bm == nil {
		return 0
	}
	return bm.GetCardinality()
}

// DistinctValues returns the number of distinct values currently
// indexed. It is O(1): the tree maintains a running key count.
func (i *Index[V]) DistinctValues() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.tree.count
}

// Kind returns "btree" — satisfies [index.Subscriber].
func (*Index[V]) Kind() string { return "btree" }

// Apply maintains a bound index (see [NewBound]) from the [index.Manager]
// change fan-out; it is a no-op for an unbound index (see [New]), which cannot
// reliably interpret arbitrary [index.Change] values without the
// caller-supplied binding (property key + value-type coercion). The bound
// rules live in [Index.applyBound] (bound.go).
func (i *Index[V]) Apply(c index.Change) {
	if i.binding == nil {
		return
	}
	i.applyBound(c)
}

// RangeCount returns the exact number of NodeIDs whose value falls within the
// inclusive interval [lo, hi] under the total order, but stops accumulating as
// soon as the running total exceeds budget and returns (budget+1, false) — the
// caller learns only that the count is "more than budget" without paying to
// walk the whole range. When the full count is ≤ budget it is returned with
// exact == true.
//
// The entries are pairwise-disjoint node-sets (each node carries exactly one
// value for the property), so the sum of per-entry cardinalities equals the
// union cardinality exactly, with no allocation and no union materialisation
// (graph-theory-expert, #1505). The early-exit bounds the gate cost to
// O(budget) cardinality probes regardless of how many distinct values the
// range spans, which keeps a non-selective range cheap to reject.
func (i *Index[V]) RangeCount(lo, hi V, budget uint64) (count uint64, exact bool) {
	if cmp.Less(hi, lo) {
		return 0, true
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	l, off := i.tree.lowerBound(lo)
	var total uint64
	for l != nil {
		for k := off; k < len(l.keys); k++ {
			if cmp.Less(hi, l.keys[k]) {
				return total, true
			}
			total += l.bms[k].GetCardinality()
			if total > budget {
				return budget + 1, false
			}
		}
		l, off = l.next, 0
	}
	return total, true
}

// btreeMagic is the four-byte magic at the head of a serialised
// btree index ('SBTR' little-endian — 0x52544253).
const btreeMagic uint32 = 0x52544253

// btreeFormatVersion is the on-disk format version of a serialised
// btree index.
const btreeFormatVersion uint32 = 1

// btreeCapHintMax caps the eager slice reservation in Deserialize so a
// hostile entryCount (up to the 1<<40 implausibility ceiling) cannot drive
// a multi-terabyte allocation before the per-entry reads have a chance to
// fail on a truncated file. It mirrors the safe sibling ceiling used by
// store/snapshot/tombstones.bin and constraints.bin (1<<20). A legitimately
// large index is unaffected: the slice grows via append.
const btreeCapHintMax uint64 = 1 << 20

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

	if err := binary.Write(tee, binary.LittleEndian, uint64(i.tree.count)); err != nil {
		return err
	}
	// Walk the leaf chain low→high so entries are emitted in ascending key
	// order — the byte-identical layout the v1 sorted-array writer produced
	// (the wire format is a logical key→nodes mapping; the tree shape is not
	// serialised). storage-engine-auditor #1514: formatVersion stays 1.
	for l := i.tree.first; l != nil; l = l.next {
		for k := range l.keys {
			key, err := encodeOrdered(l.keys[k])
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
			ids := l.bms[k].ToArray()
			if err := binary.Write(tee, binary.LittleEndian, uint64(len(ids))); err != nil {
				return err
			}
			if err := binary.Write(tee, binary.LittleEndian, ids); err != nil {
				return err
			}
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
// Keys must be STRICTLY ascending under the [cmp.Compare] total order
// — the only shape [Index.Serialize] produces. A payload that
// violates it fails fail-stop with [index.ErrIndexCorrupted] rather
// than load an index whose binary searches would silently miss live
// keys. In particular, a float64 payload written before the
// total-order fix (task #1354) that carries a NaN key after a real
// key, or duplicate NaN entries, is rejected; the index is derived
// data, so the caller recovers by rebuilding it from the primary
// graph. A single NaN entry in the leading position is the legitimate
// post-fix encoding and loads normally.
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

	// Clamp the eager reservation: a hostile entryCount (up to the 1<<40
	// implausibility ceiling) must not pre-allocate a multi-terabyte buffer
	// before the per-entry reads fail on a truncated file (storage-engine-
	// auditor #1514, mirroring #1480). The transient slices grow via append;
	// the tree is built bottom-up from them only AFTER validation succeeds.
	hint := entryCount
	if hint > btreeCapHintMax {
		hint = btreeCapHintMax
	}
	keys := make([]V, 0, hint)
	bms := make([]*roaring64.Bitmap, 0, hint)
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
		if hasPrev && cmp.Compare(v, prev) <= 0 {
			return fmt.Errorf("%w: keys not in strictly ascending order",
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
		keys = append(keys, v)
		bms = append(bms, bm)
	}

	// Build the tree bottom-up from the validated, strictly-ascending entries
	// in O(n). The strict-ascending check above gated admission, so a corrupt
	// non-ascending payload never reaches this point (auditor condition C4a).
	tree := newBplus[V]()
	tree.bulkPack(keys, bms)
	i.mu.Lock()
	i.tree = tree
	i.mu.Unlock()
	return nil
}
