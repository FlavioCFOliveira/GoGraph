package lpg

// mvcc_conflict_stores_test.go — write-write conflict detection on every
// versioned store that is NOT node labels or node properties (rmp #2300 AC2),
// reached through the per-transaction write state of rmp #2301.
//
// The stores covered here are node existence and the five per-edge side stores:
// overflow relationship types, per-handle relationship types, per-handle
// properties, per-ordinal relationship types, and per-ordinal properties. Node
// labels and node properties are covered in mvcc_writectx_test.go, and the
// adjacency in the adjlist package.
//
// # What each test proves, and how it was validated
//
// Every case is the same shape, because the defect is the same shape: writer A
// modifies an object and stays in flight; writer B, which cannot see A's
// version, modifies the SAME object. B must be refused. Each was verified
// against a build with the store's conflict check removed, where B's write
// landed on top of A's and both transactions committed — the lost update.
//
// The refusal reaches the caller through the transaction rather than through a
// return value, because these primitives return nothing; see
// [writeCtx.conflictErr]. So the assertion is on commit, which is where
// Memgraph reads its own `must_abort` flag too.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// wantConflictAt commits tx expecting a refusal from the named store.
func wantConflictAt[N comparable, W any](t *testing.T, tx *labelTx[N, W], store string) {
	t.Helper()
	ts, err := tx.commit()
	if err == nil {
		t.Fatalf("the transaction committed at %d after writing an object another writer "+
			"is still writing: its write is silently lost", ts)
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("commit failed with %v, want a serialization conflict", err)
	}
	var c *mvcc.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("the error carries no *mvcc.Conflict: %v", err)
	}
	if c.Store != store {
		t.Fatalf("Conflict.Store = %q, want %q — the conflict must name the store it "+
			"came from, or a bug report starts with a stack trace instead of an answer",
			c.Store, store)
	}
	if !c.ConcurrentWriter() {
		t.Fatal("the blocking version belongs to an in-flight writer, so this is " +
			"first-updater-wins and must report as such")
	}
	if c.TxID != tx.ctx.txID {
		t.Fatalf("the conflict is attributed to transaction %d, but the refused one is %d",
			c.TxID, tx.ctx.txID)
	}
}

// TestConflict_NodeExistence covers the birth/death chain: two writers may not
// both decide whether the same node exists.
//
// A deletes the node; B re-creates it. B cannot see A's death record, so
// without detection B's birth lands on top of it and both commit — the node
// ends up alive although the transaction that killed it also succeeded.
func TestConflict_NodeExistence(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// A removes the node and stays in flight.
	txA := g.beginLabelTx()
	txA.removeNode("a")

	// B re-creates it. AddNode on an interned, tombstoned id reaches
	// [Graph.revive] → noteNodeRevived, which records a BIRTH on the same chain
	// A just wrote a death to.
	txB := g.beginLabelTx()
	if err := txB.addNode("a"); err != nil {
		t.Fatalf("B addNode: %v", err)
	}
	wantConflictAt(t, txB, "node existence")

	tsA := mustCommit(t, txA)
	if g.NodeExistsAsOf(nodeIDOf(t, g, "a"), &Snapshot{startTS: tsA}) {
		t.Fatal("B's refused revival was recorded anyway: the node A deleted is alive")
	}
}

// nodeIDOf resolves a node's id, failing the test when it is unknown.
func nodeIDOf[N comparable, W any](t *testing.T, g *Graph[N, W], n N) graph.NodeID {
	t.Helper()
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		t.Fatalf("node %v is unknown", n)
	}
	return id
}

// TestConflict_EdgeOverflowRelTypes covers the overflow store: a pair's SECOND
// and later relationship types.
func TestConflict_EdgeOverflowRelTypes(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	// The first type lands on the adjacency slot; overflow starts at the second.
	g.SetEdgeLabel("a", "b", "FIRST")

	txA := g.beginLabelTx()
	txA.setEdgeLabel("a", "b", "FROM_A")

	txB := g.beginLabelTx()
	txB.setEdgeLabel("a", "b", "FROM_B")
	wantConflictAt(t, txB, "edge relationship types")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
}

