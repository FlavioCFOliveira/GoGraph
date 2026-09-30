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
// MergeRelationship is NOT safe for concurrent use: one operator tree is driven by
// one goroutine. Its search-then-create sequence is NOT race-free against other
// writers — nothing serialises them since rmp #2306. See [Merge] for the measured
// behaviour and the uniqueness-constraint remedy.

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
	child   Operator
	mutator GraphMutator
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
	// relPropsEvalFn evaluates the inline relationship property map against the
	// current row when it carries a non-literal value (e.g. `{kind: r.pk}`).
	// nil when every inline property is a literal ($param references are
	// resolved at build time into relPropPreds via [MergeRelationship.WithParams]).
	// The merged (literal ∪ dynamic) property set drives BOTH the existing-edge
	// search predicate and the created edge's properties, mirroring the node
	// [Merge] path — without it a row-driven inline property is silently dropped
	// (stored as null on the created edge).
	relPropsEvalFn PropsEvalFn
	// onCreateEvals / onMatchEvals map an action's target (via
	// [MergeActionEvalKey], keyed on the relationship variable and property
	// key) to a per-row RHS evaluator for a non-literal ON CREATE / ON MATCH
	// SET expression (e.g. `ON MATCH SET r.n = r.n + 1`). nil when every
	// action's RHS is a literal. See [MergeRelationship.applyRelActions].
	onCreateEvals map[string]ValueEvalFn
	onMatchEvals  map[string]ValueEvalFn
	// schema lets entity-copy actions (`SET r = a`) resolve the source
	// variable name to a row column at write time. nil when the upstream
	// builder did not thread one in.
	schema map[string]int

	relType     string // empty when the pattern declared no type (rejected upstream)
	relVar      string // empty when the relationship is anonymous
	relPropsRaw string // inline `{k: v, …}` source string, "" when absent

	relPropPreds    []propLiteral // parsed predicate values (only literals)
	onCreateActions []MergeRelAction
	onMatchActions  []MergeRelAction
	// pending holds the rows still to emit for the current driving row when
	// more than one parallel relationship matched — one row per matched
	// instance, each bound to its own handle (Merge5 [3], rmp #2939).
	// pendingIdx is the next one to emit.
	pending []Row
	// matches and handleBuf are the reusable per-row buffers the instance
	// enumeration fills, so a steady-state driving row does not allocate them.
	// They start on the inline arrays below, which cover the common pair of
	// at most a few parallel relationships without any allocation.
	matches   []mergeRelMatch
	handleBuf []uint64
	matchArr  [2]mergeRelMatch
	handleArr [4]uint64
	pendArr   [2]Row

	srcCol     int // input-row column index holding src NodeID / NodeValue
	dstCol     int // input-row column index holding dst NodeID / NodeValue
	relCol     int // output-row column index for the bound relationship; -1 when anonymous
	pendingIdx int

	relPropPredsParsed bool // tracks one-time parse of relPropsRaw
	// undirected reports whether the source pattern declared `(a)-[:T]-(b)`
	// (no arrow head). When true, the match search probes both (src, dst)
	// and (dst, src); the create path still uses the canonical (src, dst)
	// direction.
	undirected bool

	// pull receives every child Next call of this operator (see nextRow), so the
	// per-row pull does not heap-allocate its receiver.
	pull Row
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
	retainKeys []string
	replace    bool // whole-entity `=` replace (clear absent keys first)
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

// WithRelPropsEvalFn attaches a per-row evaluator for the inline relationship
// property map when it contains a non-literal value (a variable reference,
// property access, or arithmetic expression — e.g. `MERGE (a)-[r:T {kind:
// row.pk}]->(b)`). The merged (literal ∪ dynamic) property set drives both the
// existing-edge search predicate and the created edge's properties, exactly as
// the node [Merge] path does via [mergeProps]. Without it the literal-only
// parser drops the non-literal entry, so the property is neither searched on
// nor written — the created edge stores null (fail-silent Consistency defect).
// Pass nil to clear. Returns op for chaining.
func (op *MergeRelationship) WithRelPropsEvalFn(fn PropsEvalFn) *MergeRelationship {
	op.relPropsEvalFn = fn
	return op
}

