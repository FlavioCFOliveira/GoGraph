package lpg

// mvcc_edge_side_test.go — MVCC P3c (rmp #2291): the five per-edge side stores
// are versioned.
//
// Each store gets the same three questions, because a version chain can be
// wrong in three different ways and passing one of them proves nothing about
// the others:
//
//  1. ADDITION — a reader from before it must not see it;
//  2. REMOVAL — a reader from before it must still see what was removed, which
//     is the direction a missing pre-image silently loses;
//  3. ATOMICITY — a reader whose start timestamp falls BETWEEN two writes of
//     one statement sees all of them or none. Sampling only either side of the
//     transaction cannot see a torn one: that mistake was made in P4a's first
//     multi-op test and the test passed against the very defect it was written
//     for.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// sideGraph builds a two-node graph with one edge and returns it with the
// endpoint ids, so each store's test starts from a pair that exists.
func sideGraph(t *testing.T) (*Graph[string, float64], graph.NodeID, graph.NodeID) {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.ApplyAtomically(func() error {
		if err := g.AddNode("a"); err != nil {
			return err
		}
		if err := g.AddNode("b"); err != nil {
			return err
		}
		return g.AddEdge("a", "b", 1)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return g, mvccNodeID(t, g, "a"), mvccNodeID(t, g, "b")
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestEdgeOverflowVersion_AddAndRemove pins the overflow store: a pair's second
// and later relationship types.
//
// The pair is given TWO types, so the second one has nowhere to go but
// overflow — that is the state this store exists for, and asserting it is what
// stops the test quietly exercising the adjacency slot column instead.
func TestEdgeOverflowVersion_AddAndRemove(t *testing.T) {
	g, srcID, dstID := sideGraph(t)
	// A reader must actually EXIST for the past this test reads through snapAt
	// to be retained; see pinHorizon.
	pinHorizon(t, g)
	if err := g.ApplyAtomically(func() error { g.SetEdgeLabel("a", "b", "KNOWS"); return nil }); err != nil {
		t.Fatalf("first type: %v", err)
	}

	beforeSecond := g.readTS()
	if err := g.ApplyAtomically(func() error { g.SetEdgeLabel("a", "b", "LIKES"); return nil }); err != nil {
		t.Fatalf("second type: %v", err)
	}
	afterSecond := g.readTS()

	if g.edgeLabelOverflowActive.Load() == 0 {
		t.Fatal("the second relationship type did not spill to overflow, so this test is not " +
			"exercising the store it is named for")
	}

	if hasName(g.EdgeLabelsByIDAsOf(srcID, dstID, snapAt(beforeSecond)), "LIKES") {
		t.Error("a reader from before the second type was added can see it")
	}
	if !hasName(g.EdgeLabelsByIDAsOf(srcID, dstID, snapAt(afterSecond)), "LIKES") {
		t.Error("a reader from after the second type was added cannot see it")
	}

	// REMOVAL is the direction a missing pre-image silently loses.
	beforeRemove := g.readTS()
	if err := g.ApplyAtomically(func() error { g.RemoveEdgeLabel("a", "b", "LIKES"); return nil }); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !hasName(g.EdgeLabelsByIDAsOf(srcID, dstID, snapAt(beforeRemove)), "LIKES") {
		t.Error("a reader from before the removal has lost the type: the pre-image was not retained")
	}
	if hasName(g.EdgeLabelsByIDAsOf(srcID, dstID, snapAt(g.readTS())), "LIKES") {
		t.Error("a reader from after the removal still sees the removed type")
	}
}

// TestEdgeHandleVersion_LabelsAndProperties pins the two per-handle stores, and
// pins them TOGETHER in one transaction so the atomicity question is asked of
// the pair rather than of each in isolation.
func TestEdgeHandleVersion_LabelsAndProperties(t *testing.T) {
	g, srcID, dstID := sideGraph(t)
	// A reader must actually EXIST for the past this test reads through snapAt
	// to be retained; see pinHorizon.
	pinHorizon(t, g)
	const handle = uint64(7)

	before := g.readTS()
	var mid uint64
	if err := g.ApplyAtomically(func() error {
		g.SetEdgeLabelByHandle("a", "b", handle, "KNOWS")
		mid = g.readTS()
		return g.SetEdgePropertyByHandle("a", "b", handle, "since", Int64Value(2020))
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	after := g.readTS()

	// ATOMICITY, sampled INSIDE the statement.
	sawLabel := len(g.EdgeLabelsByHandleIDAsOf(srcID, dstID, handle, snapAt(mid))) > 0
	sawProp := len(g.EdgePropertiesByHandleIDAsOf(srcID, dstID, handle, snapAt(mid))) > 0
	if sawLabel != sawProp {
		t.Fatalf("a reader that started mid-statement sees label=%v property=%v — a TORN "+
			"transaction across the two per-handle stores", sawLabel, sawProp)
	}

	if len(g.EdgeLabelsByHandleIDAsOf(srcID, dstID, handle, snapAt(before))) != 0 {
		t.Error("a reader from before sees the per-handle type")
	}
	if len(g.EdgePropertiesByHandleIDAsOf(srcID, dstID, handle, snapAt(before))) != 0 {
		t.Error("a reader from before sees the per-handle properties")
	}
	if !hasName(g.EdgeLabelsByHandleIDAsOf(srcID, dstID, handle, snapAt(after)), "KNOWS") {
		t.Error("a reader from after is missing the per-handle type")
	}
	if _, ok := g.EdgePropertiesByHandleIDAsOf(srcID, dstID, handle, snapAt(after))["since"]; !ok {
		t.Error("a reader from after is missing the per-handle property")
	}

	// REMOVAL.
	beforeRemove := g.readTS()
	if err := g.ApplyAtomically(func() error {
		g.RemoveEdgeInstanceByHandle("a", "b", handle)
		return nil
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !hasName(g.EdgeLabelsByHandleIDAsOf(srcID, dstID, handle, snapAt(beforeRemove)), "KNOWS") {
		t.Error("a reader from before the instance was removed has lost its type")
	}
	if _, ok := g.EdgePropertiesByHandleIDAsOf(srcID, dstID, handle, snapAt(beforeRemove))["since"]; !ok {
		t.Error("a reader from before the instance was removed has lost its properties")
	}
	if len(g.EdgeLabelsByHandleIDAsOf(srcID, dstID, handle, snapAt(g.readTS()))) != 0 {
		t.Error("a reader from after the removal still sees the instance's type")
	}
}

// TestEdgeInstanceVersion_LabelsAndProperties pins the two by-ordinal stores.
func TestEdgeInstanceVersion_LabelsAndProperties(t *testing.T) {
	g, _, _ := sideGraph(t)
	// A reader must actually EXIST for the past this test reads through snapAt
	// to be retained; see pinHorizon.
	pinHorizon(t, g)
	const idx = int64(1)

	before := g.readTS()
	var mid uint64
	if err := g.ApplyAtomically(func() error {
		g.SetEdgeLabelAt("a", "b", idx, "KNOWS")
		mid = g.readTS()
		return g.SetEdgePropertyAt("a", "b", idx, "since", Int64Value(2020))
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	after := g.readTS()

	sawLabel := len(g.EdgeLabelsAtAsOf("a", "b", idx, snapAt(mid))) > 0
	sawProp := len(g.EdgePropertiesAtAsOf("a", "b", idx, snapAt(mid))) > 0
	if sawLabel != sawProp {
		t.Fatalf("a reader that started mid-statement sees label=%v property=%v — a TORN "+
			"transaction across the two per-instance stores", sawLabel, sawProp)
	}

	if len(g.EdgeLabelsAtAsOf("a", "b", idx, snapAt(before))) != 0 {
		t.Error("a reader from before sees the per-instance type")
	}
	if !hasName(g.EdgeLabelsAtAsOf("a", "b", idx, snapAt(after)), "KNOWS") {
		t.Error("a reader from after is missing the per-instance type")
	}
	if _, ok := g.EdgePropertiesAtAsOf("a", "b", idx, snapAt(after))["since"]; !ok {
		t.Error("a reader from after is missing the per-instance property")
	}

	beforeRemove := g.readTS()
	if err := g.ApplyAtomically(func() error { g.RemoveEdgeInstance("a", "b", idx); return nil }); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !hasName(g.EdgeLabelsAtAsOf("a", "b", idx, snapAt(beforeRemove)), "KNOWS") {
		t.Error("a reader from before the instance was removed has lost its type")
	}
	if _, ok := g.EdgePropertiesAtAsOf("a", "b", idx, snapAt(beforeRemove))["since"]; !ok {
		t.Error("a reader from before the instance was removed has lost its properties")
	}
	if len(g.EdgeLabelsAtAsOf("a", "b", idx, snapAt(g.readTS()))) != 0 {
		t.Error("a reader from after the removal still sees the instance's type")
	}
}

// TestEdgeSideVersion_SurvivesWholePairDrop pins the path clearEdgePairState
// takes: the last edge between two endpoints goes, and the whole pair's
// per-instance metadata is dropped in one map delete. A reader from before
// must still see all of it, which requires a pre-image per instance rather
// than one for the pair.
func TestEdgeSideVersion_SurvivesWholePairDrop(t *testing.T) {
	g, srcID, dstID := sideGraph(t)
	// A reader must actually EXIST for the past this test reads through snapAt
	// to be retained; see pinHorizon.
	pinHorizon(t, g)
	const h1, h2 = uint64(11), uint64(12)
	if err := g.ApplyAtomically(func() error {
		g.SetEdgeLabelByHandle("a", "b", h1, "KNOWS")
		g.SetEdgeLabelByHandle("a", "b", h2, "LIKES")
		return nil
	}); err != nil {
		t.Fatalf("seed instances: %v", err)
	}
	before := g.readTS()

	if err := g.ApplyAtomically(func() error { g.RemoveEdge("a", "b"); return nil }); err != nil {
		t.Fatalf("RemoveEdge: %v", err)
	}

	if !hasName(g.EdgeLabelsByHandleIDAsOf(srcID, dstID, h1, snapAt(before)), "KNOWS") {
		t.Error("dropping the pair lost instance h1's type for a reader from before it")
	}
	if !hasName(g.EdgeLabelsByHandleIDAsOf(srcID, dstID, h2, snapAt(before)), "LIKES") {
		t.Error("dropping the pair lost instance h2's type for a reader from before it")
	}
	if len(g.EdgeLabelsByHandleIDAsOf(srcID, dstID, h1, snapAt(g.readTS()))) != 0 {
		t.Error("a reader from after the pair was dropped still sees instance h1's type")
	}
}

// TestEdgeSideVersion_ReclaimReturnsToZero pins that the new chains are swept
// by the SAME pass as the rest, rather than accumulating behind a reclaimer
// that does not know about them.
func TestEdgeSideVersion_ReclaimReturnsToZero(t *testing.T) {
	g, _, _ := sideGraph(t)
	for i := 0; i < 64; i++ {
		if err := g.ApplyAtomically(func() error {
			g.SetEdgeLabelByHandle("a", "b", uint64(i+1), "T")
			_ = g.SetEdgePropertyByHandle("a", "b", uint64(i+1), "w", Int64Value(int64(i)))
			g.SetEdgeLabelAt("a", "b", int64(i+1), "T")
			return g.SetEdgePropertyAt("a", "b", int64(i+1), "w", Int64Value(int64(i)))
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if g.EdgeSideVersionCount() == 0 {
		t.Fatal("64 per-instance writes recorded no versions at all: the stores are still unversioned")
	}
	if err := g.ApplyAtomically(func() error { g.ReclaimNow(); return nil }); err != nil {
		t.Fatalf("ReclaimNow: %v", err)
	}
	if n := g.EdgeSideVersionCount(); n != 0 {
		t.Fatalf("after a sweep with no active reader, %d per-edge side versions remain, want 0", n)
	}
	if n := g.VersionCount(); n != 0 {
		t.Fatalf("after a sweep with no active reader, %d versions remain in total, want 0", n)
	}
}

// snapAt builds a read view pinned to an explicit instant, without registering
// it with the horizon.
//
// Registering would be wrong here: these tests deliberately reclaim while
// holding timestamps, and a registered reader would hold the watermark back and
// mask exactly the reclamation behaviour under test. Tests that need the
// horizon use [Graph.BeginRead].
// snapAt fabricates a read view at ts.
//
// It carries a start timestamp and NO HORIZON SLOT, so reclamation neither knows
// about it nor owes it anything: the versions it wants to see are, as far as the
// watermark is concerned, unreachable. Pair every use with [pinHorizon]; see there.
func snapAt(ts uint64) *Snapshot { return &Snapshot{startTS: ts} }

// pinHorizon registers a real reader at the current instant for the rest of the
// test, so nothing written after this point can be reclaimed while the test is
// still looking at the past through [snapAt].
//
// # Why it became necessary at rmp #2308
//
// A synthetic snapshot is not a reader. Until the background vacuum existed, that
// was harmless in a short test: the commit-path sweep only ran once the debt
// passed [reclaimThreshold], which a test making a dozen writes never reached, so
// nothing swept and the fabrication went unnoticed. The vacuum sweeps whenever the
// watermark ADVANCES — and with no reader registered the watermark is simply the
// clock, so it advances at the end of every write transaction and the pre-images
// go immediately. Both TestEdgeOverflowVersion_AddAndRemove and
// TestLabelIndex_RemovalIsDeferredAndVisibleToOlderReaders failed exactly that
// way, reporting that an older reader could see a newer write.
//
// Reclamation was RIGHT in both cases, which is why the fix belongs in the test:
// a test that wants the past retained has to own a reader that wants it.
func pinHorizon[N comparable, W any](t *testing.T, g *Graph[N, W]) {
	t.Helper()
	s := g.BeginRead()
	t.Cleanup(func() { g.EndRead(s) })
}
