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
	"errors"
	"fmt"
	"strings"

	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/index"
	"gograph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// SetProperty
// ─────────────────────────────────────────────────────────────────────────────

// RelCols carries the raw column indices that the Expand operator places for a
// relationship variable. SetProperty and RemoveProperty use it to reconstruct
// the (src, dst) endpoint keys when the bound entity is a relationship rather
// than a node.
//
// The edgeCol (schema[entityVar]) holds the edge-position counter; SrcCol and
// DstCol hold the corresponding endpoint NodeIDs as IntegerValue.
type RelCols struct {
	SrcCol int
	DstCol int
}

// errSetNullTarget is the sentinel resolveEntity returns when the schema
// column for the SET target carries a null value. SET on a null entity is
// a no-op per openCypher 9 §3.5.4; the operator treats this sentinel as
// "skip the mutation but keep the row in the stream".
var errSetNullTarget = errors.New("exec: SET target is NULL — no-op")

// ValueEvalFn evaluates a SET RHS expression against the current input row
// and returns the resulting property value plus a flag distinguishing the
// "no value produced" case from the "explicit null" case. The exec operator
// uses null/no-value semantics to either delete (null) or no-op (no value).
type ValueEvalFn func(row Row) (value lpg.PropertyValue, isNull bool, hasValue bool, err error)

