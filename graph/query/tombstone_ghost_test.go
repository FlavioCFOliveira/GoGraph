package query

// Regression gate for rmp task #1356: graph/query must never return
// tombstoned (deleted) nodes and must never count never-interned ghost
// slots.
//
// Background: the Mapper packs NodeIDs as (intraIndex<<8)|shard across
// 256 shards, so Mapper.MaxNodeID() rounds up to maxIntra*256. The old
// no-predicate seed added the whole [0, MaxNodeID) range, inflating
// Cardinality() of a 3-node graph to 256, and no result surface
// filtered lpg.Graph.IsTombstoned, so deleted nodes leaked through
// Collect/NodeIDs, label-seeded matches, and Out() expansion.

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// setupTriangleGraph builds the audit's repro shape: three nodes
// (alice, bob, dead), all labelled Person, with an alice->dead edge so
// Out() expansion can observe the tombstoned node through the CSR.
func setupTriangleGraph(tb testing.TB) (*lpg.Graph[string, int64], *csr.CSR[int64]) {
	tb.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"alice", "bob", "dead"} {
		if err := g.SetNodeLabel(n, "Person"); err != nil {
			tb.Fatalf("SetNodeLabel(%q): %v", n, err)
		}
	}
	if err := g.AddEdge("alice", "dead", 1); err != nil {
		tb.Fatalf("AddEdge: %v", err)
	}
	return g, csr.BuildFromAdjList(g.AdjList())
}

func TestQuery_NoPredicateCardinalityEqualsOrder(t *testing.T) {
	t.Parallel()
	g, c := setupTriangleGraph(t)
	e := New(g, c)

	got := e.Match().Vertex().Cardinality()
	if want := g.AdjList().Order(); got != want {
		t.Fatalf("Cardinality() = %d, want Order() = %d (ghost slots counted)", got, want)
	}
	if got != 3 {
		t.Fatalf("Cardinality() = %d, want 3", got)
	}
	names := e.Match().Vertex().Collect()
	slices.Sort(names)
	if !slices.Equal(names, []string{"alice", "bob", "dead"}) {
		t.Fatalf("Collect() = %v, want [alice bob dead]", names)
	}
}

func TestQuery_TombstonedNodeExcludedEverywhere(t *testing.T) {
	t.Parallel()
	g, c := setupTriangleGraph(t)
	g.RemoveNode("dead")
	e := New(g, c)

	// Cardinality must equal the live (non-tombstoned) order.
	if got, want := e.Match().Vertex().Cardinality(), g.LiveOrder(); got != want {
		t.Fatalf("Cardinality() = %d, want LiveOrder() = %d", got, want)
	}

	// Collect on a no-predicate match must not contain the deleted node.
	names := e.Match().Vertex().Collect()
	slices.Sort(names)
	if !slices.Equal(names, []string{"alice", "bob"}) {
		t.Fatalf("Collect() = %v, want [alice bob]", names)
	}

	// NodeIDs must not yield the tombstoned id.
	deadID, ok := g.AdjList().Mapper().Lookup("dead")
	if !ok {
		t.Fatal("Mapper.Lookup(dead) must still resolve (NodeID stability)")
	}
	for id := range e.Match().Vertex().NodeIDs() {
		if id == deadID {
			t.Fatalf("NodeIDs() yielded tombstoned id %d", id)
		}
	}

	// Label-seeded match (Roaring NodeIndex fast path) must not leak
	// the tombstoned node either, even when the caller did not strip
	// labels before RemoveNode.
	labelled := e.Match().Vertex(WithLabel[string, int64]("Person")).Collect()
	slices.Sort(labelled)
	if !slices.Equal(labelled, []string{"alice", "bob"}) {
		t.Fatalf("label-seeded Collect() = %v, want [alice bob]", labelled)
	}

	// Out() expansion walks the CSR snapshot, which still holds the
	// alice->dead edge; the tombstoned destination must be pruned.
	hops := e.Match().Vertex(WithLabel[string, int64]("Person")).Out().Collect()
	if len(hops) != 0 {
		t.Fatalf("Out().Collect() = %v, want [] (dead is tombstoned)", hops)
	}
}

func TestQuery_DeleteThenRecreateAppearsOnce(t *testing.T) {
	t.Parallel()
	g, c := setupTriangleGraph(t)
	g.RemoveNode("dead")
	if err := g.AddNode("dead"); err != nil { // revives the tombstoned node
		t.Fatalf("AddNode: %v", err)
	}
	e := New(g, c)

	names := e.Match().Vertex().Collect()
	slices.Sort(names)
	if !slices.Equal(names, []string{"alice", "bob", "dead"}) {
		t.Fatalf("Collect() = %v, want [alice bob dead] (revived exactly once)", names)
	}
	if got := e.Match().Vertex().Cardinality(); got != 3 {
		t.Fatalf("Cardinality() = %d, want 3 after revive", got)
	}
}

func TestQuery_EmptyGraph(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	e := New(g, csr.BuildFromAdjList(g.AdjList()))

	if got := e.Match().Vertex().Cardinality(); got != 0 {
		t.Fatalf("Cardinality() = %d, want 0 on empty graph", got)
	}
	if names := e.Match().Vertex().Collect(); len(names) != 0 {
		t.Fatalf("Collect() = %v, want [] on empty graph", names)
	}
}

func TestQuery_AllNodesDeleted(t *testing.T) {
	t.Parallel()
	g, c := setupTriangleGraph(t)
	for _, n := range []string{"alice", "bob", "dead"} {
		g.RemoveNode(n)
	}
	e := New(g, c)

	if got := e.Match().Vertex().Cardinality(); got != 0 {
		t.Fatalf("Cardinality() = %d, want 0 when every node is deleted", got)
	}
	if names := e.Match().Vertex().Collect(); len(names) != 0 {
		t.Fatalf("Collect() = %v, want [] when every node is deleted", names)
	}
	var ids []graph.NodeID
	for id := range e.Match().Vertex().NodeIDs() {
		ids = append(ids, id)
	}
	if len(ids) != 0 {
		t.Fatalf("NodeIDs() = %v, want none when every node is deleted", ids)
	}
}
