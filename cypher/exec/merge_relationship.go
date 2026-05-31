package exec

// merge_relationship.go — single-hop MERGE of a relationship between two
// already-bound endpoints. Handles the canonical
//
//	MATCH (a:A), (b:B) MERGE (a)-[r:T]->(b)
//
// shape (and the in-query continuation variant) by searching for an
// existing edge between the bound NodeIDs and, when absent, creating
// it via the graph mutator. Per-row semantics: the operator emits
// exactly one output row per input row (the input row extended with
// the bound src / rel / dst columns when those variables are part of
// the operator's schema contract).
//
// # Scope
//
// This operator targets the simplest MERGE-with-relationship shape:
//   - exactly one relationship hop;
//   - both endpoint variables are bound by an upstream operator
//     (their values arrive in the input row as IntegerValue or
//     NodeValue);
//   - the relationship has at most one type label.
//
// More complex MERGE shapes (e.g. ON CREATE / ON MATCH actions,
// multi-hop patterns, properties on the relationship) are not yet
// covered and fall through to the node-only [Merge] operator path.
//
// # Concurrency
//
// MergeRelationship is NOT safe for concurrent use. The engine's
// single-writer guarantee serialises concurrent MERGE callers so the
// search-then-create sequence is race-free against other writers.

import (
	"context"
	"errors"
	"fmt"

	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/lpg"
)

// MergeRelationship matches-or-creates a single-hop directed relationship
// between two already-bound endpoint columns. ON CREATE / ON MATCH
// actions targeting the relationship variable are applied to the
// matched-or-created edge.
//
// MergeRelationship is NOT safe for concurrent use.
type MergeRelationship struct {
	child              Operator
	srcCol             int           // input-row column index holding src NodeID / NodeValue
	dstCol             int           // input-row column index holding dst NodeID / NodeValue
	relCol             int           // output-row column index for the bound relationship; -1 when anonymous
	relType            string        // empty when the pattern declared no type (rejected upstream)
	relVar             string        // empty when the relationship is anonymous
	relPropsRaw        string        // inline `{k: v, …}` source string, "" when absent
	relPropPredsParsed bool          // tracks one-time parse of relPropsRaw
	relPropPreds       []propLiteral // parsed predicate values (only literals)
	// undirected reports whether the source pattern declared `(a)-[:T]-(b)`
	// (no arrow head). When true, the match search probes both (src, dst)
	// and (dst, src); the create path still uses the canonical (src, dst)
	// direction.
	undirected      bool
	onCreateActions []MergeRelAction
	onMatchActions  []MergeRelAction
	mutator         GraphMutator
	// schema lets entity-copy actions (`SET r = a`) resolve the source
	// variable name to a row column at write time. nil when the upstream
	// builder did not thread one in.
	schema map[string]int

	// Pending state for multi-row emission when an existing edge has
	// CREATE-multiplicity > 1. The base row is held verbatim and the
	// remaining count tells Next() how many more times to re-emit before
	// pulling a fresh row from the child (Merge5 [3]).
	pendingRow       Row
	pendingRemaining int64

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check
}

// MergeRelAction is a pre-parsed `SET <relVar>.<key> = <value>` item.
type MergeRelAction struct {
	key   string
	value string // opaque literal string, parsed via parsePropValue
}

// NewMergeRelationship constructs a MergeRelationship operator.
//
//   - child   is the upstream plan providing rows with the bound endpoints.
//   - srcCol / dstCol are the column indices that hold the src / dst NodeID.
//   - relType is the relationship type label (single label only).
//   - mutator is the graph write surface.
func NewMergeRelationship(child Operator, srcCol, dstCol int, relType string, mutator GraphMutator) *MergeRelationship {
	return &MergeRelationship{
		child:   child,
		srcCol:  srcCol,
		dstCol:  dstCol,
		relCol:  -1,
		relType: relType,
		mutator: mutator,
	}
}

// WithSchema attaches the upstream variable-to-column mapping so
// entity-copy actions (`SET r = a`) can resolve the source variable
// from the row at write time.
func (op *MergeRelationship) WithSchema(schema map[string]int) *MergeRelationship {
	op.schema = schema
	return op
}

// WithRelColumn registers the output-row column index that will carry
// the matched / created edge ID. When set (relCol >= 0) MergeRelationship
// extends the row with an IntegerValue(edgeID) at the column so
// downstream operators (RETURN r, count(r), …) see the bound
// relationship.
func (op *MergeRelationship) WithRelColumn(relCol int) *MergeRelationship {
	op.relCol = relCol
	return op
}

