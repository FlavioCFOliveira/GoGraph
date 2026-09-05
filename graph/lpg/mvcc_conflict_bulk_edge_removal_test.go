package lpg

// mvcc_conflict_bulk_edge_removal_test.go — the fifth row of the adjacency
// write-write conflict table (rmp #2694):
//
//	append(A→B) ‖ removeAllEdgesFrom(A) → conflict
//
// The other four rows and the reasoning behind them are in
// mvcc_conflict_adjacency_test.go. This row had no test and no check: the bulk
// removal was the one non-commutative adjacency write that never took the
// claim rmp #2300 introduced, so a `DETACH DELETE` stepped straight over a
// concurrent transaction's in-flight append.
//
// # Why the omission was not merely a missed refusal
//
// [AdjList.removeAllEdgesFromTx] publishes a NIL entry: it does not remove the
// arcs the removing transaction can see, it wipes the slot. So the unchecked
// bulk path destroyed the peer's uncommitted arc, and the peer's own rollback
// then had nothing left to withdraw. The end-to-end consequence — a rolled-back
// edge surviving BOTH rollbacks — is pinned at the Cypher layer by
// TestMVCCDetachDeleteRollback_DoesNotResurrectPeerArc.
//
// Both assertions below were validated against a build with the claim removed
// from [Graph.removeAllEdgesFromInfo]: the removal was admitted, `applied` came
// back true, and the peer's arc was gone from the adjacency.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestConflict_AdjacencyBulkRemovalRefusedByConcurrentAppend is the row itself.
//
// A is appending a→c and has not committed. B calls the bulk removal on a. B
// must be refused, must leave the adjacency EXACTLY as it found it — A's
// pending arc included — and must be unable to commit.
func TestConflict_AdjacencyBulkRemovalRefusedByConcurrentAppend(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
	}
	// One COMMITTED arc a→b, so the bulk removal has real work to refuse and the
	// test cannot pass merely because there was nothing to remove.
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
	if g.removeAllEdgesFromInfo("a", txB.ctx) {
		t.Fatal("the bulk removal was APPLIED while a concurrent transaction " +
			"held an in-flight append on the same source; it must be refused " +
			"(rmp #2694)")
	}
	// Nothing mutated: the committed arc AND A's pending arc are both still
	// there. This is the half that keeps A's own rollback sound — a wiped slot
	// leaves A nothing to withdraw.
	if !g.AdjList().HasEdge("a", "b") {
		t.Fatal("the refused bulk removal dropped the COMMITTED arc a→b")
	}
	if !g.AdjList().HasEdge("a", "c") {
		t.Fatal("the refused bulk removal dropped the peer's in-flight arc a→c")
	}
	if got := g.AdjList().Size(); got != 2 {
		t.Fatalf("AdjList().Size() = %d after a refused bulk removal, want 2", got)
	}
	wantConflictAt(t, txB, "adjacency")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
}

// TestConflict_AdjacencyBulkRemovalAppliesWithoutAConcurrentWriter is the
// non-vacuity control for the row above: with no peer in flight, the very same
// call APPLIES and clears the source. Without this, a
// [Graph.removeAllEdgesFromInfo] that returned false unconditionally would pass
// the refusal test.
func TestConflict_AdjacencyBulkRemovalAppliesWithoutAConcurrentWriter(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
	}
	seed := g.beginLabelTx()
	if err := seed.addEdge("a", "b", 1); err != nil {
		t.Fatalf("seed addEdge a→b: %v", err)
	}
	if _, err := seed.commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	// A second committed arc, published by its own transaction: two appends from
	// one source may not overlap (row 1 of the table).
	seed2 := g.beginLabelTx()
	if err := seed2.addEdge("a", "c", 2); err != nil {
		t.Fatalf("seed addEdge a→c: %v", err)
	}
	if _, err := seed2.commit(); err != nil {
		t.Fatalf("seed2 commit: %v", err)
	}

	tx := g.beginLabelTx()
	if !g.removeAllEdgesFromInfo("a", tx.ctx) {
		t.Fatal("the bulk removal was refused with NO concurrent writer; the " +
			"refusal in the sibling test would then be vacuous")
	}
	if _, err := tx.commit(); err != nil {
		t.Fatalf("uncontended bulk removal failed to commit: %v", err)
	}
	if g.AdjList().HasEdge("a", "b") || g.AdjList().HasEdge("a", "c") {
		t.Fatal("the applied bulk removal left an arc behind")
	}
	if got := g.AdjList().Size(); got != 0 {
		t.Fatalf("AdjList().Size() = %d after an applied bulk removal, want 0", got)
	}
}
