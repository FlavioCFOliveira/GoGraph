package lpg

// mvcc_conflict_edge_removal_test.go — rmp #2725: the PER-EDGE removal must
// REPORT its refusal, not merely perform it.
//
// The conflict itself was never in doubt: row 2 of the adjacency table
// (`append(A→B) ‖ removeEdge(A→C) → conflict`, mvcc_conflict_adjacency_test.go)
// has always held, and [Graph.removeEdgeInfo] has taken the rmp #2300 claim
// before it mutates anything since that row was written. What was missing is
// the ANSWER: the primitive was void, so a caller could not tell a refusal from
// a removal, and the Cypher adapters journalled an undo inverse on a
// present-state probe instead. An inverse for a removal that never happened
// RE-ADDS the arc — and once the winning peer has withdrawn its own arc, that
// inverse leaves an arc no transaction ever created.
//
// [Graph.removeAllEdgesFromInfo] answers this question since rmp #2694 and
// [Graph.removeEdgeByHandleInfo] since rmp #2018. This file pins the third and
// last of them.
//
// Both assertions were validated against a build in which removeEdgeInfo returns
// true unconditionally: the refusal test then fails on the report while the
// adjacency assertions still pass, which is exactly the blind spot that let the
// defect through.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestConflict_EdgeRemovalReportsItsRefusal is the contract.
//
// A is appending a→c and has not committed. B removes the COMMITTED arc a→b. B
// must be refused, must report the refusal, must leave the adjacency exactly as
// it found it, and must be unable to commit.
func TestConflict_EdgeRemovalReportsItsRefusal(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
	}
	seed := g.beginLabelTx()
	if err := seed.addEdge("a", "b", 1); err != nil {
		t.Fatalf("seed addEdge: %v", err)
	}
	if _, err := seed.commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.addEdge("a", "c", 2); err != nil {
		t.Fatalf("A addEdge: %v", err)
	}

	txB := g.beginLabelTx()
	if g.removeEdgeInfo("a", "b", txB.ctx) {
		t.Fatal("the per-edge removal reported itself APPLIED while a concurrent " +
			"transaction held an in-flight append on the same source; a caller " +
			"journalling an inverse on that answer re-adds an arc nobody removed " +
			"(rmp #2725)")
	}
	// Nothing mutated: the committed arc AND A's pending arc are both still
	// there. This is the half that keeps A's own rollback sound.
	if !g.AdjList().HasEdge("a", "b") {
		t.Fatal("the refused per-edge removal dropped the COMMITTED arc a→b")
	}
	if !g.AdjList().HasEdge("a", "c") {
		t.Fatal("the refused per-edge removal dropped the peer's in-flight arc a→c")
	}
	if got := g.AdjList().Size(); got != 2 {
		t.Fatalf("AdjList().Size() = %d after a refused per-edge removal, want 2", got)
	}
	wantConflictAt(t, txB, "adjacency")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
}

// TestConflict_EdgeRemovalAppliesWithoutAConcurrentWriter is the non-vacuity
// control: with no peer in flight the very same call APPLIES and reports true.
// Without this a removeEdgeInfo that returned false unconditionally would pass
// the refusal test and silently stop every legitimate inverse from being
// journalled — a rolled-back DELETE would then become permanent.
func TestConflict_EdgeRemovalAppliesWithoutAConcurrentWriter(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
	}
	seed := g.beginLabelTx()
	if err := seed.addEdge("a", "b", 1); err != nil {
		t.Fatalf("seed addEdge: %v", err)
	}
	if _, err := seed.commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	tx := g.beginLabelTx()
	if !g.removeEdgeInfo("a", "b", tx.ctx) {
		t.Fatal("the per-edge removal reported a refusal with NO concurrent " +
			"writer; the refusal in the sibling test would then be vacuous")
	}
	if _, err := tx.commit(); err != nil {
		t.Fatalf("uncontended per-edge removal failed to commit: %v", err)
	}
	if g.AdjList().HasEdge("a", "b") {
		t.Fatal("the applied per-edge removal left the arc behind")
	}

	// TRUE means ADMITTED, not "an arc was taken out": removing an absent edge
	// applies and removes nothing. The adapters combine this with their own
	// presence probe, so the two answers must stay distinct.
	tx2 := g.beginLabelTx()
	if !g.removeEdgeInfo("a", "b", tx2.ctx) {
		t.Fatal("removing an ABSENT edge reported a refusal; the report answers " +
			"whether the write was admitted, not whether an arc was present")
	}
	if _, err := tx2.commit(); err != nil {
		t.Fatalf("no-op removal failed to commit: %v", err)
	}
}

// TestConflict_HandleZeroRemovalReportsItsRefusal covers the third entry point
// into the same primitive: [Graph.RemoveEdgeByHandle] with a handle of 0 falls
// back to the first-match removal, and it USED to answer its caller with the
// presence probe it had taken before the call.
//
// That is the same wrong answer [Graph.removeEdgeInfo] used to give by being
// void, reached by a different door — and this door leads to a caller
// ([lpgMutatorAdapter.RemoveEdgeByHandle], [walMutatorAdapter.RemoveEdgeByHandle])
// that has gated its journal on this return since rmp #2018, so a `true` here
// goes straight into the undo log as an inverse for a removal that never
// happened (rmp #2725).
//
// A handle of 0 is not a corner case: the Cypher DELETE path falls back to it
// whenever the bound relationship carries no stable handle.
func TestConflict_HandleZeroRemovalReportsItsRefusal(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
	}
	seed := g.beginLabelTx()
	if err := seed.addEdge("a", "b", 1); err != nil {
		t.Fatalf("seed addEdge: %v", err)
	}
	if _, err := seed.commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.addEdge("a", "c", 2); err != nil {
		t.Fatalf("A addEdge: %v", err)
	}

	txB := g.beginLabelTx()
	if g.removeEdgeByHandleInfo("a", "b", 0, txB.ctx) {
		t.Fatal("the handle-0 removal reported itself APPLIED while a concurrent " +
			"transaction held an in-flight append on the same source; its caller " +
			"journals an inverse on that answer (rmp #2725)")
	}
	if !g.AdjList().HasEdge("a", "b") || !g.AdjList().HasEdge("a", "c") {
		t.Fatal("the refused handle-0 removal mutated the adjacency")
	}
	wantConflictAt(t, txB, "adjacency")

	// Non-vacuity: once the winner has published, the very same call APPLIES and
	// reports true. Without this arm a removeEdgeByHandleInfo that returned false
	// unconditionally would pass the refusal assertion and silently drop every
	// legitimate inverse.
	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
	txC := g.beginLabelTx()
	if !g.removeEdgeByHandleInfo("a", "b", 0, txC.ctx) {
		t.Fatal("the handle-0 removal reported a refusal with no concurrent " +
			"writer on this graph")
	}
	if _, err := txC.commit(); err != nil {
		t.Fatalf("uncontended handle-0 removal failed to commit: %v", err)
	}
}