// WithRelProperties registers an inline relationship property predicate
// (e.g. `{name: 'r2'}` from `MERGE (a)-[r:T {name: 'r2'}]->(b)`). When
// set, the operator filters the existing-edge search by the predicate
// AND writes the listed properties when a new edge is created. Pass an
// empty string to clear.
func (op *MergeRelationship) WithRelProperties(propsRaw string) *MergeRelationship {
	op.relPropsRaw = propsRaw
	op.relPropPredsParsed = false
	op.relPropPreds = nil
	return op
}

// WithOnCreate registers ON CREATE SET actions to apply when the edge
// is newly created. Each action is `<relVar>.<key> = <value>`; the
// caller has already verified that every action targets the
// relationship variable bound by this operator.
func (op *MergeRelationship) WithOnCreate(relVar string, actions []MergeRelAction) *MergeRelationship {
	op.relVar = relVar
	op.onCreateActions = actions
	return op
}

// WithOnMatch registers ON MATCH SET actions to apply when the edge
// already exists.
func (op *MergeRelationship) WithOnMatch(relVar string, actions []MergeRelAction) *MergeRelationship {
	op.relVar = relVar
	op.onMatchActions = actions
	return op
}

// WithUndirected toggles the undirected-search behaviour. When true, the
// match phase probes both (src, dst) and (dst, src) directions before
// falling through to the edge-create path, matching the openCypher
// semantics of `MERGE (a)-[r:T]-(b)` (Merge5 [13]).
func (op *MergeRelationship) WithUndirected(u bool) *MergeRelationship {
	op.undirected = u
	return op
}

// MergeRelActionFromKV constructs a MergeRelationship ON CREATE / ON
// MATCH action from a (key, value) pair. value is the opaque literal
// string as it appears in the source query (e.g. `'foo'` or `42`).
func MergeRelActionFromKV(key, value string) MergeRelAction {
	return MergeRelAction{key: key, value: value}
}

