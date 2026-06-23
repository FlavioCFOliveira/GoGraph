package exec

// set_all.go — SetAllProperties write operator.
//
// SetAllProperties implements the Cypher SET entity-replace (`SET n = …`) and
// entity-append (`SET n += …`) forms. The source side may be:
//   - Another bound entity (node or relationship): every property is copied
//     from the source to the target.
//   - A literal map: every key/value pair from the map is written to the
//     target. The map is parsed by [parsePropLiteral].
//   - A query parameter: the parameter must resolve to a map; each key/value
//     pair is written to the target.
//
// IsReplace=true models `SET n = …`: every existing property of the target is
// removed before the source is applied. IsReplace=false models `SET n += …`:
// existing properties are kept unless overwritten by a same-keyed entry in
// the source.
//
// In both modes, null values in a literal map remove the matching property
// from the target — openCypher requires that `SET n = {k: null}` deletes k
// rather than storing a null value.
//
// When the target row's entity column is missing or null (e.g. OPTIONAL MATCH
// produced no row, or the column carries a null sentinel), the operator
// passes the row through untouched without any write — matching the
// "ignore null when setting" scenarios in the openCypher TCK.
//
// # Concurrency
//
// SetAllProperties is NOT safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// SetAllProperties replaces or merges every property on an already-bound node
// or relationship per input row.
//
// SetAllProperties is NOT safe for concurrent use.
type SetAllProperties struct {
	entityVar  string
	isReplace  bool   // true for `SET n = …`, false for `SET n += …`
	sourceVar  string // bound entity copy source; empty when not used
	mapLiteral string // literal map text; empty when not used
	paramName  string // parameter name (without `$`); empty when not used

	schema     map[string]int
	relCols    *RelCols // non-nil when entityVar is a relationship
	srcRelCols *RelCols // non-nil when sourceVar is a relationship
	child      Operator
	mutator    GraphMutator
	params     map[string]expr.Value // query parameters for $name substitution
	reg        *ConstraintRegistry   // nil means no enforcement
	mgr        *index.Manager        // nil when reg is nil

	// parsedMap caches the parse of mapLiteral or the param resolution at
	// construction / WithParams.
	parsedMap []propLiteral
	// nullKeys lists the keys whose value is null in the literal/parameter
	// source map. Such keys must be removed from the target (openCypher
	// semantics for explicit null assignment via SET map).
	nullKeys []string

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewSetAllPropertiesFromEntity creates a SetAllProperties operator copying
// every property from sourceVar (a bound node or relationship) to entityVar.
// isReplace selects `=` (true) vs `+=` (false) semantics.
func NewSetAllPropertiesFromEntity(
	entityVar, sourceVar string,
	isReplace bool,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) *SetAllProperties {
	return &SetAllProperties{
		entityVar: entityVar,
		isReplace: isReplace,
		sourceVar: sourceVar,
		schema:    schema,
		child:     child,
		mutator:   mutator,
	}
}

// NewSetAllPropertiesFromMap creates a SetAllProperties operator writing every
// key/value pair from mapLiteral to entityVar. mapLiteral is the opaque
// literal-map string (e.g. `{a: 1, b: "x"}`) produced by the AST printer.
func NewSetAllPropertiesFromMap(
	entityVar, mapLiteral string,
	isReplace bool,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) (*SetAllProperties, error) {
	op := &SetAllProperties{
		entityVar:  entityVar,
		isReplace:  isReplace,
		mapLiteral: mapLiteral,
		schema:     schema,
		child:      child,
		mutator:    mutator,
	}
	if err := op.parseMapNow(nil); err != nil {
		return nil, fmt.Errorf("exec: SetAllProperties: parse map %q: %w", mapLiteral, err)
	}
	return op, nil
}

// NewSetAllPropertiesFromParam creates a SetAllProperties operator writing
// every key/value pair from the named query parameter to entityVar. The
// parameter must resolve to a MapValue at exec time; non-map values are
// treated as a no-op.
func NewSetAllPropertiesFromParam(
	entityVar, paramName string,
	isReplace bool,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) *SetAllProperties {
	return &SetAllProperties{
		entityVar: entityVar,
		isReplace: isReplace,
		paramName: paramName,
		schema:    schema,
		child:     child,
		mutator:   mutator,
	}
}

// WithConstraints attaches a ConstraintRegistry and index.Manager for
// pre-write enforcement. Both must be non-nil. Returns op for chaining.
func (op *SetAllProperties) WithConstraints(reg *ConstraintRegistry, mgr *index.Manager) *SetAllProperties {
	op.reg = reg
	op.mgr = mgr
	return op
}

// WithParams attaches query parameters for $name substitution in the literal
// map and for parameter-sourced operators. Returns op for chaining.
func (op *SetAllProperties) WithParams(params map[string]expr.Value) (*SetAllProperties, error) {
	op.params = params
	if op.mapLiteral != "" {
		if err := op.parseMapNow(params); err != nil {
			return nil, fmt.Errorf("exec: SetAllProperties: parse map %q with params: %w", op.mapLiteral, err)
		}
	}
	if op.paramName != "" {
		if err := op.parseParamMap(); err != nil {
			return nil, err
		}
	}
	return op, nil
}

// WithRelCols marks entityVar as a relationship variable and records the row
// columns that hold the src and dst NodeIDs. Must be called before the first
// Next invocation. Returns op for chaining.
func (op *SetAllProperties) WithRelCols(rc RelCols) *SetAllProperties {
	op.relCols = &rc
	return op
}

// WithSourceRelCols marks sourceVar as a relationship variable and records
// the row columns that hold its src and dst NodeIDs. Must be called before
// the first Next invocation when SourceVar is a relationship. Returns op
// for chaining.
func (op *SetAllProperties) WithSourceRelCols(rc RelCols) *SetAllProperties {
	op.srcRelCols = &rc
	return op
}

// parseMapNow parses op.mapLiteral with the supplied params and populates
// op.parsedMap together with op.nullKeys. Null values are recorded in
// nullKeys so the executor can delete the corresponding properties from the
// target while parsedMap holds only the non-null entries.
func (op *SetAllProperties) parseMapNow(params map[string]expr.Value) error {
	props, nulls, err := parseMapWithNulls(op.mapLiteral, params)
	if err != nil {
		return err
	}
	op.parsedMap = props
	op.nullKeys = nulls
	return nil
}

// parseParamMap reads op.paramName from op.params and stores its entries in
// op.parsedMap. It returns:
//   - a TypeError when the parameter is bound to a non-null, non-map value
//     (`SET n = $scalar` must not silently clear the target's properties);
//   - an InvalidPropertyType error when a map value is itself a map or a list
//     of maps (not a storable property value).
//
// A null parameter behaves like a null literal right-hand side: `SET n = $p`
// clears all properties (isReplace), `SET n += $p` is a no-op — both achieved
// by leaving the parsed map empty. A missing binding is left to the engine's
// parameter-presence check.
func (op *SetAllProperties) parseParamMap() error {
	if op.params == nil {
		return nil
	}
	v, ok := op.params[op.paramName]
	if !ok {
		return nil
	}
	if v == nil || expr.IsNull(v) {
		return nil
	}
	mv, isMap := v.(expr.MapValue)
	if !isMap {
		return fmt.Errorf("TypeError: SET %s with $%s: expected a Map but was %s", op.entityVar, op.paramName, v.Kind())
	}
	for k, vv := range mv {
		if vv == nil || expr.IsNull(vv) {
			op.nullKeys = append(op.nullKeys, k)
			continue
		}
		if !exprValueIsStorable(vv) {
			return fmt.Errorf("InvalidPropertyType: SET %s: value for key %q is a map or a list of maps, which cannot be stored as a property", op.entityVar, k)
		}
		// A list of primitives is a legal property value; route it through the
		// list encoder (valueToPropertyValue handles only scalars). Other
		// unconvertible kinds remain a defensive skip.
		var pv lpg.PropertyValue
		var perr error
		if lst, isList := vv.(expr.ListValue); isList {
			pv, perr = exprListToLPGList(lst)
		} else {
			pv, perr = valueToPropertyValue(vv)
		}
		if perr != nil {
			continue
		}
		op.parsedMap = append(op.parsedMap, propLiteral{key: k, value: pv})
	}
	return nil
}

// exprValueIsStorable reports whether v can be stored as a property value: a
// primitive, or a list whose elements are all (transitively) storable. A map,
// or a list containing a map, is not storable. Mirrors the package-level
// isStorableProperty check for the parameter-map ingestion path.
func exprValueIsStorable(v expr.Value) bool {
	switch x := v.(type) {
	case expr.MapValue:
		return false
	case expr.ListValue:
		for _, el := range x {
			if !exprValueIsStorable(el) {
				return false
			}
		}
	}
	return true
}

// Init initialises the operator and its child.
func (op *SetAllProperties) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child and applies the configured whole-entity
// mutation. The row is forwarded unchanged so downstream operators (e.g.
// ProduceResults) can read the affected entity.
func (op *SetAllProperties) Next(out *Row) (bool, error) {
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

	if op.targetIsNullRow(childRow) {
		*out = childRow
		return true, nil
	}

	target, terr := resolveEntityBinding(op.entityVar, op.schema, op.relCols, childRow, op.mutator)
	if terr != nil {
		if errors.Is(terr, errSetNullTarget) {
			*out = childRow
			return true, nil
		}
		return false, fmt.Errorf("exec: SetAllProperties %q: %w", op.entityVar, terr)
	}

	if op.sourceVar != "" {
		if applyErr := op.applyEntityCopy(target, childRow); applyErr != nil {
			return false, applyErr
		}
	} else {
		op.applyMap(target)
	}

	*out = childRow
	return true, nil
}

// targetIsNullRow reports whether the row's target column carries a null
// value. The openCypher TCK requires `OPTIONAL MATCH … SET a = {…}` to be a
// no-op when `a` is null (Set4 [5], Set5 [1]).
func (op *SetAllProperties) targetIsNullRow(row Row) bool {
	colIdx, ok := op.schema[op.entityVar]
	if !ok {
		return true
	}
	if colIdx >= len(row) {
		return true
	}
	if row[colIdx] == nil {
		return true
	}
	if expr.IsNull(row[colIdx]) {
		return true
	}
	return false
}

// applyEntityCopy reads every property from sourceVar and writes them to the
// target. The full property snapshot is taken before any write, so reading
// from a relationship and writing to the same relationship is safe.
func (op *SetAllProperties) applyEntityCopy(target entityBinding, row Row) error {
	src, err := resolveEntityBinding(op.sourceVar, op.schema, op.srcRelCols, row, op.mutator)
	if err != nil {
		// Source is missing/null/unbound: treat as no-op (openCypher's
		// null-source semantics).
		return nil //nolint:nilerr // missing source variable is a no-op
	}

	var sourceProps map[string]lpg.PropertyValue
	if src.isRel {
		sourceProps = op.mutator.EdgeProperties(src.relSrcKey, src.relDstKey)
	} else {
		sourceProps = op.mutator.NodeProperties(src.nodeKey)
	}

	if op.isReplace {
		op.clearTarget(target)
	}

	for k, v := range sourceProps {
		op.writeOne(target, k, v)
	}
	return nil
}

// applyMap writes the parsed literal map (or parameter map) to the target.
func (op *SetAllProperties) applyMap(target entityBinding) {
	if op.isReplace {
		op.clearTarget(target)
	}
	for _, k := range op.nullKeys {
		op.deleteOne(target, k)
	}
	for _, p := range op.parsedMap {
		op.writeOne(target, p.key, p.value)
	}
}

// clearTarget removes every property from the target entity. Used to
// implement `SET n = …` (replace) semantics. For relationships the per-pair
// removal is mirrored to the per-instance by-handle store (#1686).
func (op *SetAllProperties) clearTarget(target entityBinding) {
	if target.isRel {
		props := op.mutator.EdgeProperties(target.relSrcKey, target.relDstKey)
		for k := range props {
			op.deleteOne(target, k)
		}
		return
	}
	props := op.mutator.NodeProperties(target.nodeKey)
	for k := range props {
		op.mutator.DelNodeProperty(target.nodeKey, k)
	}
}

// writeOne writes a single (key, value) pair to the target, dispatching to
// the node or relationship mutator method. Constraint enforcement is
// applied for node writes when a registry is attached. For relationships the
// per-pair write is mirrored to the per-instance by-handle store (#1686).
func (op *SetAllProperties) writeOne(target entityBinding, key string, value lpg.PropertyValue) {
	if target.isRel {
		if serr := op.mutator.SetEdgeProperty(target.relSrcKey, target.relDstKey, key, value); serr != nil {
			return
		}
		if target.relHandle != 0 {
			_ = op.mutator.SetEdgePropertyByHandle(target.relSrcKey, target.relDstKey, target.relHandle, key, value)
		}
		return
	}
	if op.reg != nil {
		labels := op.mutator.NodeLabels(target.nodeKey)
		if cerr := op.reg.CheckSetProperty(labels, key, value, op.mgr); cerr != nil {
			return // skip on constraint violation; mirror SetProperty behaviour
		}
	}
	if serr := op.mutator.SetNodeProperty(target.nodeKey, key, value); serr != nil {
		return
	}
	if op.reg != nil {
		labels := op.mutator.NodeLabels(target.nodeKey)
		op.reg.RecordPropertySet(labels, key, value)
	}
}

// deleteOne removes a single property from the target, dispatching to the
// node or relationship mutator method. For relationships the per-pair removal
// is mirrored to the per-instance by-handle store (#1686).
func (op *SetAllProperties) deleteOne(target entityBinding, key string) {
	if target.isRel {
		op.mutator.DelEdgeProperty(target.relSrcKey, target.relDstKey, key)
		if target.relHandle != 0 {
			op.mutator.DelEdgePropertyByHandle(target.relSrcKey, target.relDstKey, target.relHandle, key)
		}
		return
	}
	op.mutator.DelNodeProperty(target.nodeKey, key)
}

// Close closes the child operator.
func (op *SetAllProperties) Close() error {
	return op.child.Close()
}

// resolveEntityBinding is a shared resolver used by SetAllProperties for both
// target and source columns. It mirrors the resolution performed by
// SetProperty.resolveEntity but is exposed as a free function so it can be
// invoked twice per row without instantiating a helper struct.
func resolveEntityBinding(
	varName string,
	schema map[string]int,
	relCols *RelCols,
	row Row,
	mut GraphMutator,
) (entityBinding, error) {
	colIdx, ok := schema[varName]
	if !ok {
		return entityBinding{}, fmt.Errorf("variable %q not in schema", varName)
	}
	if colIdx >= len(row) {
		return entityBinding{}, fmt.Errorf("column %d out of range (row len %d)", colIdx, len(row))
	}
	if row[colIdx] == nil || expr.IsNull(row[colIdx]) {
		return entityBinding{}, errSetNullTarget
	}
	switch v := row[colIdx].(type) {
	case expr.IntegerValue:
		if relCols != nil {
			return resolveRelBinding(relCols, row, mut)
		}
		nodeKey, resolved := mut.ResolveNodeLabel(graph.NodeID(v))
		if !resolved {
			return entityBinding{}, fmt.Errorf("cannot resolve NodeID %d", graph.NodeID(v))
		}
		return entityBinding{nodeKey: nodeKey}, nil
	case expr.NodeValue:
		nodeKey, resolved := mut.ResolveNodeLabel(graph.NodeID(v.ID))
		if !resolved {
			return entityBinding{}, fmt.Errorf("cannot resolve NodeID %d", graph.NodeID(v.ID))
		}
		return entityBinding{nodeKey: nodeKey}, nil
	case expr.RelationshipValue:
		srcKey, srcOK := mut.ResolveNodeLabel(graph.NodeID(v.StartID))
		dstKey, dstOK := mut.ResolveNodeLabel(graph.NodeID(v.EndID))
		if !srcOK || !dstOK {
			return entityBinding{}, fmt.Errorf("cannot resolve relationship endpoints (%d, %d)", v.StartID, v.EndID)
		}
		return entityBinding{isRel: true, relSrcKey: srcKey, relDstKey: dstKey}, nil
	default:
		return entityBinding{}, fmt.Errorf("variable %q is not IntegerValue/NodeValue/RelationshipValue (got %T)", varName, row[colIdx])
	}
}

// parseMapWithNulls is like [parsePropLiteralWithParams] but reports the keys
// whose values are explicit nulls separately, so the SetAllProperties operator
// can implement openCypher's "null in SET map removes the property" semantics.
//
// The map source is the opaque literal-map string produced by the AST printer
// (e.g. `{a: 1, b: null}`). Non-literal expressions are silently skipped, as
// in [parsePropLiteralWithParams]. An empty map (`{}` or whitespace) yields
// two empty slices and no error.
func parseMapWithNulls(s string, params map[string]expr.Value) (props []propLiteral, nullKeys []string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil, nil
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, nil, fmt.Errorf("expected map literal enclosed in {}, got %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil, nil, nil
	}
	parts := splitMapItems(inner)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		colonIdx := strings.Index(part, ":")
		if colonIdx < 0 {
			return nil, nil, fmt.Errorf("missing ':' in map item %q", part)
		}
		key := strings.TrimSpace(part[:colonIdx])
		key = strings.Trim(key, "`")
		valStr := strings.TrimSpace(part[colonIdx+1:])

		// A map, or a list that transitively contains a map, is not a storable
		// property value (openCypher: property values are primitives or
		// homogeneous lists of primitives). Reject with InvalidPropertyType
		// rather than silently dropping the key — matching the single-property
		// form `SET n.k = {…}` (Set1 [10]).
		if valueStringIsNonStorable(valStr) {
			return nil, nil, fmt.Errorf("InvalidPropertyType: value for key %q is a map or a list of maps, which cannot be stored as a property", key)
		}

		pv, perr := parsePropValueWithParams(valStr, params)
		if perr != nil {
			if errors.Is(perr, ErrPropertyValueIsNull) {
				nullKeys = append(nullKeys, key)
				continue
			}
			// Non-literal expression or unresolvable param: defer (skip).
			continue
		}
		props = append(props, propLiteral{key: key, value: pv})
	}
	return props, nullKeys, nil
}