// SetProperty sets or replaces properties on an already-bound node or
// relationship per input row. The entity is identified by entityVar. For
// nodes, the column value must be an IntegerValue-encoded NodeID or a
// NodeValue. For relationships, call WithRelCols to supply the endpoint
// column indices.
//
// SetProperty is NOT safe for concurrent use.
type SetProperty struct {
	entityVar   string
	propertyKey string // empty → whole-entity assignment
	valueExpr   string // opaque literal string from IR
	valueEvalFn ValueEvalFn // non-nil → evaluate the AST per row
	merge       bool   // true when mode is SET n += {…}
	schema      map[string]int
	relCols     *RelCols // non-nil when entityVar is a relationship
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

// WithValueEvalFn attaches a per-row evaluator for the SET RHS expression.
// The closure is invoked whenever the operator needs to compute the new
// property value; it takes priority over the literal-string parser path
// for single-property assignments.
func (op *SetProperty) WithValueEvalFn(fn ValueEvalFn) *SetProperty {
	op.valueEvalFn = fn
	return op
}

// WithParams attaches query parameters for $name substitution in value
// expressions. Returns op for chaining.
func (op *SetProperty) WithParams(params map[string]expr.Value) *SetProperty {
	op.params = params
	return op
}

// WithRelCols marks entityVar as a relationship variable and records the row
// columns that hold the src and dst NodeIDs. Must be called before the first
// Next invocation. Returns op for chaining.
func (op *SetProperty) WithRelCols(rc RelCols) *SetProperty {
	op.relCols = &rc
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

	ent, err := op.resolveEntity(op.entityVar, childRow)
	if err != nil {
		if errors.Is(err, errSetNullTarget) {
			*out = childRow
			return true, nil
		}
		return false, fmt.Errorf("exec: SetProperty %q: %w", op.entityVar, err)
	}

	if ent.isRel {
		// Relationship target: dispatch to edge property methods.
		if err := op.applyToRelationship(ent.relSrcKey, ent.relDstKey, childRow); err != nil {
			return false, err
		}
	} else {
		// Node target: dispatch to node property methods.
		if err := op.applyToNode(ent.nodeKey, childRow); err != nil {
			return false, err
		}
	}

	*out = childRow
	return true, nil
}

// entityBinding is the resolved identity of a node or relationship column.
//
// Exactly one of nodeKey or (relSrcKey, relDstKey) is valid, determined by
// isRel. This avoids separate resolver calls in Next.
type entityBinding struct {
	isRel     bool
	nodeKey   string // valid when !isRel
	relSrcKey string // valid when isRel
	relDstKey string // valid when isRel
}

// resolveEntity extracts the node or relationship identity from the column at
// varName and translates internal IDs to mutator-facing string keys.
//
// When op.relCols is set the entity is a relationship variable. The schema
// column at varName may hold either:
//   - expr.IntegerValue (edge-position counter emitted directly by Expand)
//     → endpoint NodeIDs are read from relCols.SrcCol / relCols.DstCol.
//   - expr.RelationshipValue (produced by a downstream Projection that
//     already reconstructed the relationship from the raw columns)
//     → StartID / EndID are used directly.
func (op *SetProperty) resolveEntity(varName string, row Row) (entityBinding, error) {
	colIdx, ok := op.schema[varName]
	if !ok {
		return entityBinding{}, fmt.Errorf("variable %q not in schema", varName)
	}
	if colIdx >= len(row) {
		return entityBinding{}, fmt.Errorf("column %d out of range (row len %d)", colIdx, len(row))
	}
	// openCypher 9 §3.5.4: SET on a NULL entity is a no-op (the rows pass
	// through unchanged). Surface this via a sentinel that the caller turns
	// into a row pass-through. The sentinel error preserves the existing
	// resolveEntity API for non-null callers.
	if row[colIdx] == nil || expr.IsNull(row[colIdx]) {
		return entityBinding{}, errSetNullTarget
	}
	switch v := row[colIdx].(type) {
	case expr.IntegerValue:
		// Raw Expand output: either a NodeID (node variable) or an edge-position
		// counter (relationship variable). Distinguish by relCols.
		if op.relCols != nil {
			return resolveRelBinding(op.relCols.SrcCol, op.relCols.DstCol, row, op.mutator)
		}
		nodeKey, resolved := op.mutator.ResolveNodeLabel(graph.NodeID(v))
		if !resolved {
			return entityBinding{}, fmt.Errorf("cannot resolve NodeID %d", graph.NodeID(v))
		}
		return entityBinding{nodeKey: nodeKey}, nil
	case expr.NodeValue:
		nodeKey, resolved := op.mutator.ResolveNodeLabel(graph.NodeID(v.ID))
		if !resolved {
			return entityBinding{}, fmt.Errorf("cannot resolve NodeID %d", graph.NodeID(v.ID))
		}
		return entityBinding{nodeKey: nodeKey}, nil
	case expr.RelationshipValue:
		// Post-projection row: RelationshipValue already has StartID / EndID.
		srcKey, srcOK := op.mutator.ResolveNodeLabel(graph.NodeID(v.StartID))
		dstKey, dstOK := op.mutator.ResolveNodeLabel(graph.NodeID(v.EndID))
		if !srcOK || !dstOK {
			return entityBinding{}, fmt.Errorf("cannot resolve relationship endpoints (%d, %d)", v.StartID, v.EndID)
		}
		return entityBinding{isRel: true, relSrcKey: srcKey, relDstKey: dstKey}, nil
	default:
		return entityBinding{}, fmt.Errorf("variable %q is not IntegerValue/NodeValue/RelationshipValue (got %T)", varName, row[colIdx])
	}
}

// resolveRelBinding resolves a relationship entity from the (srcCol, dstCol)
// pair of row columns that hold endpoint NodeIDs as IntegerValue.
func resolveRelBinding(srcCol, dstCol int, row Row, mut GraphMutator) (entityBinding, error) {
	if srcCol >= len(row) || dstCol >= len(row) {
		return entityBinding{}, fmt.Errorf("relationship endpoint columns (%d, %d) out of range (row len %d)", srcCol, dstCol, len(row))
	}
	srcIV, srcOK := row[srcCol].(expr.IntegerValue)
	dstIV, dstOK := row[dstCol].(expr.IntegerValue)
	if !srcOK || !dstOK {
		return entityBinding{}, fmt.Errorf("relationship endpoint columns hold non-IntegerValue (%T, %T)", row[srcCol], row[dstCol])
	}
	srcKey, srcResolved := mut.ResolveNodeLabel(graph.NodeID(srcIV))
	dstKey, dstResolved := mut.ResolveNodeLabel(graph.NodeID(dstIV))
	if !srcResolved || !dstResolved {
		return entityBinding{}, fmt.Errorf("cannot resolve relationship endpoint NodeIDs (%d, %d)", graph.NodeID(srcIV), graph.NodeID(dstIV))
	}
	return entityBinding{isRel: true, relSrcKey: srcKey, relDstKey: dstKey}, nil
}

// applyToNode applies the configured property mutation to a node identified by
// its mutator-facing key.
//
//nolint:gocyclo // three assignment modes (single, merge, replace) × optional constraint enforcement
func (op *SetProperty) applyToNode(nodeKey string, row Row) error {
	if op.propertyKey != "" {
		// AST-eval path: when the IR carries a parsed expression for the
		// RHS, evaluate it per row. This handles SET n.num = n.num + 1,
		// SET n.name = a.name, and any other non-literal value.
		if op.valueEvalFn != nil {
			pv, isNull, hasValue, evalErr := op.valueEvalFn(row)
			if evalErr != nil {
				return evalErr
			}
			if isNull {
				op.mutator.DelNodeProperty(nodeKey, op.propertyKey)
				return nil
			}
			if !hasValue {
				return nil
			}
			if op.reg != nil {
				labels := op.mutator.NodeLabels(nodeKey)
				if cerr := op.reg.CheckSetProperty(labels, op.propertyKey, pv, op.mgr); cerr != nil {
					return cerr
				}
			}
			if serr := op.mutator.SetNodeProperty(nodeKey, op.propertyKey, pv); serr != nil {
				return serr
			}
			if op.reg != nil {
				labels := op.mutator.NodeLabels(nodeKey)
				op.reg.RecordPropertySet(labels, op.propertyKey, pv)
			}
			// Read-your-own-writes: refresh the row's NodeValue snapshot
			// so downstream RETURN n.<key> reads the freshly set value
			// instead of the pre-SET snapshot captured during the upstream
			// projection/upgrade. Closes List12 [1]/[2] and similar
			// SET-then-RETURN scenarios.
			op.refreshNodeRowProperties(row, pv)
			return nil
		}
		pv, parseErr := parsePropValueWithParams(op.valueExpr, op.params)
		if parseErr != nil {
			if errors.Is(parseErr, ErrPropertyValueIsNull) {
				// openCypher: SET n.k = null removes the property k from n.
				op.mutator.DelNodeProperty(nodeKey, op.propertyKey)
				return nil
			}
			return nil // non-literal expression: no-op for current IR
		}
		if op.reg != nil {
			labels := op.mutator.NodeLabels(nodeKey)
			if cerr := op.reg.CheckSetProperty(labels, op.propertyKey, pv, op.mgr); cerr != nil {
				return cerr
			}
		}
		if serr := op.mutator.SetNodeProperty(nodeKey, op.propertyKey, pv); serr != nil {
			return serr
		}
		if op.reg != nil {
			labels := op.mutator.NodeLabels(nodeKey)
			op.reg.RecordPropertySet(labels, op.propertyKey, pv)
		}
		op.refreshNodeRowProperties(row, pv)
		return nil
	}
	if op.merge {
		if op.reg != nil {
			labels := op.mutator.NodeLabels(nodeKey)
			for _, p := range op.parsedMap {
				if cerr := op.reg.CheckSetProperty(labels, p.key, p.value, op.mgr); cerr != nil {
					return cerr
				}
			}
		}
		labels := op.mutator.NodeLabels(nodeKey)
		for _, p := range op.parsedMap {
			if serr := op.mutator.SetNodeProperty(nodeKey, p.key, p.value); serr != nil {
				return serr
			}
			if op.reg != nil {
				op.reg.RecordPropertySet(labels, p.key, p.value)
			}
		}
		return nil
	}
	// SET n = {…}: replace all properties.
	if op.reg != nil {
		labels := op.mutator.NodeLabels(nodeKey)
		for _, p := range op.parsedMap {
			if cerr := op.reg.CheckSetProperty(labels, p.key, p.value, op.mgr); cerr != nil {
				return cerr
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
			return serr
		}
		if op.reg != nil {
			op.reg.RecordPropertySet(labels, p.key, p.value)
		}
	}
	return nil
}

// applyToRelationship applies the configured property mutation to a
// relationship identified by its endpoint keys. Constraint enforcement is not
// performed for relationships (the constraint registry is node-label-scoped).
func (op *SetProperty) applyToRelationship(srcKey, dstKey string, row Row) error {
	if op.propertyKey != "" {
		if op.valueEvalFn != nil {
			pv, isNull, hasValue, evalErr := op.valueEvalFn(row)
			if evalErr != nil {
				return evalErr
			}
			if isNull {
				op.mutator.DelEdgeProperty(srcKey, dstKey, op.propertyKey)
				return nil
			}
			if !hasValue {
				return nil
			}
			return op.mutator.SetEdgeProperty(srcKey, dstKey, op.propertyKey, pv)
		}
		pv, parseErr := parsePropValueWithParams(op.valueExpr, op.params)
		if parseErr != nil {
			if errors.Is(parseErr, ErrPropertyValueIsNull) {
				// openCypher: SET r.k = null removes the property k from r.
				op.mutator.DelEdgeProperty(srcKey, dstKey, op.propertyKey)
				return nil
			}
			return nil // non-literal expression: no-op for current IR
		}
		return op.mutator.SetEdgeProperty(srcKey, dstKey, op.propertyKey, pv)
	}
	if op.merge {
		for _, p := range op.parsedMap {
			if serr := op.mutator.SetEdgeProperty(srcKey, dstKey, p.key, p.value); serr != nil {
				return serr
			}
		}
		return nil
	}
	// SET r = {…}: replace is a merge for relationships (no full-property
	// snapshot available without extending GraphMutator; consistent with the
	// node implementation that reads NodeProperties first).
	for _, p := range op.parsedMap {
		if serr := op.mutator.SetEdgeProperty(srcKey, dstKey, p.key, p.value); serr != nil {
			return serr
		}
	}
	return nil
}

// Close closes the child operator.
func (op *SetProperty) Close() error {
	return op.child.Close()
}

// refreshNodeRowProperties updates the row's NodeValue slot (when present)
// so a downstream RETURN n.<propertyKey> reads the freshly-set value
// instead of the snapshot captured by an upstream projection or
// IntegerValue→NodeValue upgrade. The Properties map is copied before
// mutation so other rows sharing the same map are not aliased.
//
// pv may be either an expr.Value (from valueEvalFn) or an
// lpg.PropertyValue (from parsePropValueWithParams). The latter is
// converted via lpgPropToExprBinding.
//
// No-op when the row slot is not a NodeValue (e.g. raw IntegerValue
// NodeID), an out-of-range column, or when the property value is null.
func (op *SetProperty) refreshNodeRowProperties(row Row, pv any) {
	if op.entityVar == "" || op.propertyKey == "" {
		return
	}
	colIdx, ok := op.schema[op.entityVar]
	if !ok || colIdx >= len(row) {
		return
	}
	nv, isNode := row[colIdx].(expr.NodeValue)
	if !isNode {
		return
	}
	var val expr.Value
	switch t := pv.(type) {
	case expr.Value:
		val = t
	case lpg.PropertyValue:
		v, okConv := lpgPropToExprBinding(t)
		if !okConv {
			return
		}
		val = v
	default:
		return
	}
	props := make(expr.MapValue, len(nv.Properties)+1)
	for k, v := range nv.Properties {
		props[k] = v
	}
	if expr.IsNull(val) {
		delete(props, op.propertyKey)
	} else {
		props[op.propertyKey] = val
	}
	nv.Properties = props
	row[colIdx] = nv
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
		if errors.Is(err, errSetNullTarget) {
			*out = childRow
			return true, nil
		}
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
	if row[colIdx] == nil || expr.IsNull(row[colIdx]) {
		return 0, errSetNullTarget
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