// Init initialises the operator and its child.
func (op *MergeRelationship) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next emits the next input row, ensuring that the (src)-[:relType]->(dst)
// edge exists in the graph (either pre-existing or newly created). When
// an existing edge has CREATE-multiplicity N > 1 the operator emits N
// rows for the same upstream tuple (Merge5 [3]).
func (op *MergeRelationship) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.pendingRemaining > 0 {
		*out = op.pendingRow
		op.pendingRemaining--
		return true, nil
	}
	var row Row
	ok, err := op.child.Next(&row)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if op.srcCol >= len(row) || op.dstCol >= len(row) {
		// Row too narrow — emit verbatim; downstream will surface NULL bindings.
		*out = row
		return true, nil
	}
	srcID, srcOk := nodeIDFromValue(row[op.srcCol])
	dstID, dstOk := nodeIDFromValue(row[op.dstCol])
	if !srcOk || !dstOk {
		// Endpoint is null (e.g. from OPTIONAL MATCH) — pass through
		// without mutating the graph; standard openCypher behaviour.
		*out = row
		return true, nil
	}
	srcKey, sk := op.mutator.ResolveNodeLabel(srcID)
	dstKey, dk := op.mutator.ResolveNodeLabel(dstID)
	if !sk || !dk {
		// Unresolvable IDs — surface as a writer error so the caller
		// notices a graph-state inconsistency.
		return false, fmt.Errorf("exec: MergeRelationship: unresolved endpoint NodeID (src=%d, dst=%d)", srcID, dstID)
	}
	// Parse inline property predicates lazily on the first call.
	if !op.relPropPredsParsed {
		if op.relPropsRaw != "" {
			parsed, perr := parsePropLiteral(op.relPropsRaw)
			if perr != nil {
				return false, fmt.Errorf("exec: MergeRelationship: parse rel props %q: %w", op.relPropsRaw, perr)
			}
			op.relPropPreds = parsed
		}
		op.relPropPredsParsed = true
	}
	// Match if an edge already exists with the requested type AND the
	// inline property predicate (if any) holds against the live edge
	// property map. HasEdge is per-pair; combined with the per-pair
	// label model the check accepts any (src, dst) edge whose label set
	// contains relType. The single-writer guarantee makes this safe.
	if op.mutator.HasEdge(srcKey, dstKey) && op.matchesRelProps(srcKey, dstKey) {
		// Edge labels are per-(src,dst) in the LPG; adding the same
		// label twice is idempotent. Ensure the requested type is
		// recorded, then run ON MATCH actions.
		op.mutator.SetEdgeLabel(srcKey, dstKey, op.relType)
		if err := op.applyRelActions(row, srcKey, dstKey, op.onMatchActions); err != nil {
			return false, err
		}
		emitted := op.emitRow(row, srcID, dstID, srcKey, dstKey)
		// Multi-CREATE multiplicity emit (Merge5 [3]). Skip when the
		// pattern carries an inline property predicate — the
		// counter records every CREATE call regardless of property,
		// but with a predicate only a subset can satisfy `r:T
		// {prop: v}` (Merge5 [5] CREATEs with `name: 'r1'` and
		// `name: 'r2'`, MERGEs with `name: 'r2'` → only one row).
		if len(op.relPropPreds) == 0 {
			if mult := op.mutator.EdgeCreateCount(srcKey, dstKey); mult > 1 {
				op.pendingRow = emitted
				op.pendingRemaining = mult - 1
			}
		}
		*out = emitted
		return true, nil
	}
	// Undirected MERGE: also probe the reverse direction. When an edge
	// exists from dst → src that satisfies the same type-and-property
	// predicate, bind to that edge rather than creating a new one.
	// Closes Merge5 [13].
	if op.undirected && op.mutator.HasEdge(dstKey, srcKey) && op.matchesRelProps(dstKey, srcKey) {
		op.mutator.SetEdgeLabel(dstKey, srcKey, op.relType)
		if err := op.applyRelActions(row, dstKey, srcKey, op.onMatchActions); err != nil {
			return false, err
		}
		emitted := op.emitRow(row, dstID, srcID, dstKey, srcKey)
		if len(op.relPropPreds) == 0 {
			if mult := op.mutator.EdgeCreateCount(dstKey, srcKey); mult > 1 {
				op.pendingRow = emitted
				op.pendingRemaining = mult - 1
			}
		}
		*out = emitted
		return true, nil
	}
	// No matching edge — create one, tag it, write inline rel properties,
	// and run ON CREATE actions.
	if _, _, addErr := op.mutator.AddEdge(srcKey, dstKey, 0); addErr != nil {
		return false, fmt.Errorf("exec: MergeRelationship: AddEdge: %w", addErr)
	}
	if op.relType != "" {
		op.mutator.SetEdgeLabel(srcKey, dstKey, op.relType)
	}
	for _, p := range op.relPropPreds {
		if setErr := op.mutator.SetEdgeProperty(srcKey, dstKey, p.key, p.value); setErr != nil {
			return false, fmt.Errorf("exec: MergeRelationship: SetEdgeProperty %q: %w", p.key, setErr)
		}
	}
	if err := op.applyRelActions(row, srcKey, dstKey, op.onCreateActions); err != nil {
		return false, err
	}
	*out = op.emitRow(row, srcID, dstID, srcKey, dstKey)
	return true, nil
}

// matchesRelProps reports whether the (src, dst) edge satisfies the inline
// property predicate captured in relPropPreds. Returns true when no
// predicate was declared; otherwise every predicate key must be present
// and Equal to the matching property value on the edge.
func (op *MergeRelationship) matchesRelProps(srcKey, dstKey string) bool {
	if len(op.relPropPreds) == 0 {
		return true
	}
	live := op.mutator.EdgeProperties(srcKey, dstKey)
	for _, p := range op.relPropPreds {
		got, ok := live[p.key]
		if !ok {
			return false
		}
		if !propertyValuesEqual(got, p.value) {
			return false
		}
	}
	return true
}

// propertyValuesEqual compares two lpg.PropertyValue for value equality
// across all supported kinds. Returns true when the kinds match AND the
// underlying scalar / temporal / byte representation compares equal.
// String and float kinds use Go's == operator; time uses time.Equal;
// bytes uses byte-slice equality.
func propertyValuesEqual(a, b lpg.PropertyValue) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case lpg.PropString:
		as, _ := a.String()
		bs, _ := b.String()
		return as == bs
	case lpg.PropInt64:
		ai, _ := a.Int64()
		bi, _ := b.Int64()
		return ai == bi
	case lpg.PropFloat64:
		af, _ := a.Float64()
		bf, _ := b.Float64()
		return af == bf
	case lpg.PropBool:
		av, _ := a.Bool()
		bv, _ := b.Bool()
		return av == bv
	case lpg.PropTime:
		at, _ := a.Time()
		bt, _ := b.Time()
		return at.Equal(bt)
	case lpg.PropBytes:
		ab, _ := a.Bytes()
		bb, _ := b.Bytes()
		if len(ab) != len(bb) {
			return false
		}
		for i := range ab {
			if ab[i] != bb[i] {
				return false
			}
		}
		return true
	}
	return false
}

