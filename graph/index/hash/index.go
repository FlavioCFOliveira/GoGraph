// Package hash provides a sharded hash index from arbitrary
// comparable property values to the set of NodeIDs that carry them,
// represented as a 64-bit Roaring bitmap.
//
// The structure answers exact-match property predicates (for example
// "every node where email == 'x@y.com'") in O(1) average time. For
// range predicates use the B+ tree index in package
// github.com/FlavioCFOliveira/GoGraph/graph/index/btree (Sprint 2, T19).
//
// Index is safe for concurrent use by any number of goroutines; the
// shard sharding aligns with [graph.NodeID]'s low-bit shard scheme.
package hash

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"hash/maphash"
	"io"
	"math"
	"sort"
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

const (
	shardCount = 256
	shardMask  = shardCount - 1
)

var seed = maphash.MakeSeed()

// Index maps property values of type V to the NodeIDs that carry
// them.
type Index[V comparable] struct {
	shards [shardCount]hashShard[V]

	// binding, when non-nil, ties the index to one (label, property)
	// pair of a live node graph so [Index.Apply] can translate
	// [index.Change] events into typed Insert / Delete calls. It is set
	// once by [NewBound] before the index is shared and never mutated
	// afterwards, so Apply reads it without synchronisation.
	binding *Binding[V]
}

// Binding ties an [Index] to a single (label, property) pair of a live
// node graph. A bound index (see [NewBound]) maintains itself from the
// [index.Manager] change fan-out: property writes insert/delete typed
// keys, and label add/remove events attach/detach a node's current
// value. An unbound index (see [New]) ignores the fan-out entirely and
// is maintained by explicit [Index.Insert] / [Index.Delete] calls.
//
// The identifier fields carry interned IDs from the owning graph's
// registries; the callbacks close over the graph so this package stays
// free of a dependency on any concrete graph implementation. Because
// changes are fanned out at commit time — after the transaction's
// mutations were applied eagerly to the graph — the callbacks observe
// the transaction's FINAL state, which is exactly the state the index
// must converge to.
type Binding[V comparable] struct {
	// PropertyID is the interned property-key identifier this index
	// covers. Property changes whose Change.Property differs are
	// ignored.
	PropertyID uint32

	// LabelID is the interned label identifier this index is scoped
	// to. Label changes whose Change.Label differs are ignored. Note
	// that interned IDs start at zero, so this field alone cannot mark
	// an unscoped binding; bindings are always label-scoped.
	LabelID uint32

	// Label and Property are the source names behind PropertyID and
	// LabelID. They let a query planner match the index against a
	// (label, property) predicate without access to the registries.
	Label, Property string

	// Project converts a Change.OldValue / Change.NewValue payload to
	// the index key type. ok is false when the payload is absent or
	// not indexable (wrong kind), in which case the event is skipped
	// for that direction.
	Project func(v any) (V, bool)

	// Eligible reports whether the node should currently be present in
	// the index: it must be live (not deleted) and carry the bound
	// label, evaluated against the graph's final state.
	Eligible func(node graph.NodeID) bool

	// CurrentValue returns the node's current value for the bound
	// property, projected to the key type. ok is false when the node
	// is not live, lacks the property, or the value is not indexable.
	// It is consulted on label add/remove events, which carry no
	// property payload.
	CurrentValue func(node graph.NodeID) (V, bool)
}

// errBindingIncomplete is returned by [NewBound] when a required
// Binding field is missing.
var errBindingIncomplete = fmt.Errorf("%w: incomplete hash index binding", index.ErrIndexValueTypeUnsupported)

// NewBound returns an empty hash index bound to b. Unlike [New], the
// returned index has a functional [Index.Apply]: it subscribes to the
// node property and label changes selected by b and keeps itself
// consistent with the graph. Returns an error when b is missing its
// Label, Property, or any of the three callbacks.
func NewBound[V comparable](b Binding[V]) (*Index[V], error) {
	if b.Label == "" || b.Property == "" ||
		b.Project == nil || b.Eligible == nil || b.CurrentValue == nil {
		return nil, errBindingIncomplete
	}
	idx := New[V]()
	idx.binding = &b
	return idx, nil
}