// valueStringIsNonStorable reports whether a SET-map value source string
// denotes a structural type that cannot be stored as a property value: a map
// literal, or a list that (transitively) contains a map. Property values are
// restricted to primitives or homogeneous lists of primitives, so these must
// surface as InvalidPropertyType rather than being silently dropped.
//
// Detection is structural and quote-aware: a value beginning with '{' is a map
// literal (a quoted string value begins with a double or single quote, never a
// brace or bracket), and a '['-delimited list is recursed into via
// [splitMapItems], which respects string and nesting boundaries.
func valueStringIsNonStorable(valStr string) bool {
	valStr = strings.TrimSpace(valStr)
	if len(valStr) >= 2 && valStr[0] == '{' && valStr[len(valStr)-1] == '}' {
		return true // map literal
	}
	if len(valStr) >= 2 && valStr[0] == '[' && valStr[len(valStr)-1] == ']' {
		inner := strings.TrimSpace(valStr[1 : len(valStr)-1])
		if inner == "" {
			return false
		}
		for _, el := range splitMapItems(inner) {
			if valueStringIsNonStorable(el) {
				return true
			}
		}
	}
	return false
}

// valueToPropertyValue converts an expr.Value to an lpg.PropertyValue when a
// faithful mapping exists. Returns an error for kinds that have no
// PropertyValue representation. Used by SetAllProperties to ingest the
// entries of a parameter MapValue.
func valueToPropertyValue(v expr.Value) (lpg.PropertyValue, error) {
	switch x := v.(type) {
	case expr.StringValue:
		return lpg.StringValue(string(x)), nil
	case expr.IntegerValue:
		return lpg.Int64Value(int64(x)), nil
	case expr.FloatValue:
		return lpg.Float64Value(float64(x)), nil
	case expr.BoolValue:
		return lpg.BoolValue(bool(x)), nil
	default:
		return lpg.PropertyValue{}, fmt.Errorf("unsupported parameter value kind %T", v)
	}
}
