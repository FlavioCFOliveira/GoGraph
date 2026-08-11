package lpg

// instmap.go — the per-pair edge-instance map's compact tiered representation
// (sprint 339, rmp #2401).
//
// # Motivation
//
// The four per-edge side stores — per-CREATE-instance labels and properties
// (edge_instance_labels.go, edge_instance_props.go) and their stable-handle
// analogues (edge_handle.go) — were each a NESTED map:
//
//	map[edgeKey]map[int64]labelBag       // by CREATE ordinal
//	map[edgeKey]map[uint64]labelBag      // by stable handle
//
// The inner map is allocated PER NODE PAIR, and a Go map holding a single
// entry costs several hundred bytes of header, control bytes and a whole
// eight-slot group. Almost every pair in a real graph carries exactly one
// relationship, so almost every one of those maps held exactly one entry.
//
// The memory audit of 2026-08-11 measured the consequence: a relationship
// created through Cypher cost 1 078.87 B, of which a heap profile attributed
// 490.1 B to the ordinal label store and 486.6 B to the handle label store —
// 87.9 % of the total — to record a relationship type the adjacency already
// holds in a four-byte column. Loading four million relationships OOM-killed
// the engine in an 8 GB container, on a graph Memgraph held in 658 MB. Full
// evidence in docs/memory-vs-neo4j-memgraph-2026-08-11.md.
//
// # Design
//
// [instMap] is the same two-state union [propBag] applies to node properties
// (sprint 207, #1587) and [labelBag] applies to node labels (sprint 221,
// #1629), one level further out. It is stored BY VALUE in the outer
// map[edgeKey]instMap, so a pair with one instance costs one small slice
// backing and no inner map at all:
//
//	small  — 0..smallInstMax (key, bag) pairs in one UNSORTED slice, scanned
//	         linearly. The dominant one-instance pair costs a single slice
//	         allocation.
//	map    — a promoted map[K]V, reached when a set would grow the small tier
//	         past smallInstMax. Promotion is ONE-WAY: a pair that acquires
//	         many parallel edges keeps the O(1)-probe map path for the rest of
//	         its life, so a hub pair does not pay a linear scan per read.
//
// The small tier is unsorted for the reason propBag's is: instance iteration
// order is not observable. The only iteration is [instMap.forEachKey], used by
// the MVCC pre-image loops in mvcc_edge_side.go, which record one pre-image per
// instance and do not depend on order; the public accessors return a []string
// or a map[string]PropertyValue built from a single instance.
//
// # Why not flatten the key instead
//
// Replacing map[edgeKey]map[K]V with a single map[instanceKey]V would remove
// the nesting outright and cost less still. It was rejected on measurement of
// a different operation: RemoveEdge drops a whole pair's instance state with
// ONE map delete (lpg.go, clearEdgePairState), and the MVCC pre-image loops
// iterate exactly the instances that pair holds. Against a flat map both
// become a scan of the whole shard, turning an O(parallel edges) delete into
// an O(shard) one — and bulk delete is already this engine's weakest path
// (rmp #2400). The nesting is load-bearing for deletes; only the inner map's
// REPRESENTATION was the defect.
//
// # Concurrency
//
// [instMap] is NOT safe for concurrent use on its own. The per-shard mutex of
// each side store guards every read and write exactly as it guarded the nested
// maps before: an instMap value is mutated only under the shard write lock and
// read only under the matching read lock. Because it is held by value, every
// mutation must be written back into the outer map under that same lock — the
// same rule propBag and labelBag already impose, and the same one their call
// sites already follow.

// smallInstMax is the largest number of instances a pair holds in the unsorted
// inline-slice tier before its instMap promotes to a map. It matches
// [smallBagMax] and the [index.NodeSet] threshold: at this cardinality a linear
// scan is competitive with a map probe, and the whole point is to keep the
// dominant one-instance pair off the map's per-pair overhead. A pair with more
// than eight parallel edges is rare enough that paying for a map there is the
// right trade.
const smallInstMax = 8

// instEntry is one (instance key, attribute bag) pair held in the small tier.
// The bag is placed first so that the larger field governs alignment and the
// key packs into the tail, mirroring [kv].
type instEntry[K comparable, V any] struct {
	val V
	key K
}

// instMap maps an edge-instance key — a 1-based CREATE ordinal or a stable
// edge handle — to that instance's attribute bag. The zero value is a valid
// empty instMap. The fields form a tagged union resolved by which is non-nil:
//   - m != nil -> map state (promoted; never demotes).
//   - m == nil -> small state, the (possibly empty) pairs slice.
type instMap[K comparable, V any] struct {
	m     map[K]V           // non-nil iff promoted to map (one-way)
	pairs []instEntry[K, V] // small state; len in [0, smallInstMax]
}

// get returns the bag stored under key and whether it is present.
func (b *instMap[K, V]) get(key K) (V, bool) {
	if b.m != nil {
		v, ok := b.m[key]
		return v, ok
	}
	for i := range b.pairs {
		if b.pairs[i].key == key {
			return b.pairs[i].val, true
		}
	}
	var zero V
	return zero, false
}

// set stores val under key, promoting to the map tier when the small tier is
// full. The receiver is a pointer, but the value it belongs to is held in the
// outer map: the caller must write the instMap back after calling set.
func (b *instMap[K, V]) set(key K, val V) {
	if b.m != nil {
		b.m[key] = val
		return
	}
	for i := range b.pairs {
		if b.pairs[i].key == key {
			b.pairs[i].val = val
			return
		}
	}
	if len(b.pairs) < smallInstMax {
		b.pairs = append(b.pairs, instEntry[K, V]{key: key, val: val})
		return
	}
	// Promote. Sized for the pairs already held plus the arrival, so the
	// promotion itself does not immediately rehash.
	b.m = make(map[K]V, len(b.pairs)+1)
	for i := range b.pairs {
		b.m[b.pairs[i].key] = b.pairs[i].val
	}
	b.m[key] = val
	b.pairs = nil
}

// del removes key. It is a no-op when key is absent. Promotion is one-way, so
// a map-state instMap that shrinks below the threshold stays a map.
func (b *instMap[K, V]) del(key K) {
	if b.m != nil {
		delete(b.m, key)
		return
	}
	for i := range b.pairs {
		if b.pairs[i].key != key {
			continue
		}
		// Order is not observable, so the cheapest removal is to move the
		// last element into the hole rather than shift the tail.
		last := len(b.pairs) - 1
		b.pairs[i] = b.pairs[last]
		// Clear the vacated slot before truncating: V may hold pointers
		// (propBag's promoted map, labelBag's), and leaving the copy behind
		// the length would keep them reachable from the backing array.
		b.pairs[last] = instEntry[K, V]{}
		b.pairs = b.pairs[:last]
		return
	}
}

// len reports how many instances the pair holds. Used by the callers that drop
// the pair's outer entry once its last instance is gone.
func (b *instMap[K, V]) len() int {
	if b.m != nil {
		return len(b.m)
	}
	return len(b.pairs)
}

// forEachKey calls fn for every instance key present, stopping early when fn
// returns false. Iteration order is unspecified in both tiers and no caller
// depends on it; the MVCC pre-image loops in mvcc_edge_side.go record one
// pre-image per instance and stop at the first refusal.
//
// fn must not mutate the instMap.
func (b *instMap[K, V]) forEachKey(fn func(K) bool) {
	if b.m != nil {
		for k := range b.m {
			if !fn(k) {
				return
			}
		}
		return
	}
	for i := range b.pairs {
		if !fn(b.pairs[i].key) {
			return
		}
	}
}
