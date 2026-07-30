package adjlist

// label_slot_allslots_test.go — the ALL-SLOTS label primitives added for rmp
// #2258: [AdjList.SetEdgeLabelSlotsAt] and [AdjList.ClearEdgeLabelSlotsValue].
//
// The pre-existing primitives are first-match-only, which is correct for the
// single-slot writer ([AdjList.AddEdgeLabeled] stamps its own slot) but leaves a
// multigraph pair's remaining parallel slots at the 0 sentinel. These two let the
// higher layer write and clear every slot of a pair in ONE copy-on-write
// publication.

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// threeParallel builds a multigraph with three parallel a→b slots plus one a→c
// slot, and returns the list with the two endpoint ids.
func threeParallel(t *testing.T) (*AdjList[string, int], graph.NodeID, graph.NodeID, graph.NodeID) {
	t.Helper()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	for i := 0; i < 3; i++ {
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge(a,b): %v", err)
		}
	}
	if err := a.AddEdge("a", "c", 1); err != nil {
		t.Fatalf("AddEdge(a,c): %v", err)
	}
	return a, slotID(t, a, "a"), slotID(t, a, "b"), slotID(t, a, "c")
}

// TestAdjList_SetEdgeLabelSlotsAt_WritesEveryListedSlot is the core contract: the
// listed indexes all receive the value, in one publication, and nothing else moves.
func TestAdjList_SetEdgeLabelSlotsAt_WritesEveryListedSlot(t *testing.T) {
	t.Parallel()
	a, srcID, dstID, _ := threeParallel(t)

	if got := a.SetEdgeLabelSlotsAt(srcID, dstID, []int{0, 1, 2}, 7); got != 3 {
		t.Fatalf("SetEdgeLabelSlotsAt wrote %d slots, want 3", got)
	}
	// Slot 3 is the a→c edge and must be untouched.
	want := []uint32{7, 7, 7, 0}
	got := labelsOf(t, a, "a")
	if len(got) != len(want) {
		t.Fatalf("label column = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("label column = %v, want %v", got, want)
		}
	}
}

// TestAdjList_SetEdgeLabelSlotsAt_SkipsIndexesThatNoLongerMatch verifies the
// dst re-check. The caller chooses indexes from a snapshot read without the shard
// lock, so a stale index must be skipped rather than stamp the wrong edge.
func TestAdjList_SetEdgeLabelSlotsAt_SkipsIndexesThatNoLongerMatch(t *testing.T) {
	t.Parallel()
	a, srcID, dstID, cID := threeParallel(t)

	// Index 3 addresses the a→c slot, not a→b: it must be skipped.
	if got := a.SetEdgeLabelSlotsAt(srcID, dstID, []int{0, 3}, 5); got != 1 {
		t.Fatalf("SetEdgeLabelSlotsAt wrote %d slots, want 1 (index 3 is a→c)", got)
	}
	if got := labelsOf(t, a, "a"); got[3] != 0 {
		t.Fatalf("the a→c slot was stamped: label column = %v", got)
	}
	// Out-of-range and negative indexes are skipped too, not panics.
	if got := a.SetEdgeLabelSlotsAt(srcID, dstID, []int{-1, 99}, 5); got != 0 {
		t.Fatalf("SetEdgeLabelSlotsAt wrote %d slots for out-of-range indexes, want 0", got)
	}
	// A neighbour the source has no slot for writes nothing.
	if got := a.SetEdgeLabelSlotsAt(srcID, cID, []int{0}, 5); got != 0 {
		t.Fatalf("SetEdgeLabelSlotsAt wrote %d slots for a non-matching dst, want 0", got)
	}
}

// TestAdjList_SetEdgeLabelSlotsAt_NoWriteNoColumn keeps the lazy-column contract:
// a call that writes nothing must neither allocate a column nor publish an entry.
func TestAdjList_SetEdgeLabelSlotsAt_NoWriteNoColumn(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	srcID, dstID := slotID(t, a, "a"), slotID(t, a, "b")
	if got := a.SetEdgeLabelSlotsAt(srcID, dstID, nil, 3); got != 0 {
		t.Fatalf("SetEdgeLabelSlotsAt(nil) wrote %d, want 0", got)
	}
	if got := a.SetEdgeLabelSlotsAt(srcID, dstID, []int{7}, 3); got != 0 {
		t.Fatalf("SetEdgeLabelSlotsAt(out of range) wrote %d, want 0", got)
	}
	if got := labelsOf(t, a, "a"); got != nil {
		t.Fatalf("label column = %v, want nil after a no-write call", got)
	}
	// An unknown source is a no-op, not a panic.
	if got := a.SetEdgeLabelSlotsAt(graph.NodeID(1<<30), dstID, []int{0}, 3); got != 0 {
		t.Fatalf("SetEdgeLabelSlotsAt on an unknown source wrote %d, want 0", got)
	}
}