// WithParams attaches query parameters for $name substitution in the inline
// relationship property map, re-parsing the raw map with parameter references
// resolved to concrete literal values. Mirrors [CreateNode.WithParams]:
// resolving parameters once here, at build time, is cheaper than a per-row
// evaluator and is correct because parameter values are constant for the whole
// query execution. Without it a parameterised inline property such as
// `MERGE (a)-[r:T {kind: $pk}]->(b)` is silently dropped, since the literal-only
// parser skips $param references (they are deferred to a resolver). Returns op
// for chaining.
func (op *MergeRelationship) WithParams(params map[string]expr.Value) (*MergeRelationship, error) {
	if len(params) == 0 {
		return op, nil
	}
	if op.relPropsRaw != "" {
		parsed, err := parsePropLiteralWithParamsMerge(op.relPropsRaw, params)
		if err != nil {
			return nil, fmt.Errorf("exec: MergeRelationship: parse rel props %q: %w", op.relPropsRaw, err)
		}
		op.relPropPreds = parsed
		op.relPropPredsParsed = true
	}
	return op, nil
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

// WithActionEvals attaches per-row RHS evaluators for ON CREATE / ON MATCH
// property-set items whose right-hand side is a non-literal expression
// (keyed by [MergeActionEvalKey] on the relationship variable and property
// key). Without these, `ON MATCH SET r.n = r.n + 1` fails to parse as a
// literal and, on this fast path, surfaced a parse error instead of
// incrementing the edge property (#1965). Returns op for chaining.
func (op *MergeRelationship) WithActionEvals(onCreate, onMatch map[string]ValueEvalFn) *MergeRelationship {
	op.onCreateEvals = onCreate
	op.onMatchEvals = onMatch
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

// mergeRelMatch is one stored relationship instance that satisfies the MERGE
// pattern: its endpoints in storage order and its stable handle (0 for a slot
// stamped without one).
type mergeRelMatch struct {
	srcKey, dstKey string
	srcID, dstID   graph.NodeID
	handle         uint64
}

// Next emits the next input row, ensuring that the (src)-[:relType]->(dst)
// relationship exists in the graph (either pre-existing or newly created).
// When N > 1 stored relationships match, the operator emits N rows for the
// same upstream tuple, one per matched instance (Merge5 [3], rmp #2939).
func (op *MergeRelationship) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.pendingIdx < len(op.pending) {
		*out = op.pending[op.pendingIdx]
		op.pending[op.pendingIdx] = nil
		op.pendingIdx++
		return true, nil
	}
	row, ok, err := nextRow(op.child, &op.pull)
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
	// Parse inline property literals lazily on the first call. When WithParams
	// resolved a parameterised map, relPropPredsParsed is already set and this
	// is skipped. A non-literal value (e.g. `{kind: r.pk}`) is not captured here
	// — it is deferred to relPropsEvalFn and merged per row below.
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
	// Resolve the effective inline property set for THIS row: the parsed
	// literals merged with any per-row dynamic entries produced by
	// relPropsEvalFn (non-literal values such as `{kind: r.pk}`). The merged
	// set drives BOTH the existing-edge search predicate and the created edge's
	// properties, exactly as the node [Merge] path does — so a row-driven
	// inline property is matched-on and written rather than silently dropped.
	effectiveProps, mpErr := mergeProps(op.relPropPreds, op.relPropsEvalFn, row)
	if mpErr != nil {
		return false, mpErr
	}
	// Match every stored relationship between the pair whose OWN type and
	// properties satisfy the pattern — one row per instance, as a MATCH of the
	// same pattern would bind (rmp #2939). The instances are enumerated by
	// handle from the adjacency and tested through their by-handle metadata:
	// the pair's label union and coalesced property map fold every parallel
	// sibling together, and the CREATE ordinal count is not rebuilt by
	// recovery, so neither can say which relationship matched. The type test
	// is also what keeps `MERGE (a)-[:T2]->(b)` from binding a T1 edge on a
	// multigraph (rmp #1683). An undirected pattern also matches the reverse
	// order (Merge5 [13]); the create path still uses (src, dst). This is safe
	// because the operator tree is single-goroutine, not because writers are
	// serialised (they are not, since rmp #2306).
	if op.matches == nil {
		op.matches, op.handleBuf, op.pending = op.matchArr[:0], op.handleArr[:0], op.pendArr[:0]
	}
	op.matches = op.matches[:0]
	op.appendMatches(srcKey, dstKey, srcID, dstID, effectiveProps)
	if op.undirected && srcKey != dstKey {
		op.appendMatches(dstKey, srcKey, dstID, srcID, effectiveProps)
	}
	if len(op.matches) > 0 {
		op.pending = op.pending[:0]
		op.pendingIdx = 0
		labelled := [2]bool{}
		for i := range op.matches {
			m := op.matches[i]
			// Relationship types are also kept as a per-pair union; re-assert
			// the matched type there once per stored order (idempotent).
			ord := 0
			if m.srcKey != srcKey {
				ord = 1
			}
			if !labelled[ord] {
				labelled[ord] = true
				op.mutator.SetEdgeLabel(m.srcKey, m.dstKey, op.relType)
			}
			if err := op.applyRelActions(row, m.srcKey, m.dstKey, m.handle, op.onMatchActions, op.onMatchEvals); err != nil {
				return false, err
			}
			op.pending = append(op.pending, op.emitRow(row, m.srcID, m.dstID, m.srcKey, m.dstKey, m.handle))
		}
		*out = op.pending[0]
		op.pending[0] = nil
		op.pendingIdx = 1
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
	for _, p := range effectiveProps {
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
	if err := op.applyRelActions(row, srcKey, dstKey, handle, op.onCreateActions, op.onCreateEvals); err != nil {
		return false, err
	}
	*out = op.emitRow(row, srcID, dstID, srcKey, dstKey, handle)
	return true, nil
}

// appendMatches appends to op.matches every relationship stored as
// (srcKey, dstKey) whose own type and properties satisfy the pattern. An
// instance already matched through the other order — an undirected graph's
// mirror slot shares its relationship's handle — is not appended twice.
func (op *MergeRelationship) appendMatches(srcKey, dstKey string, srcID, dstID graph.NodeID, preds []propLiteral) {
	op.handleBuf = op.mutator.EdgeHandles(srcKey, dstKey, op.handleBuf[:0])
	for _, h := range op.handleBuf {
		if h != 0 && op.matchedHandle(h) {
			continue
		}
		if !relInstanceHasType(op.mutator, srcKey, dstKey, h, op.relType) {
			continue
		}
		if !op.matchesRelProps(srcKey, dstKey, h, preds) {
			continue
		}
		op.matches = append(op.matches, mergeRelMatch{srcKey: srcKey, dstKey: dstKey, srcID: srcID, dstID: dstID, handle: h})
	}
}

// matchedHandle reports whether op.matches already holds handle.
func (op *MergeRelationship) matchedHandle(handle uint64) bool {
	for i := range op.matches {
		if op.matches[i].handle == handle {
			return true
		}
	}
	return false
}

// matchesRelProps reports whether the relationship instance handle stored as
// (src, dst) satisfies the inline property predicate preds — the per-row
// effective property set (parsed literals merged with any relPropsEvalFn
// dynamic entries). Returns true when no predicate was declared; otherwise
// every predicate key must be present and Equal to the matching property value
// on the instance's OWN property map.
func (op *MergeRelationship) matchesRelProps(srcKey, dstKey string, handle uint64, preds []propLiteral) bool {
	if len(preds) == 0 {
		return true
	}
	live := relInstanceProps(op.mutator, srcKey, dstKey, handle)
	for _, p := range preds {
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

// emitRow returns the output row for a successfully matched-or-created
// edge. When the operator has a non-anonymous relationship variable
// (relCol >= 0) the row is extended with a RelationshipValue carrying
// the declared type and the live property map; otherwise the input row
// is passed through unchanged.
//
// handle is the STABLE PER-EDGE HANDLE of the edge this row binds — the
// just-allocated handle on the create branch, the matched instance's own handle
// on the match branch. It is the SAME
// identity applyRelActions writes its ON CREATE / ON MATCH mirrors under, so
// the row names exactly the instance this operator's own writes landed on.
//
// It is published as the value's ID because every consumer of a
// post-projection relationship binding reads that field AS the handle
// (set.go, set_all.go, remove.go, merge_outer_target.go) and `id(r)` returns
// it verbatim. Emitting a synthetic `src<<32|dst` packing instead sent a
// standalone `SET r.k` into an orphan by-handle bag that no read ever
// consults — a silent lost write (rmp #2705) — and made `id(r)` disagree
// with the id the same relationship reports through MATCH. A zero handle
// (an edge stamped by the Go API without one) keeps the pre-existing
// per-pair-only behaviour on both the write and the read side.
func (op *MergeRelationship) emitRow(row Row, srcID, dstID graph.NodeID, srcKey, dstKey string, handle uint64) Row {
	if op.relCol < 0 {
		return row
	}
	// The instance's OWN properties, not the pair's coalesced map (rmp #2939).
	relProps := exprMapFromLPGProps(relInstanceProps(op.mutator, srcKey, dstKey, handle))
	rel := expr.RelationshipValue{
		ID:         handle,
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
// the ON CREATE path, or the matched instance's own handle on the ON MATCH path,
// which runs once per matched instance (rmp #2939). handle == 0 means the edge carries no stable
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
func (op *MergeRelationship) applyRelActions(row Row, srcKey, dstKey string, handle uint64, actions []MergeRelAction, evals map[string]ValueEvalFn) error {
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
		// Single-property write. Literal fast path first; on a non-literal
		// RHS use the per-row evaluator so `ON MATCH SET r.n = r.n + 1` reads
		// the edge's current value instead of erroring on a literal parse
		// failure (#1965).
		v, err := parsePropValue(act.value)
		if err != nil {
			if errors.Is(err, ErrPropertyValueIsNull) {
				// Literal null RHS: preserve this fast path's prior no-op
				// (the compound MergePattern and the SET-clause translator
				// remove the property, but this operator has always skipped;
				// left unchanged to keep the fix in scope — #1965).
				continue
			}
			fn, has := evals[MergeActionEvalKey(op.relVar, act.key)]
			if !has {
				return fmt.Errorf("exec: MergeRelationship: parse value %q: %w", act.value, err)
			}
			val, isNull, hasValue, evalErr := fn(op.actionEvalRow(row, srcKey, dstKey, handle))
			if evalErr != nil {
				return evalErr
			}
			if isNull {
				// RHS evaluated to null → openCypher removes the property.
				op.delEdgeProp(srcKey, dstKey, handle, act.key)
				continue
			}
			if !hasValue {
				continue // eval error / unstorable type → no-op (matches regular SET)
			}
			v = val
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

// delEdgeProp removes key from the edge's per-pair store and, when handle is
// non-zero, mirrors the removal to the by-handle store (#1684). Used when an
// ON CREATE / ON MATCH SET RHS evaluates to null (openCypher removes the
// property). Previously a null literal on this fast path was a silent skip;
// removal matches the regular SET operator and the node MERGE path.
//
// When handle is resolved and the mutator implements [relInstancePropRemover],
// the mutator performs both removals itself so -properties is gated on the
// merge-bound instance's OWN bag rather than the per-pair aggregate (#2501):
// on parallel edges the aggregate can carry a key a SIBLING wrote, which the
// bound instance never had — removing it must count 0. The handle==0 fallback
// keeps the pairwise path byte-identical.
func (op *MergeRelationship) delEdgeProp(srcKey, dstKey string, handle uint64, key string) {
	if m, ok := op.mutator.(relInstancePropRemover); ok && handle != 0 {
		m.DelEdgePropertyOnInstance(srcKey, dstKey, handle, key)
		return
	}
	op.mutator.DelEdgeProperty(srcKey, dstKey, key)
	if handle != 0 {
		op.mutator.DelEdgePropertyByHandle(srcKey, dstKey, handle, key)
	}
}

// actionEvalRow returns a row for a per-row RHS evaluator: a copy of the
// driving row with the relationship variable bound at its schema column as a
// RelationshipValue carrying the bound instance's CURRENT properties, so `r.<key>`
// resolves to the live edge value. The endpoint node columns are preserved so
// cross-variable references (`SET r.x = a.y`) still resolve. When the operator
// has no relationship column (anonymous relationship, relCol < 0) the row is
// returned unchanged — an anonymous relationship cannot be named by a SET item.
func (op *MergeRelationship) actionEvalRow(row Row, srcKey, dstKey string, handle uint64) Row {
	if op.relCol < 0 {
		return row
	}
	width := len(row)
	if op.relCol+1 > width {
		width = op.relCol + 1
	}
	out := make(Row, width)
	copy(out, row)
	out[op.relCol] = op.currentRelValue(srcKey, dstKey, handle)
	return out
}

// currentRelValue builds a RelationshipValue for the instance handle stored as
// (srcKey, dstKey), carrying its current property map converted to expr
// values. Used to bind the relationship variable in a per-row RHS evaluation
// row so a self-referential ON MATCH SET (`r.n = r.n + 1`) reads the bound
// instance's live value, not a parallel sibling's.
func (op *MergeRelationship) currentRelValue(srcKey, dstKey string, handle uint64) expr.RelationshipValue {
	props := exprMapFromLPGProps(relInstanceProps(op.mutator, srcKey, dstKey, handle))
	return expr.RelationshipValue{ID: handle, Type: op.relType, Properties: props}
}

// clearRelPropsAbsent removes every property of the bound instance whose key
// is NOT in retain, in lock-step on the per-pair store and (when handle != 0)
// the by-handle store. It implements the clear half of true openCypher REPLACE
// for `SET r = {…}` / `SET r = node` (#1687).
//
// The keys are the union of the per-pair aggregate and the instance's own bag
// ([relClearKeys], as the SET clause uses), and each removal goes through
// [MergeRelationship.delEdgeProp], which records its inverse on the transaction
// undo log — so a rolled-back statement restores the cleared values exactly —
// and counts -properties on the instance's OWN bag rather than on the pair
// aggregate a parallel sibling also contributes to (#2501).
func (op *MergeRelationship) clearRelPropsAbsent(srcKey, dstKey string, handle uint64, retain []string) {
	existing := relClearKeys(op.mutator, srcKey, dstKey, handle)
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
		op.delEdgeProp(srcKey, dstKey, handle, k)
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
