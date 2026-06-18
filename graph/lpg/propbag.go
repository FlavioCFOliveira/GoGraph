package lpg

// propbag.go — the per-node property bag's compact tiered representation
// (sprint 207, task #1587).
//
// # Motivation
//
// Node properties were stored as a nested map[graph.NodeID]map[PropertyKeyID]
// PropertyValue. The INNER map — allocated per node even when the node carries
// only two or three properties — dominated resident memory: a Go map with a
// handful of entries costs ~300 B in bucket and header overhead. A memory
// audit measured ~330 B for a 2-3-property node, the inner map accounting for
// the bulk of it.
//
// # Design
//
// [propBag] is a two-state union stored BY VALUE in the outer
// map[graph.NodeID]propBag, mirroring the small-set tier shipped for the
// secondary indexes ([index.NodeSet]):
//
//	small  — 0..smallBagMax (keyID, value) pairs held in one UNSORTED slice.
//	         A 2-property node costs a single ~64 B slice backing plus the
//	         24-byte-per-element values held inline, with no map overhead.
//	map    — a promoted map[PropertyKeyID]PropertyValue. Reached when a Set
//	         would grow the small tier past smallBagMax. Promotion is ONE-WAY:
//	         a bag that becomes a map never demotes, so a property-heavy node
//	         stays on the O(1)-probe map path.
//
// Unlike [index.NodeSet] the small tier is UNSORTED and looked up by linear
// scan. Property bags are tiny (typically 2-5 keys) and, crucially, their
// iteration order is NOT observable: the public accessors return a
// map[string]PropertyValue, and the snapshot serializer emits self-describing
// (NodeID, keyIdx, kind, value) records whose on-disk order never depended on
// bag iteration order (it was already Go-map-random). An unsorted slice
// therefore avoids the O(n) insert shift of a sorted slice while a linear scan
// at n<=8 ties or beats binary search and a map probe (graph-theory-expert,
// #1587).
//
// # Concurrency
//
// [propBag] is NOT safe for concurrent use on its own. The per-shard RWMutex
// of [nodePropShard] guards every read and write exactly as it guarded the
// nested maps before; a propBag value is mutated only under the shard write
// lock and read only under the matching read lock.

// smallBagMax is the largest number of (keyID, value) pairs held in the
// unsorted inline-slice tier before a propBag promotes to a map. Chosen to
// match the [index.NodeSet] small-set threshold and to keep the dominant
// 1-5-property case off the map's ~300 B overhead; at this cardinality a
// linear scan is competitive with a map probe and avoids the per-node map
// allocation entirely.
const smallBagMax = 8

// kv is one (interned property-key, value) pair held in the small tier.
type kv struct {
	key PropertyKeyID
	val PropertyValue
}

// propBag is the per-node property bag. The zero value is a valid empty bag.
// The fields form a tagged union resolved by which is non-nil:
//   - m != nil           -> map state (promoted; never demotes).
//   - m == nil           -> small state, the (possibly empty) pairs slice.
type propBag struct {
	pairs []kv                            // small state; len in [0, smallBagMax]
	m     map[PropertyKeyID]PropertyValue // non-nil iff promoted to map (one-way)
}

// get returns the value stored under key and whether it is present.
func (b *propBag) get(key PropertyKeyID) (PropertyValue, bool) {
	if b.m != nil {
		v, ok := b.m[key]
		return v, ok
	}
	for i := range b.pairs {
		if b.pairs[i].key == key {
			return b.pairs[i].val, true
		}
	}
	return PropertyValue{}, false
}

// set inserts or overwrites the value stored under key, promoting to the map
// tier when an insert would grow the small tier past smallBagMax.
func (b *propBag) set(key PropertyKeyID, val PropertyValue) {
	if b.m != nil {
		b.m[key] = val
		return
	}
	// Overwrite an existing key in place (set semantics, no growth).
	for i := range b.pairs {
		if b.pairs[i].key == key {
			b.pairs[i].val = val
			return
		}
	}
	if len(b.pairs) >= smallBagMax {
		// Promote: move the existing pairs plus the new one into a map.
		m := make(map[PropertyKeyID]PropertyValue, len(b.pairs)+1)
		for i := range b.pairs {
			m[b.pairs[i].key] = b.pairs[i].val
		}
		m[key] = val
		b.m = m
		b.pairs = nil
		return
	}
	b.pairs = append(b.pairs, kv{key: key, val: val})
}

// del removes the value stored under key. It reports whether the bag became
// empty as a result, so the caller can drop the node's bag entry. A bag in the
// map tier never demotes (promote-and-never-demote, mirroring [index.NodeSet]).
func (b *propBag) del(key PropertyKeyID) (nowEmpty bool) {
	if b.m != nil {
		delete(b.m, key)
		return len(b.m) == 0
	}
	for i := range b.pairs {
		if b.pairs[i].key != key {
			continue
		}
		// Swap-delete: order is not observable, so the cheapest removal is to
		// move the tail element into the gap and shrink by one.
		last := len(b.pairs) - 1
		b.pairs[i] = b.pairs[last]
		b.pairs[last] = kv{} // drop the value reference so it can be GC'd.
		b.pairs = b.pairs[:last]
		break
	}
	return len(b.pairs) == 0
}

// len returns the number of properties in the bag.
func (b *propBag) len() int {
	if b.m != nil {
		return len(b.m)
	}
	return len(b.pairs)
}

// forEach invokes fn once per (keyID, value) pair in the bag. The iteration
// order is unspecified, matching the prior map-backed behaviour. fn must not
// mutate the bag.
func (b *propBag) forEach(fn func(key PropertyKeyID, val PropertyValue)) {
	if b.m != nil {
		for k, v := range b.m {
			fn(k, v)
		}
		return
	}
	for i := range b.pairs {
		fn(b.pairs[i].key, b.pairs[i].val)
	}
}
