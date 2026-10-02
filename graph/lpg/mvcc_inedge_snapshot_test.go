package lpg

// mvcc_inedge_snapshot_test.go — rmp #2884: the delete path's in-edge question
// is answered from the reader's snapshot, and a directed arc removal claims its
// destination.
//
// The in-edge index (graph/adjlist/reverse.go) records the PRESENT. A removal
// by a concurrent transaction takes the arc out of it at write time, so a reader
// whose snapshot still holds the arc could not find it there; the removal now
// leaves a ghost candidate that the reader confirms against the versioned
// forward adjacency. The destination claim is the second half: a transaction
// that removed x→d and rolled back re-created the arc into a d that a concurrent
// delete had retired, because the delete claimed nothing the removal touched.
//
// Layer: short. No goroutines are spawned.

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func seedInEdge(t *testing.T) *Graph[string, int64] {
	t.Helper()
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	seed := g.beginLabelTx()
	if err := seed.addEdge("x", "d", 1); err != nil {
		t.Fatalf("seed addEdge: %v", err)
	}
	if _, err := seed.commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return g
}

// TestInEdgeSnapshot_RemovedArcStaysVisibleToAnOlderSnapshot pins the ghost: an
// arc removed after a snapshot began is still that snapshot's in-edge, is not
// the remover's, and stops being anyone's once the removal is reclaimable.
func TestInEdgeSnapshot_RemovedArcStaysVisibleToAnOlderSnapshot(t *testing.T) {
	g := seedInEdge(t)
	defer func() { _ = g.Close() }()

	snap := g.BeginRead()
	remover := g.beginLabelTx()
	if !remover.removeEdge("x", "d") {
		t.Fatal("premise: the removal of the committed arc x→d was refused")
	}

	// The removal is uncommitted, so a committed-only present read still lists
	// x (rmp #2965, round 5); the remover's own view, checked below, does not.
	if got := g.AdjList().InNeighbours("d"); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("the present in-edge read lists %v while the removal is uncommitted, want [x]", got)
	}
	if got := g.ReadAt(snap).InNeighbours("d"); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("a snapshot older than the removal reads in-neighbours %v of d, want [x]: "+
			"the present index was consulted instead of the snapshot", got)
	}
	if !g.ReadAt(snap).HasInNeighbour("d") {
		t.Fatal("HasInNeighbour disagrees with InNeighbours for the older snapshot")
	}
	if got := g.ReadAt(&remover.ctx.snap).InNeighbours("d"); len(got) != 0 {
		t.Fatalf("the remover's own view reads in-neighbours %v of d, want none: "+
			"a transaction must see its own removal", got)
	}
	if _, err := remover.commit(); err != nil {
		t.Fatalf("remover commit: %v", err)
	}
	if got := g.ReadAt(snap).InNeighbours("d"); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("after the removal committed, the older snapshot reads %v, want [x]", got)
	}
	if n := g.AdjList().RecordedInEdgeGhosts(); n == 0 {
		t.Fatal("no ghost is recorded while a snapshot older than the removal is open")
	}

	g.EndRead(snap)
	_ = g.ReclaimNow()
	if n := g.AdjList().RecordedInEdgeGhosts(); n != 0 {
		t.Fatalf("%d ghost(s) survive reclamation past every reader: the candidates grow without bound", n)
	}
	fresh := g.BeginRead()
	defer g.EndRead(fresh)
	if got := g.ReadAt(fresh).InNeighbours("d"); len(got) != 0 {
		t.Fatalf("a snapshot newer than the removal reads in-neighbours %v of d, want none", got)
	}
}

// TestInEdgeSnapshot_DirectedRemovalClaimsTheDestination pins the destination
// claim: while a transaction's removal of x→d is in flight, a concurrent
// retirement of d is refused as a serialization conflict.
func TestInEdgeSnapshot_DirectedRemovalClaimsTheDestination(t *testing.T) {
	g := seedInEdge(t)
	defer func() { _ = g.Close() }()

	remover := g.beginLabelTx()
	if !remover.removeEdge("x", "d") {
		t.Fatal("premise: the removal of the committed arc x→d was refused")
	}
	deleter := g.beginLabelTx()
	if g.removeNodeInfo("d", deleter.ctx) {
		t.Fatal("d was retired while another transaction's removal of x→d is in flight: " +
			"a rollback of that removal re-creates the arc into a deleted node")
	}
	wantConflictAt(t, deleter, "adjacency")
	if _, err := remover.commit(); err != nil {
		t.Fatalf("the remover, the first writer, was refused: %v", err)
	}
}

// TestInEdgeSnapshot_RetirementWithoutAConcurrentRemovalIsAdmitted is the
// non-vacuity control: with no removal in flight the same retirement applies.
func TestInEdgeSnapshot_RetirementWithoutAConcurrentRemovalIsAdmitted(t *testing.T) {
	g := seedInEdge(t)
	defer func() { _ = g.Close() }()

	remover := g.beginLabelTx()
	if !remover.removeEdge("x", "d") {
		t.Fatal("premise: the removal of the committed arc x→d was refused")
	}
	if _, err := remover.commit(); err != nil {
		t.Fatalf("remover commit: %v", err)
	}
	deleter := g.beginLabelTx()
	if !g.removeNodeInfo("d", deleter.ctx) {
		t.Fatal("the retirement of d was refused although no concurrent write touches d")
	}
	if _, err := deleter.commit(); err != nil {
		t.Fatalf("deleter commit: %v", err)
	}
}
