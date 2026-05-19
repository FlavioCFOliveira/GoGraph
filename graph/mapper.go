package graph

import (
	"hash/maphash"
	"sync"
)

// mapperShardCount is the number of independently locked shards used by
// every Mapper. It must be a power of two so the modulo operation on the
// shard index collapses to a bitwise AND.
const (
	mapperShardCount = 256
	mapperShardBits  = 8 // log2(mapperShardCount)
	mapperShardMask  = mapperShardCount - 1
)

// mapperSeed is the shared maphash seed used for routing comparable
// values to shards. A single process-wide seed is sufficient: collision
// resistance across mapper instances is not a security property of this
// package.
var mapperSeed = maphash.MakeSeed()

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

// mapperShardFor routes a comparable value to a shard index using the
// runtime's typehash, exposed via [hash/maphash.Comparable].
func mapperShardFor[N comparable](k N) uint64 {
	return maphash.Comparable(mapperSeed, k) & mapperShardMask
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
