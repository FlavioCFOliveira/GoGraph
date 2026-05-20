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
	"fmt"

	"gograph/cypher/expr"
	"gograph/graph"
)

// ─────────────────────────────────────────────────────────────────────────────
// RemoveProperty
// ─────────────────────────────────────────────────────────────────────────────

// RemoveProperty removes a single named property from an already-bound node
// per input row. The node is identified by entityVar, whose column value must
// be an IntegerValue-encoded NodeID.
//
// RemoveProperty is NOT safe for concurrent use.
type RemoveProperty struct {
	entityVar   string
	propertyKey string
	schema      map[string]int
	child       Operator
	mutator     GraphMutator
	ctx         context.Context //nolint:containedctx // stored for per-Next ctx check
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

	nodeID, err := resolveNodeIDFromRow(op.entityVar, op.schema, childRow)
	if err != nil {
		return false, fmt.Errorf("exec: RemoveProperty %q: %w", op.entityVar, err)
	}
	nodeKey, resolved := op.mutator.ResolveNodeLabel(nodeID)
	if !resolved {
		return false, fmt.Errorf("exec: RemoveProperty: cannot resolve NodeID %d", nodeID)
	}

	if op.propertyKey != "" {
		op.mutator.DelNodeProperty(nodeKey, op.propertyKey)
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
// Shared helper
// ─────────────────────────────────────────────────────────────────────────────

// resolveNodeIDFromRow extracts the NodeID stored at the column position of
// varName using the provided schema map. It accepts both IntegerValue (NodeID
// emitted by scan/create operators) and NodeValue (full node value).
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
	default:
		return 0, fmt.Errorf("variable %q is not IntegerValue/NodeValue (got %T)", varName, row[colIdx])
	}
}
