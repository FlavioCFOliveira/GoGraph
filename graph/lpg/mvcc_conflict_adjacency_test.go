package lpg

// mvcc_conflict_adjacency_test.go — write-write conflict detection on the
// ADJACENCY store, and on the two stores that were instrumented without a
// dedicated test of their own: node labels and node properties (rmp #2300 AC2).
//
// # Why adjacency needs its own file and its own table
//
// Every other store here answers one question — may I displace the newest
// version at this key? — and one test shape covers it. Adjacency has a TABLE:
//
//	append(A→B)      ‖ append(A→C)             → conflict   (rmp #2445)
//	append(A→B)      ‖ removeEdge(A→C)         → conflict
//	removeEdge(A→B)  ‖ removeEdge(A→C)         → conflict
//	append(A→B)      ‖ removeAllEdgesFrom(A)   → conflict   (rmp #2694)
//	append(A→B)      ‖ append(Z→C)             → both commit (disjoint sources)
//
// The fourth row lives in mvcc_conflict_bulk_edge_removal_test.go, because the
// bulk removal `DETACH DELETE` uses was the one non-commutative adjacency write
// that never took the claim, and the consequence needed more than a refusal
// assertion to state.
//
// The first row used to read "both commit" — appends were treated as
// commutative facts. The DST multi-session mode disproved the premise
// (rmp #2445): an adjacency ENTRY is an immutable snapshot built from the
// node's current slot, so the second transaction's entry EMBEDS the first
// one's still-pending arc; when the embedder commits, readers see an
// uncommitted edge, and when the arc's owner aborts, the aborted arc survives
// in the committed entry permanently. The node is therefore the unit of
// write-write conflict for appends exactly as rmp #2444 made it for deletes —
// Memgraph's PrepareForWrite semantics.
// TestConflict_AdjacencyAppendsCommuteAndAreStillRecorded therefore also proves
// the append was RECORDED, by showing a subsequent removal is refused because of
// it — so the row cannot pass vacuously.
//
// The rule and the Memgraph source behind it are documented on [adjVersions].
//
// # How every case here was validated
//
// Each positive case was run against a build with the corresponding check
// removed from graph/lpg (noteAppend/noteExclusive's conflict branch, or the
// store's own `tx.conflicts` guard), where the second writer's mutation landed on
// top of the first and BOTH transactions committed — the lost update. A test that
// has not been seen to fail is not evidence.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestConflict_AdjacencyAppendsConflictPerNode is the first row of the table:
// two transactions appending DIFFERENT arcs from the same source conflict —
// the second is refused (rmp #2445; see the file comment for the entry-
// snapshot embedding that retired the old "appends commute" row). Sequential
// appends on the same source still succeed once the first has committed and
// published, which is also what proves the stamps are RECORDED rather than
// the refusal firing vacuously — and a removal concurrent with an in-flight
// append stays refused, as before.
func TestConflict_AdjacencyAppendsConflictPerNode(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
	}

	// Two appends from the same source, both in flight at once: the second is
	// refused, because its entry snapshot would embed A's pending arc.
	txA := g.beginLabelTx()
	if err := txA.addEdge("a", "b", 1); err != nil {
		t.Fatalf("A addEdge: %v", err)
	}
	txB := g.beginLabelTx()
	if err := txB.addEdge("a", "c", 2); err == nil {
		t.Fatal("B's concurrent append on the same source was admitted; it must conflict (rmp #2445)")
	}
	// wantConflictAt drives txB to commit, asserts the typed refusal, and the
	// failed commit aborts the transaction.
	wantConflictAt(t, txB, "adjacency")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
	if !g.AdjList().HasEdge("a", "b") {
		t.Fatal("A's committed arc is missing")
	}
	if g.AdjList().HasEdge("a", "c") {
		t.Fatal("B's refused arc leaked into the adjacency")
	}

	// SEQUENTIAL appends on the same source still succeed: a transaction begun
	// after A's commit sees A's stamp as visible. This is what proves the
	// appends are recorded rather than the refusal above firing vacuously.
	txB2 := g.beginLabelTx()
	if err := txB2.addEdge("a", "c", 2); err != nil {
		t.Fatalf("sequential append on the same source was refused: %v", err)
	}
	if _, err := txB2.commit(); err != nil {
		t.Fatalf("sequential append refused at commit: %v", err)
	}
	if !g.AdjList().HasEdge("a", "c") {
		t.Fatal("sequential append's arc is missing")
	}

	// A removal concurrent with an in-flight append on the same source stays
	// refused.
	txC := g.beginLabelTx()
	if err := txC.addEdge("a", "d", 3); err != nil {
		t.Fatalf("C addEdge: %v", err)
	}
	txD := g.beginLabelTx()
	txD.removeEdge("a", "b")
	wantConflictAt(t, txD, "adjacency")

	if _, err := txC.commit(); err != nil {
		t.Fatalf("C, the first writer, was refused: %v", err)
	}
	if !g.AdjList().HasEdge("a", "b") {
		t.Fatal("D's refused removal was applied anyway: the arc C's transaction did not touch is gone")
	}
}

