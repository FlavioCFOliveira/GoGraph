package exec

// remove.go — RemoveProperty and RemoveLabels write operators (task-272).
//
// RemoveProperty removes a single named property from an already-bound node
// or relationship.
//
// RemoveLabels removes one or more labels from an already-bound node.
//
// Both operators pass the input row through unchanged (write-through
// semantics), allowing downstream operators to continue consuming the same
// row after the mutation.
//
// # Concurrency
//
// RemoveProperty and RemoveLabels are NOT safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ─────────────────────────────────────────────────────────────────────────────
// RemoveProperty
// ─────────────────────────────────────────────────────────────────────────────

// RemoveProperty removes a single named property from an already-bound node
// or relationship per input row. For relationships, call WithRelCols to supply
// the endpoint column indices.
//
// RemoveProperty is NOT safe for concurrent use.
type RemoveProperty struct {
	entityVar   string
	propertyKey string
	schema      map[string]int
	relCols     *RelCols // non-nil when entityVar is a relationship
	child       Operator
	mutator     GraphMutator
	reg         *ConstraintRegistry // nil means no registry maintenance
	ctx         context.Context     //nolint:containedctx // stored for per-Next ctx check
}

// NewRemoveProperty creates a RemoveProperty operator.
func NewRemoveProperty(
	entityVar, propertyKey string,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) *RemoveProperty {
	return &RemoveProperty{
		entityVar:   entityVar,
		propertyKey: propertyKey,
		schema:      schema,
		child:       child,
		mutator:     mutator,
	}
}

// WithConstraintRegistry attaches a ConstraintRegistry so RemoveProperty
// releases unique-constraint value reservations when a node property is
// removed. Returns op for chaining.
func (op *RemoveProperty) WithConstraintRegistry(reg *ConstraintRegistry) *RemoveProperty {
	op.reg = reg
	return op
}

// WithRelCols marks entityVar as a relationship variable and records the row
// columns that hold the src and dst NodeIDs. Must be called before the first
// Next invocation. Returns op for chaining.
func (op *RemoveProperty) WithRelCols(rc RelCols) *RemoveProperty {
	op.relCols = &rc
	return op
}

// Init initialises the operator and its child.
func (op *RemoveProperty) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child and removes the specified property.
func (op *RemoveProperty) Next(out *Row) (bool, error) {
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

	ent, entErr := resolveEntityMaybeRel(op.entityVar, op.schema, op.relCols, childRow, op.mutator)
	if entErr != nil {
		if errors.Is(entErr, errNullTarget) {
			// openCypher: REMOVE on a NULL target is silently ignored.
			// Pass the row through unchanged so OPTIONAL MATCH chains
			// still produce their null-padded rows downstream.
			*out = childRow
			return true, nil
		}
		return false, fmt.Errorf("exec: RemoveProperty %q: %w", op.entityVar, entErr)
	}

	if op.propertyKey != "" {
		if ent.isRel {
			op.mutator.DelEdgeProperty(ent.relSrcKey, ent.relDstKey, op.propertyKey)
		} else {
			// Release the constrained value before removing the property so
			// the slot is freed in the registry.
			if op.reg != nil {
				if oldVal, had := op.mutator.NodeProperties(ent.nodeKey)[op.propertyKey]; had {
					op.reg.ReleasePropertyValue(op.mutator.NodeLabels(ent.nodeKey), op.propertyKey, oldVal)
				}
			}
			op.mutator.DelNodeProperty(ent.nodeKey, op.propertyKey)
		}
	}
	// Empty propertyKey is treated as a no-op (whole-entity remove is not a
	// valid Cypher operation; the IR translator emits this only for malformed
	// input and the sema layer should have rejected it).

	*out = childRow
	return true, nil
}

