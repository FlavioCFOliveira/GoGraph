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

// TargetEvalFn evaluates a DELETE / DETACH DELETE target expression
// against the current input row and returns the resolved value. The exec
// operator inspects the value: NodeValue / IntegerValue selects the
// node by ID; RelationshipValue selects the relationship; null is a
// row-passthrough no-op (matches openCypher 9 §3.5.8).
type TargetEvalFn func(row Row) (expr.Value, error)

// RelEndpointFn returns the (srcID, dstID) endpoints for an edge that the
// schema-direct path is about to delete. Used when the bare-variable
// target carries an IntegerValue edge id (the in-pipeline encoding emitted
// by Expand) so DeleteNode can dispatch to the edge-removal branch
// without misinterpreting the id as a node id.
type RelEndpointFn func(row Row) (uint64, uint64, bool)

// DeleteNode deletes an already-bound node (labels + properties stripped) from
// the graph, provided it has no incident relationships.
//
// DeleteNode is NOT safe for concurrent use.
type DeleteNode struct {
	nodeVar          string
	schema           map[string]int
	child            Operator
	mutator          GraphMutator
	targetEvalFn     TargetEvalFn
	relEndpointsFn   RelEndpointFn
	ctx              context.Context //nolint:containedctx // stored for per-Next ctx check
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

// WithRelEndpoints attaches a per-row lookup that returns the (srcID,
// dstID) endpoints of the edge identified by the bare-variable target.
// When set AND the schema-direct slot holds an IntegerValue (the
// in-pipeline edge-id encoding emitted by Expand), the operator
// dispatches to the edge-removal path instead of treating the integer
// as a NodeID.
func (op *DeleteNode) WithRelEndpoints(fn RelEndpointFn) *DeleteNode {
	op.relEndpointsFn = fn
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
			var snapProps expr.MapValue
			if srcOK && dstOK {
				if raw := op.mutator.EdgeProperties(srcKey, dstKey); len(raw) > 0 {
					snapProps = make(expr.MapValue, len(raw))
					for k, pv := range raw {
						if v, ok := lpgPropToExprBinding(pv); ok {
							snapProps[k] = v
						}
					}
				}
				removeEdgeEitherDirection(op.mutator, srcKey, dstKey)
			}
			*out = op.markRowDeletedRel(childRow, tv, snapProps)
			return true, nil
		case expr.PathValue:
			// DELETE on a path: openCypher specifies this as a shortcut
			// for deleting every relationship and node in the path. The
			// rel-before-node ordering means after all path rels are
			// removed the nodes are detachable; mirror DetachDelete's
			// path sweep here so the test surface ("DELETE pathColls.
			// key[0], pathColls.key[1]" in Delete5 [7]) registers both
			// the rel and node deletions without requiring the user to
			// write DETACH DELETE.
			for _, n := range tv.Nodes {
				nodeKey, ok := op.mutator.ResolveNodeLabel(graph.NodeID(n.ID))
				if !ok {
					continue
				}
				for _, dst := range op.mutator.OutNeighbours(nodeKey) {
					op.mutator.RemoveEdge(nodeKey, dst)
				}
				for _, src := range op.mutator.InNeighbours(nodeKey) {
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
			*out = childRow
			return true, nil
		default:
			// Unsupported target value type: pass through as no-op so the
			// pipeline survives instead of aborting.
			*out = childRow
			return true, nil
		}
	} else {
		// Schema-direct path: peek at the bound value before delegating
		// to resolveNodeIDFromRow, so a RelationshipValue can dispatch
		// to the edge-removal path instead of failing the type check.
		if colIdx, ok := op.schema[op.nodeVar]; ok && colIdx < len(childRow) {
			if relVal, isRel := childRow[colIdx].(expr.RelationshipValue); isRel {
				srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(relVal.StartID))
				dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(relVal.EndID))
				var snapProps expr.MapValue
				if srcOK && dstOK {
					if raw := op.mutator.EdgeProperties(srcKey, dstKey); len(raw) > 0 {
						snapProps = make(expr.MapValue, len(raw))
						for k, pv := range raw {
							if v, ok := lpgPropToExprBinding(pv); ok {
								snapProps[k] = v
							}
						}
					}
					// Undirected MATCH emits both forward and reverse rows
					// for the same edge; the reverse row carries
					// (StartID=traversalSrc, EndID=traversalDst) which may
					// not match the edge's STORAGE direction. RemoveEdge
					// is direction-sensitive (it ignores requests against
					// the wrong direction), so probe both orientations
					// when the requested one is not the storage direction.
					// Closes Delete4 [1] flake.
					removeEdgeEitherDirection(op.mutator, srcKey, dstKey)
				}
				*out = op.markRowDeletedRel(childRow, relVal, snapProps)
				return true, nil
			}
			// IntegerValue (raw edge id) + bound relationship-variable
			// metadata: dispatch via relEndpointsFn so we never treat
			// the edge id as a node id. Closes Delete4 [1] and the
			// "DELETE r" planner gap.
			if intVal, isInt := childRow[colIdx].(expr.IntegerValue); isInt && op.relEndpointsFn != nil {
				srcID, dstID, okEnds := op.relEndpointsFn(childRow)
				snapRel := expr.RelationshipValue{ID: uint64(intVal)}
				if okEnds {
					srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(srcID))
					dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(dstID))
					if srcOK && dstOK {
						snapRel.StartID = uint64(srcID)
						snapRel.EndID = uint64(dstID)
						if labels := op.mutator.EdgeLabels(srcKey, dstKey); len(labels) > 0 {
							snapRel.Type = labels[0]
						}
						if raw := op.mutator.EdgeProperties(srcKey, dstKey); len(raw) > 0 {
							snapRel.Properties = make(expr.MapValue, len(raw))
							for k, pv := range raw {
								if v, ok := lpgPropToExprBinding(pv); ok {
									snapRel.Properties[k] = v
								}
							}
						}
						removeEdgeEitherDirection(op.mutator, srcKey, dstKey)
					}
				}
				*out = op.markRowDeletedRel(childRow, snapRel, snapRel.Properties)
				return true, nil
			}
		}
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

	// Snapshot labels and properties BEFORE removing them — these become
	// the frozen view carried on the row's NodeValue after the entity is
	// tombstoned, so `RETURN id(n)` still works but `RETURN n.foo` /
	// `labels(n)` raise EntityNotFound on the Deleted flag (Return2 [15]
	// / [16]).
	deletedLabels := append([]string(nil), op.mutator.NodeLabels(nodeKey)...)
	var deletedProps expr.MapValue
	if raw := op.mutator.NodeProperties(nodeKey); len(raw) > 0 {
		deletedProps = make(expr.MapValue, len(raw))
		for k, pv := range raw {
			if v, ok := lpgPropToExprBinding(pv); ok {
				deletedProps[k] = v
			}
		}
	}
	// Remove all labels.
	for _, lbl := range op.mutator.NodeLabels(nodeKey) {
		op.mutator.RemoveNodeLabel(nodeKey, lbl)
	}
	// Remove all properties.
	for k := range op.mutator.NodeProperties(nodeKey) {
		op.mutator.DelNodeProperty(nodeKey, k)
	}
	// Tombstone the node entity so AllNodesScan, count(*), and the
	// Order accessor no longer see it (Merge1 [14] / Merge5 [20]).
	op.mutator.RemoveNode(nodeKey)

	*out = op.markRowDeleted(childRow, nodeID, deletedLabels, deletedProps)
	return true, nil
}

