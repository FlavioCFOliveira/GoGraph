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
	"fmt"

	"gograph/cypher/expr"
	"gograph/graph"
)

// MergeRelationship matches-or-creates a single-hop directed relationship
// between two already-bound endpoint columns. ON CREATE / ON MATCH
// actions targeting the relationship variable are applied to the
// matched-or-created edge.
//
// MergeRelationship is NOT safe for concurrent use.
type MergeRelationship struct {
	child           Operator
	srcCol          int    // input-row column index holding src NodeID / NodeValue
	dstCol          int    // input-row column index holding dst NodeID / NodeValue
	relType         string // empty when the pattern declared no type (rejected upstream)
	relVar          string // empty when the relationship is anonymous
	onCreateActions []MergeRelAction
	onMatchActions  []MergeRelAction
	mutator         GraphMutator

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
		relType: relType,
		mutator: mutator,
	}
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
// edge exists in the graph (either pre-existing or newly created).
func (op *MergeRelationship) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
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
	// Match if an edge already exists with the requested type. HasEdge
	// is per-pair; combined with the per-pair label model in the LPG
	// the check accepts any (src, dst) edge whose label set contains
	// relType. The single-writer guarantee makes this safe.
	if op.mutator.HasEdge(srcKey, dstKey) {
		// Edge labels are per-(src,dst) in the LPG; adding the same
		// label twice is idempotent. Ensure the requested type is
		// recorded, then run ON MATCH actions.
		op.mutator.SetEdgeLabel(srcKey, dstKey, op.relType)
		if err := op.applyRelActions(srcKey, dstKey, op.onMatchActions); err != nil {
			return false, err
		}
		*out = row
		return true, nil
	}
	// No matching edge — create one, tag it, run ON CREATE actions.
	if _, _, addErr := op.mutator.AddEdge(srcKey, dstKey, 0); addErr != nil {
		return false, fmt.Errorf("exec: MergeRelationship: AddEdge: %w", addErr)
	}
	if op.relType != "" {
		op.mutator.SetEdgeLabel(srcKey, dstKey, op.relType)
	}
	if err := op.applyRelActions(srcKey, dstKey, op.onCreateActions); err != nil {
		return false, err
	}
	*out = row
	return true, nil
}

// applyRelActions sets every action's property on the (src, dst) edge
// via the graph mutator. value parsing reuses parsePropValue (the same
// helper the literal-property paths use) so the formats accepted are
// consistent across MERGE / CREATE / SET.
func (op *MergeRelationship) applyRelActions(srcKey, dstKey string, actions []MergeRelAction) error {
	for _, act := range actions {
		v, err := parsePropValue(act.value)
		if err != nil {
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