// BoundNode returns the (label, property) pair this index is bound to,
// with ok reporting whether the index is bound at all. Query planners
// use it to decide whether the index may serve a predicate: a bound
// index covers exactly its (label, property) pair, while an unbound
// index carries no coverage metadata.
func (i *Index[V]) BoundNode() (label, property string, ok bool) {
	if i.binding == nil {
		return "", "", false
	}
	return i.binding.Label, i.binding.Property, true
}

type hashShard[V comparable] struct {
	mu      sync.RWMutex
	entries map[V]index.NodeSet
}

// New returns an empty hash index.
func New[V comparable]() *Index[V] {
	idx := &Index[V]{}
	for i := range idx.shards {
		idx.shards[i].entries = make(map[V]index.NodeSet)
	}
	return idx
}

func (i *Index[V]) shard(v V) *hashShard[V] {
	return &i.shards[maphash.Comparable(seed, v)&shardMask]
}

// nanKey reports whether v is a float32 or float64 IEEE 754 NaN.
// Go maps use the language == operator: NaN != NaN is always true, so
// inserting a NaN key creates an entry that can never be looked up or
// deleted, causing unbounded accumulation (task #1408). The Insert,
// Delete and Lookup methods skip NaN values entirely to prevent this.
//
//nolint:gocritic // dupSubExpr: f != f is the canonical generic NaN test.
func nanKey[V comparable](v V) bool {
	switch f := any(v).(type) {
	case float64:
		return f != f
	case float32:
		return f != f
	}
	return false
}

// Insert records that node carries the given value. Insert is a no-op
// when value is a float32 or float64 NaN: Go map equality is language-
// fixed (NaN != NaN), so a NaN map key can never be looked up or
// deleted; skipping it prevents unbounded accumulation (task #1408).
func (i *Index[V]) Insert(value V, node graph.NodeID) {
	if nanKey(value) {
		return
	}
	s := i.shard(value)
	s.mu.Lock()
	// NodeSet is stored by value; read-modify-write so an inline-tier
	// mutation (which updates the struct's fields/slice) is recorded. A
	// bitmap-tier Add mutates the shared *roaring64.Bitmap in place, so the
	// store-back is a harmless no-op in that state.
	set := s.entries[value]
	set.Add(uint64(node))
	s.entries[value] = set
	s.mu.Unlock()
}

// Delete removes node from the set associated with value. No-op if
// absent or if value is a NaN (see [Index.Insert] for the rationale).
func (i *Index[V]) Delete(value V, node graph.NodeID) {
	if nanKey(value) {
		return
	}
	s := i.shard(value)
	s.mu.Lock()
	if set, ok := s.entries[value]; ok {
		if set.Remove(uint64(node)) {
			delete(s.entries, value)
		} else {
			s.entries[value] = set
		}
	}
	s.mu.Unlock()
}

// Lookup returns a clone of the Roaring bitmap of NodeIDs that carry
// the given value, or an empty bitmap when the value is unknown or is
// a NaN (see [Index.Insert] for the rationale).
// Clone avoids returning the live bitmap to the caller, which could
// otherwise be mutated by concurrent writers.
func (i *Index[V]) Lookup(value V) *roaring64.Bitmap {
	if nanKey(value) {
		return roaring64.New()
	}
	s := i.shard(value)
	s.mu.RLock()
	set, ok := s.entries[value]
	if !ok {
		s.mu.RUnlock()
		return roaring64.New()
	}
	bm, shared := set.Bitmap()
	if shared {
		// Bitmap state: the returned bitmap aliases the live one, so clone
		// it under the lock to give the caller an independent copy that a
		// later writer cannot mutate (the pre-refactor Lookup contract).
		out := bm.Clone()
		s.mu.RUnlock()
		return out
	}
	// Inline state: Bitmap already materialised a fresh, caller-owned
	// bitmap — no clone needed.
	s.mu.RUnlock()
	return bm
}

