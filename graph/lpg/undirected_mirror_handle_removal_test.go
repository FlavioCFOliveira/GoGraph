package lpg

// undirected_mirror_handle_removal_test.go — rmp #2887.
//
// On an undirected multigraph the per-handle label and property stores are
// keyed by the DIRECTED pair the metadata was written under, while the
// adjacency removes both mirror slots of a relationship whichever direction the
// removal names. RemoveEdgeByHandle with siblings remaining clears the removed
// handle's records through removeEdgeInstanceByHandleInfo(src, dst, handle),
// under the direction the CALL names only. The question is whether a removal
// named through the mirror (dst, src) leaves the creation-direction (src, dst)
// record of the removed handle in place.
//
// Each case builds two parallel undirected relationships a–b, records a label
// and a property for each under the creation direction (a, b), removes the
// second through the mirror (b, a), and then inspects the removed handle's
// records in BOTH directions. The committed state must hold no record for a
// handle that no longer exists in the adjacency, and the sibling must keep its
// own.
//
// A ROLLED-BACK mirror removal must leave no trace either: the removal takes a
// version on each key it clears, so the aborted transaction's clear is
// withdrawn and the removed handle's creation-direction records return intact,
// while the mirror key still holds none.
//
// Layer: short. goleak-clean (graphs are local).

