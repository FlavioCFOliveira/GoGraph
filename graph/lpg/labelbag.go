package lpg

// labelbag.go — the per-node label set's compact tiered representation
// (sprint 221, #1629).
//
// # Motivation
//
// Node labels were stored as map[graph.NodeID]map[LabelID]struct{}. The INNER
// map — allocated per node even when the node carries a single label — costs
// ~300 B in bucket and header overhead. A memory audit measured ~150 B per
// node attributable to the label store alone on a graph where most nodes carry
// one or two labels (the dominant case: every node is at least one type).
//
// # Design
//
// [labelBag] is a tagged union stored BY VALUE in the outer
// map[graph.NodeID]labelBag, mirroring the per-node property [propBag] and the
// secondary-index [index.NodeSet]:
//
//	empty      — count 0, no allocation.
//	singleton  — exactly one label, held inline in single; no allocation. This
//	             captures the dominant one-label-per-node case at zero heap cost.
//	small      — 2..smallLabelMax labels in one UNSORTED []LabelID. One slice
//	             allocation, 4 B per element.
//	map        — a promoted map[LabelID]struct{}. Reached when an add would grow
//	             the small tier past smallLabelMax. Promotion is ONE-WAY.
//
// Like [propBag], the small tier is UNSORTED and looked up by linear scan:
// label iteration order is NOT observable (NodeLabels/NodeLabelsByID document
// "unspecified order", and the snapshot serializer emits self-describing
// records whose on-disk order never depended on bag iteration order), so a
// linear scan at n<=8 ties or beats a map probe and avoids the per-node map
// allocation entirely.
//
// # Concurrency
//
// [labelBag] is NOT safe for concurrent use on its own. The per-shard RWMutex
// of [nodeLabelShard] guards every read and write exactly as it guarded the
// nested maps before; a labelBag value is mutated only under the shard write
// lock and read only under the matching read lock.

// smallLabelMax is the largest number of labels held in the unsorted
// inline-slice tier before a labelBag promotes to a map. Chosen to match the
// [propBag]/[index.NodeSet] small-set threshold and to keep the dominant
// 1-2-label case off the map's ~300 B overhead.
const smallLabelMax = 8

// labelBag is the per-node label set. The zero value is a valid empty bag.
// The fields form a tagged union resolved by which is non-nil/non-zero:
//   - m != nil           -> map state (promoted; never demotes).
//   - m == nil, ids != nil -> small state (len(ids) in [2, smallLabelMax]).
//   - m == nil, ids == nil, count == 1 -> singleton state (single).
//   - m == nil, ids == nil, count == 0 -> empty.
type labelBag struct {
	ids    []LabelID            // small state; len in [2, smallLabelMax]
	m      map[LabelID]struct{} // non-nil iff promoted to map (one-way)
	single LabelID              // the lone label in singleton state
	count  uint8                // 0 empty, 1 singleton, else len(ids); ignored once m != nil
}

// has reports whether the bag contains lid.
func (b *labelBag) has(lid LabelID) bool {
	if b.m != nil {
		_, ok := b.m[lid]
		return ok
	}
	switch b.count {
	case 0:
		return false
	case 1:
		return b.single == lid
	default:
		for _, v := range b.ids {
			if v == lid {
				return true
			}
		}
		return false
	}
}

// add inserts lid, promoting to a map when the small tier would overflow.
// Adding a label already present is a no-op (set semantics).
func (b *labelBag) add(lid LabelID) {
	if b.m != nil {
		b.m[lid] = struct{}{}
		return
	}
	switch b.count {
	case 0:
		b.single = lid
		b.count = 1
	case 1:
		if lid == b.single {
			return
		}
		b.ids = []LabelID{b.single, lid}
		b.single = 0
		b.count = 2
	default:
		for _, v := range b.ids {
			if v == lid {
				return // already present
			}
		}
		if len(b.ids) >= smallLabelMax {
			m := make(map[LabelID]struct{}, len(b.ids)+1)
			for _, v := range b.ids {
				m[v] = struct{}{}
			}
			m[lid] = struct{}{}
			b.m = m
			b.ids = nil
			b.count = 0
			return
		}
		b.ids = append(b.ids, lid)
		b.count = uint8(len(b.ids))
	}
}

// del removes lid from the bag. It reports whether the bag became EMPTY as a
// result, so the caller can drop the node's entry. A bag in the map tier never
// demotes (promote-and-never-demote, mirroring [propBag]/[index.NodeSet]).
func (b *labelBag) del(lid LabelID) (nowEmpty bool) {
	if b.m != nil {
		delete(b.m, lid)
		return len(b.m) == 0
	}
	switch b.count {
	case 0:
		return false
	case 1:
		if b.single != lid {
			return false
		}
		b.single = 0
		b.count = 0
		return true
	default:
		for i, v := range b.ids {
			if v != lid {
				continue
			}
			// Swap-delete: order is not observable.
			last := len(b.ids) - 1
			b.ids[i] = b.ids[last]
			b.ids = b.ids[:last]
			break
		}
		switch len(b.ids) {
		case 0:
			b.ids = nil
			b.count = 0
			return true
		case 1:
			// Collapse back to the singleton state.
			b.single = b.ids[0]
			b.ids = nil
			b.count = 1
			return false
		default:
			b.count = uint8(len(b.ids))
			return false
		}
	}
}

// len returns the number of labels in the bag.
func (b *labelBag) len() int {
	if b.m != nil {
		return len(b.m)
	}
	if b.count == 1 {
		return 1
	}
	return len(b.ids)
}

// forEach invokes fn once per label in the bag. The iteration order is
// unspecified, matching the prior map-backed behaviour. fn must not mutate
// the bag.
func (b *labelBag) forEach(fn func(LabelID)) {
	if b.m != nil {
		for lid := range b.m {
			fn(lid)
		}
		return
	}
	switch b.count {
	case 0:
	case 1:
		fn(b.single)
	default:
		for _, lid := range b.ids {
			fn(lid)
		}
	}
}