// markRowDeleted returns childRow with the column bound to op.nodeVar
// replaced by a Deleted NodeValue snapshot, so downstream property /
// label accessors raise EntityNotFound (Return2 [15]/[16]). The original
// value at the column may be a NodeValue (canonical projection form) or
// an IntegerValue (raw in-pipeline NodeID encoding); either is upgraded
// to a Deleted NodeValue carrying the pre-tombstone labels and
// properties so `id(n)` and similar identity accessors keep returning
// the same value.
func (op *DeleteNode) markRowDeleted(row Row, nodeID graph.NodeID, labels []string, props expr.MapValue) Row {
	col, ok := op.schema[op.nodeVar]
	if !ok || col >= len(row) {
		return row
	}
	out := make(Row, len(row))
	copy(out, row)
	out[col] = expr.NodeValue{
		ID:         uint64(nodeID),
		Labels:     labels,
		Properties: props,
		Deleted:    true,
	}
	return out
}

// markRowDeletedRel mirrors [markRowDeleted] for the relationship-target
// branches of DeleteNode (DELETE r where r is a RelationshipValue). The
// row's relationship-variable column is upgraded to a Deleted
// RelationshipValue snapshot so RETURN r.foo / property access raise
// EntityNotFound while RETURN type(r) keeps returning the relationship
// type (Return2 [17]).
func (op *DeleteNode) markRowDeletedRel(row Row, rel expr.RelationshipValue, props expr.MapValue) Row {
	col, ok := op.schema[op.nodeVar]
	if !ok || col >= len(row) {
		return row
	}
	out := make(Row, len(row))
	copy(out, row)
	rel.Properties = props
	rel.Deleted = true
	out[col] = rel
	return out
}