// Cardinality returns the number of NodeIDs associated with value.
// It is exposed for query planners to choose between index lookup
// and full-scan plans.
func (i *Index[V]) Cardinality(value V) uint64 {
	s := i.shard(value)
	s.mu.RLock()
	set, ok := s.entries[value]
	if !ok {
		s.mu.RUnlock()
		return 0
	}
	c := set.Cardinality()
	s.mu.RUnlock()
	return c
}

// Contains reports whether node is in the set associated with value.
// Faster than Lookup when only existence matters.
func (i *Index[V]) Contains(value V, node graph.NodeID) bool {
	s := i.shard(value)
	s.mu.RLock()
	set, ok := s.entries[value]
	if !ok {
		s.mu.RUnlock()
		return false
	}
	c := set.Contains(uint64(node))
	s.mu.RUnlock()
	return c
}

// DistinctValues returns the number of distinct values currently
// indexed. Exposed for cardinality estimation by the query planner.
func (i *Index[V]) DistinctValues() uint64 {
	var n uint64
	for k := range i.shards {
		s := &i.shards[k]
		s.mu.RLock()
		n += uint64(len(s.entries))
		s.mu.RUnlock()
	}
	return n
}

// Kind returns "hash" — satisfies [index.Subscriber].
func (*Index[V]) Kind() string { return "hash" }

// Apply maintains a bound index (see [NewBound]) from the
// [index.Manager] change fan-out; it is a no-op for an unbound index
// (see [New]), which cannot reliably interpret arbitrary
// [index.Change] values without the caller-supplied binding (property
// key + value-type coercion).
//
// For a bound index the rules are, per change:
//
//   - SetNodeProperty on the bound property: the old value (when
//     present and projectable) is deleted unconditionally, and the new
//     value is inserted when the node is eligible in the graph's final
//     state. The unconditional old-value delete is what clears a stale
//     entry even when a label removal in the same batch is replayed
//     before the property change.
//   - DelNodeProperty on the bound property: the old value is deleted.
//   - Add/RemoveNodeLabel on the bound label: the node's CURRENT
//     property value is inserted / deleted. Because changes are
//     applied at commit time the current value is the transaction's
//     final value, so an interleaved property change in the same batch
//     converges to the same final state regardless of replay order.
//
// Apply is idempotent (bitmap add/remove) and safe for concurrent use
// with readers; writers are serialised upstream by the engine's
// single-writer transaction contract. Edge changes and changes for
// other properties/labels are ignored.
//
// On recovery from a corrupted snapshot, the index is left empty;
// callers re-populate via [Index.Insert] from the live LPG.
func (i *Index[V]) Apply(c index.Change) {
	b := i.binding
	if b == nil {
		return
	}
	switch c.Op {
	case index.OpSetNodeProperty:
		if c.Property != b.PropertyID {
			return
		}
		if old, ok := b.Project(c.OldValue); ok {
			i.Delete(old, c.Node)
		}
		if nv, ok := b.Project(c.NewValue); ok && b.Eligible(c.Node) {
			i.Insert(nv, c.Node)
		}
	case index.OpDelNodeProperty:
		if c.Property != b.PropertyID {
			return
		}
		if old, ok := b.Project(c.OldValue); ok {
			i.Delete(old, c.Node)
		}
	case index.OpAddNodeLabel:
		if c.Label != b.LabelID {
			return
		}
		if v, ok := b.CurrentValue(c.Node); ok && b.Eligible(c.Node) {
			i.Insert(v, c.Node)
		}
	case index.OpRemoveNodeLabel:
		if c.Label != b.LabelID {
			return
		}
		if v, ok := b.CurrentValue(c.Node); ok {
			i.Delete(v, c.Node)
		}
	}
}

