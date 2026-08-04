package lpg

// mvcc_write_test.go — MVCC P4a (rmp #2288): the shared commit record, and the
// reclamation driver that arming it obliges.
//
// The property under test is the one the whole substrate exists for: a
// transaction becomes visible AT ONE INSTANT, across every store it touched. A
// reader from before it sees none of its labels, none of its properties and
// none of its topology; a reader from after sees all three. There is no reader
// that sees some.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func mvccGraph(t *testing.T) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if !g.MVCCEnabled() {
		t.Fatal("a new graph must have MVCC armed (rmp #2288)")
	}
	return g
}

func mvccNodeID(t *testing.T, g *Graph[string, float64], k string) graph.NodeID {
	t.Helper()
	id, ok := g.adj.Mapper().Lookup(k)
	if !ok {
		t.Fatalf("%s not interned", k)
	}
	return id
}

// TestMVCCWrite_MultiOpStatementIsAtomicallyVisible is the reason the shared
// record exists. Before it, each delta of a multi-op statement took its own
// commit timestamp from the clock, so `CREATE (a)-[:R]->(b)` stamped the label,
// the property and the edge at three different instants and a reader could land
// between them.
func TestMVCCWrite_MultiOpStatementIsAtomicallyVisible(t *testing.T) {
	g := mvccGraph(t)
	// Seed the endpoints in their own transaction so the one under test changes
	// only labels, properties and topology.
	if err := g.ApplyAtomically(func() error {
		if err := g.AddNode("a"); err != nil {
			return err
		}
		return g.AddNode("b")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := mvccNodeID(t, g, "a")

	before := g.readTS()
	// midTS is sampled BETWEEN two writes of the same statement, and that is the
	// whole test. Sampling only either side of the transaction proves nothing:
	// with per-delta timestamps a reader from before it still sees none of the
	// writes and one from after still sees all of them, so a boundary-only
	// assertion passes against the very defect it is supposed to catch. This
	// was verified by reverting the fix and watching the boundary version pass.
	var midTS uint64
	if err := g.ApplyAtomically(func() error {
		if err := g.SetNodeLabel("a", "Person"); err != nil {
			return err
		}
		midTS = g.readTS()
		if err := g.SetNodeProperty("a", "name", StringValue("ada")); err != nil {
			return err
		}
		return g.AddEdge("a", "b", 1)
	}); err != nil {
		t.Fatalf("ApplyAtomically: %v", err)
	}
	after := g.readTS()

	if before == after {
		t.Fatal("the transaction did not advance the clock, so no timestamp separates the two readers")
	}

	// A reader whose start timestamp falls BETWEEN the statement's own writes
	// must still see all of them or none of them. Seeing the label without the
	// edge is the torn state the shared commit record exists to make impossible.
	midLabels := g.labelBagAsOf(id, midTS, 0)
	midProps := g.propBagAsOf(id, midTS, 0)
	sawLabel := midLabels.has(g.reg.Intern("Person"))
	_, sawProp := midProps.get(g.pkeys.Intern("name"))
	sawEdge := len(g.adj.EntryNeighboursAsOf(id, midTS, 0)) > 0
	if sawLabel != sawProp || sawProp != sawEdge {
		t.Fatalf("a reader that started mid-statement sees label=%v property=%v edge=%v — a TORN "+
			"transaction. Each modification took its own commit timestamp instead of the "+
			"statement's shared record (rmp #2288)", sawLabel, sawProp, sawEdge)
	}

	// A reader from BEFORE must see none of the three.
	if bag := g.labelBagAsOf(id, before, 0); bag.has(g.reg.Intern("Person")) {
		t.Error("a reader from before the transaction sees its LABEL")
	}
	if bag := g.propBagAsOf(id, before, 0); func() bool { _, ok := bag.get(g.pkeys.Intern("name")); return ok }() {
		t.Error("a reader from before the transaction sees its PROPERTY")
	}
	if n := len(g.adj.EntryNeighboursAsOf(id, before, 0)); n != 0 {
		t.Errorf("a reader from before the transaction sees %d of its EDGES, want 0", n)
	}

	// A reader from AFTER must see all three.
	if bag := g.labelBagAsOf(id, after, 0); !bag.has(g.reg.Intern("Person")) {
		t.Error("a reader from after the transaction is missing its LABEL")
	}
	if bag := g.propBagAsOf(id, after, 0); func() bool { _, ok := bag.get(g.pkeys.Intern("name")); return ok }() == false {
		t.Error("a reader from after the transaction is missing its PROPERTY")
	}
	if n := len(g.adj.EntryNeighboursAsOf(id, after, 0)); n != 1 {
		t.Errorf("a reader from after the transaction sees %d of its EDGES, want 1", n)
	}
}

// TestMVCCWrite_ExplicitTransactionSharesOneRecord pins that every statement
// applied under one LockBarrier publishes together. Without it a
// multi-statement transaction would become visible statement by statement,
// which is the same defect one level up.
func TestMVCCWrite_ExplicitTransactionSharesOneRecord(t *testing.T) {
	g := mvccGraph(t)
	if err := g.ApplyAtomically(func() error {
		if err := g.AddNode("a"); err != nil {
			return err
		}
		return g.AddNode("b")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := mvccNodeID(t, g, "a")
	before := g.readTS()

	g.LockBarrier()
	_ = g.ApplyInsideLocked(func() error { return g.SetNodeLabel("a", "One") })
	// Mid-transaction: a reader from before must still see nothing, and so must
	// a reader that starts NOW — the transaction has not published.
	midTS := g.readTS()
	if bag := g.labelBagAsOf(id, midTS, 0); bag.has(g.reg.Intern("One")) {
		g.UnlockBarrier()
		t.Fatal("a statement inside an open explicit transaction is already visible: the " +
			"transaction is publishing statement by statement instead of as a whole")
	}
	_ = g.ApplyInsideLocked(func() error { return g.AddEdge("a", "b", 1) })
	g.UnlockBarrier()

	after := g.readTS()
	if bag := g.labelBagAsOf(id, before, 0); bag.has(g.reg.Intern("One")) {
		t.Error("a reader from before the transaction sees its label")
	}
	if bag := g.labelBagAsOf(id, after, 0); !bag.has(g.reg.Intern("One")) {
		t.Error("a reader from after the transaction is missing its label")
	}
	if n := len(g.adj.EntryNeighboursAsOf(id, after, 0)); n != 1 {
		t.Errorf("a reader from after the transaction sees %d edges, want 1", n)
	}
}

// TestMVCCWrite_DirectMutationTakesItsOwnTimestamp pins that a write made
// outside any transaction is committed the instant it is made, which is the Go
// API's documented per-operation contract, and is NOT merged into whatever
// happened before it.
func TestMVCCWrite_DirectMutationTakesItsOwnTimestamp(t *testing.T) {
	g := mvccGraph(t)
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	id := mvccNodeID(t, g, "a")

	before := g.readTS()
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if bag := g.labelBagAsOf(id, before, 0); bag.has(g.reg.Intern("L")) {
		t.Error("a reader from before a direct write sees it")
	}
	if bag := g.labelBagAsOf(id, g.readTS(), 0); !bag.has(g.reg.Intern("L")) {
		t.Error("a reader from after a direct write does not see it")
	}
}

// TestMVCCReclaim_BoundedUnderChurn is the bounded-resources gate. Arming
// versioning without a driver would make every modification leak a record until
// the process exits; this asserts the driver actually runs and that the ceiling
// is the stated one rather than a hope.
func TestMVCCReclaim_BoundedUnderChurn(t *testing.T) {
	g := mvccGraph(t)
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	const churn = reclaimThreshold * 8
	for i := 0; i < churn; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// The INSTANTANEOUS ceiling since rmp #2308: the sweep runs on a background
	// vacuum, so what bounds a burst is the debt at which a committer waits for
	// it, plus one threshold for what was charged while the relieving pass ran.
	if limit := int64(reclaimDebtCeiling + reclaimThreshold); g.VersionCount() > limit {
		t.Fatalf("after %d modifications the substrate holds %d versions, want at most %d: "+
			"reclamation is not being driven and the memory is unbounded",
			churn, g.VersionCount(), limit)
	}
	// And it settles back to the churn bound on its own, with nothing further
	// written and no reclaimer called.
	waitWithinBound(t, g)
	// And it must go to zero when asked, with no reader holding it back.
	if err := g.ApplyAtomically(func() error { g.ReclaimNow(); return nil }); err != nil {
		t.Fatalf("ReclaimNow: %v", err)
	}
	if got := g.VersionCount(); got != 0 {
		t.Fatalf("an explicit sweep with no active reader left %d versions, want 0", got)
	}
}

// TestMVCCReclaim_HeldBackByAnActiveReader pins the other half of the contract:
// a registered reader's start timestamp holds versions alive, because it can
// still reach them. That cost is not a defect — it is the same contract
// PostgreSQL has with a long transaction and VACUUM — but it must be real, or
// reclamation would be freeing versions a reader still needs.
func TestMVCCReclaim_HeldBackByAnActiveReader(t *testing.T) {
	g := mvccGraph(t)
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.ApplyAtomically(func() error {
		return g.SetNodeProperty("a", "w", Int64Value(0))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := mvccNodeID(t, g, "a")

	// A reader pins the present, then the graph moves on.
	startTS := g.readTS()
	slot := g.Horizon().Enter(startTS)

	for i := 1; i <= 16; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := g.ApplyAtomically(func() error { g.ReclaimNow(); return nil }); err != nil {
		t.Fatalf("ReclaimNow: %v", err)
	}
	if got := g.VersionCount(); got == 0 {
		t.Fatal("reclamation freed every version while a reader was registered at an older " +
			"start timestamp: that reader can no longer reconstruct its own snapshot")
	}
	// And the pinned reader still resolves to what it pinned.
	bag := g.propBagAsOf(id, startTS, 0)
	v, ok := bag.get(g.pkeys.Intern("w"))
	if !ok {
		t.Fatal("the pinned reader lost the property entirely")
	}
	if got, _ := v.Int64(); got != 0 {
		t.Fatalf("the pinned reader sees w=%d, want 0 — the version it started on", got)
	}

	// Once it leaves, everything behind it goes.
	g.Horizon().Leave(slot)
	if err := g.ApplyAtomically(func() error { g.ReclaimNow(); return nil }); err != nil {
		t.Fatalf("ReclaimNow: %v", err)
	}
	if got := g.VersionCount(); got != 0 {
		t.Fatalf("after the reader left, %d versions remain, want 0", got)
	}
}

// TestMVCCWrite_TransactionSpanningEveryStoreIsAtomicallyVisible is rmp #2301
// AC2: publishing a transaction is still ONE atomic store after the write state
// moved to per-transaction ownership, across EVERY store a transaction can
// touch.
//
// [TestMVCCWrite_MultiOpStatementIsAtomicallyVisible] proves the property for
// three stores — node labels, node properties and topology — and pins rmp #2288.
// This one adds the two that the per-edge side stores contribute, relationship
// types and edge properties, so the assertion covers all five. A transaction
// that becomes visible in the label store and the edge-property store at
// different instants is torn even if each store is internally consistent, and
// nothing else in the suite would notice.
//
// The mid-statement sample is the whole test, for the reason its sibling
// records: a reader from before the transaction sees none of it and one from
// after sees all of it EVEN IF each store publishes at its own instant, so a
// boundary-only assertion passes against the very defect it exists to catch.
func TestMVCCWrite_TransactionSpanningEveryStoreIsAtomicallyVisible(t *testing.T) {
	g := mvccGraph(t)

	// Seed the endpoints and one edge in their own transaction, so the
	// transaction under test is the only writer of everything it asserts. The
	// handle is what addresses the two per-edge side stores.
	var handle uint64
	if err := g.ApplyAtomically(func() error {
		if err := g.AddNode("a"); err != nil {
			return err
		}
		if err := g.AddNode("b"); err != nil {
			return err
		}
		h, err := g.AddEdgeH("a", "b", 1)
		handle = h
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if handle == 0 {
		t.Fatal("seed produced no edge handle, so the per-edge side stores cannot be addressed")
	}
	id := mvccNodeID(t, g, "a")

	before := g.readTS()

	// One transaction touching all five stores, sampling the clock AFTER EVERY
	// WRITE rather than once in the middle.
	//
	// Sampling once is not enough, and that was measured rather than reasoned:
	// the first version of this test took a single mid-statement sample before
	// the relationship-type write, and it PASSED against a build in which that
	// store deliberately published at its own instant. A reader positioned before
	// a torn store's early commit sees nothing from any store and looks perfectly
	// consistent. Only a sample taken AFTER the torn write, and before the
	// transaction's own commit, can see the tear — so every write gets one.
	var samples []uint64
	sample := func() { samples = append(samples, g.readTS()) }
	if err := g.ApplyAtomically(func() error {
		if err := g.SetNodeLabel("a", "Person"); err != nil { // node labels
			return err
		}
		sample()
		if err := g.SetNodeProperty("a", "name", StringValue("ada")); err != nil { // node properties
			return err
		}
		sample()
		if err := g.AddEdge("a", "c", 2); err != nil { // topology
			return err
		}
		sample()
		g.SetEdgeLabelByHandle("a", "b", handle, "KNOWS") // relationship types
		sample()
		if err := g.SetEdgePropertyByHandle("a", "b", handle, "since", Int64Value(1815)); err != nil {
			return err // edge properties
		}
		sample()
		return nil
	}); err != nil {
		t.Fatalf("ApplyAtomically: %v", err)
	}
	after := g.readTS()
	if before == after {
		t.Fatal("the transaction did not advance the clock, so no timestamp separates the readers")
	}

	// observe reports what a reader at ts sees of each of the five writes.
	observe := func(ts uint64) map[string]bool {
		v := g.ReadAt(&Snapshot{startTS: ts})
		types := v.EdgeLabelsByHandle("a", "b", handle)
		sawType := false
		for _, ty := range types {
			if ty == "KNOWS" {
				sawType = true
			}
		}
		_, sawEdgeProp := v.EdgePropertiesByHandle("a", "b", handle)["since"]
		labels := g.labelBagAsOf(id, ts, 0)
		props := g.propBagAsOf(id, ts, 0)
		_, sawProp := props.get(g.pkeys.Intern("name"))
		return map[string]bool{
			"node label":        labels.has(g.reg.Intern("Person")),
			"node property":     sawProp,
			"topology":          v.HasEdge("a", "c"),
			"relationship type": sawType,
			"edge property":     sawEdgeProp,
		}
	}

	// A reader that started at ANY point inside the statement must see all five
	// writes or none of them.
	for i, ts := range samples {
		got := observe(ts)
		sawAny, sawAll := false, true
		for _, ok := range got {
			sawAny = sawAny || ok
			sawAll = sawAll && ok
		}
		if sawAny && !sawAll {
			t.Fatalf("a reader that started mid-statement (sample %d of %d, ts=%d) sees a "+
				"TORN transaction: %v — the stores published at different instants instead "+
				"of sharing one commit record, so a client can observe a relationship type "+
				"without the edge property set by the same statement",
				i+1, len(samples), ts, got)
		}
	}

	// And the boundaries still hold, which is what makes the mid sample meaningful.
	for what, ok := range observe(before) {
		if ok {
			t.Errorf("a reader from BEFORE the transaction sees its %s", what)
		}
	}
	for what, ok := range observe(after) {
		if !ok {
			t.Errorf("a reader from AFTER the transaction does not see its %s", what)
		}
	}
}