import (
	"errors"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// mirrorHandleGraph builds the two parallel undirected relationships a–b the
// cases share, each with a label and a property recorded under the creation
// direction (a, b).
func mirrorHandleGraph(t *testing.T) (g *Graph[string, float64], h1, h2 uint64) {
	t.Helper()
	g = New[string, float64](adjlist.Config{Directed: false, Multigraph: true})
	var err error
	if h1, err = g.AddEdgeH("a", "b", 0); err != nil {
		t.Fatalf("AddEdgeH h1: %v", err)
	}
	if h2, err = g.AddEdgeH("a", "b", 0); err != nil {
		t.Fatalf("AddEdgeH h2: %v", err)
	}
	g.SetEdgeLabelByHandle("a", "b", h1, "T1")
	g.SetEdgeLabelByHandle("a", "b", h2, "T2")
	if err := g.SetEdgePropertyByHandle("a", "b", h1, "k", Int64Value(1)); err != nil {
		t.Fatalf("SetEdgePropertyByHandle h1: %v", err)
	}
	if err := g.SetEdgePropertyByHandle("a", "b", h2, "k", Int64Value(2)); err != nil {
		t.Fatalf("SetEdgePropertyByHandle h2: %v", err)
	}
	return g, h1, h2
}

// assertHandleRecords checks, through read view rv, that handle h carries
// exactly wantLabel and wantK under (a, b) — or nothing when wantLabel is empty
// — and nothing at all under the mirror (b, a).
func assertHandleRecords(t *testing.T, rv *ReadView[string, float64], name string, h uint64, wantLabel string, wantK int64) {
	t.Helper()
	labels := rv.EdgeLabelsByHandle("a", "b", h)
	v, present := rv.EdgePropertyByHandle("a", "b", h, "k")
	if wantLabel == "" {
		if len(labels) != 0 || present {
			t.Errorf("%s: handle %d still has labels %v / property k=%v under a->b, want none", name, h, labels, v)
		}
	} else if !slices.Equal(labels, []string{wantLabel}) || !present || v != Int64Value(wantK) {
		t.Errorf("%s: handle %d under a->b has labels %v and k=%v (present %v), want [%s] and %d",
			name, h, labels, v, present, wantLabel, wantK)
	}
	if got := rv.EdgeLabelsByHandle("b", "a", h); len(got) != 0 {
		t.Errorf("%s: handle %d has labels %v under the mirror b->a, want none", name, h, got)
	}
	if got := rv.EdgePropertiesByHandle("b", "a", h); len(got) != 0 {
		t.Errorf("%s: handle %d has properties %v under the mirror b->a, want none", name, h, got)
	}
}

type mirrorRemoval struct {
	name   string
	remove func(g *Graph[string, float64], h uint64) bool
}

func mirrorRemovals() []mirrorRemoval {
	return []mirrorRemoval{
		{"autocommit", func(g *Graph[string, float64], h uint64) bool {
			return g.RemoveEdgeByHandle("b", "a", h)
		}},
		{"write transaction", func(g *Graph[string, float64], h uint64) bool {
			var ok bool
			if err := g.ApplyAtomicallyTx(func(tx WriteTx) error {
				ok = g.Writer(tx).RemoveEdgeByHandle("b", "a", h)
				return nil
			}); err != nil {
				return false
			}
			return ok
		}},
	}
}

func TestUndirectedMirrorHandleRemoval_LeavesNoRecordForTheRemovedHandle(t *testing.T) {
	t.Parallel()
	for _, rm := range mirrorRemovals() {
		t.Run(rm.name, func(t *testing.T) {
			t.Parallel()
			g, h1, h2 := mirrorHandleGraph(t)

			if !rm.remove(g, h2) {
				t.Fatal("RemoveEdgeByHandle(b, a, h2) returned false, want true")
			}

			// Premise: the adjacency no longer holds h2 in either direction, and
			// the sibling h1 survives in both.
			for _, dir := range [][2]string{{"a", "b"}, {"b", "a"}} {
				id, _ := g.AdjList().Mapper().Lookup(dir[0])
				_, _, handles := g.AdjList().LoadEntryH(id)
				if len(handles) != 1 || handles[0] != h1 {
					t.Fatalf("premise: adjacency %s->%s handles=%v, want only h1=%d", dir[0], dir[1], handles, h1)
				}
			}

			for _, dir := range [][2]string{{"a", "b"}, {"b", "a"}} {
				if got := g.EdgeLabelsByHandle(dir[0], dir[1], h2); len(got) != 0 {
					t.Errorf("removed handle h2 still has labels %v under %s->%s", got, dir[0], dir[1])
				}
				if got := g.EdgePropertiesByHandle(dir[0], dir[1], h2); len(got) != 0 {
					t.Errorf("removed handle h2 still has properties %v under %s->%s", got, dir[0], dir[1])
				}
			}
			if got := g.EdgeLabelsByHandle("a", "b", h1); len(got) != 1 || got[0] != "T1" {
				t.Errorf("sibling h1 labels under a->b = %v, want [T1]", got)
			}
			if v, ok := g.EdgePropertyByHandle("a", "b", h1, "k"); !ok || v != Int64Value(1) {
				t.Errorf("sibling h1 property k under a->b = %v (present %v), want 1", v, ok)
			}
		})
	}
}

// TestUndirectedMirrorHandleRemoval_RollbackRestoresTheRemovedHandle is the
// rollback case: a write transaction removes h2 through the mirror and then
// aborts. Neither a snapshot taken afterwards nor a present-time read may
// observe the aborted clear, and the mirror key still holds nothing.
func TestUndirectedMirrorHandleRemoval_RollbackRestoresTheRemovedHandle(t *testing.T) {
	t.Parallel()
	g, h1, h2 := mirrorHandleGraph(t)
	defer func() { _ = g.Close() }()

	removed := false
	err := g.ApplyVersioned(func(tx WriteTx) error {
		removed = g.Writer(tx).RemoveEdgeByHandle("b", "a", h2)
		// Roll the transaction back exactly as a lost conflict does: doom it.
		if e := tx.w.conflictErr("adjacency", ^uint64(0)); e == nil {
			t.Fatal("conflictErr returned nil; the transaction was not doomed")
		}
		return tx.w.err()
	})
	if !removed {
		t.Fatal("premise: RemoveEdgeByHandle(b, a, h2) inside the transaction returned false")
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("premise: the doomed transaction returned %v, want a serialization conflict", err)
	}

	snap := g.BeginRead()
	defer g.EndRead(snap)
	rv := g.ReadAt(snap)
	for _, dir := range [][2]string{{"a", "b"}, {"b", "a"}} {
		id, _ := g.AdjList().Mapper().Lookup(dir[0])
		handles := rv.EntryView(id).Handles
		if !slices.Contains(handles, h1) || !slices.Contains(handles, h2) {
			t.Errorf("after the rollback the adjacency %s->%s holds handles %v, want both h1=%d and h2=%d",
				dir[0], dir[1], handles, h1, h2)
		}
	}
	assertHandleRecords(t, rv, "snapshot after rollback", h1, "T1", 1)
	assertHandleRecords(t, rv, "snapshot after rollback", h2, "T2", 2)
	present := g.ReadAt(nil)
	assertHandleRecords(t, present, "present after rollback", h1, "T1", 1)
	assertHandleRecords(t, present, "present after rollback", h2, "T2", 2)
}
