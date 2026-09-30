package exec

// rel_instance.go — relationship identity by stable handle.
//
// A relationship is identified by its stable per-edge handle, never by its
// (src, dst) endpoint pair: a multigraph pair can hold several parallel
// relationships, and an undirected graph stores one relationship under two
// adjacency orders that share one handle. The helpers below are the single
// place the write operators resolve a relationship instance's own type and
// properties, its stored endpoint order, and its removal, so MERGE, DELETE,
// SET and REMOVE all name the instance the read path resolves.
//
// The instance rule is the read path's rule (see buildEdgeProps in package
// cypher): a handle whose type or property record exists is authoritative for
// that instance; a handle-less slot, or a handle with no record at all (an edge
// stamped through the Go API), falls back to the per-pair store.

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// relInstanceLabels returns the relationship type(s) of the instance handle
// stored as (src, dst). It reads the instance's own by-handle type record and
// falls back to the per-pair label union only when the instance has none.
func relInstanceLabels(mut GraphMutator, src, dst string, handle uint64) []string {
	if handle != 0 {
		if labels := mut.EdgeLabelsByHandle(src, dst, handle); len(labels) > 0 {
			return labels
		}
	}
	return mut.EdgeLabels(src, dst)
}

// relInstanceProps returns the property map of the instance handle stored as
// (src, dst). The instance's own by-handle bag is authoritative whenever the
// instance carries a by-handle type or property record — including an EMPTY
// bag, which must never be read as the pair aggregate, because the aggregate
// holds the properties of every parallel sibling. Otherwise it returns the
// per-pair store.
func relInstanceProps(mut GraphMutator, src, dst string, handle uint64) map[string]lpg.PropertyValue {
	if handle != 0 {
		bag := mut.EdgePropertiesByHandle(src, dst, handle)
		if len(bag) > 0 || len(mut.EdgeLabelsByHandle(src, dst, handle)) > 0 {
			return bag
		}
	}
	return mut.EdgeProperties(src, dst)
}

// relInstanceHasType reports whether the instance handle stored as (src, dst)
// carries relType. An empty relType matches every instance.
func relInstanceHasType(mut GraphMutator, src, dst string, handle uint64, relType string) bool {
	if relType == "" {
		return true
	}
	return containsLabel(relInstanceLabels(mut, src, dst, handle), relType)
}

// containsLabel reports whether labels contains want.
func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// relStoredOrder returns the adjacency order (start, end) under which the
// relationship instance handle is stored, given its endpoints in either order,
// and whether handle names a stored instance of the pair at all. On an
// undirected graph both orders carry the handle; the order holding its
// metadata is chosen by [relStorageDirection].
func relStoredOrder(mut GraphMutator, a, b string, handle uint64) (start, end string, ok bool) {
	if handle == 0 {
		return a, b, false
	}
	fwd, rev := mut.HasEdgeHandle(a, b, handle), mut.HasEdgeHandle(b, a, handle)
	switch {
	case fwd && rev:
		// Both orders carry the handle only on an undirected graph (the
		// mirror slot) or for a self-loop; the metadata decides.
		start, end = relStorageDirection(mut, a, b, handle)
		return start, end, true
	case fwd:
		return a, b, true
	case rev:
		return b, a, true
	default:
		return a, b, false
	}
}

// relValueEntity resolves a post-projection relationship value to the stored
// instance it names: its endpoints in STORAGE order and its handle (the value's
// ID since rmp #2317). The value's StartID/EndID are normalised because a value
// bound through an undirected pattern, or through the mirror slot of an
// undirected graph, can carry them in traversal order, and every edge mutator
// is keyed by the stored order (rmp #2945, the value-form twin of #2817).
func relValueEntity(mut GraphMutator, v expr.RelationshipValue) (entityBinding, bool) {
	srcKey, srcOK := mut.ResolveNodeLabel(graph.NodeID(v.StartID))
	dstKey, dstOK := mut.ResolveNodeLabel(graph.NodeID(v.EndID))
	if !srcOK || !dstOK {
		return entityBinding{}, false
	}
	st, en := relStorageDirection(mut, srcKey, dstKey, v.ID)
	return entityBinding{isRel: true, relSrcKey: st, relDstKey: en, relHandle: v.ID}, true
}

// removeBoundRelationship removes the ONE relationship a DELETE target names,
// given its endpoints (in either order) and its handle, and returns the removed
// instance's type and properties for the row's deleted-entity snapshot.
//
// A handle that names a stored instance of the pair removes exactly that
// instance ([GraphMutator.RemoveEdgeByHandle]) and snapshots its own metadata.
// A value that names no stored instance falls back to the pair removal only
// when the pair holds a slot without a handle — handle 0, or the positional
// identity a graph without handles emits — because that is the only identity
// such a slot has. A non-zero handle that no slot carries names an instance
// already removed, and removes nothing (rmp #2940).
//
// wantType selects whether the removed instance's type is resolved; callers
// whose deleted-row value already carries its type pass false and skip the read.
func removeBoundRelationship(mut GraphMutator, srcKey, dstKey string, handle uint64, wantType bool) (relType string, props expr.MapValue) {
	if st, en, ok := relStoredOrder(mut, srcKey, dstKey, handle); ok {
		// The rule of relInstanceLabels / relInstanceProps, reading the
		// by-handle type record at most once.
		bag := mut.EdgePropertiesByHandle(st, en, handle)
		var labels []string
		if wantType || len(bag) == 0 {
			labels = mut.EdgeLabelsByHandle(st, en, handle)
		}
		raw := bag
		if len(bag) == 0 && len(labels) == 0 {
			raw = mut.EdgeProperties(st, en)
		}
		if wantType {
			if len(labels) == 0 {
				labels = mut.EdgeLabels(st, en)
			}
			if len(labels) > 0 {
				relType = labels[0]
			}
		}
		props = exprMapFromLPGProps(raw)
		mut.RemoveEdgeByHandle(st, en, handle)
		mut.DecEdgeCreateCount(st, en)
		return relType, props
	}
	if handle != 0 && !pairHasHandlelessSlot(mut, srcKey, dstKey) {
		// The value names a handled instance that is no longer stored — an
		// earlier row of the same statement already deleted it (an undirected
		// match binds one relationship on two rows). Removing the pair's first
		// slot here would delete a DIFFERENT relationship.
		return "", nil
	}
	st, en := srcKey, dstKey
	if !mut.HasEdge(st, en) && mut.HasEdge(en, st) {
		st, en = en, st
	}
	if wantType {
		if labels := mut.EdgeLabels(st, en); len(labels) > 0 {
			relType = labels[0]
		}
	}
	props = exprMapFromLPGProps(mut.EdgeProperties(st, en))
	removeEdgeEitherDirection(mut, st, en)
	return relType, props
}

// pairHasHandlelessSlot reports whether either order of the pair stores a slot
// stamped without a handle. Only such a slot can be named by a relationship
// value whose identity is not a stored handle.
func pairHasHandlelessSlot(mut GraphMutator, a, b string) bool {
	for _, h := range mut.EdgeHandles(a, b, nil) {
		if h == 0 {
			return true
		}
	}
	for _, h := range mut.EdgeHandles(b, a, nil) {
		if h == 0 {
			return true
		}
	}
	return false
}
