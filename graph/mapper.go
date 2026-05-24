package graph

import (
	"fmt"
	"hash/fnv"
	"sync"
)

// FNV-1a 64-bit constants. Kept inline (not via hash/fnv) so the fast
// paths in [mapperShardFor] run zero-allocation; hash/fnv.New64a
// returns an interface and forces a heap allocation per call.
const (
	fnvOffset uint64 = 14695981039346656037
	fnvPrime  uint64 = 1099511628211
)

// fnv1aString hashes s with FNV-1a, byte by byte, without copying s
// into a temporary []byte. Suitable for the hot path of
// [mapperShardFor] when N=string.
func fnv1aString(s string) uint64 {
	h := fnvOffset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	return h
}

// fnv1aUint64 hashes a fixed-size 8-byte little-endian encoding of v.
// Used by the integer fast paths.
func fnv1aUint64(v uint64) uint64 {
	h := fnvOffset
	for i := 0; i < 8; i++ {
		h ^= v & 0xff
		h *= fnvPrime
		v >>= 8
	}
	return h
}

// mapperShardCount is the number of independently locked shards used by
// every Mapper. It must be a power of two so the modulo operation on the
// shard index collapses to a bitwise AND.
const (
	mapperShardCount = 256
	mapperShardBits  = 8 // log2(mapperShardCount)
	mapperShardMask  = mapperShardCount - 1
)

// MapperShardCount returns the number of independently locked shards
// every [Mapper] uses internally. Test and tooling code that needs to
// reason about shard placement at runtime — e.g. adversarial key
// generators that collapse many distinct keys into a single shard —
// should call this function instead of hardcoding the constant so the
// caller is automatically robust to future configuration changes.
func MapperShardCount() int { return mapperShardCount }

// MapperShardOf returns the shard index encoded in id. It is the
// public counterpart of [unpackNodeID] restricted to the shard
// component and exists so external tooling can validate placement
// without depending on the internal [packNodeID] layout.
func MapperShardOf(id NodeID) uint64 { return uint64(id) & mapperShardMask }

// Mapper interns user-facing identifiers of type N as compact [NodeID]
// values. Interning is stable for the lifetime of the Mapper: a value
// always resolves to the same NodeID and a NodeID always resolves back
// to the value that produced it.
//
// Mapper is safe for concurrent use by any number of goroutines. The
// implementation is sharded into 256 independent stripes keyed by the
// hash of N, so contention only arises between goroutines operating on
// values that collide on the lowest 8 bits of their hash.
//
// The zero value of Mapper is not usable; construct one with [NewMapper].
type Mapper[N comparable] struct {
	shards [mapperShardCount]mapperShard[N]
}

// mapperShard is one of the independently locked stripes of a Mapper.
// The forward map answers Intern; the reverse slice answers Resolve.
type mapperShard[N comparable] struct {
	mu      sync.RWMutex
	forward map[N]NodeID
	reverse []N
}

// NewMapper returns a fresh, empty Mapper ready for concurrent use.
func NewMapper[N comparable]() *Mapper[N] {
	m := &Mapper[N]{}
	for i := range m.shards {
		m.shards[i].forward = make(map[N]NodeID)
	}
	return m
}

// Intern returns the [NodeID] associated with k, allocating a new one
// on first encounter. Subsequent calls with the same value return the
// same NodeID. The fast path (k already interned) takes a read lock
// only and performs no heap allocation.
func (m *Mapper[N]) Intern(k N) NodeID {
	shardIdx := mapperShardFor(k)
	s := &m.shards[shardIdx]

	s.mu.RLock()
	id, ok := s.forward[k]
	s.mu.RUnlock()
	if ok {
		return id
	}
	return m.internSlow(s, shardIdx, k)
}

// internSlow is the write-locked slow path of [Mapper.Intern]. It is
// also called directly by concurrency tests to deterministically
// exercise the double-check that resolves the race between two
// goroutines that both miss the read-locked fast path for the same
// key.
func (m *Mapper[N]) internSlow(s *mapperShard[N], shardIdx uint64, k N) NodeID {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.forward[k]; ok {
		return id
	}
	idx := uint64(len(s.reverse))
	id := packNodeID(shardIdx, idx)
	s.reverse = append(s.reverse, k)
	s.forward[k] = id
	return id
}

// Lookup returns the [NodeID] previously assigned to k and true, or
// the zero NodeID and false when k has not been interned. The fast
// path holds a read lock only and performs no heap allocation. Unlike
// [Mapper.Intern], Lookup never mutates the Mapper, which makes it
// the right primitive for read-only operations (HasEdge, Neighbours,
// existence checks) on backends layered above the Mapper.
func (m *Mapper[N]) Lookup(k N) (NodeID, bool) {
	shardIdx := mapperShardFor(k)
	s := &m.shards[shardIdx]
	s.mu.RLock()
	id, ok := s.forward[k]
	s.mu.RUnlock()
	return id, ok
}

// Resolve returns the value previously interned under id, or the zero
// value of N and false when id was not produced by this Mapper.
func (m *Mapper[N]) Resolve(id NodeID) (N, bool) {
	// unpackNodeID always returns a shard index in [0, mapperShardCount)
	// thanks to the mask, so no further bounds check on shardIdx is
	// needed; the intra-shard index is bounds-checked against the
	// reverse slice length.
	shardIdx, idx := unpackNodeID(id)
	s := &m.shards[shardIdx]
	s.mu.RLock()
	defer s.mu.RUnlock()
	if idx >= uint64(len(s.reverse)) {
		var zero N
		return zero, false
	}
	return s.reverse[idx], true
}

