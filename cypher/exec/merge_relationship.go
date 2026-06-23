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

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
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

// MergeRelAction is a pre-parsed `SET <relVar>.<key> = <value>` item, or a
// whole-entity REPLACE sentinel (#1687).
//
// Three shapes:
//   - Single-property write: key != "", value is the literal string.
//   - Entity-copy: key == "", value == "<sourceVar>". When replace is true the
//     edge's properties absent from the source entity are cleared first.
//   - Replace-map sentinel: key == "", value == "", replace == true. retainKeys
//     lists the RHS map keys; the edge's properties absent from retainKeys are
//     cleared. The sentinel is immediately followed by the per-key write actions
//     for the map, so the clear precedes the writes. An empty (non-nil)
//     retainKeys clears every property (`SET r = {}`).
type MergeRelAction struct {
	key        string
	value      string // opaque literal string, parsed via parsePropValue
	replace    bool   // whole-entity `=` replace (clear absent keys first)
	retainKeys []string
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

// MergeRelActionReplaceFromKV constructs a MergeRelationship ON CREATE / ON
// MATCH action carrying the whole-entity REPLACE marker (#1687). When replace
// is true the action is either a replace-map sentinel (key == "", value == "",
// retainKeys lists the RHS keys to keep) or an entity-copy replace
// (key == "", value == "<sourceVar>", retainKeys nil → retain the source's
// live keys). retainKeys is copied defensively so the caller may reuse its
// slice. When replace is false this is equivalent to MergeRelActionFromKV.
func MergeRelActionReplaceFromKV(key, value string, replace bool, retainKeys []string) MergeRelAction {
	var rk []string
	if retainKeys != nil {
		rk = make([]string, len(retainKeys))
		copy(rk, retainKeys)
	}
	return MergeRelAction{key: key, value: value, replace: replace, retainKeys: rk}
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
	// property map. The type check is essential on a multigraph: HasEdge
	// is type-agnostic per (src, dst) pair, so without edgeHasRequestedType
	// a `MERGE (a)-[:T2]->(b)` issued after a `T1` edge already exists
	// would bind to the T1 edge and never create the distinct T2 parallel
	// edge (rmp #1683). The single-writer guarantee makes this safe.
	if op.mutator.HasEdge(srcKey, dstKey) && op.edgeHasRequestedType(srcKey, dstKey) && op.matchesRelProps(srcKey, dstKey) {
		// Edge labels are per-(src,dst) in the LPG; adding the same
		// label twice is idempotent. Ensure the requested type is
		// recorded, then run ON MATCH actions.
		op.mutator.SetEdgeLabel(srcKey, dstKey, op.relType)
		// Resolve the matched edge's by-pair handle so ON MATCH property writes
		// mirror onto its by-handle store (#1684); 0 ⇒ per-pair store only.
		matchedHandle, _ := op.mutator.FirstEdgeHandle(srcKey, dstKey)
		if err := op.applyRelActions(row, srcKey, dstKey, matchedHandle, op.onMatchActions); err != nil {
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
	if op.undirected && op.mutator.HasEdge(dstKey, srcKey) && op.edgeHasRequestedType(dstKey, srcKey) && op.matchesRelProps(dstKey, srcKey) {
		op.mutator.SetEdgeLabel(dstKey, srcKey, op.relType)
		// Reverse-direction match: the edge is stored (dstKey -> srcKey), so
		// resolve and mirror against that stored pair (#1684).
		matchedHandle, _ := op.mutator.FirstEdgeHandle(dstKey, srcKey)
		if err := op.applyRelActions(row, dstKey, srcKey, matchedHandle, op.onMatchActions); err != nil {
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
	// and run ON CREATE actions. Use AddEdgeH (not AddEdge) so the new
	// edge carries a stable per-edge handle, and record the type/properties
	// by that handle as well as the per-pair union. Without the per-handle
	// identity, two MERGE-created parallel edges of distinct types would
	// share the per-pair label union and the read path would report a
	// single merged type for both (rmp #1683); the handle lets the read
	// path resolve each parallel edge's own type, exactly as a parallel
	// CREATE does. The per-pair SetEdgeLabel is retained so the match path
	// above (edgeHasRequestedType, which reads the pair union) keeps
	// recognising the type on a subsequent idempotent MERGE.
	_, _, handle, addErr := op.mutator.AddEdgeH(srcKey, dstKey, 0)
	if addErr != nil {
		return false, fmt.Errorf("exec: MergeRelationship: AddEdge: %w", addErr)
	}
	if op.relType != "" {
		op.mutator.SetEdgeLabel(srcKey, dstKey, op.relType)
		op.mutator.SetEdgeLabelByHandle(srcKey, dstKey, handle, op.relType)
	}
	for _, p := range op.relPropPreds {
		if setErr := op.mutator.SetEdgeProperty(srcKey, dstKey, p.key, p.value); setErr != nil {
			return false, fmt.Errorf("exec: MergeRelationship: SetEdgeProperty %q: %w", p.key, setErr)
		}
		if setErr := op.mutator.SetEdgePropertyByHandle(srcKey, dstKey, handle, p.key, p.value); setErr != nil {
			return false, fmt.Errorf("exec: MergeRelationship: SetEdgePropertyByHandle %q: %w", p.key, setErr)
		}
	}
	// ON CREATE actions target the edge just allocated above, so pass its known
	// handle directly — in a multigraph FirstEdgeHandle could resolve a
	// pre-existing parallel sibling's slot, not this new edge's (#1684).
	if err := op.applyRelActions(row, srcKey, dstKey, handle, op.onCreateActions); err != nil {
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
		// Route through the shared MERGE equality helper so relationship
		// MERGE matches with the same openCypher `=` semantics as node MERGE,
		// including cross-type numeric equality (1 == 1.0). See
		// [mergePropValueEquals] in merge_search.go (rmp #1240).
		if !mergePropValueEquals(got, p.value) {
			return false
		}
	}
	return true
}

// edgeHasRequestedType reports whether the directed edge (src, dst)
// carries op.relType among its labels. MERGE always declares exactly one
// relationship type (the empty case is rejected upstream), so a match
// requires that type to be present on the pair; otherwise the pattern
// must create its own (possibly parallel) edge. On a multigraph the pair
// label set is the union over every parallel edge, so this answers "does
// SOME edge of this type already exist between the pair" — the right
// pair-level question for MERGE's match-or-create decision (rmp #1683).
func (op *MergeRelationship) edgeHasRequestedType(srcKey, dstKey string) bool {
	if op.relType == "" {
		return true
	}
	for _, l := range op.mutator.EdgeLabels(srcKey, dstKey) {
		if l == op.relType {
			return true
		}
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

// applyRelActions applies each ON MATCH / ON CREATE SET action to the edge
// (srcKey, dstKey) via the graph mutator. Value parsing reuses parsePropValue
// (the same helper the literal-property paths use) so the formats accepted are
// consistent across MERGE / CREATE / SET. A null property value is silently
// skipped — openCypher SET name = null on a missing property is a no-op (and on
// an existing property the SET-clause translator routes ErrPropertyValueIsNull
// to DelEdgeProperty; the merge path simply skips).
//
// Every per-pair property write is mirrored onto the edge's by-handle store
// (#1684) under handle, so the by-handle READ path reports the post-action value
// rather than a stale CREATE-time snapshot (Merge7 [1]-[5]). handle is the stable
// per-edge handle of the edge the actions target: the just-allocated handle on
// the ON CREATE path, or the matched edge's by-PAIR first-slot handle (via
// [GraphMutator.FirstEdgeHandle]) on the ON MATCH path. MERGE binds a single
// logical (srcKey, dstKey) edge, so the by-pair handle is the right identity —
// not a positional instance handle. handle == 0 means the edge carries no stable
// handle (simple-graph / pre-handle storage): the by-handle mirror is skipped and
// only the per-pair store is written, byte-identical to the pre-#1684 behaviour.
//
// A whole-entity REPLACE item (`SET r = {…}` / `SET r = node` with the `=`
// operator) carries true openCypher REPLACE semantics (#1687): the edge's
// existing properties that are absent from the right-hand side are cleared
// before the RHS is applied. The clear is performed key by key via
// DelEdgeProperty / DelEdgePropertyByHandle so it lands in the same
// transaction (the mutator records each deletion's inverse on the undo log,
// so a rolled-back statement restores the cleared values exactly) and the
// by-handle store stays congruent with the per-pair store (#1684). The
// mutate form (`SET r += {…}`) and single-property writes are additive: no
// clear is performed.
func (op *MergeRelationship) applyRelActions(row Row, srcKey, dstKey string, handle uint64, actions []MergeRelAction) error {
	for _, act := range actions {
		// Replace-map sentinel: key=="" && value=="" && replace. Clear every
		// existing edge property absent from retainKeys before the per-key
		// write actions that follow apply the new values.
		if act.replace && act.key == "" && act.value == "" {
			op.clearRelPropsAbsent(srcKey, dstKey, handle, act.retainKeys)
			continue
		}
		// Entity-copy sentinel: key=="" carries the source variable name in
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
			srcProps := op.mutator.NodeProperties(nodeKey)
			// REPLACE (`SET r = node`): clear the edge's properties absent
			// from the source entity before the copy, so the edge ends up
			// with exactly the source's property set (#1687). The mutate
			// form (`SET r += node`) skips the clear and is additive.
			if act.replace {
				retain := make([]string, 0, len(srcProps))
				for k := range srcProps {
					retain = append(retain, k)
				}
				op.clearRelPropsAbsent(srcKey, dstKey, handle, retain)
			}
			// Copy, mirrored key-by-key to the by-handle store so both
			// stores stay in lock-step (by-handle == per-pair for the
			// matched edge, #1684).
			for k, v := range srcProps {
				if setErr := op.mutator.SetEdgeProperty(srcKey, dstKey, k, v); setErr != nil {
					return fmt.Errorf("exec: MergeRelationship: SetEdgeProperty(entity-copy) %q: %w", k, setErr)
				}
				if handle != 0 {
					if setErr := op.mutator.SetEdgePropertyByHandle(srcKey, dstKey, handle, k, v); setErr != nil {
						return fmt.Errorf("exec: MergeRelationship: SetEdgePropertyByHandle(entity-copy) %q: %w", k, setErr)
					}
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
		if handle != 0 {
			if setErr := op.mutator.SetEdgePropertyByHandle(srcKey, dstKey, handle, act.key, v); setErr != nil {
				return fmt.Errorf("exec: MergeRelationship: SetEdgePropertyByHandle: %w", setErr)
			}
		}
	}
	return nil
}

// clearRelPropsAbsent removes every property currently set on the directed
// edge (srcKey, dstKey) whose key is NOT in retain, in lock-step on the
// per-pair store and (when handle != 0) the by-handle store. It implements the
// clear half of true openCypher REPLACE for `SET r = {…}` / `SET r = node`
// (#1687).
//
// The deletions go through the mutator's DelEdgeProperty /
// DelEdgePropertyByHandle, each of which records its inverse on the
// transaction undo log, so a rolled-back statement restores the cleared values
// exactly (atomicity). The retained set is taken from the per-pair snapshot;
// because every per-pair write is mirrored by-handle (#1684) the by-handle
// store holds the same key set, so clearing the same absent keys on both keeps
// them congruent.
func (op *MergeRelationship) clearRelPropsAbsent(srcKey, dstKey string, handle uint64, retain []string) {
	existing := op.mutator.EdgeProperties(srcKey, dstKey)
	if len(existing) == 0 {
		return
	}
	keep := make(map[string]struct{}, len(retain))
	for _, k := range retain {
		keep[k] = struct{}{}
	}
	for k := range existing {
		if _, ok := keep[k]; ok {
			continue
		}
		op.mutator.DelEdgeProperty(srcKey, dstKey, k)
		if handle != 0 {
			op.mutator.DelEdgePropertyByHandle(srcKey, dstKey, handle, k)
		}
	}
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
