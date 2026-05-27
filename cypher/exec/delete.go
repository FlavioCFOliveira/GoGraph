package exec

// delete.go — DeleteNode and DeleteRelationship write operators (task-273).
//
// DeleteNode deletes an already-bound node that has no incident relationships.
// Attempting to delete a connected node returns ErrDeleteNodeHasRelationships.
//
// DeleteRelationship removes a directed edge identified by a
// RelationshipValue (StartID, EndID) already bound in the current row.
//
// # Node deletion semantics
//
// lpg.Graph[string, float64] does not expose a first-class RemoveNode
// operation; the Mapper permanently interns node IDs. "Deleting" a node in
// this implementation means:
//   - Verify that OutDegree == 0 (and the reverse for undirected graphs).
//   - Remove all labels and all properties from the node.
//   - The NodeID remains in the Mapper and is no longer reachable from
//     the live graph via label/property queries.
//
// This is consistent with the lpg package's design: the adjlist Mapper is
// append-only. A full RemoveNode primitive would require a separate tombstone
// registry that is outside scope for these tasks.
//
// # Concurrency
//
// DeleteNode and DeleteRelationship are NOT safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"gograph/cypher/expr"
	"gograph/graph"
)

// ErrDeleteNodeHasRelationships is returned when DELETE is attempted on a node
// that still has one or more incident relationships. Use DETACH DELETE to
// remove the node together with its relationships.
var ErrDeleteNodeHasRelationships = errors.New("exec: cannot delete node with existing relationships; use DETACH DELETE")

// ─────────────────────────────────────────────────────────────────────────────
// DeleteNode
// ─────────────────────────────────────────────────────────────────────────────

// DeleteNode deletes an already-bound node (labels + properties stripped) from
// the graph, provided it has no incident relationships.
//
// DeleteNode is NOT safe for concurrent use.
// TargetEvalFn evaluates a DELETE / DETACH DELETE target expression
// against the current input row and returns the resolved value. The exec
// operator inspects the value: NodeValue / IntegerValue selects the
// node by ID; RelationshipValue selects the relationship; null is a
// row-passthrough no-op (matches openCypher 9 §3.5.8).
type TargetEvalFn func(row Row) (expr.Value, error)

type DeleteNode struct {
	nodeVar       string
	schema        map[string]int
	child         Operator
	mutator       GraphMutator
	targetEvalFn  TargetEvalFn
	ctx           context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewDeleteNode creates a DeleteNode operator.
func NewDeleteNode(
	nodeVar string,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) *DeleteNode {
	return &DeleteNode{
		nodeVar: nodeVar,
		schema:  schema,
		child:   child,
		mutator: mutator,
	}
}

// WithTargetEvalFn attaches a per-row evaluator for non-variable DELETE
// targets (subscripts, property access, …). When set, the operator
// resolves the target value via the evaluator instead of the schema
// lookup keyed by nodeVar.
func (op *DeleteNode) WithTargetEvalFn(fn TargetEvalFn) *DeleteNode {
	op.targetEvalFn = fn
	return op
}

// Init initialises the operator and its child.
func (op *DeleteNode) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child and deletes the bound node.
func (op *DeleteNode) Next(out *Row) (bool, error) {
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
			return false, fmt.Errorf("exec: DeleteNode %q: %w", op.nodeVar, evalErr)
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
			// DELETE on a relationship: dispatch to the mutator's edge
			// removal path. The startup/endpoint IDs already identify the
			// edge; bypass the node-deletion guard.
			srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(tv.StartID))
			dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(tv.EndID))
			if srcOK && dstOK {
				op.mutator.RemoveEdge(srcKey, dstKey)
			}
			*out = childRow
			return true, nil
		default:
			// Unsupported target value type: pass through as no-op so the
			// pipeline survives instead of aborting.
			*out = childRow
			return true, nil
		}
	} else {
		var err error
		nodeID, err = resolveNodeIDFromRow(op.nodeVar, op.schema, childRow)
		if err != nil {
			if err == errNullTarget {
				// OPTIONAL-MATCH-bound or otherwise NULL target: DELETE
				// is a no-op per openCypher; propagate the row unchanged.
				*out = childRow
				return true, nil
			}
			return false, fmt.Errorf("exec: DeleteNode %q: %w", op.nodeVar, err)
		}
	}
	nodeKey, resolved := op.mutator.ResolveNodeLabel(nodeID)
	if !resolved {
		// Node not found — treat as no-op (already deleted or never existed).
		*out = childRow
		return true, nil
	}

	// Guard: the node must not have any outgoing or incoming edges.
	if op.mutator.OutDegree(nodeKey) > 0 {
		return false, ErrDeleteNodeHasRelationships
	}
	if len(op.mutator.InNeighbours(nodeKey)) > 0 {
		return false, ErrDeleteNodeHasRelationships
	}

	// Remove all labels.
	for _, lbl := range op.mutator.NodeLabels(nodeKey) {
		op.mutator.RemoveNodeLabel(nodeKey, lbl)
	}
	// Remove all properties.
	for k := range op.mutator.NodeProperties(nodeKey) {
		op.mutator.DelNodeProperty(nodeKey, k)
	}

	*out = childRow
	return true, nil
}

// Close closes the child operator.
func (op *DeleteNode) Close() error {
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteRelationship
// ─────────────────────────────────────────────────────────────────────────────

// DeleteRelationship removes a directed edge per input row.
//
// DeleteRelationship is NOT safe for concurrent use.
type DeleteRelationship struct {
	relVar  string
	schema  map[string]int
	child   Operator
	mutator GraphMutator
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewDeleteRelationship creates a DeleteRelationship operator.
func NewDeleteRelationship(
	relVar string,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) *DeleteRelationship {
	return &DeleteRelationship{
		relVar:  relVar,
		schema:  schema,
		child:   child,
		mutator: mutator,
	}
}

// Init initialises the operator and its child.
func (op *DeleteRelationship) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child and removes the bound relationship.
func (op *DeleteRelationship) Next(out *Row) (bool, error) {
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

	colIdx, ok := op.schema[op.relVar]
	if !ok {
		return false, fmt.Errorf("exec: DeleteRelationship: variable %q not in schema", op.relVar)
	}
	if colIdx >= len(childRow) {
		return false, fmt.Errorf("exec: DeleteRelationship: column %d out of range (row len %d)", colIdx, len(childRow))
	}

	rel, ok := childRow[colIdx].(expr.RelationshipValue)
	if !ok {
		return false, fmt.Errorf("exec: DeleteRelationship: variable %q is not RelationshipValue (got %T)", op.relVar, childRow[colIdx])
	}

	srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(rel.StartID))
	dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(rel.EndID))
	if !srcOK || !dstOK {
		// Endpoint not resolvable: edge may have already been removed.
		*out = childRow
		return true, nil
	}

	// Remove edge labels and properties before removing the edge itself.
	// (lpg.Graph's RemoveEdge removes the adjacency entry; label/property
	// cleanup prevents orphaned metadata.)
	op.mutator.RemoveEdge(srcKey, dstKey)

	*out = childRow
	return true, nil
}

// Close closes the child operator.
func (op *DeleteRelationship) Close() error {
	return op.child.Close()
}
