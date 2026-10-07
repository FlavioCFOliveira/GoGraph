package graph

import (
	"errors"
	"fmt"
	"reflect"
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
// with the natural key's hash-derived shard, an intra-shard index at or
// above the shard's recorded high-water mark, or a duplicate key.
var ErrMapperEntryCorrupted = errors.New("graph: Mapper.LoadFrom entries corrupted")

// ErrMapperKeyNotPortable is returned by [Mapper.LoadFrom], wrapped in
// [ErrMapperEntryCorrupted], when the shard disagreement it detected is
// explained not by a damaged snapshot but by the KEY TYPE: the key carries a
// pointer, an unsafe.Pointer or a channel, so its value is an ADDRESS and cannot
// be reproduced in another process.
//
// Such a key is unusable as a persisted natural key, and not only because of the
// hash. In Go, a comparable type containing a pointer compares by ADDRESS, so a
// key decoded by [encoding.BinaryUnmarshaler] into a fresh allocation is not
// equal to the key that was written even when it carries the same data — its
// identity, not merely its shard, is unreproducible. The sentinel exists so that
// diagnosis lands on the key type instead of on the snapshot writer or the disk,
// which is where "entries corrupted" alone had pointed it (rmp #2528).
var ErrMapperKeyNotPortable = errors.New("graph: mapper key type is address-dependent and cannot be persisted")

// MapperEntry describes one (NodeID -> natural key) pair as serialised
// by the snapshot writer. The NodeID is packed so the unpacked shard
// index matches mapperShardFor(Key), and the intra-shard index agrees
// with the slot the key would occupy if all entries for that shard
// were interned in NodeID-ascending order. The snapshot writer
// guarantees both invariants by enumerating pairs via [Mapper.Walk].
type MapperEntry[N comparable] struct {
	Key N
	ID  NodeID
}

// LoadFrom rebuilds m's internal state from a snapshot's entries and the
// per-shard high-water marks next (the snapshot's nodeids.bin). It is intended
// for one-shot recovery initialisation against a fresh (zero-state) Mapper.
//
// The pre-conditions enforced are:
//
//  1. m must be empty in every shard ([ErrMapperNotEmpty] otherwise).
//  2. Each entry's NodeID, when unpacked, must yield a shard index
//     equal to mapperShardFor(entry.Key). The writer guarantees this
//     because it composes NodeIDs via [packNodeID] from the same hash.
//  3. When next is non-nil, every entry's intra index must lie below
//     next[shard]; an index at or above it is [ErrMapperEntryCorrupted].
//  4. Within a shard, no two entries may collide on the natural key or on
//     the intra index.
//
// Gaps are accepted (WAL v2 step 1): an intra index below a shard's
// high-water mark that no entry names is a HOLE — an id that was assigned and
// never born, which a capture leaves out of the image. Holes resolve to
// nothing ([Mapper.Resolve] false) and are skipped by [Mapper.Walk].
//
// Post-condition: a previously seeded key interns to its original NodeID; a
// new key in shard s receives intra index next[s] (or the highest restored
// index + 1 when next is nil, which is how a snapshot without nodeids.bin
// loads).
//
// LoadFrom is safe for concurrent goroutines only with respect to
// other LoadFrom calls (which would all fail with ErrMapperNotEmpty
// after the first); it must not run concurrently with any
// Intern/Lookup/Resolve/Walk call on the same Mapper.
func (m *Mapper[N]) LoadFrom(entries []MapperEntry[N], next *[MapperShards]uint64) error {
	// Pre-flight: every shard must be untouched. Walking under RLock
	// is cheap and catches the "reseed a live mapper" mistake at the
	// boundary instead of after we have mutated half the shards.
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		empty := len(s.forward) == 0 && len(s.reverse) == 0 && s.next == 0
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
		key   N
		intra uint64
	}
	buckets := make([][]indexedEntry, mapperShardCount)
	for _, e := range entries {
		shardIdx, intraIdx := unpackNodeID(e.ID)
		expected := mapperShardFor(e.Key)
		if shardIdx != expected {
			// A shard disagreement has two possible causes and they call for
			// opposite responses, so name the one that actually applies. A
			// damaged snapshot is an integrity problem; an address-dependent key
			// type is a design problem in the caller's schema that no amount of
			// re-reading the file will fix. Checking is free here because this is
			// already the failure path.
			if why := addressDependentKeyPath(e.Key); why != "" {
				return fmt.Errorf("%w: %w: key %s formats and compares by address, so NodeID %d (shard %d) cannot be reproduced (this process hashes it to shard %d)",
					ErrMapperEntryCorrupted, ErrMapperKeyNotPortable, why, uint64(e.ID), shardIdx, expected)
			}
			return fmt.Errorf("%w: NodeID %d shard %d != mapperShardFor(key) %d",
				ErrMapperEntryCorrupted, uint64(e.ID), shardIdx, expected)
		}
		if next != nil && intraIdx >= next[shardIdx] {
			return fmt.Errorf("%w: NodeID %d intra index %d is at or above shard %d's high-water mark %d",
				ErrMapperEntryCorrupted, uint64(e.ID), intraIdx, shardIdx, next[shardIdx])
		}
		buckets[shardIdx] = append(buckets[shardIdx], indexedEntry{intra: intraIdx, key: e.Key})
	}

	// For every shard, sort by intra-index, assert uniqueness, then commit
	// forward/reverse in one shot, materialising reverse only to the highest
	// restored index + 1 with holes for the gaps.
	for shardIdx := range buckets {
		bucket := buckets[shardIdx]
		var hw uint64
		if next != nil {
			hw = next[shardIdx]
		}
		if len(bucket) == 0 {
			if hw > 0 {
				s := &m.shards[shardIdx]
				s.mu.Lock()
				s.next = hw
				s.mu.Unlock()
			}
			continue
		}
		sort.Slice(bucket, func(i, j int) bool {
			return bucket[i].intra < bucket[j].intra
		})
		s := &m.shards[shardIdx]
		s.mu.Lock()
		top := bucket[len(bucket)-1].intra + 1
		s.reverse = make([]N, 0, top)
		s.forward = make(map[N]NodeID, len(bucket))
		for i, ie := range bucket {
			if i > 0 && ie.intra == bucket[i-1].intra {
				s.mu.Unlock()
				return fmt.Errorf("%w: shard %d duplicate intra index %d", ErrMapperEntryCorrupted, shardIdx, ie.intra)
			}
			if _, dup := s.forward[ie.key]; dup {
				s.mu.Unlock()
				return fmt.Errorf("%w: shard %d duplicate key", ErrMapperEntryCorrupted, shardIdx)
			}
			s.growToLocked(ie.intra)
			s.reverse = append(s.reverse, ie.key)
			s.forward[ie.key] = packNodeID(uint64(shardIdx), ie.intra)
		}
		s.next = max(hw, top)
		s.mu.Unlock()
	}

	return nil
}

