package graph

import (
	"fmt"
	"hash/fnv"
	"sync"
	"unsafe"
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
	// kind classifies N once at construction so the per-call shard hash can
	// dispatch on a plain integer instead of boxing the key into interface{}.
	// Boxing a string into any() heap-allocates its 16-byte header on every
	// call; on the string-keyed Cypher/LPG read path that single allocation
	// dominated per-row cost. kind is set once in NewMapper and never mutated,
	// so concurrent reads in shardFor are race-free.
	kind keyKind
}

// keyKind enumerates the concrete key types that [Mapper.shardFor] can hash
// without boxing. Types not covered here (named string/int types, custom
// structs) fall through to [mapperShardFor], which boxes once via a type
// switch — acceptable because those types are not on any hot path.
type keyKind uint8

const (
	kindOther keyKind = iota
	kindString
	kindInt
	kindInt8
	kindInt16
	kindInt32
	kindInt64
	kindUint
	kindUint8
	kindUint16
	kindUint32
	kindUint64
	kind16Bytes
)

// detectKeyKind classifies N exactly once (at Mapper construction). The single
// any() boxing here is amortised over the Mapper's whole lifetime. A type
// switch matches exact types, not underlying types, so a defined type such as
// `type Key string` resolves to kindOther and uses the boxing fallback — the
// same behaviour [mapperShardFor] already had for such types.
func detectKeyKind[N comparable]() keyKind {
	var zero N
	switch any(zero).(type) {
	case string:
		return kindString
	case int:
		return kindInt
	case int8:
		return kindInt8
	case int16:
		return kindInt16
	case int32:
		return kindInt32
	case int64:
		return kindInt64
	case uint:
		return kindUint
	case uint8:
		return kindUint8
	case uint16:
		return kindUint16
	case uint32:
		return kindUint32
	case uint64:
		return kindUint64
	case [16]byte:
		return kind16Bytes
	default:
		return kindOther
	}
}

// shardFor selects the shard for key k. It produces the identical index to
// [mapperShardFor] (same FNV-1a hash, same mask) but avoids boxing k into
// interface{} on the hot path: the key's concrete type is known from m.kind,
// so the bytes of k are reinterpreted directly via unsafe.Pointer. This is
// sound because m.kind was derived from N's exact dynamic type, so the
// reinterpretation matches N's memory layout precisely. The shared FNV
// helpers guarantee shard placement stays byte-for-byte stable across
// processes, which the persistence/restore path depends on.
func (m *Mapper[N]) shardFor(k N) uint64 {
	// Each unsafe reinterpretation below is sound because m.kind was derived
	// from N's exact dynamic type in detectKeyKind, so &k addresses memory whose
	// layout matches the target pointer type precisely. The pointer never
	// escapes (the value is read, hashed, and discarded), so no allocation or
	// aliasing hazard arises. The //nolint:gosec markers acknowledge the
	// project's audit policy for intentional zero-copy reinterpretation.
	switch m.kind {
	case kindString:
		return fnv1aString(*(*string)(unsafe.Pointer(&k))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of a string key
	case kindInt:
		return fnv1aUint64(uint64(*(*int)(unsafe.Pointer(&k)))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of an int key
	case kindInt8:
		return fnv1aUint64(uint64(uint8(*(*int8)(unsafe.Pointer(&k))))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of an int8 key
	case kindInt16:
		return fnv1aUint64(uint64(uint16(*(*int16)(unsafe.Pointer(&k))))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of an int16 key
	case kindInt32:
		return fnv1aUint64(uint64(uint32(*(*int32)(unsafe.Pointer(&k))))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of an int32 key
	case kindInt64:
		return fnv1aUint64(uint64(*(*int64)(unsafe.Pointer(&k)))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of an int64 key
	case kindUint:
		return fnv1aUint64(uint64(*(*uint)(unsafe.Pointer(&k)))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of a uint key
	case kindUint8:
		return fnv1aUint64(uint64(*(*uint8)(unsafe.Pointer(&k)))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of a uint8 key
	case kindUint16:
		return fnv1aUint64(uint64(*(*uint16)(unsafe.Pointer(&k)))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of a uint16 key
	case kindUint32:
		return fnv1aUint64(uint64(*(*uint32)(unsafe.Pointer(&k)))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of a uint32 key
	case kindUint64:
		return fnv1aUint64(*(*uint64)(unsafe.Pointer(&k))) & mapperShardMask //nolint:gosec // G103: audited reinterpretation of a uint64 key
	case kind16Bytes:
		v := *(*[16]byte)(unsafe.Pointer(&k)) //nolint:gosec // G103: audited reinterpretation of a [16]byte key
		h := fnvOffset
		for i := 0; i < 16; i++ {
			h ^= uint64(v[i])
			h *= fnvPrime
		}
		return h & mapperShardMask
	default:
		// Exotic key types (named types, structs): one boxing per call, but
		// these never appear on a hot path.
		return mapperShardFor(k)
	}
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
	m := &Mapper[N]{kind: detectKeyKind[N]()}
	for i := range m.shards {
		m.shards[i].forward = make(map[N]NodeID)
	}
	return m
}

// Reserve pre-allocates interning capacity for an expected total of n
// distinct keys, spread evenly across the Mapper's shards. It is a pure
// capacity hint: it never assigns a NodeID, never changes the NodeID a
// future [Mapper.Intern] will assign, and never changes iteration or
// resolution order. Calling Reserve only reduces the number of map and
// slice re-growths the subsequent Intern calls incur, which is the
// project's "pre-size all slices and maps" mandate applied to bulk
// ingest. A non-positive n is a no-op.
//
// Because keys hash uniformly across the 256 shards, the per-shard
// reservation is ceil(n / shardCount); a heavily skewed key
// distribution may still grow some shards past the hint, which is
// harmless. Reserve takes each shard's write lock in turn and is safe
// for concurrent use, though it is intended to be called once, before
// ingest begins, on a quiescent Mapper.
func (m *Mapper[N]) Reserve(n int) {
	if n <= 0 {
		return
	}
	perShard := (n + mapperShardCount - 1) / mapperShardCount
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		// Only grow: never discard already-interned state. A map cannot be
		// shrunk in place, and the reverse slice is grown via a capacity-
		// preserving reallocation when it is still smaller than the hint.
		if len(s.forward) < perShard {
			grown := make(map[N]NodeID, perShard)
			for k, v := range s.forward {
				grown[k] = v
			}
			s.forward = grown
		}
		if cap(s.reverse) < perShard {
			grown := make([]N, len(s.reverse), perShard)
			copy(grown, s.reverse)
			s.reverse = grown
		}
		s.mu.Unlock()
	}
}

// Intern returns the [NodeID] associated with k, allocating a new one
// on first encounter. Subsequent calls with the same value return the
// same NodeID. The fast path (k already interned) takes a read lock
// only and performs no heap allocation.
func (m *Mapper[N]) Intern(k N) NodeID {
	shardIdx := m.shardFor(k)
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
	shardIdx := m.shardFor(k)
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
//
// The callback must not re-enter this Mapper — directly or through
// any structure layered above it (Lookup, Intern, Resolve, Len) —
// while a concurrent writer may be running: a key walked in shard S
// re-locks shard S on Lookup, and once a writer's Intern queues on
// that shard's write lock, sync.RWMutex admits no new readers, so
// the nested read lock deadlocks the callback, the writer, and every
// future operation on the shard. Snapshot the (NodeID, value) pairs
// inside the callback and resolve any dependent state after Walk
// returns instead (see cypher task #1339).
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