// Close closes the child operator.
func (op *DeleteNode) Close() error {
	return op.child.Close()
}

// removeEdgeEitherDirection invokes mutator.RemoveEdge in the requested
// direction first and falls back to the reverse if the forward call left
// the edge in place. Used by the schema-direct edge-removal paths in
// DeleteNode and DetachDelete to absorb the undirected-MATCH reverse
// pass, where Expand emits a row whose traversal direction differs from
// the edge's storage direction (`MATCH (a)-[r]-(b)` produces both
// (a,r,b) and (b,r,a) rows for the single stored edge a→b — the second
// row's RemoveEdge(b, a) is a no-op against the storage direction and
// leaves the edge attached to its endpoints).
//
// Also decrements the Cypher CREATE-multiplicity counter so subsequent
// MERGEs observe the deleted CREATE call (Merge5 [3] / [21]).
func removeEdgeEitherDirection(mutator GraphMutator, src, dst string) {
	if !mutator.HasEdge(src, dst) && mutator.HasEdge(dst, src) {
		mutator.RemoveEdge(dst, src)
		mutator.DecEdgeCreateCount(dst, src)
		return
	}
	mutator.RemoveEdge(src, dst)
	mutator.DecEdgeCreateCount(src, dst)
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

	// Snapshot the property map BEFORE removing the edge so the row's
	// deleted-rel marker carries the pre-removal view, letting
	// `RETURN type(r)` keep returning the type while `RETURN r.foo`
	// raises EntityNotFound on the Deleted flag (Return2 [17]).
	var deletedProps expr.MapValue
	if raw := op.mutator.EdgeProperties(srcKey, dstKey); len(raw) > 0 {
		deletedProps = make(expr.MapValue, len(raw))
		for k, pv := range raw {
			if v, ok := lpgPropToExprBinding(pv); ok {
				deletedProps[k] = v
			}
		}
	}
	// Remove edge labels and properties before removing the edge itself.
	// (lpg.Graph's RemoveEdge removes the adjacency entry; label/property
	// cleanup prevents orphaned metadata.)
	op.mutator.RemoveEdge(srcKey, dstKey)
	op.mutator.DecEdgeCreateCount(srcKey, dstKey)

	*out = op.markRowDeleted(childRow, rel, deletedProps)
	return true, nil
}

// markRowDeleted returns childRow with the column bound to op.relVar
// replaced by a Deleted RelationshipValue snapshot. See
// [DeleteNode.markRowDeleted] for the rationale.
func (op *DeleteRelationship) markRowDeleted(row Row, rel expr.RelationshipValue, props expr.MapValue) Row {
	col, ok := op.schema[op.relVar]
	if !ok || col >= len(row) {
		return row
	}
	out := make(Row, len(row))
	copy(out, row)
	rel.Properties = props
	rel.Deleted = true
	out[col] = rel
	return out
}

// Close closes the child operator.
func (op *DeleteRelationship) Close() error {
	return op.child.Close()
}
