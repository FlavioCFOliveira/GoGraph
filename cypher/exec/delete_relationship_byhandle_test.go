package exec_test

// delete_relationship_byhandle_test.go — direct operator coverage for the
// instance-precise by-handle removal path in DeleteRelationship (rmp #2018).
//
// The Cypher planner currently lowers every DELETE target to DeleteNode, so
// DeleteRelationship is not on the live query path; this test exercises the
// operator directly (with a stub GraphMutator) so the WithRelCols → by-handle
// removal branch is verified and stays correct if the planner ever emits
// DeleteRelationship. The load-bearing engine-level coverage of the same bug
// fix lives in the cypher package (DeleteNode path).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestDeleteRelationship_ByHandle_RemovesBoundInstance binds edge position 7 to
// the SECOND parallel instance (handle h2) via the stub's handleAt resolver and
// asserts that DELETE removes exactly h2 — leaving the sibling h1 intact — and
// that the deleted-row marker carries h2's OWN by-handle property snapshot.
func TestDeleteRelationship_ByHandle_RemovesBoundInstance(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	mustAddEdge(t, mut, "a", "b", 0)

	const h1, h2 = uint64(1), uint64(2)
	// Two parallel instances distinguished by a per-handle property.
	if err := mut.SetEdgePropertyByHandle("a", "b", h1, "w", lpg.Int64Value(1)); err != nil {
		t.Fatalf("seed h1: %v", err)
	}
	if err := mut.SetEdgePropertyByHandle("a", "b", h2, "w", lpg.Int64Value(2)); err != nil {
		t.Fatalf("seed h2: %v", err)
	}

	// The row BINDS h2 directly: since rmp #2317 the identity column carries the
	// stable handle, so there is no position to resolve and no resolver to wire.
	// The instance a DELETE retires is the one the row names, not the one a
	// per-query CSR snapshot happens to map a position onto.
	//
	// r at column 0 (a post-projection RelationshipValue); endpoints and the edge
	// identity at columns 1..3, matching a WithRelCols wiring.
	schema := map[string]int{"r": 0}
	rel := expr.RelationshipValue{StartID: uint64(aID), EndID: uint64(bID), Type: "T2"}
	row := exec.Row{rel, expr.IntegerValue(aID), expr.IntegerValue(bID), expr.IntegerValue(int64(h2))}
	src := newSliceOperator(row)
	op := exec.NewDeleteRelationship("r", schema, src, mut).
		WithRelCols(exec.RelCols{SrcCol: 1, DstCol: 2, EdgeCol: 3})

	out, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The bound instance (h2) is gone; the sibling (h1) survives with w=1.
	if got := mut.EdgePropertiesByHandle("a", "b", h2); got != nil {
		t.Fatalf("bound instance h2 not removed: %v", got)
	}
	h1props := mut.EdgePropertiesByHandle("a", "b", h1)
	if w, _ := h1props["w"].Int64(); w != 1 {
		t.Fatalf("sibling h1 corrupted: w=%d want 1 (props=%v)", w, h1props)
	}

	// The deleted-row marker carries the bound instance's (h2's) property view.
	if len(out) != 1 {
		t.Fatalf("Drain yielded %d rows, want 1", len(out))
	}
	drel, ok := out[0][0].(expr.RelationshipValue)
	if !ok || !drel.Deleted {
		t.Fatalf("output row col 0 = %v (%T), want a Deleted RelationshipValue", out[0][0], out[0][0])
	}
	wv, ok := drel.Properties["w"]
	if !ok {
		t.Fatalf("deleted-row snapshot missing by-handle property w: %v", drel.Properties)
	}
	if iv, ok := wv.(expr.IntegerValue); !ok || int64(iv) != 2 {
		t.Fatalf("deleted-row w = %v, want IntegerValue 2 (h2's own property)", wv)
	}
}

// TestDeleteRelationship_NoRelCols_FallsBackToEndpointRemoval asserts that
// without WithRelCols (no resolvable handle) DeleteRelationship keeps its
// original first-match endpoint behaviour: the edge presence is removed.
func TestDeleteRelationship_NoRelCols_FallsBackToEndpointRemoval(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	mustAddEdge(t, mut, "a", "b", 0)

	schema := map[string]int{"r": 0}
	rel := expr.RelationshipValue{StartID: uint64(aID), EndID: uint64(bID), Type: "REL"}
	op := exec.NewDeleteRelationship("r", schema, newSliceOperator(exec.Row{rel}), mut)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if mut.HasEdge("a", "b") {
		t.Error("edge a→b should have been removed via the endpoint fallback")
	}
}
