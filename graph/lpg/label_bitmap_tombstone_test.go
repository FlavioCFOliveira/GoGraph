package lpg

// label_bitmap_tombstone_test.go — regression gate for task #1409:
// RemoveNode must strip the removed id from every label NodeIndex bitmap,
// so label-index consumers see the node as absent without consulting
// IsTombstoned. The inverse contract: reviving a tombstoned node via
// AddNode must restore those bitmap entries.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// newTestGraph returns a directed LPG for use in tests.
func newTestGraph() *Graph[string, float64] {
	return New[string, float64](adjlist.Config{Directed: true})
}

// TestRemoveNode_StripsLabelBitmap is the primary gate for task #1409:
// after RemoveNode the id must not appear in any label bitmap.
func TestRemoveNode_StripsLabelBitmap(t *testing.T) {
	t.Parallel()
	g := newTestGraph()

	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("alice", "Employee"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	id, ok := g.AdjList().Mapper().Lookup("alice")
	if !ok {
		t.Fatal("mapper lookup failed")
	}
	lidPerson := uint32(g.Registry().Intern("Person"))
	lidEmployee := uint32(g.Registry().Intern("Employee"))

	// Pre-condition: bitmap contains the node.
	if !g.NodeIndex().Has(lidPerson, id) {
		t.Fatal("Person bitmap missing alice before RemoveNode")
	}
	if !g.NodeIndex().Has(lidEmployee, id) {
		t.Fatal("Employee bitmap missing alice before RemoveNode")
	}

	g.RemoveNode("alice")

	// Post-condition: bitmap must NOT contain the node (task #1409).
	if g.NodeIndex().Has(lidPerson, id) {
		t.Errorf("Person bitmap still contains alice after RemoveNode")
	}
	if g.NodeIndex().Has(lidEmployee, id) {
		t.Errorf("Employee bitmap still contains alice after RemoveNode")
	}
}

// TestRemoveNode_StripsLabelBitmap_NoPriorRemoveNodeLabel verifies the
// fix when RemoveNodeLabel is NOT called before RemoveNode (direct API).
func TestRemoveNode_StripsLabelBitmap_NoPriorRemoveNodeLabel(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("bob", "Manager"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	id, _ := g.AdjList().Mapper().Lookup("bob")
	lid := uint32(g.Registry().Intern("Manager"))

	// RemoveNode directly — no RemoveNodeLabel call first.
	g.RemoveNode("bob")

	if g.NodeIndex().Has(lid, id) {
		t.Errorf("Manager bitmap still contains bob after direct RemoveNode (stale entry, task #1409)")
	}
}

// TestRevive_RestoresLabelBitmap verifies the inverse: after a revive
// the node must re-appear in the label bitmaps for labels still in
// its label bag. This handles the Go-API path where RemoveNode is
// called without prior RemoveNodeLabel (so labels remain in the bag).
func TestRevive_RestoresLabelBitmap(t *testing.T) {
	t.Parallel()
	g := newTestGraph()

	if err := g.AddNode("carol"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("carol", "Admin"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	id, _ := g.AdjList().Mapper().Lookup("carol")
	lid := uint32(g.Registry().Intern("Admin"))

	// RemoveNode (no prior label strip) — strips from bitmap.
	g.RemoveNode("carol")
	if g.NodeIndex().Has(lid, id) {
		t.Fatal("Admin bitmap still contains carol after RemoveNode (pre-condition failed)")
	}

	// Revive via AddNode — must restore bitmap.
	if err := g.AddNode("carol"); err != nil {
		t.Fatalf("AddNode (revive): %v", err)
	}
	if !g.NodeIndex().Has(lid, id) {
		t.Errorf("Admin bitmap missing carol after revive (task #1409)")
	}
}

// TestRevive_ExecutorPath verifies that the executor's explicit-label-strip
// delete path still works correctly: after RemoveNodeLabel + RemoveNode +
// revive, the bitmap is empty (labels were stripped from the bag), so
// a subsequent SetNodeLabel adds them correctly.
func TestRevive_ExecutorPath(t *testing.T) {
	t.Parallel()
	g := newTestGraph()

	if err := g.AddNode("dave"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("dave", "Dev"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	id, _ := g.AdjList().Mapper().Lookup("dave")
	lid := uint32(g.Registry().Intern("Dev"))

	// Executor path: strip label first, then remove.
	g.RemoveNodeLabel("dave", "Dev")
	g.RemoveNode("dave")

	// After executor delete: bitmap is empty.
	if g.NodeIndex().Has(lid, id) {
		t.Fatal("Dev bitmap contains dave after executor-path delete")
	}

	// Revive.
	if err := g.AddNode("dave"); err != nil {
		t.Fatalf("AddNode (revive): %v", err)
	}
	// Label bag is empty after executor removed Dev → bitmap stays empty.
	if g.NodeIndex().Has(lid, id) {
		t.Error("Dev bitmap contains dave after revive with empty label bag")
	}

	// Re-add label explicitly (what the executor does on re-CREATE).
	if err := g.SetNodeLabel("dave", "Dev"); err != nil {
		t.Fatalf("SetNodeLabel after revive: %v", err)
	}
	if !g.NodeIndex().Has(lid, id) {
		t.Error("Dev bitmap missing dave after SetNodeLabel post-revive")
	}
}

// TestRemoveNode_MultipleNodes verifies that stripping one node's bitmap
// entries does not affect other nodes sharing the same labels.
func TestRemoveNode_MultipleNodes(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	names := []string{"n1", "n2", "n3"}
	for _, name := range names {
		if err := g.AddNode(name); err != nil {
			t.Fatalf("AddNode %s: %v", name, err)
		}
		if err := g.SetNodeLabel(name, "Shared"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", name, err)
		}
	}
	lid := uint32(g.Registry().Intern("Shared"))

	// Remove just n2.
	g.RemoveNode("n2")
	n2id, _ := g.AdjList().Mapper().Lookup("n2")
	n1id, _ := g.AdjList().Mapper().Lookup("n1")
	n3id, _ := g.AdjList().Mapper().Lookup("n3")

	if g.NodeIndex().Has(lid, n2id) {
		t.Error("Shared bitmap still contains n2 after RemoveNode")
	}
	// Siblings must be unaffected.
	if !g.NodeIndex().Has(lid, n1id) {
		t.Error("Shared bitmap lost n1 after unrelated RemoveNode")
	}
	if !g.NodeIndex().Has(lid, n3id) {
		t.Error("Shared bitmap lost n3 after unrelated RemoveNode")
	}
}

// TestRemoveNode_NoLabels verifies that RemoveNode on a label-less node
// does not panic or corrupt the graph.
func TestRemoveNode_NoLabels(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	if err := g.AddNode("bare"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.RemoveNode("bare") // must not panic
	id, _ := g.AdjList().Mapper().Lookup("bare")
	if !g.IsTombstoned(id) {
		t.Error("bare node is not tombstoned after RemoveNode")
	}
}

// TestRemoveNode_IdempotentBitmap verifies that calling RemoveNode twice
// does not panic or corrupt other nodes.
func TestRemoveNode_IdempotentBitmap(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	if err := g.AddNode("dup"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("dup", "X"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	g.RemoveNode("dup")
	g.RemoveNode("dup") // second call must not panic

	id, _ := g.AdjList().Mapper().Lookup("dup")
	lid := uint32(g.Registry().Intern("X"))
	if g.NodeIndex().Has(lid, id) {
		t.Error("X bitmap contains dup after double RemoveNode")
	}
}
