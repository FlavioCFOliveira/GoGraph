package lpg

// degree_snapshot_liveness_test.go — rmp #2969: the typed and matching degree
// walkers resolve a neighbour's liveness at the read position, not from the
// stored tombstone bitmap, so a removal committed after a snapshot, or one no
// transaction has committed, does not hide a neighbour the reader still sees.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

var errFsyncForTest = errors.New("injected fsync failure")

func degreeLivenessFixture(t *testing.T) (*Graph[string, float64], LabelID) {
	t.Helper()
	g := newDirectTxGraph(t, true)
	requireNoErr(t, g.AddNode("a"), g.AddNode("b"))
	if _, err := g.AddEdgeH("a", "b", 1); err != nil {
		t.Fatal(err)
	}
	requireNoErr(t, g.SetEdgeLabel("a", "b", "T"))
	lid, ok := g.Registry().Lookup("T")
	if !ok {
		t.Fatal("relationship type T not interned")
	}
	return g, lid
}

func TestDegree_SnapshotSeesANeighbourRemovedAfterIt(t *testing.T) {
	g, lid := degreeLivenessFixture(t)
	a, _ := g.AdjList().Mapper().Lookup("a")
	snap := g.BeginRead()
	defer g.EndRead(snap)
	if err := g.ApplyVersioned(func(wtx WriteTx) error {
		g.Writer(wtx).RemoveNode("b")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !g.IsTombstonedStored(degreeNodeID(t, g, "b")) {
		t.Fatal("premise: b is not tombstoned after its committed removal")
	}
	if n, _ := g.OutDegreeByTypeBoundedByIDAsOf(a, lid, 10, snap); n != 1 {
		t.Errorf("typed degree as of the older snapshot = %d, want 1: the later removal hid b", n)
	}
	if n, _ := g.OutDegreeMatchingBoundedByIDAsOf(a, lid, false, 10, anyNode, snap); n != 1 {
		t.Errorf("matching degree as of the older snapshot = %d, want 1", n)
	}
	if n, _ := g.OutDegreeBoundedByIDAsOf(a, 10, snap); n != 1 {
		t.Errorf("bounded degree as of the older snapshot = %d, want 1", n)
	}
}

func TestDegree_PresentReadIgnoresAnUncommittedNeighbourRemoval(t *testing.T) {
	g, lid := degreeLivenessFixture(t)
	var typed, matching, bounded int
	err := g.ApplyDurable(func(wtx WriteTx) error {
		g.Writer(wtx).RemoveNode("b")
		return nil
	}, func() error {
		done := make(chan struct{})
		go func() {
			defer close(done)
			typed, _ = g.OutDegreeByType("a", lid)
			a, _ := g.AdjList().Mapper().Lookup("a")
			matching, _ = g.OutDegreeMatchingBoundedByID(a, lid, true, 10, anyNode)
			bounded, _ = g.OutDegreeBoundedByID(a, 10)
		}()
		<-done
		return errFsyncForTest
	})
	if err == nil {
		t.Fatal("the durable step's failure was not reported")
	}
	if typed != 1 || matching != 1 || bounded != 1 {
		t.Errorf("present degrees during the uncommitted removal = typed %d, matching %d, bounded %d; want 1, 1, 1",
			typed, matching, bounded)
	}
	if n, _ := g.OutDegreeByType("a", lid); n != 1 {
		t.Errorf("present typed degree after the withdrawn removal = %d, want 1", n)
	}
}

func degreeNodeID(t *testing.T, g *Graph[string, float64], k string) graph.NodeID {
	t.Helper()
	x, ok := g.AdjList().Mapper().Lookup(k)
	if !ok {
		t.Fatalf("%s is not interned", k)
	}
	return x
}

// anyNode is a far-endpoint predicate that accepts every node, so the matching
// walker counts on liveness alone.
func anyNode(graph.NodeID) bool { return true }