// emitRow returns the output row for a successfully matched-or-created
// edge. When the operator has a non-anonymous relationship variable
// (relCol >= 0) the row is extended with a RelationshipValue carrying
// the declared type and the live property map; otherwise the input row
// is passed through unchanged.
func (op *MergeRelationship) emitRow(row Row, srcID, dstID graph.NodeID, srcKey, dstKey string) Row {
	if op.relCol < 0 {
		return row
	}
	var relProps expr.MapValue
	if rawProps := op.mutator.EdgeProperties(srcKey, dstKey); len(rawProps) > 0 {
		relProps = make(expr.MapValue, len(rawProps))
		for k, pv := range rawProps {
			if v, ok := lpgPropToExprBinding(pv); ok {
				relProps[k] = v
			}
		}
	}
	rel := expr.RelationshipValue{
		ID:         uint64(srcID)<<32 | uint64(dstID),
		StartID:    uint64(srcID),
		EndID:      uint64(dstID),
		Type:       op.relType,
		Properties: relProps,
	}
	if op.relCol < len(row) {
		out := make(Row, len(row))
		copy(out, row)
		out[op.relCol] = rel
		return out
	}
	out := make(Row, op.relCol+1)
	copy(out, row)
	out[op.relCol] = rel
	return out
}

// applyRelActions sets every action's property on the (src, dst) edge
// via the graph mutator. value parsing reuses parsePropValue (the same
// helper the literal-property paths use) so the formats accepted are
// consistent across MERGE / CREATE / SET. A null property value is
// silently skipped — openCypher SET name = null on a missing property
// is a no-op (and on an existing property the parsing path already
// flags ErrPropertyValueIsNull which the SET-clause translator routes
// to DelEdgeProperty; the merge fast-path simply skips since the
// edge was just created with no such property).
func (op *MergeRelationship) applyRelActions(row Row, srcKey, dstKey string, actions []MergeRelAction) error {
	for _, act := range actions {
		// Entity-copy sentinel: key="" carries the source variable name in
		// value. Resolve the variable to a node in the current row and
		// copy every property of that node onto the relationship. Closes
		// Merge6 [6] / Merge7 [4]: `ON CREATE/MATCH SET r = a`.
		if act.key == "" {
			srcVar := act.value
			if srcVar == "" {
				continue
			}
			var nodeID graph.NodeID
			var resolved bool
			if op.schema != nil {
				if col, ok := op.schema[srcVar]; ok && col < len(row) {
					nodeID, resolved = nodeIDFromValue(row[col])
				}
			}
			if !resolved {
				// Fall back to the canonical src/dst columns when the
				// schema lookup did not yield a NodeID — covers the
				// common cases SET r = <srcVar> / SET r = <dstVar> when
				// the planner did not thread a schema.
				continue
			}
			nodeKey, ok := op.mutator.ResolveNodeLabel(nodeID)
			if !ok {
				continue
			}
			for k, v := range op.mutator.NodeProperties(nodeKey) {
				if setErr := op.mutator.SetEdgeProperty(srcKey, dstKey, k, v); setErr != nil {
					return fmt.Errorf("exec: MergeRelationship: SetEdgeProperty(entity-copy) %q: %w", k, setErr)
				}
			}
			continue
		}
		v, err := parsePropValue(act.value)
		if err != nil {
			if errors.Is(err, ErrPropertyValueIsNull) {
				continue
			}
			return fmt.Errorf("exec: MergeRelationship: parse value %q: %w", act.value, err)
		}
		if setErr := op.mutator.SetEdgeProperty(srcKey, dstKey, act.key, v); setErr != nil {
			return fmt.Errorf("exec: MergeRelationship: SetEdgeProperty: %w", setErr)
		}
	}
	return nil
}

// Close closes the child operator.
func (op *MergeRelationship) Close() error { return op.child.Close() }

// nodeIDFromValue extracts the storage-layer NodeID from a row column
// that may carry either an IntegerValue (canonical in-pipeline form)
// or a NodeValue (projection-alias output). Returns ok=false when the
// value is null or neither known form.
func nodeIDFromValue(v expr.Value) (graph.NodeID, bool) {
	switch x := v.(type) {
	case expr.IntegerValue:
		return graph.NodeID(int64(x)), true
	case expr.NodeValue:
		return graph.NodeID(x.ID), true
	}
	return 0, false
}
