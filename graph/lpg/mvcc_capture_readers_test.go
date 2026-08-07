package lpg

// mvcc_capture_readers_test.go — rmp #2310: the three as-of readers a checkpoint
// capture needs and that sprint 333's read half did not cover.
//
// Layer: short.
//
// Each test has the same shape, because each reader has the same job: take a
// snapshot, commit a change AFTER it, and assert the reader still reports the state
// as of the snapshot. A reader that quietly reads the present passes nothing here.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestWalkEdgeHandlesAsOf_IgnoresLaterCommits asserts the handle walk reports the
// adjacency as of the snapshot, not as of the present.
//
// This is the reader that decides whether a capture can fold half a transaction: it
// walks every node, and the unversioned form reads each node's LIVE entry, so a
// commit landing mid-walk lands in the image for the nodes not yet visited.
func TestWalkEdgeHandlesAsOf_IgnoresLaterCommits(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}
	hAB, err := g.AddEdgeH("a", "b", 1)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}

	snap := g.BeginRead()
	defer g.EndRead(snap)

	// Committed AFTER the snapshot: it must not appear.
	hAC, err := g.AddEdgeH("a", "c", 1)
	if err != nil {
		t.Fatalf("AddEdgeH after snapshot: %v", err)
	}

	seen := map[uint64]bool{}
	g.WalkEdgeHandlesAsOf(snap, func(tr EdgeHandleTriple) bool {
		seen[tr.Handle] = true
		return true
	})
	if !seen[hAB] {
		t.Error("the handle committed BEFORE the snapshot is missing from the as-of walk")
	}
	if seen[hAC] {
		t.Error("the handle committed AFTER the snapshot appears in the as-of walk: the " +
			"reader is reading the present, so a capture using it folds a transaction that " +
			"committed while the capture ran")
	}

	// The present-time form must still see both — otherwise this test would pass
	// against a build where the write simply never landed.
	live := map[uint64]bool{}
	g.WalkEdgeHandles(func(tr EdgeHandleTriple) bool { live[tr.Handle] = true; return true })
	if !live[hAB] || !live[hAC] {
		t.Fatalf("the present-time walk sees before=%v after=%v; the fixture did not commit "+
			"what this test assumes, so the assertion above proved nothing", live[hAB], live[hAC])
	}
}

// TestForEachPairOverflowRelTypeByIDAsOf_IgnoresLaterCommits asserts the pair's
// overflow relationship types are reported as of the snapshot.
func TestForEachPairOverflowRelTypeByIDAsOf_IgnoresLaterCommits(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	// Two types: the FIRST lives on the slot, the second and later in the overflow
	// store, which is the structure under test.
	g.SetEdgeLabel("a", "b", "R1")
	g.SetEdgeLabel("a", "b", "R2")

	snap := g.BeginRead()
	defer g.EndRead(snap)

	g.SetEdgeLabel("a", "b", "R3")

	src, dst := nodeIDOf(t, g, "a"), nodeIDOf(t, g, "b")
	asOf := map[string]bool{}
	g.ForEachPairOverflowRelTypeByIDAsOf(src, dst, snap, func(name string) { asOf[name] = true })
	live := map[string]bool{}
	g.ForEachPairOverflowRelTypeByID(src, dst, func(name string) { live[name] = true })

	if !live["R3"] {
		t.Fatalf("the present-time reader does not see R3 (live=%v); the fixture did not "+
			"commit what this test assumes", live)
	}
	if asOf["R3"] {
		t.Errorf("the as-of reader sees R3, committed after the snapshot (as-of=%v)", asOf)
	}
	if !asOf["R2"] {
		t.Errorf("the as-of reader lost R2, committed before the snapshot (as-of=%v)", asOf)
	}
}

// TestTombstonedIDsAsOf_ReportsTheSnapshotsDeadSet asserts the dead set is the one
// the snapshot saw: a node removed after the snapshot is still LIVE as of it, and a
// node removed before it is dead.
//
// The result must also be ascending, because the snapshot writer relies on that
// ordering and the present-time form gets it free from the bitmap while this one
// gets it from the mapper walk.
func TestTombstonedIDsAsOf_ReportsTheSnapshotsDeadSet(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}
	g.RemoveNode("b")

	snap := g.BeginRead()
	defer g.EndRead(snap)

	g.RemoveNode("c") // after the snapshot

	dead := map[graph.NodeID]bool{}
	got := g.TombstonedIDsAsOf(snap)
	for _, id := range got {
		dead[id] = true
	}
	if !dead[nodeIDOf(t, g, "b")] {
		t.Error("b was removed BEFORE the snapshot and is not in the as-of dead set")
	}
	if dead[nodeIDOf(t, g, "c")] {
		t.Error("c was removed AFTER the snapshot but appears in the as-of dead set: the " +
			"reader is reading the present")
	}
	if dead[nodeIDOf(t, g, "a")] || dead[nodeIDOf(t, g, "d")] {
		t.Errorf("a live node is in the dead set: %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("the dead set is not strictly ascending at %d: %v", i, got)
		}
	}
	// The present-time form must disagree, or the fixture proved nothing.
	if len(g.TombstonedIDs()) != len(got)+1 {
		t.Fatalf("the present-time dead set has %d entries and the as-of one %d; they must "+
			"differ by exactly the node removed after the snapshot",
			len(g.TombstonedIDs()), len(got))
	}
}
