package exec

// set.go — SetProperty and SetLabels write operators (task-271).
//
// SetProperty updates a single property (or replaces/merges all properties)
// on a node or relationship that is already bound in the current row.
//
// SetLabels adds one or more labels to an already-bound node.
//
// # Property assignment modes
//
// The IR translator encodes three assignment modes via the PropertyKey field:
//   - Non-empty key + value expression: single-property SET n.key = expr
//   - Empty key + value expression: whole-entity replacement  SET n = {…}
//   - Empty key + "+=" prefix in value string: merge  SET n += {…}
//
// For the current IR (opaque string expressions) only literal map values are
// evaluated; variable expressions are treated as Null and produce a no-op.
//
// # Concurrency
//
// SetProperty and SetLabels are NOT safe for concurrent use.

import (
	"context"
	"fmt"
	"strings"

	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/index"
)

// ─────────────────────────────────────────────────────────────────────────────
// SetProperty
// ─────────────────────────────────────────────────────────────────────────────

// SetProperty sets or replaces properties on an already-bound node per input
// row. The node is identified by entityVar, whose column value must be an
// IntegerValue-encoded NodeID.
//
// SetProperty is NOT safe for concurrent use.
type SetProperty struct {
	entityVar   string
	propertyKey string // empty → whole-entity assignment
	valueExpr   string // opaque literal string from IR
	merge       bool   // true when mode is SET n += {…}
	schema      map[string]int
	child       Operator
	mutator     GraphMutator
	params      map[string]expr.Value // query parameters for $name substitution
	reg         *ConstraintRegistry   // nil means no enforcement
	mgr         *index.Manager        // nil when reg is nil
	parsedMap   []propLiteral         // cached parse of valueExpr when it is a literal map
	ctx         context.Context       //nolint:containedctx // stored for per-Next ctx check
}

// NewSetProperty creates a SetProperty operator.
//
// entityVar is the variable name of the target node or relationship. propertyKey
// is the property key for single-property mode; pass empty for whole-entity
// mode. valueExpr is the opaque literal string from the IR. schema maps
// variable names to column indices. mutator is the graph write surface.
func NewSetProperty(
	entityVar, propertyKey, valueExpr string,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) (*SetProperty, error) {
	merge := false
	rawExpr := valueExpr
	if strings.HasPrefix(rawExpr, "+=") {
		merge = true
		rawExpr = strings.TrimSpace(rawExpr[2:])
	}

	var parsedMap []propLiteral
	if propertyKey == "" {
		// Whole-entity mode: parse the map literal now so construction fails
		// fast on invalid syntax.
		var err error
		parsedMap, err = parsePropLiteral(rawExpr)
		if err != nil {
			return nil, fmt.Errorf("exec: SetProperty: parse map %q: %w", rawExpr, err)
		}
	}

	return &SetProperty{
		entityVar:   entityVar,
		propertyKey: propertyKey,
		valueExpr:   rawExpr,
		merge:       merge,
		schema:      schema,
		child:       child,
		mutator:     mutator,
		parsedMap:   parsedMap,
	}, nil
}

// WithConstraints attaches a ConstraintRegistry and index.Manager for
// pre-write enforcement. Both must be non-nil. Returns op for chaining.
func (op *SetProperty) WithConstraints(reg *ConstraintRegistry, mgr *index.Manager) *SetProperty {
	op.reg = reg
	op.mgr = mgr
	return op
}

// WithParams attaches query parameters for $name substitution in value
// expressions. Returns op for chaining.
func (op *SetProperty) WithParams(params map[string]expr.Value) *SetProperty {
	op.params = params
	return op
}

