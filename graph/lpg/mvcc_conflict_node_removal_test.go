package lpg

// mvcc_conflict_node_removal_test.go — rmp #2726: the NODE removal must REPORT
// its refusal, not merely perform it.
//
// The conflict itself was never in doubt. [Graph.removeNodeInfo] has claimed the
// death in the existence store BEFORE any mutation since rmp #2444, and it
// cross-checks the node's properties, its labels and its adjacency straight
// after. What was missing is the ANSWER: the primitive was void, so a caller
// could not tell a refusal from a retirement, and both Cypher adapters
// journalled an undo inverse on a present-state IsTombstoned probe instead.
//
// An inverse for a retirement that never happened REVIVES the node — and once a
// peer has deleted that node for real and committed, the revival restores no
// labels and no properties, because the peer's commit took them. What comes back
// is a BARE node that no transaction ever created: alive in the present, with no
// life record, absent from every label bitmap. The DST reports exactly that
// shape on crash seed 790.
//
// [Graph.removeEdgeByHandleInfo] has answered this question since rmp #2018,
// [Graph.removeAllEdgesFromInfo] since rmp #2694 and [Graph.removeEdgeInfo]
// since rmp #2725. This file pins the node, which is the last of them.
//
// Every assertion here was validated against a build in which removeNodeInfo
// returns true unconditionally: the two refusal tests then fail on the report
// while their state assertions still pass, which is precisely the blind spot
// that let the defect through.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestConflict_NodeRemovalReportsItsRefusalWhenDoomed is the shape the DST
// found, reduced to one factor.
//
// B is doomed by a write-write conflict on an UNRELATED node, and then deletes a
// node that is still perfectly live. removeNodeInfo refuses at the death claim,
// before any mutation, and must SAY SO — the node is still live, so a caller
// gating on its own present-state probe would journal an inverse and resurrect
// it after a peer's real delete commits.
func TestConflict_NodeRemovalReportsItsRefusalWhenDoomed(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"victim", "contended"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
	}
	idVictim, ok := g.AdjList().Mapper().Lookup("victim")
	if !ok {
		t.Fatal("victim was never interned")
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeProperty("contended", "v", Int64Value(1)); err != nil {
		t.Fatalf("A set: %v", err)
	}
	txB := g.beginLabelTx()
	// B loses the race and is doomed from here on.
	if err := txB.setNodeProperty("contended", "v", Int64Value(2)); err == nil {
		t.Fatal("the second writer on the same node property was not refused; " +
			"the doom this test depends on never happened")
	}

	if txB.removeNode("victim") {
		t.Fatal("the node removal reported itself APPLIED on an already-doomed " +
			"transaction; a caller journalling an inverse on that answer revives " +
			"a node it never deleted, and the revival restores no labels and no " +
			"properties (rmp #2726)")
	}
	// Nothing mutated: the refusal is taken before the tombstone flip, which is
	// what keeps the peer's own delete sound.
	if g.IsTombstoned(idVictim) {
		t.Fatal("the refused node removal tombstoned the node anyway")
	}
	if _, err := txB.commit(); err == nil {
		t.Fatal("the doomed transaction committed")
	}
	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
}

// TestConflict_NodeRemovalReportsACrossStoreRefusal covers the OTHER three
// refusal doors into the same primitive: the node's property head, its label
// head, and its adjacency claim, each checked AFTER the death has been claimed.
//
// The transaction is healthy when it arrives, so the doom test in the sibling
// above cannot answer for this path — and the node is left LIVE here too,
// because the cross-checks all run before the tombstone flip.
func TestConflict_NodeRemovalReportsACrossStoreRefusal(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("victim"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	idVictim, ok := g.AdjList().Mapper().Lookup("victim")
	if !ok {
		t.Fatal("victim was never interned")
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeProperty("victim", "v", Int64Value(1)); err != nil {
		t.Fatalf("A set: %v", err)
	}
	txB := g.beginLabelTx()
	if txB.removeNode("victim") {
		t.Fatal("the node removal reported itself APPLIED while a peer held an " +
			"in-flight property write on the same node; the cross-check refused " +
			"it and the caller was told otherwise (rmp #2726)")
	}
	if g.IsTombstoned(idVictim) {
		t.Fatal("the cross-check refusal tombstoned the node anyway")
	}
	if _, err := txB.commit(); err == nil {
		t.Fatal("the refused transaction committed")
	}
	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
}

// TestConflict_NodeRemovalAppliesWithoutAConcurrentWriter is the non-vacuity
// control. Without it a removeNodeInfo returning false unconditionally would
// pass both refusal tests and silently stop every LEGITIMATE inverse from being
// journalled — a rolled-back DELETE would then become permanent, which is
// rmp #2445's defect reopened from the other side.
func TestConflict_NodeRemovalAppliesWithoutAConcurrentWriter(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	idA, ok := g.AdjList().Mapper().Lookup("a")
	if !ok {
		t.Fatal("a was never interned")
	}

	tx := g.beginLabelTx()
	if !tx.removeNode("a") {
		t.Fatal("the node removal reported a refusal with NO concurrent writer; " +
			"the refusals in the sibling tests would then be vacuous")
	}
	if _, err := tx.commit(); err != nil {
		t.Fatalf("uncontended removal failed to commit: %v", err)
	}
	if !g.IsTombstoned(idA) {
		t.Fatal("the applied removal left the node live")
	}

	// TRUE means ADMITTED, not "a node was retired": an already-tombstoned node
	// and a never-interned key both apply and retire nothing. The adapters
	// combine this with their own IsTombstoned probe, so the two answers must
	// stay distinct.
	tx2 := g.beginLabelTx()
	if !tx2.removeNode("a") {
		t.Fatal("removing an ALREADY-TOMBSTONED node reported a refusal; the " +
			"report answers whether the write was admitted, not whether a node " +
			"was live")
	}
	if _, err := tx2.commit(); err != nil {
		t.Fatalf("no-op removal failed to commit: %v", err)
	}
	tx3 := g.beginLabelTx()
	if !tx3.removeNode("never-interned") {
		t.Fatal("removing a NEVER-INTERNED key reported a refusal")
	}
	if _, err := tx3.commit(); err != nil {
		t.Fatalf("unknown-key removal failed to commit: %v", err)
	}
}