// TestConflict_EdgeRelTypeByHandle covers the per-handle relationship-type
// store: one parallel edge instance, addressed by its stable handle.
func TestConflict_EdgeRelTypeByHandle(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	h, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}

	txA := g.beginLabelTx()
	txA.setEdgeLabelByHandle("a", "b", h, "FROM_A")

	txB := g.beginLabelTx()
	txB.setEdgeLabelByHandle("a", "b", h, "FROM_B")
	wantConflictAt(t, txB, "edge relationship types by handle")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
}

// TestConflict_EdgePropertyByHandle covers the per-handle property store.
func TestConflict_EdgePropertyByHandle(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	h, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.setEdgePropertyByHandle("a", "b", h, "w", Int64Value(1)); err != nil {
		t.Fatalf("A setEdgePropertyByHandle: %v", err)
	}

	txB := g.beginLabelTx()
	if err := txB.setEdgePropertyByHandle("a", "b", h, "w", Int64Value(2)); err != nil {
		t.Fatalf("B setEdgePropertyByHandle returned early: %v", err)
	}
	wantConflictAt(t, txB, "edge properties by handle")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
}

// TestConflict_EdgeRelTypeByOrdinal covers the per-ordinal relationship-type
// store: the same instance addressed by its position in the pair rather than by
// its handle. It is a separate chain, so it needs its own gate.
func TestConflict_EdgeRelTypeByOrdinal(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	txA := g.beginLabelTx()
	txA.setEdgeLabelAt("a", "b", 1, "FROM_A")

	txB := g.beginLabelTx()
	txB.setEdgeLabelAt("a", "b", 1, "FROM_B")
	wantConflictAt(t, txB, "edge relationship types by ordinal")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
}

// TestConflict_EdgePropertyByOrdinal covers the per-ordinal property store.
func TestConflict_EdgePropertyByOrdinal(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.setEdgePropertyAt("a", "b", 1, "w", Int64Value(1)); err != nil {
		t.Fatalf("A setEdgePropertyAt: %v", err)
	}

	txB := g.beginLabelTx()
	if err := txB.setEdgePropertyAt("a", "b", 1, "w", Int64Value(2)); err != nil {
		t.Fatalf("B setEdgePropertyAt returned early: %v", err)
	}
	wantConflictAt(t, txB, "edge properties by ordinal")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
}

// TestConflict_DisjointEdgeWritersDoNotConflict is the counterpart every one of
// the tests above needs: detection that fires on disjoint objects is a global
// lock wearing a new name, and would make the whole sprint pointless.
//
// It exercises all five side stores at once, on two disjoint pairs.
func TestConflict_DisjointEdgeWritersDoNotConflict(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	hAB, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH a->b: %v", err)
	}
	hCD, err := g.AddEdgeH("c", "d", 0)
	if err != nil {
		t.Fatalf("AddEdgeH c->d: %v", err)
	}
	g.SetEdgeLabel("a", "b", "FIRST")
	g.SetEdgeLabel("c", "d", "FIRST")

	txA := g.beginLabelTx()
	txB := g.beginLabelTx()

	txA.setEdgeLabel("a", "b", "SECOND_A")
	txB.setEdgeLabel("c", "d", "SECOND_B")
	txA.setEdgeLabelByHandle("a", "b", hAB, "BY_HANDLE_A")
	txB.setEdgeLabelByHandle("c", "d", hCD, "BY_HANDLE_B")
	if err := txA.setEdgePropertyByHandle("a", "b", hAB, "w", Int64Value(1)); err != nil {
		t.Fatalf("A property by handle: %v", err)
	}
	if err := txB.setEdgePropertyByHandle("c", "d", hCD, "w", Int64Value(2)); err != nil {
		t.Fatalf("B property by handle: %v", err)
	}
	txA.setEdgeLabelAt("a", "b", 1, "BY_ORDINAL_A")
	txB.setEdgeLabelAt("c", "d", 1, "BY_ORDINAL_B")
	if err := txA.setEdgePropertyAt("a", "b", 1, "v", Int64Value(1)); err != nil {
		t.Fatalf("A property by ordinal: %v", err)
	}
	if err := txB.setEdgePropertyAt("c", "d", 1, "v", Int64Value(2)); err != nil {
		t.Fatalf("B property by ordinal: %v", err)
	}

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A was refused despite touching only its own pair: %v — detection is "+
			"behaving as a global lock", err)
	}
	if _, err := txB.commit(); err != nil {
		t.Fatalf("B was refused despite touching only its own pair: %v — detection is "+
			"behaving as a global lock", err)
	}
}

