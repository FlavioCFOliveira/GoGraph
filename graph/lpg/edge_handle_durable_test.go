package lpg

// edge_handle_durable_test.go — unit coverage for the Stage 2/3 durability
// surface (HasEdgeHandle / AddEdgeHIfAbsent / SeedEdgeHandle /
// WalkEdgeHandles and the NodeID-keyed per-handle accessors).
//
// Layer: short. goleak-clean (graphs are local).

import (
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func newDurableGraph(t *testing.T) *Graph[string, float64] {
	t.Helper()
	return New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
}

// TestHasEdgeHandle_PresentAbsent confirms HasEdgeHandle reports true only
// for a handle actually stamped on the (src, dst) pair, and false for the
// no-handle sentinel, an unknown endpoint, and a never-stored handle.
func TestHasEdgeHandle_PresentAbsent(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	h, err := g.AddEdgeH("a", "b", 1)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	if !g.HasEdgeHandle("a", "b", h) {
		t.Fatalf("HasEdgeHandle(a,b,%d) = false, want true", h)
	}
	if g.HasEdgeHandle("a", "b", 0) {
		t.Fatal("HasEdgeHandle with 0 sentinel = true, want false")
	}
	if g.HasEdgeHandle("a", "b", h+1000) {
		t.Fatal("HasEdgeHandle for never-stored handle = true, want false")
	}
	if g.HasEdgeHandle("a", "zzz", h) {
		t.Fatal("HasEdgeHandle with unknown dst = true, want false")
	}
	if g.HasEdgeHandle("zzz", "b", h) {
		t.Fatal("HasEdgeHandle with unknown src = true, want false")
	}
}

// TestAddEdgeHIfAbsent_Idempotent confirms a second AddEdgeHIfAbsent with the
// same handle is a no-op (inserted=false) and does not create a parallel
// duplicate — the snapshot+full-WAL replay idempotency contract.
func TestAddEdgeHIfAbsent_Idempotent(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	const h uint64 = 42
	ins, err := g.AddEdgeHIfAbsent("a", "b", 1, h)
	if err != nil {
		t.Fatalf("first AddEdgeHIfAbsent: %v", err)
	}
	if !ins {
		t.Fatal("first AddEdgeHIfAbsent inserted=false, want true")
	}
	ins, err = g.AddEdgeHIfAbsent("a", "b", 1, h)
	if err != nil {
		t.Fatalf("second AddEdgeHIfAbsent: %v", err)
	}
	if ins {
		t.Fatal("second AddEdgeHIfAbsent inserted=true, want false (idempotent)")
	}
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	nbs, _, _ := g.AdjList().LoadEntryH(srcID)
	if len(nbs) != 1 {
		t.Fatalf("neighbour count = %d, want 1 (no duplicate)", len(nbs))
	}
}

// TestAddEdgeHIfAbsent_DistinctHandlesParallel confirms two distinct handles
// on the same ordered pair produce two parallel edges (multigraph), each
// resolvable by its own handle.
func TestAddEdgeHIfAbsent_DistinctHandlesParallel(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	if _, err := g.AddEdgeHIfAbsent("a", "b", 1, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddEdgeHIfAbsent("a", "b", 1, 11); err != nil {
		t.Fatal(err)
	}
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	nbs, _, handles := g.AdjList().LoadEntryH(srcID)
	if len(nbs) != 2 {
		t.Fatalf("neighbour count = %d, want 2 parallel edges", len(nbs))
	}
	if !g.HasEdgeHandle("a", "b", 10) || !g.HasEdgeHandle("a", "b", 11) {
		t.Fatalf("both handles must be present; handles=%v", handles)
	}
}

// TestAddEdgeHIfAbsent_ZeroHandleFallsBack confirms a 0 handle falls back to a
// plain handle-less AddEdge so pre-Stage-2 frames still replay.
func TestAddEdgeHIfAbsent_ZeroHandleFallsBack(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	ins, err := g.AddEdgeHIfAbsent("a", "b", 1, 0)
	if err != nil {
		t.Fatalf("AddEdgeHIfAbsent zero: %v", err)
	}
	if !ins {
		t.Fatal("inserted=false, want true")
	}
	if !g.AdjList().HasEdge("a", "b") {
		t.Fatal("edge not present after zero-handle insert")
	}
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	_, _, handles := g.AdjList().LoadEntryH(srcID)
	if handles != nil {
		t.Fatalf("zero-handle insert left a handle column: %v", handles)
	}
}

// TestSeedEdgeHandle_Monotone confirms SeedEdgeHandle raises the counter so
// the next minted handle is >= next, and never rewinds it.
func TestSeedEdgeHandle_Monotone(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	g.SeedEdgeHandle(100)
	got := g.NextEdgeHandle()
	if got < 100 {
		t.Fatalf("after SeedEdgeHandle(100), NextEdgeHandle = %d, want >= 100", got)
	}
	// A stale seed must not rewind the counter.
	g.SeedEdgeHandle(5)
	got2 := g.NextEdgeHandle()
	if got2 <= got {
		t.Fatalf("stale SeedEdgeHandle rewound the counter: %d then %d", got, got2)
	}
	// Seeding with 0 is a no-op.
	before := g.NextEdgeHandle()
	g.SeedEdgeHandle(0)
	after := g.NextEdgeHandle()
	if after <= before {
		t.Fatalf("SeedEdgeHandle(0) disturbed the counter: %d then %d", before, after)
	}
}

// TestWalkEdgeHandles_EnumeratesLiveHandles confirms WalkEdgeHandles yields
// every live non-zero-handle slot and skips handle-less slots.
func TestWalkEdgeHandles_EnumeratesLiveHandles(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	h1, _ := g.AddEdgeH("a", "b", 1)
	h2, _ := g.AddEdgeH("a", "b", 1) // parallel
	h3, _ := g.AddEdgeH("c", "d", 1)
	// A handle-less edge must NOT appear in the walk.
	if err := g.AddEdge("e", "f", 1); err != nil {
		t.Fatal(err)
	}

	got := map[uint64]struct{}{}
	g.WalkEdgeHandles(func(tr EdgeHandleTriple) bool {
		got[tr.Handle] = struct{}{}
		return true
	})
	for _, h := range []uint64{h1, h2, h3} {
		if _, ok := got[h]; !ok {
			t.Fatalf("WalkEdgeHandles missed handle %d (got %v)", h, got)
		}
	}
	if _, ok := got[0]; ok {
		t.Fatal("WalkEdgeHandles yielded a 0 handle")
	}
	if len(got) != 3 {
		t.Fatalf("WalkEdgeHandles yielded %d distinct handles, want 3", len(got))
	}
}

// TestWalkEdgeHandles_EarlyStop confirms the walk halts when fn returns false.
func TestWalkEdgeHandles_EarlyStop(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	for i := 0; i < 5; i++ {
		if _, err := g.AddEdgeH("a", "b", 1); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	g.WalkEdgeHandles(func(EdgeHandleTriple) bool {
		count++
		return count < 2
	})
	if count != 2 {
		t.Fatalf("WalkEdgeHandles visited %d, want 2 (early stop)", count)
	}
}

// TestByHandleID_RoundTrip confirms the NodeID-keyed setters/getters agree
// with the natural-key variants on the same handle.
func TestByHandleID_RoundTrip(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	h, _ := g.AddEdgeH("a", "b", 1)
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	dstID, _ := g.AdjList().Mapper().Lookup("b")

	g.SetEdgeLabelByHandleID(srcID, dstID, h, "KNOWS")
	g.SetEdgePropertyByHandleID(srcID, dstID, h, "since", Int64Value(2020))

	gotLabels := g.EdgeLabelsByHandleID(srcID, dstID, h)
	if len(gotLabels) != 1 || gotLabels[0] != "KNOWS" {
		t.Fatalf("EdgeLabelsByHandleID = %v, want [KNOWS]", gotLabels)
	}
	// The natural-key reader must see the same record.
	natLabels := g.EdgeLabelsByHandle("a", "b", h)
	sort.Strings(natLabels)
	if len(natLabels) != 1 || natLabels[0] != "KNOWS" {
		t.Fatalf("EdgeLabelsByHandle = %v, want [KNOWS]", natLabels)
	}
	props := g.EdgePropertiesByHandleID(srcID, dstID, h)
	v, ok := props["since"]
	if !ok {
		t.Fatalf("EdgePropertiesByHandleID missing 'since': %v", props)
	}
	if i, _ := v.Int64(); i != 2020 {
		t.Fatalf("'since' = %v, want 2020", v)
	}
}

// TestByHandleID_ZeroHandleNoop confirms the NodeID-keyed setters/getters are
// no-ops for the 0 sentinel.
func TestByHandleID_ZeroHandleNoop(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatal(err)
	}
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	dstID, _ := g.AdjList().Mapper().Lookup("b")
	g.SetEdgeLabelByHandleID(srcID, dstID, 0, "X")
	if l := g.EdgeLabelsByHandleID(srcID, dstID, 0); l != nil {
		t.Fatalf("EdgeLabelsByHandleID(0) = %v, want nil", l)
	}
	g.SetEdgePropertyByHandleID(srcID, dstID, 0, "k", Int64Value(1))
	if p := g.EdgePropertiesByHandleID(srcID, dstID, 0); p != nil {
		t.Fatalf("EdgePropertiesByHandleID(0) = %v, want nil", p)
	}
}

// TestWalkEdgeHandles_NodeIDOrdering confirms the triples reference resolvable
// NodeIDs (sanity check that the walk yields valid endpoints).
func TestWalkEdgeHandles_NodeIDOrdering(t *testing.T) {
	t.Parallel()
	g := newDurableGraph(t)
	if _, err := g.AddEdgeH("a", "b", 1); err != nil {
		t.Fatal(err)
	}
	g.WalkEdgeHandles(func(tr EdgeHandleTriple) bool {
		if _, ok := g.AdjList().Mapper().Resolve(graph.NodeID(tr.Src)); !ok {
			t.Fatalf("walk yielded unresolvable src %d", tr.Src)
		}
		if _, ok := g.AdjList().Mapper().Resolve(graph.NodeID(tr.Dst)); !ok {
			t.Fatalf("walk yielded unresolvable dst %d", tr.Dst)
		}
		return true
	})
}