// hashMagic is the four-byte magic at the head of a serialised hash
// index ('SHSH' little-endian — 0x48534853).
const hashMagic uint32 = 0x48534853

// hashFormatVersion is the on-disk format version of a serialised
// hash index.
const hashFormatVersion uint32 = 1

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// encodeValue serialises one supported value type to bytes. The
// generic Index[V] supports value-type encoding for the most common
// LPG property kinds; other types return
// [index.ErrIndexValueTypeUnsupported]. Callers that need to
// persist an index keyed by an exotic V should convert to one of
// the supported types before registering the index for snapshot.
//
// Supported types and their wire form:
//
//	string   -> raw utf-8 bytes
//	[]byte   -> raw bytes
//	int64    -> 8 bytes little-endian two's-complement
//	int32    -> 4 bytes little-endian
//	uint64   -> 8 bytes little-endian
//	uint32   -> 4 bytes little-endian
//	float64  -> 8 bytes math.Float64bits little-endian
//	bool     -> 1 byte (0x00 / 0x01)
func encodeValue[V comparable](v V) ([]byte, error) {
	switch x := any(v).(type) {
	case string:
		return []byte(x), nil
	case []byte:
		return x, nil
	case int64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(x))
		return buf[:], nil
	case int32:
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], uint32(x))
		return buf[:], nil
	case uint64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], x)
		return buf[:], nil
	case uint32:
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], x)
		return buf[:], nil
	case float64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(x))
		return buf[:], nil
	case bool:
		if x {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	}
	return nil, fmt.Errorf("%w: %T", index.ErrIndexValueTypeUnsupported, v)
}

