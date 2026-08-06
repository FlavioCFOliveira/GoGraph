package lpg

// mvcc_endpoint_birth_test.go — rmp #2331: a node an EDGE created is born at its
// transaction's instant, not at the beginning of time.
//
// Layer: short.
//
// # The defect this pins, as measured
//
// adjlist.addEdge interns its endpoints with the plain Mapper.Intern, which fires no
// birth hook, so a node created by an append carried no versioned birth record.
// [Graph.noteNodeLife] documents the invariant that made that fatal: a node with no
// record is one that "has existed for longer than any reader can remember", so every
// as-of reader of node existence read the absence as "exists".
//
// The result was an asymmetry WITHIN one transaction: its arc was correctly withheld
// from a snapshot taken before it committed, and its endpoints were not. Measured
// inside a checkpoint capture with writers running `tx.AddEdge(freshSrc, freshDst, 0)`:
// at an instant where four transactions were visible, the image held four arcs and TEN
// nodes rather than eight.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestEndpointBirth_CreatedByAnEdgeIsInvisibleBeforeItsCommit asserts an endpoint the
// append created is invisible to a snapshot older than the transaction that created
// it — the same answer its own arc gives.
//
// The two halves are asserted TOGETHER on purpose. Either one alone would be satisfied
// by a build that hides everything or shows everything; what the transaction owes is
// that they AGREE.
func TestEndpointBirth_CreatedByAnEdgeIsInvisibleBeforeItsCommit(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	// A snapshot taken BEFORE anything is written. Nothing below may reach it.
	before := g.BeginRead()
	defer g.EndRead(before)

	if err := g.ApplyVersioned(func(tx WriteTx) error {
		// AddEdge with two keys that do not exist: the append creates both.
		return g.Writer(tx).AddEdge("a", "b", 1)
	}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	srcID, dstID := nodeIDOf(t, g, "a"), nodeIDOf(t, g, "b")

	// The ARC as of `before`: the adjacency has always got this right.
	arcVisible := len(g.adj.EntryViewAsOf(srcID, before.startTS, before.txID).Neighbours) > 0
	if arcVisible {
		t.Fatal("the arc is visible to a snapshot older than the transaction that created " +
			"it; the adjacency's own versioning has regressed and this test's premise is gone")
	}

	// The ENDPOINTS as of `before` must agree with the arc.
	if g.NodeExistsAsOf(srcID, before) || g.NodeExistsAsOf(dstID, before) {
		t.Errorf("a node CREATED BY THE APPEND exists as of a snapshot older than the "+
			"transaction that created it (src=%v dst=%v), while its own arc does not. One "+
			"transaction became visible in two pieces",
			g.NodeExistsAsOf(srcID, before), g.NodeExistsAsOf(dstID, before))
	}
	// And a snapshot taken AFTER must see all three, or the fix has hidden real work.
	after := g.BeginRead()
	defer g.EndRead(after)
	if !g.NodeExistsAsOf(srcID, after) || !g.NodeExistsAsOf(dstID, after) {
		t.Error("the endpoints are invisible to a snapshot taken after their transaction " +
			"committed: the birth record is stamped at the wrong instant")
	}
	if len(g.adj.EntryViewAsOf(srcID, after.startTS, after.txID).Neighbours) == 0 {
		t.Error("the arc is invisible to a snapshot taken after its transaction committed")
	}
}

// TestEndpointBirth_NodesAndEdgesStayInStep is the measured DIAG turned into an
// assertion: with a transaction in flight, the number of nodes visible as of a
// snapshot must be exactly twice the number of edges visible as of it.
//
// The fixture guarantees that ratio by construction — every transaction creates two
// fresh nodes and one edge between them — which is what makes it an ABSOLUTE oracle
// rather than a comparison against the implementation's own answer.
func TestEndpointBirth_NodesAndEdgesStayInStep(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	const committedTx = 5
	for i := 0; i < committedTx; i++ {
		a, b := "a"+string(rune('A'+i)), "b"+string(rune('A'+i))
		if err := g.ApplyVersioned(func(tx WriteTx) error {
			return g.Writer(tx).AddEdge(a, b, 1)
		}); err != nil {
			t.Fatalf("AddEdge %d: %v", i, err)
		}
	}

	// The snapshot every count below is taken at.
	at := g.BeginRead()
	defer g.EndRead(at)

	// An IN-FLIGHT transaction, opened AFTER the snapshot and never committed while
	// the counts are taken. Its two endpoints are exactly what used to leak.
	inflight := g.BeginVersionedTx()
	if err := g.Writer(inflight).AddEdge("ghost-a", "ghost-b", 1); err != nil {
		g.EndVersionedTx(inflight)
		t.Fatalf("in-flight AddEdge: %v", err)
	}

	var nodes, edges int
	g.adj.Mapper().Walk(func(id graph.NodeID, _ string) bool {
		if g.NodeExistsAsOf(id, at) {
			nodes++
		}
		edges += len(g.adj.EntryViewAsOf(id, at.startTS, at.txID).Neighbours)
		return true
	})
	g.EndVersionedTx(inflight)

	if edges != committedTx {
		t.Fatalf("%d edges are visible as of the snapshot, want %d: the in-flight "+
			"transaction's arc leaked, or a committed one was lost, and the node count "+
			"below cannot be interpreted", edges, committedTx)
	}
	if nodes != 2*edges {
		t.Errorf("%d nodes and %d edges are visible as of the same snapshot, want %d nodes: "+
			"every transaction in this fixture creates exactly two nodes and one edge, so a "+
			"higher node count is an in-flight transaction's ENDPOINTS becoming visible "+
			"while its own arc correctly did not", nodes, edges, 2*edges)
	}
}