// addressDependentKeyPath reports the path within k, in Go field notation, at
// which an ADDRESS is stored — a pointer, an unsafe.Pointer, or a channel — or
// "" when k's value is entirely address-independent.
//
// It inspects the VALUE rather than only the static type, because a comparable
// struct may hold an interface field whose dynamic type is what decides: the
// static type says "maybe", the value says which. It is called only from the
// failure path in [Mapper.LoadFrom], so it costs nothing in normal operation.
//
// Slices, maps and functions cannot appear: a type containing one is not
// comparable, so it cannot instantiate [Mapper].
func addressDependentKeyPath[N comparable](k N) string {
	return addressDependentPath(reflect.ValueOf(k), "key")
}

// addressDependentPath is the recursive worker behind
// [addressDependentKeyPath]. path accumulates the human-readable location.
func addressDependentPath(v reflect.Value, path string) string {
	if !v.IsValid() {
		return ""
	}
	switch v.Kind() {
	case reflect.Pointer:
		return path + " (" + v.Type().String() + ")"
	case reflect.UnsafePointer:
		return path + " (unsafe.Pointer)"
	case reflect.Chan:
		return path + " (" + v.Type().String() + ")"
	case reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return addressDependentPath(v.Elem(), path)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			// Field(i) on an unexported field is readable for inspection; only
			// Interface() would panic, and this walk never calls it.
			if why := addressDependentPath(v.Field(i), path+"."+v.Type().Field(i).Name); why != "" {
				return why
			}
		}
		return ""
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if why := addressDependentPath(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); why != "" {
				return why
			}
		}
		return ""
	default:
		// Strings, numerics, bools and complex values are byte-for-byte
		// reproducible in any process.
		return ""
	}
}