// TestConflict_OwnSecondWriteToEdgeStores is the case Memgraph's
// PrepareForWrite tests first, applied to the side stores: a transaction must
// be free to write the same object twice.
func TestConflict_OwnSecondWriteToEdgeStores(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	h, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	g.SetEdgeLabel("a", "b", "FIRST")

	tx := g.beginLabelTx()
	tx.setEdgeLabel("a", "b", "SECOND")
	tx.setEdgeLabel("a", "b", "THIRD")
	tx.setEdgeLabelByHandle("a", "b", h, "ONE")
	tx.setEdgeLabelByHandle("a", "b", h, "TWO")
	if err := tx.setEdgePropertyByHandle("a", "b", h, "w", Int64Value(1)); err != nil {
		t.Fatalf("first property write: %v", err)
	}
	if err := tx.setEdgePropertyByHandle("a", "b", h, "w", Int64Value(2)); err != nil {
		t.Fatalf("a transaction was refused its own second write to the same edge: %v", err)
	}
	tx.setEdgeLabelAt("a", "b", 1, "ORD_ONE")
	tx.setEdgeLabelAt("a", "b", 1, "ORD_TWO")
	if err := tx.setEdgePropertyAt("a", "b", 1, "v", Int64Value(1)); err != nil {
		t.Fatalf("first ordinal property write: %v", err)
	}
	if err := tx.setEdgePropertyAt("a", "b", 1, "v", Int64Value(2)); err != nil {
		t.Fatalf("a transaction was refused its own second ordinal property write: %v", err)
	}

	if _, err := tx.commit(); err != nil {
		t.Fatalf("a transaction that only ever wrote its own objects was refused: %v", err)
	}
}

// TestConflict_EdgeOverflowRelTypeRemoval covers the REMOVAL half of the
// overflow store, which reaches the chain through removeOverflowVersioned
// rather than addOverflowVersioned.
//
// It matters on its own because the removal path is the one that cannot report
// an error to its caller — removeEdgeLabel returns nothing — so the refusal has
// to travel on the transaction and surface at commit. Verified to lose the
// update against a build with detection off: B's removal landed on top of A's
// still-in-flight write and both committed.
func TestConflict_EdgeOverflowRelTypeRemoval(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	// The first type lands on the adjacency slot; the second goes to overflow,
	// which is the chain this test is about.
	g.SetEdgeLabel("a", "b", "FIRST")
	g.SetEdgeLabel("a", "b", "SECOND")

	// A writes the overflow list and stays in flight.
	txA := g.beginLabelTx()
	txA.setEdgeLabel("a", "b", "THIRD")

	// B removes from the same list.
	txB := g.beginLabelTx()
	txB.removeEdgeLabel("a", "b", "SECOND")
	wantConflictAt(t, txB, "edge relationship types")

	// A's write survives and B's removal did not happen.
	tsA := mustCommit(t, txA)
	labels := g.ReadAt(&Snapshot{startTS: tsA}).EdgeLabels("a", "b")
	var sawSecond, sawThird bool
	for _, l := range labels {
		switch l {
		case "SECOND":
			sawSecond = true
		case "THIRD":
			sawThird = true
		}
	}
	if !sawSecond {
		t.Fatalf("B's refused removal was applied anyway: labels = %v", labels)
	}
	if !sawThird {
		t.Fatalf("A's write did not survive despite B being refused: labels = %v", labels)
	}
}