// Init initialises the operator and its child.
func (op *SetProperty) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child and applies the property mutation.
//
//nolint:gocyclo // three assignment modes (single, merge, replace) × optional constraint enforcement
func (op *SetProperty) Next(out *Row) (bool, error) {
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

	nodeID, err := op.resolveNodeID(op.entityVar, childRow)
	if err != nil {
		return false, fmt.Errorf("exec: SetProperty %q: %w", op.entityVar, err)
	}
	nodeKey, resolved := op.mutator.ResolveNodeLabel(nodeID)
	if !resolved {
		return false, fmt.Errorf("exec: SetProperty: cannot resolve NodeID %d", nodeID)
	}

	if op.propertyKey != "" {
		// Single property SET n.key = literal (or $param).
		pv, parseErr := parsePropValueWithParams(op.valueExpr, op.params)
		if parseErr != nil {
			// Non-literal expression: treat as no-op for current IR.
			*out = childRow
			return true, nil
		}
		// Constraint enforcement for single-property assignment.
		if op.reg != nil {
			labels := op.mutator.NodeLabels(nodeKey)
			if cerr := op.reg.CheckSetProperty(labels, op.propertyKey, pv, op.mgr); cerr != nil {
				return false, cerr
			}
		}
		if serr := op.mutator.SetNodeProperty(nodeKey, op.propertyKey, pv); serr != nil {
			return false, serr
		}
		if op.reg != nil {
			labels := op.mutator.NodeLabels(nodeKey)
			op.reg.RecordPropertySet(labels, op.propertyKey, pv)
		}
	} else if op.merge {
		// SET n += {…}: add/update without removing existing properties.
		if op.reg != nil {
			labels := op.mutator.NodeLabels(nodeKey)
			for _, p := range op.parsedMap {
				if cerr := op.reg.CheckSetProperty(labels, p.key, p.value, op.mgr); cerr != nil {
					return false, cerr
				}
			}
		}
		labels := op.mutator.NodeLabels(nodeKey)
		for _, p := range op.parsedMap {
			if serr := op.mutator.SetNodeProperty(nodeKey, p.key, p.value); serr != nil {
				return false, serr
			}
			if op.reg != nil {
				op.reg.RecordPropertySet(labels, p.key, p.value)
			}
		}
	} else {
		// SET n = {…}: replace all properties.
		if op.reg != nil {
			labels := op.mutator.NodeLabels(nodeKey)
			for _, p := range op.parsedMap {
				if cerr := op.reg.CheckSetProperty(labels, p.key, p.value, op.mgr); cerr != nil {
					return false, cerr
				}
			}
		}
		existing := op.mutator.NodeProperties(nodeKey)
		for k := range existing {
			op.mutator.DelNodeProperty(nodeKey, k)
		}
		labels := op.mutator.NodeLabels(nodeKey)
		for _, p := range op.parsedMap {
			if serr := op.mutator.SetNodeProperty(nodeKey, p.key, p.value); serr != nil {
				return false, serr
			}
			if op.reg != nil {
				op.reg.RecordPropertySet(labels, p.key, p.value)
			}
		}
	}

	*out = childRow
	return true, nil
}

// resolveNodeID extracts the NodeID from the column at varName.
func (op *SetProperty) resolveNodeID(varName string, row Row) (graph.NodeID, error) {
	colIdx, ok := op.schema[varName]
	if !ok {
		return 0, fmt.Errorf("variable %q not in schema", varName)
	}
	if colIdx >= len(row) {
		return 0, fmt.Errorf("column %d out of range (row len %d)", colIdx, len(row))
	}
	iv, ok := row[colIdx].(expr.IntegerValue)
	if !ok {
		return 0, fmt.Errorf("variable %q is not IntegerValue (got %T)", varName, row[colIdx])
	}
	return graph.NodeID(iv), nil
}

// Close closes the child operator.
func (op *SetProperty) Close() error {
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// SetLabels
// ─────────────────────────────────────────────────────────────────────────────

// SetLabels adds one or more labels to an already-bound node per input row.
//
// SetLabels is NOT safe for concurrent use.
type SetLabels struct {
	nodeVar string
	labels  []string
	schema  map[string]int
	child   Operator
	mutator GraphMutator
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewSetLabels creates a SetLabels operator.
func NewSetLabels(
	nodeVar string,
	labels []string,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) *SetLabels {
	lb := make([]string, len(labels))
	copy(lb, labels)
	return &SetLabels{
		nodeVar: nodeVar,
		labels:  lb,
		schema:  schema,
		child:   child,
		mutator: mutator,
	}
}

// Init initialises the operator and its child.
func (op *SetLabels) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child and adds the specified labels.
func (op *SetLabels) Next(out *Row) (bool, error) {
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

	nodeID, err := op.resolveNodeID(op.nodeVar, childRow)
	if err != nil {
		return false, fmt.Errorf("exec: SetLabels %q: %w", op.nodeVar, err)
	}
	nodeKey, resolved := op.mutator.ResolveNodeLabel(nodeID)
	if !resolved {
		return false, fmt.Errorf("exec: SetLabels: cannot resolve NodeID %d", nodeID)
	}

	for _, lbl := range op.labels {
		if err := op.mutator.SetNodeLabel(nodeKey, lbl); err != nil {
			return false, fmt.Errorf("exec: SetLabels SetNodeLabel: %w", err)
		}
	}

	*out = childRow
	return true, nil
}

// resolveNodeID extracts the NodeID from the column at varName.
func (op *SetLabels) resolveNodeID(varName string, row Row) (graph.NodeID, error) {
	colIdx, ok := op.schema[varName]
	if !ok {
		return 0, fmt.Errorf("variable %q not in schema", varName)
	}
	if colIdx >= len(row) {
		return 0, fmt.Errorf("column %d out of range (row len %d)", colIdx, len(row))
	}
	iv, ok := row[colIdx].(expr.IntegerValue)
	if !ok {
		// Attempt NodeValue (when the row carries a full NodeValue from a scan).
		if nv, ok2 := row[colIdx].(expr.NodeValue); ok2 {
			return graph.NodeID(nv.ID), nil
		}
		return 0, fmt.Errorf("variable %q is not IntegerValue/NodeValue (got %T)", varName, row[colIdx])
	}
	return graph.NodeID(iv), nil
}

// Close closes the child operator.
func (op *SetLabels) Close() error {
	return op.child.Close()
}
