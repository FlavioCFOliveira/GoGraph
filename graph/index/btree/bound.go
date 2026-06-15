package btree

// bound.go — self-maintaining (bound) B+tree index (#1505).
//
// An unbound index (see [New]) has a no-op [Index.Apply]: it is maintained
// only by explicit Insert/Delete/BulkLoad calls. A bound index (see
// [NewBound]) ties the index to one (label, property) pair of a live node
// graph and maintains itself from the index.Manager change fan-out, exactly
// like the bound hash index (graph/index/hash, task #1340).
//
// This is the prerequisite that makes a Cypher CREATE INDEX ... {indexType:
// 'btree'} index actually usable: before #1505 such an index was registered
// empty with a no-op Apply and never populated, so a range seek over it would
// have returned zero rows. The bound index converges to the same node-set the
// live graph holds for the bound (label, property) pair.

import (
	"cmp"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// Binding ties an [Index] to a single (label, property) pair of a live node
// graph. The shape mirrors hash.Binding so the engine's CREATE INDEX wiring
// can build either kind from the same closures. Because changes are fanned
// out at commit time — after the transaction's mutations were applied eagerly
// to the graph — the callbacks observe the transaction's FINAL state, which is
// exactly the state the index must converge to.
type Binding[V cmp.Ordered] struct {
	// PropertyID is the interned property-key identifier this index covers.
	// Property changes whose Change.Property differs are ignored.
	PropertyID uint32

	// LabelID is the interned label identifier this index is scoped to.
	// Label changes whose Change.Label differs are ignored.
	LabelID uint32

	// Label and Property are the source names behind PropertyID and LabelID.
	// They let a query planner match the index against a (label, property)
	// predicate without access to the registries.
	Label, Property string

	// Project converts a Change.OldValue / Change.NewValue payload to the
	// index key type. ok is false when the payload is absent or not indexable
	// (wrong kind), in which case the event is skipped for that direction.
	Project func(v any) (V, bool)

	// Eligible reports whether the node should currently be present in the
	// index: it must be live (not deleted) and carry the bound label,
	// evaluated against the graph's final state.
	Eligible func(node graph.NodeID) bool

	// CurrentValue returns the node's current value for the bound property,
	// projected to the key type. ok is false when the node is not live, lacks
	// the property, or the value is not indexable. It is consulted on label
	// add/remove events, which carry no property payload.
	CurrentValue func(node graph.NodeID) (V, bool)
}

// errBindingIncomplete is returned by [NewBound] when a required Binding
// field is missing.
var errBindingIncomplete = fmt.Errorf("%w: incomplete btree index binding", index.ErrIndexValueTypeUnsupported)

// NewBound returns an empty B+tree index bound to b. Unlike [New], the
// returned index has a functional [Index.Apply]: it subscribes to the node
// property and label changes selected by b and keeps itself consistent with
// the graph. Returns an error when b is missing its Label, Property, or any
// of the three callbacks.
func NewBound[V cmp.Ordered](b Binding[V]) (*Index[V], error) {
	if b.Label == "" || b.Property == "" ||
		b.Project == nil || b.Eligible == nil || b.CurrentValue == nil {
		return nil, errBindingIncomplete
	}
	idx := New[V]()
	idx.binding = &b
	return idx, nil
}

// BoundNode returns the (label, property) pair this index is bound to, with
// ok reporting whether the index is bound at all. Query planners use it to
// decide whether the index may serve a predicate: a bound index covers
// exactly its (label, property) pair, while an unbound index carries no
// coverage metadata and must NOT be served by a range seek (it is never
// maintained from the fan-out).
func (i *Index[V]) BoundNode() (label, property string, ok bool) {
	if i.binding == nil {
		return "", "", false
	}
	return i.binding.Label, i.binding.Property, true
}

// applyBound maintains a bound index from one [index.Change]. The rules are
// identical to hash.Index.Apply (the unconditional old-value delete on a
// property write is what makes the index order-independent across an
// index.Manager batch that the Manager does not order across subscribers):
//
//   - SetNodeProperty on the bound property: the old value (when present and
//     projectable) is deleted unconditionally, and the new value is inserted
//     when the node is eligible in the graph's final state.
//   - DelNodeProperty on the bound property: the old value is deleted.
//   - Add/RemoveNodeLabel on the bound label: the node's CURRENT property
//     value is inserted / deleted.
//
// Apply is idempotent (bitmap add/remove) and safe for concurrent use with
// readers; writers are serialised upstream by the engine's single-writer
// transaction contract. Edge changes and changes for other properties/labels
// are ignored.
func (i *Index[V]) applyBound(c index.Change) {
	b := i.binding
	switch c.Op {
	case index.OpSetNodeProperty:
		if c.Property != b.PropertyID {
			return
		}
		if old, ok := b.Project(c.OldValue); ok {
			i.Delete(old, c.Node)
		}
		if nv, ok := b.Project(c.NewValue); ok && b.Eligible(c.Node) {
			i.Insert(nv, c.Node)
		}
	case index.OpDelNodeProperty:
		if c.Property != b.PropertyID {
			return
		}
		if old, ok := b.Project(c.OldValue); ok {
			i.Delete(old, c.Node)
		}
	case index.OpAddNodeLabel:
		if c.Label != b.LabelID {
			return
		}
		if v, ok := b.CurrentValue(c.Node); ok && b.Eligible(c.Node) {
			i.Insert(v, c.Node)
		}
	case index.OpRemoveNodeLabel:
		if c.Label != b.LabelID {
			return
		}
		if v, ok := b.CurrentValue(c.Node); ok {
			i.Delete(v, c.Node)
		}
	}
}
