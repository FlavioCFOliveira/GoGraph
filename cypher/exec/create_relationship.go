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
	"errors"
	"fmt"

	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/lpg"
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
		if errors.Is(err, errNullEndpoint) {
			// Null endpoint (typically from OPTIONAL MATCH): propagate
			// the row unchanged, leaving the relationship variable
			// (if any) at NULL.
			*out = nullRowWithRel(childRow, op.relVar)
			return true, nil
		}
		return false, fmt.Errorf("exec: CreateRelationship start: %w", err)
	}
	dstID, err := op.resolveNodeID(op.endVar, childRow)
	if err != nil {
		if errors.Is(err, errNullEndpoint) {
			*out = nullRowWithRel(childRow, op.relVar)
			return true, nil
		}
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
	// Bump the Cypher CREATE-multiplicity counter even when AddEdge
	// silently no-ops a duplicate (a→b) in simple-graph storage —
	// `MATCH (a:A),(b:B) MERGE (a)-[r:T]->(b) RETURN count(r)` must
	// see one row per CREATE statement, not one per distinct storage
	// entry (Merge5 [3]).
	instanceIdx := op.mutator.IncEdgeCreateCount(srcLabel, dstLabel)
	if op.relType != "" {
		op.mutator.SetEdgeLabelAt(srcLabel, dstLabel, instanceIdx, op.relType)
	}

	props := mergeProps(op.props, op.propsExprFn, childRow)

	for _, p := range props {
		if err := op.mutator.SetEdgeProperty(srcLabel, dstLabel, p.key, p.value); err != nil {
			return false, fmt.Errorf("exec: CreateRelationship SetEdgeProperty %q: %w", p.key, err)
		}
		op.mutator.SetEdgePropertyAt(srcLabel, dstLabel, instanceIdx, p.key, p.value)
	}

	if op.relVar == "" {
		*out = childRow
		return true, nil
	}

	// Mirror the just-set properties onto the emitted RelationshipValue
	// so a following `RETURN r.prop` reads the value through the row
	// binding without an extra graph round-trip. The keys map onto
	// expr.Value via lpgPropertyValueToExpr; for now propagate the
	// well-typed scalars directly via the small set we know about
	// (string/int/float/bool — temporals retain their SOH-tagged form
	// when round-tripped through the graph, so a fresh read would
	// reconstruct them, but the per-row binding does not).
	relProps := make(expr.MapValue, len(props))
	for _, p := range props {
		if v, ok := lpgPropToExprBinding(p.value); ok {
			relProps[p.key] = v
		}
	}
	rel := expr.RelationshipValue{
		ID:         uint64(actualSrcID)<<32 | uint64(actualDstID), // synthetic edge ID
		StartID:    uint64(actualSrcID),
		EndID:      uint64(actualDstID),
		Type:       op.relType,
		Properties: relProps,
	}
	newRow := make(Row, len(childRow)+1)
	copy(newRow, childRow)
	newRow[len(childRow)] = rel
	*out = newRow
	return true, nil
}

// resolveNodeID extracts the NodeID stored at the column position of varName.
// The bound value may be either an IntegerValue (the canonical encoding the
// physical operators emit) or a NodeValue (the form a projection alias
// carries after `WITH n AS a` — node values flow through unchanged). Both
// are accepted; other kinds raise the type error.
func (op *CreateRelationship) resolveNodeID(varName string, row Row) (graph.NodeID, error) {
	colIdx, ok := op.schema[varName]
	if !ok {
		return 0, fmt.Errorf("variable %q not in schema", varName)
	}
	if colIdx >= len(row) {
		return 0, fmt.Errorf("variable %q column index %d out of range (row len %d)", varName, colIdx, len(row))
	}
	switch val := row[colIdx].(type) {
	case expr.IntegerValue:
		return graph.NodeID(val), nil
	case expr.NodeValue:
		return graph.NodeID(val.ID), nil
	}
	if expr.IsNull(row[colIdx]) {
		// OPTIONAL-MATCH-bound or otherwise null receiver: signal the
		// caller to skip the relationship creation gracefully.
		return 0, errNullEndpoint
	}
	return 0, fmt.Errorf("variable %q is not an IntegerValue (got %T)", varName, row[colIdx])
}

// errNullEndpoint signals that a CreateRelationship endpoint variable
// resolved to NULL — callers convert this to "skip this row" rather than
// surfacing it as a hard error, matching the openCypher semantics of
// `CREATE (a)-[:T]->(b)` after an OPTIONAL MATCH that did not bind a/b.
var errNullEndpoint = fmt.Errorf("create relationship: endpoint is null")

// nullRowWithRel returns a copy of childRow extended by one column
// holding NULL when the operator binds a relationship variable, or the
// original row unchanged otherwise.
func nullRowWithRel(childRow Row, relVar string) Row {
	if relVar == "" {
		return childRow
	}
	r := make(Row, len(childRow)+1)
	copy(r, childRow)
	r[len(childRow)] = expr.Null
	return r
}

// Close closes the child operator.
func (op *CreateRelationship) Close() error {
	return op.child.Close()
}

// lpgPropToExprBinding converts an [lpg.PropertyValue] into the
// corresponding [expr.Value] for a row-binding payload. It mirrors the
// SOH-tagged temporal decoding handled by cypher/api.go's
// lpgPropToExpr / decodeTemporalString — duplicated here so the
// CreateRelationship operator can populate the RelationshipValue.
// Properties map without importing the cypher package (which would
// be a cycle).
func lpgPropToExprBinding(pv lpg.PropertyValue) (expr.Value, bool) {
	switch pv.Kind() {
	case lpg.PropString:
		if s, ok := pv.String(); ok {
			if v, decoded := decodeTemporalBinding(s); decoded {
				return v, true
			}
			return expr.StringValue(s), true
		}
	case lpg.PropInt64:
		if i, ok := pv.Int64(); ok {
			return expr.IntegerValue(i), true
		}
	case lpg.PropFloat64:
		if f, ok := pv.Float64(); ok {
			return expr.FloatValue(f), true
		}
	case lpg.PropBool:
		if b, ok := pv.Bool(); ok {
			return expr.BoolValue(b), true
		}
	case lpg.PropList:
		if elems, ok := pv.List(); ok {
			lv := make(expr.ListValue, 0, len(elems))
			for _, el := range elems {
				if v, ok2 := lpgPropToExprBinding(el); ok2 {
					lv = append(lv, v)
				}
			}
			return lv, true
		}
	}
	return nil, false
}

// decodeTemporalBinding mirrors cypher/api.go's decodeTemporalString:
// recognises the SOH-range tag introduced by
// cypher/exec/temporal_literal.go and returns the matching temporal
// Value. Returns (nil, false) when s does not start with a recognised
// tag byte.
func decodeTemporalBinding(s string) (expr.Value, bool) {
	if len(s) < 2 {
		return nil, false
	}
	body := s[1:]
	switch s[0] {
	case 0x01:
		if v, err := expr.ParseDate(body); err == nil {
			return v, true
		}
	case 0x02:
		if v, err := expr.ParseLocalDateTime(body); err == nil {
			return v, true
		}
	case 0x03:
		if v, err := expr.ParseDateTime(body); err == nil {
			return v, true
		}
	case 0x04:
		if v, err := expr.ParseLocalTime(body); err == nil {
			return v, true
		}
	case 0x05:
		if v, err := expr.ParseTime(body); err == nil {
			return v, true
		}
	case 0x06:
		if v, err := expr.ParseDuration(body); err == nil {
			return v, true
		}
	}
	return nil, false
}
