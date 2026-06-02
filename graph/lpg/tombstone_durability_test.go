package lpg

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// Why the pre-existing suite missed the node-deletion durability defect:
// the historical RemoveNode tests only ever exercised a SINGLE, long-lived
// in-memory graph, where a tombstone — once set — is never expected to be
// cleared. They never asserted the resurrection contract (re-creating a
// node with the same natural key must make it live again under the SAME
// stable NodeID), and they never round-tripped the tombstone set through a
// persist→reopen boundary. The defect lives precisely in those two gaps:
// a node that is deleted and then re-created stays an invisible, undeletable
// ghost, and the tombstone set is silently lost on snapshot/replay.
//
// These tests lock the in-memory half of the contract (Gap 3, resurrection,
// plus the TombstonedIDs accessor that the snapshot writer needs). The
// persist→reopen half is locked in store/snapshot and store/recovery.

// TestRemoveNode_AddNode_Resurrects asserts the resurrection invariant:
// AddNode on a tombstoned key revives the node (same NodeID), it is no
// longer tombstoned, and the live count returns to one. RED on the
// pre-fix code: AddNode never clears the tombstone, so the node stays an
// invisible ghost.
func TestRemoveNode_AddNode_Resurrects(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("auth", "Spec"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	id, ok := g.AdjList().Mapper().Lookup("auth")
	if !ok {
		t.Fatal("node auth was not interned")
	}

	g.RemoveNode("auth")
	if !g.IsTombstoned(id) {
		t.Fatal("node auth should be tombstoned immediately after RemoveNode")
	}
	if got := g.LiveOrder(); got != 0 {
		t.Fatalf("LiveOrder = %d, want 0 while auth is tombstoned", got)
	}

	// Re-create the same key. The node must come back to life under the
	// SAME stable NodeID rather than remain an undeletable ghost.
	if err := g.AddNode("auth"); err != nil {
		t.Fatalf("AddNode (re-create): %v", err)
	}
	id2, _ := g.AdjList().Mapper().Lookup("auth")
	if id2 != id {
		t.Fatalf("NodeID changed across resurrection: got %d, want %d", id2, id)
	}
	if g.IsTombstoned(id) {
		t.Fatal("node auth should be live again after AddNode (resurrection)")
	}
	if got := g.LiveOrder(); got != 1 {
		t.Fatalf("LiveOrder = %d, want 1 after resurrection", got)
	}
}

// TestSetNodeLabel_DoesNotRevive locks the deliberate design choice that
// SetNodeLabel is NOT a resurrection path: only AddNode revives. Making
// SetNodeLabel revive would let the recovery step that re-applies snapshot
// labels AFTER WAL replay resurrect a node deleted in the WAL tail — a
// Durability violation. A tombstoned node is never matched by a read
// clause, so a label can only legitimately reach a removed key after
// AddNode has already revived it.
func TestSetNodeLabel_DoesNotRevive(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("auth"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	id, _ := g.AdjList().Mapper().Lookup("auth")
	g.RemoveNode("auth")
	if !g.IsTombstoned(id) {
		t.Fatal("auth should be tombstoned")
	}
	if err := g.SetNodeLabel("auth", "Spec"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if !g.IsTombstoned(id) {
		t.Fatal("SetNodeLabel must NOT revive a tombstoned node (only AddNode does)")
	}
}

// TestRemoveEdge_StripsPerPairStateOnFullDisconnect locks the edge analogue
// of tombstone hygiene: once RemoveEdge leaves the endpoint pair with no
// remaining edge, the per-pair edge label and property surfaces are cleared
// so a later re-add does not resurrect them.
func TestRemoveEdge_StripsPerPairStateOnFullDisconnect(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel("a", "b", "REL")
	if err := g.SetEdgeProperty("a", "b", "since", Int64Value(2020)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}
	if got := g.EdgeLabels("a", "b"); len(got) != 1 || got[0] != "REL" {
		t.Fatalf("precondition EdgeLabels = %v, want [REL]", got)
	}

	g.RemoveEdge("a", "b")

	if g.AdjList().HasEdge("a", "b") {
		t.Fatal("edge a->b should be gone after RemoveEdge")
	}
	if got := g.EdgeLabels("a", "b"); len(got) != 0 {
		t.Fatalf("edge labels = %v, want empty after full disconnect", got)
	}
	if _, ok := g.GetEdgeProperty("a", "b", "since"); ok {
		t.Fatal("edge property should be cleared after full disconnect")
	}

	// Re-create the pair: it must start clean, not resurrect REL/since.
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge (re-create): %v", err)
	}
	if got := g.EdgeLabels("a", "b"); len(got) != 0 {
		t.Fatalf("re-created edge labels = %v, want empty (no resurrection)", got)
	}
}

// TestRemoveEdge_KeepsPerPairStateWhileParallelEdgeRemains locks the
// multigraph safety guard: removing one of several parallel edges must NOT
// strip the shared per-pair label, since surviving parallel edges still use
// it. Only the final removal (full disconnect) clears it.
func TestRemoveEdge_KeepsPerPairStateWhileParallelEdgeRemains(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge 1: %v", err)
	}
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge 2: %v", err)
	}
	g.SetEdgeLabel("a", "b", "REL")

	g.RemoveEdge("a", "b") // one parallel edge remains
	if !g.AdjList().HasEdge("a", "b") {
		t.Fatal("a parallel edge a->b should still exist")
	}
	if got := g.EdgeLabels("a", "b"); len(got) != 1 || got[0] != "REL" {
		t.Fatalf("per-pair label = %v, want [REL] retained while a parallel edge remains", got)
	}

	g.RemoveEdge("a", "b") // now fully disconnected
	if g.AdjList().HasEdge("a", "b") {
		t.Fatal("edge a->b should be gone")
	}
	if got := g.EdgeLabels("a", "b"); len(got) != 0 {
		t.Fatalf("per-pair label = %v, want empty after the last parallel edge is removed", got)
	}
}