// decodeValue is the inverse of [encodeValue]. It is generic over V
// and works by populating a zero V of the right kind from the buffer.
// Like encodeValue it supports the subset of types documented above;
// any other V returns [index.ErrIndexValueTypeUnsupported].
//
//nolint:gocyclo // type switch over supported value kinds
func decodeValue[V comparable](b []byte) (V, error) {
	var zero V
	switch any(zero).(type) {
	case string:
		var out V
		// safe: V is string here
		assignAny(&out, string(b))
		return out, nil
	case []byte:
		var out V
		cp := make([]byte, len(b))
		copy(cp, b)
		assignAny(&out, cp)
		return out, nil
	case int64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: int64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := int64(binary.LittleEndian.Uint64(b))
		var out V
		assignAny(&out, v)
		return out, nil
	case int32:
		if len(b) != 4 {
			return zero, fmt.Errorf("%w: int32 wants 4 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := int32(binary.LittleEndian.Uint32(b))
		var out V
		assignAny(&out, v)
		return out, nil
	case uint64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: uint64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := binary.LittleEndian.Uint64(b)
		var out V
		assignAny(&out, v)
		return out, nil
	case uint32:
		if len(b) != 4 {
			return zero, fmt.Errorf("%w: uint32 wants 4 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := binary.LittleEndian.Uint32(b)
		var out V
		assignAny(&out, v)
		return out, nil
	case float64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: float64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(b))
		var out V
		assignAny(&out, v)
		return out, nil
	case bool:
		if len(b) != 1 {
			return zero, fmt.Errorf("%w: bool wants 1 byte, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, b[0] != 0)
		return out, nil
	}
	return zero, fmt.Errorf("%w: %T", index.ErrIndexValueTypeUnsupported, zero)
}

// assignAny copies src into *dst, treating dst as an any. The
// caller must guarantee dst's concrete type matches src.
func assignAny[V any](dst *V, src any) {
	*dst = src.(V)
}

// Serialize writes every (value, NodeID-set) pair currently in the
// index to w in the format documented in docs/persistence.md:
//
//	uint32 magic ('SHSH')
//	uint32 formatVersion
//	uint64 entryCount
//	repeat entryCount times:
//	  uint32 valueLen
//	  [valueLen]byte value (kind-specific encoding)
//	  uint64 idCount
//	  [idCount]uint64 NodeIDs (sorted ascending)
//	uint32 crc32c (little-endian, covers every byte above)
//
// Returns [index.ErrIndexValueTypeUnsupported] when V is not one of
// the documented supported types.
func (i *Index[V]) Serialize(w io.Writer) error {
	type entry struct {
		key []byte
		ids []uint64
	}
	// Snapshot every shard under its RLock and materialise into a
	// flat slice. We sort the slice by raw key bytes for
	// deterministic output (helps fixture diffs and test stability).
	var entries []entry
	for k := range i.shards {
		s := &i.shards[k]
		s.mu.RLock()
		if entries == nil {
			entries = make([]entry, 0, len(s.entries))
		}
		for v, set := range s.entries {
			b, err := encodeValue(v)
			if err != nil {
				s.mu.RUnlock()
				return err
			}
			// Clone the bytes so we do not retain references into the
			// shard map's key (string headers can be aliased safely
			// but []byte keys are not allowed for comparable maps).
			cp := make([]byte, len(b))
			copy(cp, b)
			// ToArray is the sorted-ascending logical NodeID list — the
			// representation-independent wire form, identical to the
			// pre-refactor bm.ToArray().
			ids := set.ToArray()
			entries = append(entries, entry{key: cp, ids: ids})
		}
		s.mu.RUnlock()
	}
	sort.Slice(entries, func(a, b int) bool {
		return bytes.Compare(entries[a].key, entries[b].key) < 0
	})

	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, hashMagic); err != nil {
		return err
	}
	if err := binary.Write(tee, binary.LittleEndian, hashFormatVersion); err != nil {
		return err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(entries))); err != nil {
		return err
	}
	for k := range entries {
		if uint64(len(entries[k].key)) > uint64(^uint32(0)) {
			return fmt.Errorf("hash: value too long to serialize: %d", len(entries[k].key))
		}
		if err := binary.Write(tee, binary.LittleEndian, uint32(len(entries[k].key))); err != nil {
			return err
		}
		if _, err := tee.Write(entries[k].key); err != nil {
			return err
		}
		if err := binary.Write(tee, binary.LittleEndian, uint64(len(entries[k].ids))); err != nil {
			return err
		}
		if err := binary.Write(tee, binary.LittleEndian, entries[k].ids); err != nil {
			return err
		}
	}

	if err := binary.Write(bw, binary.LittleEndian, hasher.Sum32()); err != nil {
		return err
	}
	return bw.Flush()
}

// Deserialize replaces the receiver's state with the contents of r.
// Returns [index.ErrIndexCorrupted] on structural or CRC errors and
// [index.ErrIndexValueTypeUnsupported] when V cannot be decoded.
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
	if magic != hashMagic {
		return fmt.Errorf("%w: bad magic %#x", index.ErrIndexCorrupted, magic)
	}
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		return fmt.Errorf("%w: version: %w", index.ErrIndexCorrupted, err)
	}
	if version != hashFormatVersion {
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

	// Build into a fresh shards array, then atomically swap in.
	var fresh [shardCount]hashShard[V]
	for k := range fresh {
		fresh[k].entries = make(map[V]index.NodeSet)
	}

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
		v, derr := decodeValue[V](kbuf)
		if derr != nil {
			return derr
		}
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
		// The writer emits ids in sorted-ascending ToArray order, so
		// NodeSetFromSorted picks the cheapest representation for this
		// cardinality without a re-sort. Ownership of ids transfers.
		// Pick the shard the way the live Insert path would.
		sh := &fresh[maphash.Comparable(seed, v)&shardMask]
		sh.entries[v] = index.NodeSetFromSorted(ids)
	}

	// Atomic shard-by-shard swap.
	for k := range i.shards {
		i.shards[k].mu.Lock()
		i.shards[k].entries = fresh[k].entries
		i.shards[k].mu.Unlock()
	}
	return nil
}
