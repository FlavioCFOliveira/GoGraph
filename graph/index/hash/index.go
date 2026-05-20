// Package hash provides a sharded hash index from arbitrary
// comparable property values to the set of NodeIDs that carry them,
// represented as a 64-bit Roaring bitmap.
//
// The structure answers exact-match property predicates (for example
// "every node where email == 'x@y.com'") in O(1) average time. For
// range predicates use the B+ tree index in package
// gograph/graph/index/btree (Sprint 2, T19).
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

	"gograph/graph"
	"gograph/graph/index"
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
}

type hashShard[V comparable] struct {
	mu      sync.RWMutex
	entries map[V]*roaring64.Bitmap
}

// New returns an empty hash index.
func New[V comparable]() *Index[V] {
	idx := &Index[V]{}
	for i := range idx.shards {
		idx.shards[i].entries = make(map[V]*roaring64.Bitmap)
	}
	return idx
}

func (i *Index[V]) shard(v V) *hashShard[V] {
	return &i.shards[maphash.Comparable(seed, v)&shardMask]
}

// Insert records that node carries the given value.
func (i *Index[V]) Insert(value V, node graph.NodeID) {
	s := i.shard(value)
	s.mu.Lock()
	bm, ok := s.entries[value]
	if !ok {
		bm = roaring64.New()
		s.entries[value] = bm
	}
	bm.Add(uint64(node))
	s.mu.Unlock()
}

// Delete removes node from the set associated with value. No-op if
// absent.
func (i *Index[V]) Delete(value V, node graph.NodeID) {
	s := i.shard(value)
	s.mu.Lock()
	if bm, ok := s.entries[value]; ok {
		bm.Remove(uint64(node))
		if bm.IsEmpty() {
			delete(s.entries, value)
		}
	}
	s.mu.Unlock()
}

// Lookup returns a clone of the Roaring bitmap of NodeIDs that carry
// the given value, or an empty bitmap when the value is unknown.
// Clone avoids returning the live bitmap to the caller, which could
// otherwise be mutated by concurrent writers.
func (i *Index[V]) Lookup(value V) *roaring64.Bitmap {
	s := i.shard(value)
	s.mu.RLock()
	bm, ok := s.entries[value]
	if !ok {
		s.mu.RUnlock()
		return roaring64.New()
	}
	out := bm.Clone()
	s.mu.RUnlock()
	return out
}

// Cardinality returns the number of NodeIDs associated with value.
// It is exposed for query planners to choose between index lookup
// and full-scan plans.
func (i *Index[V]) Cardinality(value V) uint64 {
	s := i.shard(value)
	s.mu.RLock()
	bm, ok := s.entries[value]
	if !ok {
		s.mu.RUnlock()
		return 0
	}
	c := bm.GetCardinality()
	s.mu.RUnlock()
	return c
}

// Contains reports whether node is in the set associated with value.
// Faster than Lookup when only existence matters.
func (i *Index[V]) Contains(value V, node graph.NodeID) bool {
	s := i.shard(value)
	s.mu.RLock()
	bm, ok := s.entries[value]
	if !ok {
		s.mu.RUnlock()
		return false
	}
	c := bm.Contains(uint64(node))
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

// Apply is a no-op for the generic hash index. The Manager fans
// every change to every subscriber, but the hash index cannot
// reliably interpret arbitrary [index.Change] values without
// caller-supplied bindings (property key + value-type coercion).
// Callers that need automatic fan-out into a hash index should wrap
// the index in a thin shim that does the projection.
//
// On recovery from a corrupted snapshot, the index is left empty;
// callers re-populate via [Index.Insert] from the live LPG.
func (*Index[V]) Apply(index.Change) {}

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
		for v, bm := range s.entries {
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
			ids := bm.ToArray()
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
		fresh[k].entries = make(map[V]*roaring64.Bitmap)
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
		bm := roaring64.New()
		bm.AddMany(ids)
		// Pick the shard the way the live Insert path would.
		sh := &fresh[maphash.Comparable(seed, v)&shardMask]
		sh.entries[v] = bm
	}

	// Atomic shard-by-shard swap.
	for k := range i.shards {
		i.shards[k].mu.Lock()
		i.shards[k].entries = fresh[k].entries
		i.shards[k].mu.Unlock()
	}
	return nil
}