// TestRemoveEdge_DirectedDoesNotStripReverseEdge guards against over-strip:
// in a directed graph, removing a->b must not touch the distinct b->a edge.
func TestRemoveEdge_DirectedDoesNotStripReverseEdge(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge a->b: %v", err)
	}
	if err := g.AddEdge("b", "a", 0); err != nil {
		t.Fatalf("AddEdge b->a: %v", err)
	}
	g.SetEdgeLabel("a", "b", "FWD")
	g.SetEdgeLabel("b", "a", "REV")

	g.RemoveEdge("a", "b")

	if got := g.EdgeLabels("a", "b"); len(got) != 0 {
		t.Fatalf("a->b labels = %v, want empty", got)
	}
	if got := g.EdgeLabels("b", "a"); len(got) != 1 || got[0] != "REV" {
		t.Fatalf("b->a labels = %v, want [REV] (the reverse directed edge must be untouched)", got)
	}
}

// TestTombstonedIDs reports exactly the tombstoned set, sorted, and is empty
// for a graph that never deleted a node. RED on the pre-fix code: the
// accessor does not exist (compile failure locks the missing API).
func TestTombstonedIDs(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if got := g.TombstonedIDs(); len(got) != 0 {
		t.Fatalf("TombstonedIDs on a fresh graph = %v, want empty", got)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
	}
	idA, _ := g.AdjList().Mapper().Lookup("a")
	idC, _ := g.AdjList().Mapper().Lookup("c")
	g.RemoveNode("a")
	g.RemoveNode("c")

	got := g.TombstonedIDs()
	if len(got) != 2 {
		t.Fatalf("TombstonedIDs len = %d, want 2", len(got))
	}
	// The accessor must return the set in ascending NodeID order so the
	// snapshot component is deterministic across writes.
	if got[0] > got[1] {
		t.Fatalf("TombstonedIDs not sorted ascending: %v", got)
	}
	want := map[uint64]bool{uint64(idA): true, uint64(idC): true}
	for _, id := range got {
		if !want[uint64(id)] {
			t.Fatalf("TombstonedIDs returned unexpected id %d (set %v)", id, got)
		}
	}
}
