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
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// DetachDelete removes all incident edges from a node and then strips the
// node's labels and properties.
//
// DetachDelete is NOT safe for concurrent use.
type DetachDelete struct {
	nodeVar      string
	schema       map[string]int
	child        Operator
	mutator      GraphMutator
	targetEvalFn TargetEvalFn
	ctx          context.Context //nolint:containedctx // stored for per-Next ctx check
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

// WithTargetEvalFn attaches a per-row evaluator for non-variable DETACH
// DELETE targets (subscripts, property access, …).
func (op *DetachDelete) WithTargetEvalFn(fn TargetEvalFn) *DetachDelete {
	op.targetEvalFn = fn
	return op
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

	var nodeID graph.NodeID
	if op.targetEvalFn != nil {
		v, evalErr := op.targetEvalFn(childRow)
		if evalErr != nil {
			return false, fmt.Errorf("exec: DetachDelete %q: %w", op.nodeVar, evalErr)
		}
		if v == nil || expr.IsNull(v) {
			*out = childRow
			return true, nil
		}
		switch tv := v.(type) {
		case expr.NodeValue:
			nodeID = graph.NodeID(tv.ID)
		case expr.IntegerValue:
			nodeID = graph.NodeID(tv)
		case expr.RelationshipValue:
			srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(tv.StartID))
			dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(tv.EndID))
			if srcOK && dstOK {
				op.mutator.RemoveEdge(srcKey, dstKey)
			}
			*out = childRow
			return true, nil
		case expr.PathValue:
			// DETACH DELETE on a path: detach-delete every node in
			// the path. Relationships are removed implicitly via the
			// per-node incident-edge sweep. Per-node delete uses the
			// helper below.
			if err := op.detachDeletePath(tv); err != nil {
				return false, err
			}
			*out = childRow
			return true, nil
		default:
			*out = childRow
			return true, nil
		}
	} else {
		// Schema-direct path: peek for RelationshipValue / PathValue
		// before delegating to resolveNodeIDFromRow (which only
		// handles node IDs).
		if colIdx, ok := op.schema[op.nodeVar]; ok && colIdx < len(childRow) {
			switch tv := childRow[colIdx].(type) {
			case expr.RelationshipValue:
				srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(tv.StartID))
				dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(tv.EndID))
				if srcOK && dstOK {
					op.mutator.RemoveEdge(srcKey, dstKey)
				}
				*out = childRow
				return true, nil
			case expr.PathValue:
				if err := op.detachDeletePath(tv); err != nil {
					return false, err
				}
				*out = childRow
				return true, nil
			}
		}
		var err error
		nodeID, err = resolveNodeIDFromRow(op.nodeVar, op.schema, childRow)
		if err != nil {
			if errors.Is(err, errNullTarget) {
				*out = childRow
				return true, nil
			}
			return false, fmt.Errorf("exec: DetachDelete %q: %w", op.nodeVar, err)
		}
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
	// Tombstone the node so subsequent scans treat it as absent.
	op.mutator.RemoveNode(nodeKey)

	*out = childRow
	return true, nil
}

// Close closes the child operator.
func (op *DetachDelete) Close() error {
	return op.child.Close()
}

// detachDeletePath performs DETACH DELETE on every node in p. Each node
// has its incident edges removed and its labels and properties stripped.
// Relationships are deleted implicitly via the incident-edge sweep, so
// duplicate or path-internal edges are handled automatically.
func (op *DetachDelete) detachDeletePath(p expr.PathValue) error {
	for _, n := range p.Nodes {
		nodeKey, resolved := op.mutator.ResolveNodeLabel(graph.NodeID(n.ID))
		if !resolved {
			continue
		}
		outgoing := op.mutator.OutNeighbours(nodeKey)
		incoming := op.mutator.InNeighbours(nodeKey)
		for _, dst := range outgoing {
			op.mutator.RemoveEdge(nodeKey, dst)
		}
		for _, src := range incoming {
			op.mutator.RemoveEdge(src, nodeKey)
		}
		for _, lbl := range op.mutator.NodeLabels(nodeKey) {
			op.mutator.RemoveNodeLabel(nodeKey, lbl)
		}
		for k := range op.mutator.NodeProperties(nodeKey) {
			op.mutator.DelNodeProperty(nodeKey, k)
		}
		op.mutator.RemoveNode(nodeKey)
	}
	return nil
}
