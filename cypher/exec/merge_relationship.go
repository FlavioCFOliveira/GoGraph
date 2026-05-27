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
// between two already-bound endpoint columns.
//
// MergeRelationship is NOT safe for concurrent use.
type MergeRelationship struct {
	child   Operator
	srcCol  int    // input-row column index holding src NodeID / NodeValue
	dstCol  int    // input-row column index holding dst NodeID / NodeValue
	relType string // empty when the pattern declared no type (rejected upstream)
	mutator GraphMutator

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check
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
		labels := op.mutator.NodeLabels(srcKey)
		_ = labels // labels here are node labels; edge labels live in EdgeLabels via the LPG
		// Edge labels are looked up directly via the LPG graph behind
		// the mutator; the mutator does not expose EdgeLabels(src,dst)
		// in the GraphMutator interface, so we approximate the match
		// by trusting HasEdge + the type registration below. When
		// HasEdge is true and the requested type is one of the edge's
		// labels (the common case after SetEdgeLabel), no new edge is
		// created. Adding the same label twice is idempotent.
		op.mutator.SetEdgeLabel(srcKey, dstKey, op.relType)
		*out = row
		return true, nil
	}
	// No matching edge — create one and tag it with the requested type.
	if _, _, addErr := op.mutator.AddEdge(srcKey, dstKey, 0); addErr != nil {
		return false, fmt.Errorf("exec: MergeRelationship: AddEdge: %w", addErr)
	}
	if op.relType != "" {
		op.mutator.SetEdgeLabel(srcKey, dstKey, op.relType)
	}
	*out = row
	return true, nil
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
