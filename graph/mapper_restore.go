package graph

import (
	"errors"
	"fmt"
	"sort"
)

// ErrMapperNotEmpty is returned by [Mapper.LoadFrom] when the caller
// tries to seed a Mapper that already holds at least one interned
// value. LoadFrom is intended for one-shot recovery initialisation
// against a fresh (zero-state) Mapper; reseeding a live Mapper would
// silently shadow its existing entries and is a programmer error.
var ErrMapperNotEmpty = errors.New("graph: Mapper.LoadFrom on non-empty mapper")

// ErrMapperEntryCorrupted is returned by [Mapper.LoadFrom] when the
// supplied entries violate the on-disk invariants the snapshot writer
// is responsible for upholding: an intra-shard index that disagrees
// with the natural key's hash-derived shard, a non-contiguous
// intra-shard slot sequence, or a duplicate (NodeID, key) record.
var ErrMapperEntryCorrupted = errors.New("graph: Mapper.LoadFrom entries corrupted")

// MapperEntry describes one (NodeID -> natural key) pair as serialised
// by the snapshot writer. The NodeID is packed so the unpacked shard
// index matches mapperShardFor(Key), and the intra-shard index agrees
// with the slot the key would occupy if all entries for that shard
// were interned in NodeID-ascending order. The snapshot writer
// guarantees both invariants by enumerating pairs via [Mapper.Walk].
type MapperEntry[N comparable] struct {
	ID  NodeID
	Key N
}

// LoadFrom rebuilds m's internal state from a snapshot's entries. It
// is intended for one-shot recovery initialisation against a fresh
// (zero-state) Mapper.
//
// The pre-conditions enforced are:
//
//  1. m must be empty in every shard ([ErrMapperNotEmpty] otherwise).
//  2. Each entry's NodeID, when unpacked, must yield a shard index
//     equal to mapperShardFor(entry.Key). The writer guarantees this
//     because it composes NodeIDs via [packNodeID] from the same hash.
//  3. After grouping entries by shard and sorting by intra-index,
//     intra-indexes must form the contiguous sequence 0..N-1. Any gap
//     surfaces as [ErrMapperEntryCorrupted].
//  4. Within a shard, no two entries may collide on the natural key.
//
// Post-condition: subsequent [Mapper.Intern] calls with a previously
// seeded key return the original NodeID; new keys get fresh slots
// after the seeded ones (the next intra-index is len(reverse)).
//
// LoadFrom is safe for concurrent goroutines only with respect to
// other LoadFrom calls (which would all fail with ErrMapperNotEmpty
// after the first); it must not run concurrently with any
// Intern/Lookup/Resolve/Walk call on the same Mapper.
func (m *Mapper[N]) LoadFrom(entries []MapperEntry[N]) error {
	// Pre-flight: every shard must be untouched. Walking under RLock
	// is cheap and catches the "reseed a live mapper" mistake at the
	// boundary instead of after we have mutated half the shards.
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		empty := len(s.forward) == 0 && len(s.reverse) == 0
		s.mu.RUnlock()
		if !empty {
			return ErrMapperNotEmpty
		}
	}

	// Group entries by their hash-derived shard. The writer enumerates
	// pairs in Walk order (shard-major, intra-index-major), so the
	// natural caller passes us already-bucketed data; we still rebucket
	// here to keep LoadFrom robust against callers that hand us entries
	// in an arbitrary order.
	type indexedEntry struct {
		intra uint64
		key   N
	}
	buckets := make([][]indexedEntry, mapperShardCount)
	for _, e := range entries {
		shardIdx, intraIdx := unpackNodeID(e.ID)
		expected := mapperShardFor(e.Key)
		if shardIdx != expected {
			return fmt.Errorf("%w: NodeID %d shard %d != mapperShardFor(key) %d",
				ErrMapperEntryCorrupted, uint64(e.ID), shardIdx, expected)
		}
		buckets[shardIdx] = append(buckets[shardIdx], indexedEntry{intra: intraIdx, key: e.Key})
	}

	// For every shard, sort by intra-index, assert contiguity and
	// uniqueness, then commit forward/reverse in one shot.
	for shardIdx := range buckets {
		bucket := buckets[shardIdx]
		if len(bucket) == 0 {
			continue
		}
		sort.Slice(bucket, func(i, j int) bool {
			return bucket[i].intra < bucket[j].intra
		})
		s := &m.shards[shardIdx]
		s.mu.Lock()
		// Pre-size to the exact count so subsequent inserts do not
		// reshuffle the underlying slice's backing array. The forward
		// map allocates one bucket per pair, which is the steady-state
		// cost regardless of how the mapper was originally populated.
		s.reverse = make([]N, 0, len(bucket))
		s.forward = make(map[N]NodeID, len(bucket))
		for i, ie := range bucket {
			if ie.intra != uint64(i) {
				s.mu.Unlock()
				return fmt.Errorf("%w: shard %d intra-index gap: got %d at slot %d",
					ErrMapperEntryCorrupted, shardIdx, ie.intra, i)
			}
			if _, dup := s.forward[ie.key]; dup {
				s.mu.Unlock()
				return fmt.Errorf("%w: shard %d duplicate key", ErrMapperEntryCorrupted, shardIdx)
			}
			id := packNodeID(uint64(shardIdx), ie.intra)
			s.reverse = append(s.reverse, ie.key)
			s.forward[ie.key] = id
		}
		s.mu.Unlock()
	}

	return nil
}