// TestConflict_AdjacencyAppendRefusedByConcurrentRemoval is the second row in
// the other order: the REMOVAL is in flight first and the append arrives second.
//
// Both orders have to be covered separately because they are detected by
// different halves of the rule — an append consults only the exclusive stamp,
// while a removal consults both. Testing one order and assuming the other is how
// an asymmetric rule ships half-implemented.
func TestConflict_AdjacencyAppendRefusedByConcurrentRemoval(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddNode("c"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// A removes an arc from source "a" and stays in flight.
	txA := g.beginLabelTx()
	txA.removeEdge("a", "b")

	// B appends a different arc from the same source. The removal is not
	// commutative with it, so B must be refused even though its own write is an
	// append.
	txB := g.beginLabelTx()
	if err := txB.addEdge("a", "c", 2); err == nil {
		t.Fatal("B's append was accepted over an in-flight removal on the same source; " +
			"the append must be refused by the exclusive stamp")
	}
	wantConflictAt(t, txB, "adjacency")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
	if g.AdjList().HasEdge("a", "c") {
		t.Fatal("B's refused append landed anyway: a doomed transaction mutated the adjacency")
	}
}

// TestConflict_AdjacencyConcurrentRemovals is the third row: two removals from
// the same source are not commutative with each other either.
func TestConflict_AdjacencyConcurrentRemovals(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for _, e := range [][2]string{{"a", "b"}, {"a", "c"}} {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge(%s,%s): %v", e[0], e[1], err)
		}
	}

	txA := g.beginLabelTx()
	txA.removeEdge("a", "b")

	txB := g.beginLabelTx()
	txB.removeEdge("a", "c")
	wantConflictAt(t, txB, "adjacency")

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A, the first writer, was refused: %v", err)
	}
	if !g.AdjList().HasEdge("a", "c") {
		t.Fatal("B's refused removal was applied anyway: the arc it targeted is gone")
	}
}

// TestConflict_AdjacencyDisjointSourcesDoNotConflict is the closing condition
// the whole design exists for: writers on DIFFERENT sources never meet.
//
// Without it the table above could be satisfied by a store that conflicts on
// everything, which would be the naive port and would cost the sprint its
// objective.
func TestConflict_AdjacencyDisjointSourcesDoNotConflict(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for _, e := range [][2]string{{"a", "x"}, {"b", "y"}} {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge(%s,%s): %v", e[0], e[1], err)
		}
	}

	// Even the non-commutative write does not conflict across sources.
	txA := g.beginLabelTx()
	txA.removeEdge("a", "x")
	txB := g.beginLabelTx()
	txB.removeEdge("b", "y")

	if _, err := txB.commit(); err != nil {
		t.Fatalf("B was refused for removing an arc from an unrelated source: %v", err)
	}
	if _, err := txA.commit(); err != nil {
		t.Fatalf("A was refused: %v", err)
	}
	if g.AdjList().HasEdge("a", "x") || g.AdjList().HasEdge("b", "y") {
		t.Fatal("both disjoint removals committed, so neither arc may remain")
	}
}