// Walk invokes fn for every interned (NodeID, value) pair, taking
// each shard's RLock once for the whole iteration instead of once
// per Resolve call. Returns early when fn returns false.
//
// Concurrency: Walk holds each shard's RLock while iterating that
// shard, so concurrent Intern calls on the same shard block until
// Walk advances past it. Concurrent Resolve calls on the same shard
// also block (the read lock is held for the duration of the inner
// loop). Use Walk for bulk export where many Resolves would
// otherwise dominate; prefer Resolve for individual lookups.
func (m *Mapper[N]) Walk(fn func(NodeID, N) bool) {
	for shardIdx := uint64(0); shardIdx < mapperShardCount; shardIdx++ {
		s := &m.shards[shardIdx]
		s.mu.RLock()
		for intraIdx, v := range s.reverse {
			if !fn(packNodeID(shardIdx, uint64(intraIdx)), v) {
				s.mu.RUnlock()
				return
			}
		}
		s.mu.RUnlock()
	}
}

// Len returns the total number of values currently interned across
// every shard. The returned count is a consistent snapshot per shard
// but may not reflect concurrent inserts in other shards.
func (m *Mapper[N]) Len() int {
	n := 0
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		n += len(s.reverse)
		s.mu.RUnlock()
	}
	return n
}

// MaxNodeID returns one more than the largest [NodeID] that has been
// assigned by this Mapper. It is the natural size for an array
// indexed directly by NodeID (e.g. a CSR offsets array). Returns 0
// when no value has been interned.
func (m *Mapper[N]) MaxNodeID() NodeID {
	var maxIntra uint64
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		if uint64(len(s.reverse)) > maxIntra {
			maxIntra = uint64(len(s.reverse))
		}
		s.mu.RUnlock()
	}
	if maxIntra == 0 {
		return 0
	}
	// Last assignable intra-shard index is maxIntra-1. The largest
	// possible NodeID is therefore packed with that index and shard
	// 255. We return one more than that, so the result equals the
	// length of a NodeID-indexed array large enough to cover every id.
	return packNodeID(mapperShardCount-1, maxIntra-1) + 1
}

// mapperShardFor routes a comparable value to a shard index using a
// deterministic FNV-1a hash. The hash is stable across processes, so a
// snapshot written by one process and reopened by another agrees on the
// same NodeID for the same natural key — the prerequisite for
// cross-process snapshot+recovery without label drift in
// [snapshot.ApplyLabelsToGraph] / [snapshot.ApplyPropertiesToGraph].
//
// The previous implementation hashed via [hash/maphash.Comparable] with
// a process-local seed; the seed cannot be serialised (see the
// hash/maphash godoc), so durability of NodeID assignments required
// either persisting the mapper or replacing the hash. The FNV path
// here is the smaller fix and was selected after audit (Sprint 56 T3).
//
// Trade-off: FNV-1a does not resist hash flooding from attacker-
// controlled keys. The GoGraph mapper is an internal interning table
// behind a graph-level API; callers that expose natural keys to
// untrusted input should validate / rate-limit before [Mapper.Intern].
//
// Common comparable types (string, ints, fixed-size byte arrays) take
// dedicated zero-allocation fast paths driven by [fnv1aString] /
// [fnv1aUint64]; less common comparable types fall through to a
// [hash/fnv.New64a] + fmt.Fprintf path that handles arbitrary
// fmt-encodable values.
func mapperShardFor[N comparable](k N) uint64 {
	switch v := any(k).(type) {
	case string:
		return fnv1aString(v) & mapperShardMask
	case int:
		return fnv1aUint64(uint64(v)) & mapperShardMask
	case int8:
		return fnv1aUint64(uint64(uint8(v))) & mapperShardMask
	case int16:
		return fnv1aUint64(uint64(uint16(v))) & mapperShardMask
	case int32:
		return fnv1aUint64(uint64(uint32(v))) & mapperShardMask
	case int64:
		return fnv1aUint64(uint64(v)) & mapperShardMask
	case uint:
		return fnv1aUint64(uint64(v)) & mapperShardMask
	case uint8:
		return fnv1aUint64(uint64(v)) & mapperShardMask
	case uint16:
		return fnv1aUint64(uint64(v)) & mapperShardMask
	case uint32:
		return fnv1aUint64(uint64(v)) & mapperShardMask
	case uint64:
		return fnv1aUint64(v) & mapperShardMask
	case [16]byte: // UUID-style keys (txn.NewUUIDCodec)
		h := fnvOffset
		for i := 0; i < 16; i++ {
			h ^= uint64(v[i])
			h *= fnvPrime
		}
		return h & mapperShardMask
	default:
		// Fallback for less common comparable types (custom structs,
		// arrays, etc.). fmt.Sprintf into a fresh hash is acceptable
		// here because the hot-path types are covered above.
		h := fnv.New64a()
		_, _ = fmt.Fprintf(h, "%v", v)
		return h.Sum64() & mapperShardMask
	}
}

// packNodeID encodes a (shard, intra-shard index) pair into a NodeID.
// The shard index occupies the low mapperShardBits bits and the
// intra-shard index occupies the high bits.
func packNodeID(shard, idx uint64) NodeID {
	return NodeID((idx << mapperShardBits) | (shard & mapperShardMask))
}

// unpackNodeID is the inverse of [packNodeID].
func unpackNodeID(id NodeID) (shard, idx uint64) {
	v := uint64(id)
	return v & mapperShardMask, v >> mapperShardBits
}
