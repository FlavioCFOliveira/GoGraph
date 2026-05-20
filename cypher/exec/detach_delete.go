package exec

// detach_delete.go — DetachDelete write operator (task-274).
//
// DetachDelete removes all incident edges from a node, then "deletes" the node
// (strips its labels and properties). It is the safe form of DELETE for nodes
// that may have relationships.
//
// # Enumeration strategy
//
// Outgoing edges are enumerated via graphMutator.OutNeighbours. Incoming edges
// are enumerated via graphMutator.InNeighbours. Each edge is removed with
// graphMutator.RemoveEdge before the node itself is cleaned up.
//
// Snapshot before mutate: outgoing and incoming neighbour lists are
// captured into local slices before the removal loop begins so that the
// removal itself does not invalidate an in-progress iterator.
//
// # Concurrency
//
// DetachDelete is NOT safe for concurrent use.

import (
	"context"
	"fmt"
)

// DetachDelete removes all incident edges from a node and then strips the
// node's labels and properties.
//
// DetachDelete is NOT safe for concurrent use.
type DetachDelete struct {
	nodeVar string
	schema  map[string]int
	child   Operator
	mutator GraphMutator
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewDetachDelete creates a DetachDelete operator.
func NewDetachDelete(
	nodeVar string,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) *DetachDelete {
	return &DetachDelete{
		nodeVar: nodeVar,
		schema:  schema,
		child:   child,
		mutator: mutator,
	}
}

// Init initialises the operator and its child.
func (op *DetachDelete) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child, removes all incident edges of the bound
// node, then strips the node's labels and properties.
func (op *DetachDelete) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	var childRow Row
	ok, err := op.child.Next(&childRow)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	nodeID, err := resolveNodeIDFromRow(op.nodeVar, op.schema, childRow)
	if err != nil {
		return false, fmt.Errorf("exec: DetachDelete %q: %w", op.nodeVar, err)
	}
	nodeKey, resolved := op.mutator.ResolveNodeLabel(nodeID)
	if !resolved {
		*out = childRow
		return true, nil
	}

	// Snapshot outgoing and incoming neighbours before mutation.
	outgoing := op.mutator.OutNeighbours(nodeKey)
	incoming := op.mutator.InNeighbours(nodeKey)

	// Remove all outgoing edges.
	for _, dst := range outgoing {
		op.mutator.RemoveEdge(nodeKey, dst)
	}
	// Remove all incoming edges.
	for _, src := range incoming {
		op.mutator.RemoveEdge(src, nodeKey)
	}

	// Strip all labels.
	for _, lbl := range op.mutator.NodeLabels(nodeKey) {
		op.mutator.RemoveNodeLabel(nodeKey, lbl)
	}
	// Strip all properties.
	for k := range op.mutator.NodeProperties(nodeKey) {
		op.mutator.DelNodeProperty(nodeKey, k)
	}

	*out = childRow
	return true, nil
}

// Close closes the child operator.
func (op *DetachDelete) Close() error {
	return op.child.Close()
}