// TestConflict_NodeLabels covers the node-label store, which carried a conflict
// check from the first draft of rmp #2300 and never had a test that named it.
//
// A writes a label and stays in flight; B writes a different label to the SAME
// node. Both land on one delta chain, so without detection B's version is
// prepended over A's and A's label is lost while both transactions commit.
func TestConflict_NodeLabels(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeLabel("a", "FROM_A"); err != nil {
		t.Fatalf("A setNodeLabel: %v", err)
	}

	txB := g.beginLabelTx()
	if err := txB.setNodeLabel("a", "FROM_B"); err == nil {
		t.Fatal("B's label write was accepted over an in-flight writer's version on the " +
			"same node: A's label is about to be lost")
	}
	wantConflictAt(t, txB, "node labels")

	tsA := mustCommit(t, txA)
	snap := &Snapshot{startTS: tsA}
	if !g.HasNodeLabelAsOf("a", "FROM_A", snap) {
		t.Fatal("A committed, so its label must be there")
	}
	if g.HasNodeLabelAsOf("a", "FROM_B", snap) {
		t.Fatal("B was refused, so its label must NOT be there")
	}
}

// TestConflict_NodeProperties covers the node-property store on the same
// footing, and for the same reason: instrumented since the first draft, never
// named by a test.
func TestConflict_NodeProperties(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeProperty("a", "k", Int64Value(1)); err != nil {
		t.Fatalf("A setNodeProperty: %v", err)
	}

	txB := g.beginLabelTx()
	if err := txB.setNodeProperty("a", "k", Int64Value(2)); err == nil {
		t.Fatal("B's property write was accepted over an in-flight writer's version on " +
			"the same key: A's value is about to be lost")
	}
	wantConflictAt(t, txB, "node properties")

	tsA := mustCommit(t, txA)
	v, ok := g.GetNodePropertyAsOf("a", "k", &Snapshot{startTS: tsA})
	if !ok {
		t.Fatal("A committed, so its property must be there")
	}
	if got, _ := v.Int64(); got != 1 {
		t.Fatalf("property k = %d, want 1 — B was refused, so its value must not have landed", got)
	}
}

// TestConflict_AdjacencyStampsAreReclaimed is the bounded-resources assertion.
//
// The stamps are pure write-side bookkeeping: no pre-image, never read, no part
// in rollback. Nothing else has a reason to remove them, so if the reclamation
// sweep does not, the map grows to one entry per node ever written
// transactionally and stays there for the life of the process. That is precisely
// the shape of leak rmp #2289 had to close for direct writes, and it would not
// show up in any correctness test.
//
// The test drives committed transactional adjacency writes over many distinct
// sources, confirms the stamps accumulated, then sweeps with no reader open and
// requires them gone.
func TestConflict_AdjacencyStampsAreReclaimed(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})

	// HOLD THE WATERMARK WHILE THE STAMPS ACCUMULATE (rmp #2424).
	//
	// The two assertions below sample g.adjVer.len() and MVCCStats().AdjConflictStamps
	// separately and require them to agree, which they cannot while a BACKGROUND sweep
	// is freeing stamps between the two reads: measured as "AdjConflictStamps = 17,
	// want 5" against an intermediate #2424 fix that woke the vacuum on any
	// sub-threshold charge. The shipped fix wakes only when retention exceeds the
	// bound, which 64 small transactions never reach, so this hold is DEFENSIVE — it
	// keeps the agreement of two samples from depending on the wake policy.
	//
	// EnterHolding claims a slot without publishing an instant, so Horizon.Oldest
	// reports zero and no pass frees anything. It is released BEFORE the final
	// assertion, which is the one that needs the watermark to advance.
	hold := g.horizon.EnterHolding()

	const sources = 64
	for i := 0; i < sources; i++ {
		src := "s" + strconv.Itoa(i)
		tx := g.beginLabelTx()
		if err := tx.addEdge(src, "d", 1); err != nil {
			t.Fatalf("addEdge from %s: %v", src, err)
		}
		if _, err := tx.commit(); err != nil {
			t.Fatalf("commit for %s: %v", src, err)
		}
	}

	if got := g.adjVer.len(); got == 0 {
		t.Fatal("no adjacency stamps were recorded at all, so this test cannot observe " +
			"whether they are reclaimed")
	}
	if got, want := g.MVCCStats().AdjConflictStamps, int64(g.adjVer.len()); got != want {
		t.Fatalf("MVCCStats().AdjConflictStamps = %d, want %d — the count must be "+
			"observable or the bound cannot be monitored", got, want)
	}

	// No reader is open, so the watermark advances past every stamp above and the
	// sweep must free all of them.
	g.horizon.Leave(hold)
	g.ReclaimNow()
	if got := g.adjVer.len(); got != 0 {
		t.Fatalf("%d adjacency stamps survived a sweep with no reader open: the map is "+
			"unbounded and grows once per node written, for the life of the process", got)
	}
}
