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

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ErrDeleteNodeHasRelationships is returned when DELETE is attempted on a node
// that still has one or more incident relationships. Use DETACH DELETE to
// remove the node together with its relationships.
var ErrDeleteNodeHasRelationships = errors.New("exec: cannot delete node with existing relationships; use DETACH DELETE")

// txVisibleAdjacencyReader is the optional part of a [GraphMutator] that
// answers the delete path's adjacency questions from THIS transaction's view:
// its snapshot plus its own writes, never another in-flight transaction's work
// (rmp #2884).
//
// # Why the delete path must not read the present
//
// The DELETE guard ("does this node still have relationships") and DETACH
// DELETE's enumeration of the incoming relationships it must remove are
// decisions, and a decision taken from the newest stored state is taken from
// other transactions' uncommitted work. Measured before this reader existed,
// with T2 open and T1 deleting d:
//
//	T2 removes x->d, T1 runs DELETE d: T1 saw no in-edge and committed, T2
//	rolled back, and the committed graph held x->d into a deleted d;
//	T2 adds x->d, T1 runs DELETE d: T1 was refused for a relationship its
//	snapshot does not hold.
//
// Eight of the twelve interleavings in
// cypher.TestDeleteInEdgeIndex_DecidesFromTheTransactionSnapshot violated
// snapshot isolation this way.
type txVisibleAdjacencyReader interface {
	// InNeighboursInTx is [GraphMutator.InNeighbours] in this transaction's
	// view.
	InNeighboursInTx(n string) []string
	// HasInNeighbourInTx reports whether InNeighboursInTx would return anything.
	HasInNeighbourInTx(n string) bool
	// OutDegreeInTx is [GraphMutator.OutDegree] in this transaction's view.
	OutDegreeInTx(n string) int
}

// inNeighboursInTx returns n's incoming neighbours as THIS transaction sees
// them. The fallback is not a degradation: a mutator without the reader carries
// no transaction — the read-only and test stubs — and for such a caller the
// present IS its view (the reading [labelsInTx] gives the same fallback).
func inNeighboursInTx(mut GraphMutator, n string) []string {
	if tv, ok := mut.(txVisibleAdjacencyReader); ok {
		return tv.InNeighboursInTx(n)
	}
	return mut.InNeighbours(n)
}

// hasRelationshipsInTx reports whether n has any outgoing or incoming
// relationship in THIS transaction's view — the DELETE guard. See
// [inNeighboursInTx] for the fallback.
func hasRelationshipsInTx(mut GraphMutator, n string) bool {
	if tv, ok := mut.(txVisibleAdjacencyReader); ok {
		return tv.OutDegreeInTx(n) > 0 || tv.HasInNeighbourInTx(n)
	}
	return mut.OutDegree(n) > 0 || len(mut.InNeighbours(n)) > 0
}

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
	child          Operator
	mutator        GraphMutator
	ctx            context.Context //nolint:containedctx // stored for per-Next ctx check
	schema         map[string]int
	targetEvalFn   TargetEvalFn
	relEndpointsFn RelEndpointFn
	reg            *ConstraintRegistry // nil means no registry maintenance
	nodeVar        string

	// deferredPathNodes are path nodes whose deletion waits because a
	// relationship outside the path still held them when their path was
	// deleted; see [DeleteNode.deletePath].
	deferredPathNodes []graph.NodeID

	// pull receives every child Next call of this operator (see nextRow), so the
	// per-row pull does not heap-allocate its receiver.
	pull Row
}

// deletePath deletes a path: exactly the path's own relationships, each by its
// stable handle in its stored order, and then the path's nodes with DELETE
// semantics — a node that still has a relationship is not detached (rmp #2950).
// It used to remove EVERY relationship attached to every path node, deleting
// relationships the path does not contain.
//
// A node still held by another relationship is not refused on the spot. The
// same statement can delete that relationship later — a second path in the
// same DELETE clause (Delete5 [7]) or a later row — so the node is deferred and
// [DeleteNode.flushDeferredPathNodes] decides once every row has been
// processed: deleted if the relationship has gone by then, refused with
// [ErrDeleteNodeHasRelationships] otherwise, which rolls the statement back.
func (op *DeleteNode) deletePath(p expr.PathValue) error {
	for _, r := range p.Relationships {
		if r.Deleted {
			continue
		}
		srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(r.StartID))
		dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(r.EndID))
		if srcOK && dstOK {
			removeBoundRelationship(op.mutator, srcKey, dstKey, r.ID, false)
		}
	}
	for _, n := range p.Nodes {
		id := graph.NodeID(n.ID)
		nodeKey, ok := op.mutator.ResolveNodeLabel(id)
		if !ok || op.mutator.IsTombstoned(id) {
			continue
		}
		if hasRelationshipsInTx(op.mutator, nodeKey) {
			op.deferredPathNodes = append(op.deferredPathNodes, id)
			continue
		}
		op.removeDetachedNode(nodeKey)
	}
	return nil
}