// Close closes the child operator.
func (op *RemoveProperty) Close() error {
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// RemoveLabels
// ─────────────────────────────────────────────────────────────────────────────

// RemoveLabels removes one or more labels from an already-bound node per input
// row.
//
// RemoveLabels is NOT safe for concurrent use.
type RemoveLabels struct {
	nodeVar string
	labels  []string
	schema  map[string]int
	child   Operator
	mutator GraphMutator
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewRemoveLabels creates a RemoveLabels operator.
func NewRemoveLabels(
	nodeVar string,
	labels []string,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) *RemoveLabels {
	lb := make([]string, len(labels))
	copy(lb, labels)
	return &RemoveLabels{
		nodeVar: nodeVar,
		labels:  lb,
		schema:  schema,
		child:   child,
		mutator: mutator,
	}
}

// Init initialises the operator and its child.
func (op *RemoveLabels) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child and removes the specified labels.
func (op *RemoveLabels) Next(out *Row) (bool, error) {
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
		// OPTIONAL MATCH (a) … REMOVE a:L on an unmatched outer row: the
		// target is null per openCypher, so the REMOVE is a no-op and
		// the row passes through unchanged. Mirrors the RemoveProperty
		// branch above.
		if errors.Is(err, errNullTarget) {
			*out = childRow
			return true, nil
		}
		return false, fmt.Errorf("exec: RemoveLabels %q: %w", op.nodeVar, err)
	}
	nodeKey, resolved := op.mutator.ResolveNodeLabel(nodeID)
	if !resolved {
		return false, fmt.Errorf("exec: RemoveLabels: cannot resolve NodeID %d", nodeID)
	}

	for _, lbl := range op.labels {
		op.mutator.RemoveNodeLabel(nodeKey, lbl)
	}

	*out = childRow
	return true, nil
}

// Close closes the child operator.
func (op *RemoveLabels) Close() error {
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────

// resolvedEntity holds the mutator-facing identity of a node or relationship.
// Exactly one of nodeKey or (relSrcKey, relDstKey) is populated, selected by
// isRel.
type resolvedEntity struct {
	isRel     bool
	nodeKey   string // valid when !isRel
	relSrcKey string // valid when isRel
	relDstKey string // valid when isRel
}

// resolveEntityFromRow extracts the node or relationship identity from the
// column at varName using the provided schema map and translates internal IDs
// to mutator-facing string keys. It accepts IntegerValue, NodeValue, and
// RelationshipValue column values.
func resolveEntityFromRow(varName string, schema map[string]int, row Row, mut GraphMutator) (resolvedEntity, error) {
	colIdx, ok := schema[varName]
	if !ok {
		return resolvedEntity{}, fmt.Errorf("variable %q not in schema", varName)
	}
	if colIdx >= len(row) {
		return resolvedEntity{}, fmt.Errorf("column %d out of range (row len %d)", colIdx, len(row))
	}
	switch v := row[colIdx].(type) {
	case expr.IntegerValue:
		nodeKey, resolved := mut.ResolveNodeLabel(graph.NodeID(v))
		if !resolved {
			return resolvedEntity{}, fmt.Errorf("cannot resolve NodeID %d", graph.NodeID(v))
		}
		return resolvedEntity{nodeKey: nodeKey}, nil
	case expr.NodeValue:
		nodeKey, resolved := mut.ResolveNodeLabel(graph.NodeID(v.ID))
		if !resolved {
			return resolvedEntity{}, fmt.Errorf("cannot resolve NodeID %d", graph.NodeID(v.ID))
		}
		return resolvedEntity{nodeKey: nodeKey}, nil
	case expr.RelationshipValue:
		srcKey, srcOK := mut.ResolveNodeLabel(graph.NodeID(v.StartID))
		dstKey, dstOK := mut.ResolveNodeLabel(graph.NodeID(v.EndID))
		if !srcOK || !dstOK {
			return resolvedEntity{}, fmt.Errorf("cannot resolve relationship endpoints (%d, %d)", v.StartID, v.EndID)
		}
		return resolvedEntity{isRel: true, relSrcKey: srcKey, relDstKey: dstKey}, nil
	}
	if expr.IsNull(row[colIdx]) {
		return resolvedEntity{}, errNullTarget
	}
	return resolvedEntity{}, fmt.Errorf("variable %q is not IntegerValue/NodeValue/RelationshipValue (got %T)", varName, row[colIdx])
}

// resolveEntityMaybeRel resolves the entity at varName. When rc is non-nil
// the variable is known to be a relationship: if the schema column holds an
// IntegerValue (raw Expand output), endpoint IDs are read from rc.SrcCol /
// rc.DstCol; if it holds a RelationshipValue (post-projection), StartID /
// EndID are used directly. For node variables (rc == nil) it delegates to
// resolveEntityFromRow.
func resolveEntityMaybeRel(varName string, schema map[string]int, rc *RelCols, row Row, mut GraphMutator) (resolvedEntity, error) {
	if rc == nil {
		return resolveEntityFromRow(varName, schema, row, mut)
	}
	colIdx, ok := schema[varName]
	if !ok {
		return resolvedEntity{}, fmt.Errorf("variable %q not in schema", varName)
	}
	if colIdx >= len(row) {
		return resolvedEntity{}, fmt.Errorf("column %d out of range (row len %d)", colIdx, len(row))
	}
	switch v := row[colIdx].(type) {
	case expr.IntegerValue:
		_ = v // edge-position counter; use endpoint columns
		return resolveRelBindingFromRow(rc.SrcCol, rc.DstCol, row, mut)
	case expr.RelationshipValue:
		srcKey, srcOK := mut.ResolveNodeLabel(graph.NodeID(v.StartID))
		dstKey, dstOK := mut.ResolveNodeLabel(graph.NodeID(v.EndID))
		if !srcOK || !dstOK {
			return resolvedEntity{}, fmt.Errorf("cannot resolve relationship endpoints (%d, %d)", v.StartID, v.EndID)
		}
		return resolvedEntity{isRel: true, relSrcKey: srcKey, relDstKey: dstKey}, nil
	}
	if expr.IsNull(row[colIdx]) {
		return resolvedEntity{}, errNullTarget
	}
	return resolvedEntity{}, fmt.Errorf("variable %q is not IntegerValue/RelationshipValue for relationship entity (got %T)", varName, row[colIdx])
}

// resolveRelBindingFromRow resolves a relationship entity from the (srcCol,
// dstCol) pair of row columns that hold endpoint NodeIDs as IntegerValue.
// Mirrors resolveRelBinding in set.go but returns resolvedEntity.
func resolveRelBindingFromRow(srcCol, dstCol int, row Row, mut GraphMutator) (resolvedEntity, error) {
	if srcCol >= len(row) || dstCol >= len(row) {
		return resolvedEntity{}, fmt.Errorf("relationship endpoint columns (%d, %d) out of range (row len %d)", srcCol, dstCol, len(row))
	}
	srcIV, srcOK := row[srcCol].(expr.IntegerValue)
	dstIV, dstOK := row[dstCol].(expr.IntegerValue)
	if !srcOK || !dstOK {
		return resolvedEntity{}, fmt.Errorf("relationship endpoint columns hold non-IntegerValue (%T, %T)", row[srcCol], row[dstCol])
	}
	srcKey, srcResolved := mut.ResolveNodeLabel(graph.NodeID(srcIV))
	dstKey, dstResolved := mut.ResolveNodeLabel(graph.NodeID(dstIV))
	if !srcResolved || !dstResolved {
		return resolvedEntity{}, fmt.Errorf("cannot resolve relationship endpoint NodeIDs (%d, %d)", graph.NodeID(srcIV), graph.NodeID(dstIV))
	}
	return resolvedEntity{isRel: true, relSrcKey: srcKey, relDstKey: dstKey}, nil
}

// resolveNodeIDFromRow extracts the NodeID stored at the column position of
// varName using the provided schema map. It accepts both IntegerValue (NodeID
// emitted by scan/create operators) and NodeValue (full node value).
// Used by RemoveLabels which operates on nodes only. A NULL value (typically
// from OPTIONAL MATCH that did not bind the variable) is signalled via the
// sentinel [errNullTarget] so callers can treat the row as a no-op rather
// than a hard error — matches the openCypher contract that DELETE / REMOVE /
// SET on a NULL target is silently skipped.
func resolveNodeIDFromRow(varName string, schema map[string]int, row Row) (graph.NodeID, error) {
	colIdx, ok := schema[varName]
	if !ok {
		return 0, fmt.Errorf("variable %q not in schema", varName)
	}
	if colIdx >= len(row) {
		return 0, fmt.Errorf("column %d out of range (row len %d)", colIdx, len(row))
	}
	switch v := row[colIdx].(type) {
	case expr.IntegerValue:
		return graph.NodeID(v), nil
	case expr.NodeValue:
		return graph.NodeID(v.ID), nil
	}
	if expr.IsNull(row[colIdx]) {
		return 0, errNullTarget
	}
	return 0, fmt.Errorf("variable %q is not IntegerValue/NodeValue (got %T)", varName, row[colIdx])
}

// errNullTarget signals that a DELETE / REMOVE / SET target variable
// resolved to NULL. Callers treat the row as a no-op (silently
// passing the row through unchanged) per openCypher's "null inputs
// are silently ignored by mutating clauses" rule.
var errNullTarget = fmt.Errorf("target is null")
