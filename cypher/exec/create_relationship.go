package exec

// create_relationship.go — CreateRelationship write operator (task-270).
//
// CreateRelationship resolves two already-bound node variables from the
// current row, calls AddEdge on the underlying graph, attaches a type label
// and properties to the new edge, and optionally binds a relationship variable
// to the new edge's identity (encoded as a RelationshipValue).
//
// # Variable resolution
//
// startVar and endVar are column names whose values in the current row are
// expected to be expr.IntegerValue-encoded NodeIDs (as emitted by
// CreateNode and AllNodesScan). The schema map provided at construction
// time translates variable name → column index.
//
// # Concurrency
//
// CreateRelationship is NOT safe for concurrent use.

import (
	"context"
	"fmt"

	"gograph/cypher/expr"
	"gograph/graph"
)

// CreateRelationship creates a new directed edge per input row between two
// already-bound nodes.
//
// CreateRelationship is NOT safe for concurrent use.
type CreateRelationship struct {
	startVar    string
	endVar      string
	relVar      string
	relType     string
	propsRaw    string
	props       []propLiteral
	propsExprFn PropsEvalFn    // nil when all properties are literals
	schema      map[string]int // variable name → column index
	child       Operator
	mutator     GraphMutator
	ctx         context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewCreateRelationship creates a CreateRelationship operator.
//
// startVar and endVar are the variable names (column indices are looked up in
// schema) of the source and destination nodes. relVar is the variable name
// bound to the new relationship (may be empty). relType is the relationship
// type label. properties is the opaque literal property-map string. schema
// maps currently bound variable names to their column indices.
func NewCreateRelationship(
	startVar, endVar, relVar, relType, properties string,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) (*CreateRelationship, error) {
	props, err := parsePropLiteral(properties)
	if err != nil {
		return nil, fmt.Errorf("exec: CreateRelationship: parse properties %q: %w", properties, err)
	}
	return &CreateRelationship{
		startVar: startVar,
		endVar:   endVar,
		relVar:   relVar,
		relType:  relType,
		propsRaw: properties,
		props:    props,
		schema:   schema,
		child:    child,
		mutator:  mutator,
	}, nil
}

// WithPropsEvalFn attaches a per-row property evaluator. See [CreateNode.WithPropsEvalFn].
func (op *CreateRelationship) WithPropsEvalFn(fn PropsEvalFn) *CreateRelationship {
	op.propsExprFn = fn
	return op
}

// WithParams re-parses the property map with the supplied query parameters for
// $name substitution. Returns op for chaining.
func (op *CreateRelationship) WithParams(params map[string]expr.Value) (*CreateRelationship, error) {
	if len(params) == 0 {
		return op, nil
	}
	props, err := parsePropLiteralWithParams(op.propsRaw, params)
	if err != nil {
		return nil, fmt.Errorf("exec: CreateRelationship: parse properties %q: %w", op.propsRaw, err)
	}
	op.props = props
	return op, nil
}

// Init initialises the operator and its child.
func (op *CreateRelationship) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child, resolves the endpoint NodeIDs, creates
// the edge, and appends an optional RelationshipValue column.
func (op *CreateRelationship) Next(out *Row) (bool, error) {
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

	srcID, err := op.resolveNodeID(op.startVar, childRow)
	if err != nil {
		return false, fmt.Errorf("exec: CreateRelationship start: %w", err)
	}
	dstID, err := op.resolveNodeID(op.endVar, childRow)
	if err != nil {
		return false, fmt.Errorf("exec: CreateRelationship end: %w", err)
	}

	srcLabel, srcOK := op.mutator.ResolveNodeLabel(srcID)
	dstLabel, dstOK := op.mutator.ResolveNodeLabel(dstID)
	if !srcOK {
		return false, fmt.Errorf("exec: CreateRelationship: cannot resolve src NodeID %d", srcID)
	}
	if !dstOK {
		return false, fmt.Errorf("exec: CreateRelationship: cannot resolve dst NodeID %d", dstID)
	}

	actualSrcID, actualDstID, err := op.mutator.AddEdge(srcLabel, dstLabel, 0)
	if err != nil {
		return false, fmt.Errorf("exec: CreateRelationship AddEdge: %w", err)
	}
	if op.relType != "" {
		op.mutator.SetEdgeLabel(srcLabel, dstLabel, op.relType)
	}

	props := mergeProps(op.props, op.propsExprFn, childRow)

	for _, p := range props {
		if err := op.mutator.SetEdgeProperty(srcLabel, dstLabel, p.key, p.value); err != nil {
			return false, fmt.Errorf("exec: CreateRelationship SetEdgeProperty %q: %w", p.key, err)
		}
	}

	if op.relVar == "" {
		*out = childRow
		return true, nil
	}

	rel := expr.RelationshipValue{
		ID:      uint64(actualSrcID)<<32 | uint64(actualDstID), // synthetic edge ID
		StartID: uint64(actualSrcID),
		EndID:   uint64(actualDstID),
		Type:    op.relType,
	}
	newRow := make(Row, len(childRow)+1)
	copy(newRow, childRow)
	newRow[len(childRow)] = rel
	*out = newRow
	return true, nil
}

// resolveNodeID extracts the NodeID stored at the column position of varName.
func (op *CreateRelationship) resolveNodeID(varName string, row Row) (graph.NodeID, error) {
	colIdx, ok := op.schema[varName]
	if !ok {
		return 0, fmt.Errorf("variable %q not in schema", varName)
	}
	if colIdx >= len(row) {
		return 0, fmt.Errorf("variable %q column index %d out of range (row len %d)", varName, colIdx, len(row))
	}
	iv, ok := row[colIdx].(expr.IntegerValue)
	if !ok {
		return 0, fmt.Errorf("variable %q is not an IntegerValue (got %T)", varName, row[colIdx])
	}
	return graph.NodeID(iv), nil
}

// Close closes the child operator.
func (op *CreateRelationship) Close() error {
	return op.child.Close()
}