// TestAdjList_ClearEdgeLabelSlotsValue_ClearsEveryMatchingSlot is the inverse:
// every dst-matching slot carrying the value is cleared, and slots carrying a
// DIFFERENT value survive.
func TestAdjList_ClearEdgeLabelSlotsValue_ClearsEveryMatchingSlot(t *testing.T) {
	t.Parallel()
	a, srcID, dstID, _ := threeParallel(t)

	// Slots 0 and 2 carry 7; slot 1 carries 9.
	if got := a.SetEdgeLabelSlotsAt(srcID, dstID, []int{0, 2}, 7); got != 2 {
		t.Fatalf("SetEdgeLabelSlotsAt(7) wrote %d, want 2", got)
	}
	if got := a.SetEdgeLabelSlotsAt(srcID, dstID, []int{1}, 9); got != 1 {
		t.Fatalf("SetEdgeLabelSlotsAt(9) wrote %d, want 1", got)
	}

	if got := a.ClearEdgeLabelSlotsValue(srcID, dstID, 7); got != 2 {
		t.Fatalf("ClearEdgeLabelSlotsValue(7) cleared %d slots, want 2", got)
	}
	want := []uint32{0, 9, 0, 0}
	got := labelsOf(t, a, "a")
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("label column = %v, want %v", got, want)
		}
	}
	// Idempotent: nothing carries 7 any more.
	if got := a.ClearEdgeLabelSlotsValue(srcID, dstID, 7); got != 0 {
		t.Fatalf("second ClearEdgeLabelSlotsValue(7) cleared %d, want 0", got)
	}
	// The 0 sentinel is never a target.
	if got := a.ClearEdgeLabelSlotsValue(srcID, dstID, 0); got != 0 {
		t.Fatalf("ClearEdgeLabelSlotsValue(0) cleared %d, want 0", got)
	}
}

// TestAdjList_ClearEdgeLabelSlotsValue_NoColumnIsNoOp keeps the lazy-column
// contract on the clear side.
func TestAdjList_ClearEdgeLabelSlotsValue_NoColumnIsNoOp(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	srcID, dstID := slotID(t, a, "a"), slotID(t, a, "b")
	if got := a.ClearEdgeLabelSlotsValue(srcID, dstID, 4); got != 0 {
		t.Fatalf("ClearEdgeLabelSlotsValue with no column cleared %d, want 0", got)
	}
	if got := labelsOf(t, a, "a"); got != nil {
		t.Fatalf("label column = %v, want nil", got)
	}
	if got := a.ClearEdgeLabelSlotsValue(graph.NodeID(1<<30), dstID, 4); got != 0 {
		t.Fatalf("ClearEdgeLabelSlotsValue on an unknown source cleared %d, want 0", got)
	}
}

// TestAdjList_AllSlotLabelWrites_RaceWithReaders is the copy-on-write proof
// obligation both new primitives inherit: a lock-free reader holding a prior
// snapshot must be unaffected while they publish new columns. Run under -race.
func TestAdjList_AllSlotLabelWrites_RaceWithReaders(t *testing.T) {
	t.Parallel()
	a, srcID, dstID, _ := threeParallel(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				nbs, _ := a.LoadEntry(srcID)
				labs := a.LoadEntryLabels(srcID)
				// The reader only has to observe a self-consistent snapshot; the
				// column, when present, is never shorter than the neighbours it was
				// published with.
				if labs != nil && len(labs) < len(nbs) {
					t.Errorf("torn snapshot: %d labels for %d neighbours", len(labs), len(nbs))
					return
				}
			}
		}()
	}
	for i := uint32(1); i <= 2000; i++ {
		a.SetEdgeLabelSlotsAt(srcID, dstID, []int{0, 1, 2}, i)
		a.ClearEdgeLabelSlotsValue(srcID, dstID, i)
	}
	close(stop)
	wg.Wait()
}