// flushDeferredPathNodes deletes every deferred path node that no longer has a
// relationship and refuses the statement when one still has. A node another
// row or operator already deleted is skipped.
func (op *DeleteNode) flushDeferredPathNodes() error {
	pending := op.deferredPathNodes
	op.deferredPathNodes = nil
	for _, id := range pending {
		nodeKey, ok := op.mutator.ResolveNodeLabel(id)
		if !ok || op.mutator.IsTombstoned(id) {
			continue
		}
		if hasRelationshipsInTx(op.mutator, nodeKey) {
			return ErrDeleteNodeHasRelationships
		}
		op.removeDetachedNode(nodeKey)
	}
	return nil
}

// removeDetachedNode strips a node that has no relationship left and
// tombstones it. Stripping the labels and properties is internal teardown, not
// a user-visible side effect: openCypher declares DELETE as -nodes only
// (#2212), so effect counting is suppressed for the span. RemoveNodeLabel
// releases the node's constrained values (rmp #2358).
func (op *DeleteNode) removeDetachedNode(nodeKey string) {
	resumeCounting := suppressEffectCounting(op.mutator)
	for _, lbl := range labelsInTx(op.mutator, nodeKey) {
		op.mutator.RemoveNodeLabel(nodeKey, lbl)
	}
	for k := range op.mutator.NodeProperties(nodeKey) {
		op.mutator.DelNodeProperty(nodeKey, k)
	}
	resumeCounting()
	op.mutator.RemoveNode(nodeKey)
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

// WithConstraintRegistry attaches a ConstraintRegistry so DeleteNode releases
// unique-constraint value reservations when a node is deleted. Returns op for
// chaining.
func (op *DeleteNode) WithConstraintRegistry(reg *ConstraintRegistry) *DeleteNode {
	op.reg = reg
	return op
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

	childRow, ok, err := nextRow(op.child, &op.pull)
	if err != nil {
		return false, err
	}
	if !ok {
		// Every row has been processed: a path node deferred because another
		// relationship still held it must be free now (rmp #2950).
		return false, op.flushDeferredPathNodes()
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
			// The value's ID is the relationship's stable handle (rmp #2317), so
			// the removal and the snapshot address exactly that instance rather
			// than the pair's first slot (rmp #2940).
			srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(tv.StartID))
			dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(tv.EndID))
			var snapProps expr.MapValue
			if srcOK && dstOK {
				_, snapProps = removeBoundRelationship(op.mutator, srcKey, dstKey, tv.ID, false)
			}
			*out = op.markRowDeletedRel(childRow, tv, snapProps)
			return true, nil
		case expr.PathValue:
			if err := op.deletePath(tv); err != nil {
				return false, err
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
			if pv, isPath := childRow[colIdx].(expr.PathValue); isPath {
				// A bare path variable (`DELETE p`) takes the same path branch
				// as an expression target; resolveNodeIDFromRow would read the
				// path as its first node and refuse it for the path's own
				// relationship (rmp #2950).
				if err := op.deletePath(pv); err != nil {
					return false, err
				}
				*out = childRow
				return true, nil
			}
			if relVal, isRel := childRow[colIdx].(expr.RelationshipValue); isRel {
				// Instance-precise by the value's handle; an undirected match's
				// reverse row carries traversal-order endpoints, which
				// removeBoundRelationship normalises to the stored order.
				srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(relVal.StartID))
				dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(relVal.EndID))
				var snapProps expr.MapValue
				if srcOK && dstOK {
					_, snapProps = removeBoundRelationship(op.mutator, srcKey, dstKey, relVal.ID, false)
				}
				*out = op.markRowDeletedRel(childRow, relVal, snapProps)
				return true, nil
			}
			// IntegerValue (raw forward-CSR edge position) + bound
			// relationship-variable metadata: dispatch via relEndpointsFn so we
			// never treat the edge id as a node id. Closes Delete4 [1] and the
			// "DELETE r" planner gap. The IntegerValue at this column IS the
			// bound instance's forward-CSR edge position, so it resolves that
			// instance's stable handle for an instance-precise removal in a
			// multigraph (rmp #2018).
			if intVal, isInt := childRow[colIdx].(expr.IntegerValue); isInt && op.relEndpointsFn != nil {
				srcID, dstID, okEnds := op.relEndpointsFn(childRow)
				snapRel := expr.RelationshipValue{ID: uint64(intVal)}
				if okEnds {
					srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(srcID))
					dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(dstID))
					if srcOK && dstOK {
						snapRel.StartID = uint64(srcID)
						snapRel.EndID = uint64(dstID)
						// Resolve the bound parallel instance's stable handle from
						// its forward-CSR edge position (the IntegerValue in this
						// column). A non-zero handle removes the EXACT slot plus its
						// per-handle metadata and snapshots the deleted-row view by
						// handle; a zero handle (simple graph, or a position that no
						// longer resolves) falls back to the first-match endpoint
						// removal — unchanged behaviour.
						// The column IS the handle since rmp #2317; it used to be a
						// forward-CSR position needing a lookup to recover it.
						var handle uint64
						if intVal >= 0 {
							handle = uint64(intVal)
						}
						relType, props := removeBoundRelationship(op.mutator, srcKey, dstKey, handle, true)
						if relType != "" {
							snapRel.Type = relType
						}
						snapRel.Properties = props
					}
				}
				*out = op.markRowDeletedRel(childRow, snapRel, snapRel.Properties)
				return true, nil
			}
		}
		var err error
		nodeID, err = resolveNodeIDFromRow(op.nodeVar, op.schema, childRow)
		if err != nil {
			if errors.Is(err, errNullTarget) {
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

	// Guard: the node must not have any outgoing or incoming edges IN THIS
	// TRANSACTION'S VIEW (rmp #2884). A relationship another transaction added
	// and has not committed does not refuse the delete; that transaction's
	// adjacency claim on this node refuses it instead, as a serialization
	// conflict, when the node is retired below.
	if hasRelationshipsInTx(op.mutator, nodeKey) {
		return false, ErrDeleteNodeHasRelationships
	}

	// Snapshot labels and properties BEFORE removing them — these become
	// the frozen view carried on the row's NodeValue after the entity is
	// tombstoned, so `RETURN id(n)` still works but `RETURN n.foo` /
	// `labels(n)` raise EntityNotFound on the Deleted flag (Return2 [15]
	// / [16]).
	deletedLabels := append([]string(nil), labelsInTx(op.mutator, nodeKey)...)
	var deletedProps expr.MapValue
	if raw := op.mutator.NodeProperties(nodeKey); len(raw) > 0 {
		deletedProps = make(expr.MapValue, len(raw))
		for k, pv := range raw {
			if v, ok := lpgPropToExprBinding(pv); ok {
				deletedProps[k] = v
			}
		}
	}
	// The constrained values this node holds are given back by RemoveNodeLabel
	// below, at the mutator choke point (rmp #2358). The bulk release that used to
	// stand here would now be a SECOND release of the same reservations, and a
	// release is not idempotent: it would hand a live value back and admit a
	// genuine duplicate afterwards.
	// Stripping the node's labels and properties is internal teardown, not a
	// user-visible side effect: openCypher declares DELETE as -nodes only
	// (#2212). Suppress effect counting for the span.
	resumeCounting := suppressEffectCounting(op.mutator)
	// Remove all labels.
	for _, lbl := range labelsInTx(op.mutator, nodeKey) {
		op.mutator.RemoveNodeLabel(nodeKey, lbl)
	}
	// Remove all properties.
	for k := range op.mutator.NodeProperties(nodeKey) {
		op.mutator.DelNodeProperty(nodeKey, k)
	}
	resumeCounting()
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
	child   Operator
	mutator GraphMutator
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
	schema  map[string]int
	relCols *RelCols // non-nil enables instance-precise by-handle removal
	relVar  string

	// pull receives every child Next call of this operator (see nextRow), so the
	// per-row pull does not heap-allocate its receiver.
	pull Row
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

// WithRelCols records the row columns that hold the bound relationship's
// endpoint NodeIDs and its forward-CSR edge position, so Next resolves the
// bound parallel instance's stable handle and removes the EXACT instance in a
// multigraph (rmp #2018) rather than the first-match endpoint slot. When unset
// (or when the position resolves no handle), Next falls back to the endpoint
// removal. Must be called before the first Next. Returns op for chaining.
func (op *DeleteRelationship) WithRelCols(rc RelCols) *DeleteRelationship {
	op.relCols = &rc
	return op
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

	childRow, ok, err := nextRow(op.child, &op.pull)
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

	// Resolve the bound parallel instance's stable handle from its forward-CSR
	// edge position (when the endpoint/edge-position columns were wired via
	// WithRelCols). A non-zero handle removes the EXACT instance plus its
	// per-handle metadata and snapshots the deleted-row view by handle; a zero
	// handle (no RelCols, simple graph, or a post-projection binding whose edge
	// position is gone) falls back to the first-match endpoint removal —
	// unchanged behaviour (rmp #2018).
	// With no relationship columns wired the value's own ID is the handle
	// (rmp #2317); it is never ignored in favour of the pair's first slot.
	handle := rel.ID
	if op.relCols != nil {
		handle = resolveRelHandle(op.relCols, childRow, srcKey, dstKey, op.mutator)
	}

	// Snapshot the property map BEFORE removing the edge so the row's
	// deleted-rel marker carries the pre-removal view, letting
	// `RETURN type(r)` keep returning the type while `RETURN r.foo`
	// raises EntityNotFound on the Deleted flag (Return2 [17]).
	_, deletedProps := removeBoundRelationship(op.mutator, srcKey, dstKey, handle, false)

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
