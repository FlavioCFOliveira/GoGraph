package cypher

// undo_record_parallel_test.go — unit tests for the two undo inverses rmp #2885
// found addressing an endpoint PAIR where they had to address one parallel edge
// INSTANCE. They drive the helpers directly against an lpg graph, below the
// Cypher executor, and assert the adjacency slots — handle, weight and
// per-handle type — that the rollback must restore.
//
// Layer: short.

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// edgeSlot is one adjacency slot as the undo must restore it.
type edgeSlot struct {
	dst    string
	handle uint64
	weight float64
	types  string
}

// slotsFrom returns src's adjacency slots, sorted by handle, with each slot's
// per-handle type set.
func slotsFrom(t *testing.T, g *lpg.Graph[string, float64], src string) []edgeSlot {
	t.Helper()
	id, ok := g.AdjList().Mapper().Lookup(src)
	if !ok {
		t.Fatalf("node %q is not interned", src)
	}
	nbs, weights, handles := g.AdjList().LoadEntryH(id)
	out := make([]edgeSlot, 0, len(nbs))
	for i, nb := range nbs {
		dst, _ := g.AdjList().Mapper().Resolve(nb)
		s := edgeSlot{dst: dst, weight: weights[i]}
		if handles != nil {
			s.handle = handles[i]
		}
		lbls := g.EdgeLabelsByHandle(src, dst, s.handle)
		slices.Sort(lbls)
		for _, l := range lbls {
			s.types += l + ","
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b edgeSlot) int {
		if a.handle < b.handle {
			return -1
		}
		if a.handle > b.handle {
			return 1
		}
		return 0
	})
	return out
}

// addTyped appends a typed parallel instance src→dst and returns its handle.
func addTyped(t *testing.T, g *lpg.Graph[string, float64], src, dst string, w float64, typ string) uint64 {
	t.Helper()
	h, err := g.AddEdgeH(src, dst, w)
	if err != nil {
		t.Fatalf("AddEdgeH(%s,%s): %v", src, dst, err)
	}
	if err := g.SetEdgeLabel(src, dst, typ); err != nil {
		t.Fatal(err)
	}
	if err := g.SetEdgeLabelByHandle(src, dst, h, typ); err != nil {
		t.Fatal(err)
	}
	return h
}

// nopRelCounter satisfies relDeleteCounter for the bulk journal.
type nopRelCounter struct{ n int }

func (c *nopRelCounter) countRelDeleted() { c.n++ }

// TestUndo_BulkOutEdgeRemoval_RestoresEveryParallelInstance_2885: the inverse of
// a bulk out-edge removal must re-add EVERY parallel slot under its own handle.
// Before the fix every occurrence of b was captured under the first slot's
// handle, so the replay re-added one instance to b and dropped the other two.
func TestUndo_BulkOutEdgeRemoval_RestoresEveryParallelInstance_2885(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	addTyped(t, g, "a", "b", 1, "T")
	addTyped(t, g, "a", "b", 2, "U")
	addTyped(t, g, "a", "c", 3, "T")
	addTyped(t, g, "a", "b", 4, "U")
	addTyped(t, g, "a", "a", 5, "L") // self-loop
	want := slotsFrom(t, g, "a")

	u := &undoLog{}
	m := mutationUndo{wv: g.Writer(lpg.WriteTx{}), undo: u}
	var outgoing []string
	for nb := range g.AdjList().Neighbours("a") {
		outgoing = append(outgoing, nb)
	}
	pre := captureAllOutEdgePreimages(m, g, "a", outgoing)
	if !m.wv.RemoveAllEdgesFrom("a") {
		t.Fatal("RemoveAllEdgesFrom was not applied")
	}
	var c nopRelCounter
	journalAllOutEdgesRemoved(m, &c, pre)
	if c.n != len(want) {
		t.Fatalf("journalled %d removals, want %d", c.n, len(want))
	}
	if !u.replay() {
		t.Fatal("undo replay failed")
	}
	if got := slotsFrom(t, g, "a"); !slices.Equal(got, want) {
		t.Fatalf("slots after rollback:\n got  %+v\n want %+v", got, want)
	}
}

// TestUndo_AddEdge_RemovesTheCreatedInstance_2885: the inverse of appending a
// parallel instance must retire THAT instance. Before the fix it removed the
// pair's first slot — the committed sibling — and kept the appended one, so the
// surviving edge carried the rolled-back type.
func TestUndo_AddEdge_RemovesTheCreatedInstance_2885(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	addTyped(t, g, "a", "b", 1, "T")
	want := slotsFrom(t, g, "a")

	u := &undoLog{}
	m := mutationUndo{wv: g.Writer(lpg.WriteTx{}), undo: u}
	h, err := m.wv.AddEdgeH("a", "b", 2)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	m.recordAddEdge("a", "b", h, false, false)
	if err := m.wv.SetEdgeLabelByHandle("a", "b", h, "V"); err != nil {
		t.Fatal(err)
	}
	if !u.replay() {
		t.Fatal("undo replay failed")
	}
	if got := slotsFrom(t, g, "a"); !slices.Equal(got, want) {
		t.Fatalf("slots after rollback:\n got  %+v\n want %+v", got, want)
	}
	if lbls := g.EdgeLabelsByHandle("a", "b", h); len(lbls) != 0 {
		t.Fatalf("rolled-back instance %d still carries per-handle labels %v", h, lbls)
	}
}
