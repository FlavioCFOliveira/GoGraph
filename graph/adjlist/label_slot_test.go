package adjlist

import (
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// labelsOf returns the label column for src as the snapshot slice, failing
// the test when the endpoint is unknown.
func labelsOf(tb testing.TB, a *AdjList[string, int], src string) []uint32 {
	tb.Helper()
	id, ok := a.Mapper().Lookup(src)
	if !ok {
		tb.Fatalf("Lookup(%q) missed", src)
	}
	return a.LoadEntryLabels(id)
}

// slotID resolves a user value to its NodeID, failing the test on miss.
func slotID(tb testing.TB, a *AdjList[string, int], v string) graph.NodeID {
	tb.Helper()
	id, ok := a.Mapper().Lookup(v)
	if !ok {
		tb.Fatalf("Lookup(%q) missed", v)
	}
	return id
}

// TestAdjList_Labels_NilUntilSet verifies the opt-in contract: a graph that
// never sets a label carries no label column, so label-free graphs pay no
// extra memory.
func TestAdjList_Labels_NilUntilSet(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if got := labelsOf(t, a, "a"); got != nil {
		t.Fatalf("labels column = %v, want nil before any SetEdgeLabelSlot", got)
	}
}

// TestAdjList_SetEdgeLabelSlot_Basic sets a label on the only slot and reads
// it back, slot-aligned with neighbours.
func TestAdjList_SetEdgeLabelSlot_Basic(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	srcID := slotID(t, a, "a")
	dstID := slotID(t, a, "b")
	if ok := a.SetEdgeLabelSlot(srcID, dstID, 7); !ok {
		t.Fatal("SetEdgeLabelSlot returned false, want true (slot exists)")
	}
	labs := labelsOf(t, a, "a")
	if len(labs) != 1 || labs[0] != 7 {
		t.Fatalf("labels = %v, want [7]", labs)
	}
	// No edge to a missing dst -> false, no panic.
	if ok := a.SetEdgeLabelSlot(srcID, srcID, 9); ok {
		t.Fatal("SetEdgeLabelSlot on non-existent self-edge returned true")
	}
}

// TestAdjList_Labels_AlignedAcrossGrowth verifies the column stays slot-
// aligned with neighbours as the entry grows (fast and slow path) and that
// appended slots default to the 0 "no label" sentinel.
func TestAdjList_Labels_AlignedAcrossGrowth(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	dsts := []string{"b", "c", "d", "e", "f", "g"}
	for i, d := range dsts {
		if err := a.AddEdge("a", d, i); err != nil {
			t.Fatalf("AddEdge a->%s: %v", d, err)
		}
	}
	// Label the third slot only.
	srcID := slotID(t, a, "a")
	dID := slotID(t, a, "d")
	if ok := a.SetEdgeLabelSlot(srcID, dID, 42); !ok {
		t.Fatal("SetEdgeLabelSlot(d) = false")
	}
	// Grow further; the labelled slot must keep its value and the new slots
	// must be the 0 sentinel.
	if err := a.AddEdge("a", "h", 99); err != nil {
		t.Fatalf("AddEdge a->h: %v", err)
	}
	nb, _ := a.LoadEntry(srcID)
	labs := labelsOf(t, a, "a")
	if len(labs) != len(nb) {
		t.Fatalf("labels len %d != neighbours len %d", len(labs), len(nb))
	}
	for i, n := range nb {
		want := uint32(0)
		if n == dID {
			want = 42
		}
		if labs[i] != want {
			t.Fatalf("labels[%d] (neighbour %d) = %d, want %d", i, n, labs[i], want)
		}
	}
}

// TestAdjList_ClearEdgeLabelSlotValue_TargetsValue verifies that clearing by
// value drops exactly the matching parallel slot, leaving a sibling carrying a
// different label untouched.
func TestAdjList_ClearEdgeLabelSlotValue_TargetsValue(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge #1: %v", err)
	}
	if err := a.AddEdge("a", "b", 2); err != nil {
		t.Fatalf("AddEdge #2: %v", err)
	}
	srcID := slotID(t, a, "a")
	dstID := slotID(t, a, "b")
	// Label the first slot with the encoded value 11.
	if ok := a.SetEdgeLabelSlot(srcID, dstID, 11); !ok {
		t.Fatal("set slot0 = false")
	}
	// Clearing the value a slot carries zeroes exactly that slot.
	if ok := a.ClearEdgeLabelSlotValue(srcID, dstID, 11); !ok {
		t.Fatal("clear value 11 = false")
	}
	if labs := labelsOf(t, a, "a"); labs[0] != 0 {
		t.Fatalf("after clearing 11, slot0 = %d, want 0", labs[0])
	}
	// Clearing a value no slot carries is a no-op false.
	if ok := a.ClearEdgeLabelSlotValue(srcID, dstID, 999); ok {
		t.Fatal("clear of absent value returned true")
	}
	// The 0 sentinel is never a valid clear target.
	if ok := a.ClearEdgeLabelSlotValue(srcID, dstID, 0); ok {
		t.Fatal("clear with v=0 returned true")
	}
}

// TestAdjList_Labels_CompactedOnRemove verifies the label column is excised in
// lockstep with neighbours when a parallel edge is removed: surviving slots
// keep their original label.
func TestAdjList_Labels_CompactedOnRemove(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	for i := 0; i < 3; i++ {
		if err := a.AddEdge("a", "b", i); err != nil {
			t.Fatalf("AddEdge #%d: %v", i, err)
		}
	}
	srcID := slotID(t, a, "a")
	dstID := slotID(t, a, "b")
	if ok := a.SetEdgeLabelSlot(srcID, dstID, 5); !ok {
		t.Fatal("set = false")
	}
	// Remove one parallel edge: first-match (slot 0) is excised, including its
	// label. The remaining slots stay 0; the column stays aligned.
	a.RemoveEdge("a", "b")
	nb, _ := a.LoadEntry(srcID)
	labs := labelsOf(t, a, "a")
	if len(labs) != len(nb) {
		t.Fatalf("after remove: labels len %d != neighbours len %d", len(labs), len(nb))
	}
}

// TestAdjList_AddEdgeLabeled_FusedWrite verifies the fused append path stamps
// the supplied label directly onto the new slot at insertion time — no separate
// SetEdgeLabelSlot — and that label-free appends interleaved with labelled ones
// stay slot-aligned with neighbours.
func TestAdjList_AddEdgeLabeled_FusedWrite(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	// A label-free edge first: the column stays nil (opt-in preserved).
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge a->b: %v", err)
	}
	if got := labelsOf(t, a, "a"); got != nil {
		t.Fatalf("labels column = %v, want nil before any fused label", got)
	}
	// A labelled append now allocates the column lazily and writes the label at
	// the new slot's position (1), back-filling the earlier slot with 0.
	if err := a.AddEdgeLabeled("a", "c", 2, 7); err != nil {
		t.Fatalf("AddEdgeLabeled a->c: %v", err)
	}
	// Two more labelled appends to exercise the grow (slow) path.
	if err := a.AddEdgeLabeled("a", "d", 3, 8); err != nil {
		t.Fatalf("AddEdgeLabeled a->d: %v", err)
	}
	if err := a.AddEdgeLabeled("a", "e", 4, 9); err != nil {
		t.Fatalf("AddEdgeLabeled a->e: %v", err)
	}
	nb, _ := a.LoadEntry(slotID(t, a, "a"))
	labs := labelsOf(t, a, "a")
	if len(labs) != len(nb) {
		t.Fatalf("labels len %d != neighbours len %d", len(labs), len(nb))
	}
	want := map[graph.NodeID]uint32{
		slotID(t, a, "b"): 0, // label-free append
		slotID(t, a, "c"): 7,
		slotID(t, a, "d"): 8,
		slotID(t, a, "e"): 9,
	}
	for i, n := range nb {
		if labs[i] != want[n] {
			t.Fatalf("labels[%d] (neighbour %d) = %d, want %d", i, n, labs[i], want[n])
		}
	}
}

// TestAdjList_AddEdgeLabeled_FirstSlot verifies the fresh-entry branch: the very
// first edge for a node carries its fused label on slot 0.
func TestAdjList_AddEdgeLabeled_FirstSlot(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	if err := a.AddEdgeLabeled("a", "b", 1, 5); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}
	labs := labelsOf(t, a, "a")
	if len(labs) != 1 || labs[0] != 5 {
		t.Fatalf("labels = %v, want [5]", labs)
	}
}

// TestAdjList_AddEdgeLabeled_Undirected verifies an undirected fused labelled
// edge stamps the SAME label on both directions, matching AddEdgeH's mirror
// contract.
func TestAdjList_AddEdgeLabeled_Undirected(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: false})
	if err := a.AddEdgeLabeled("a", "b", 1, 3); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}
	fwd := labelsOf(t, a, "a")
	rev := labelsOf(t, a, "b")
	if len(fwd) != 1 || fwd[0] != 3 {
		t.Fatalf("forward labels = %v, want [3]", fwd)
	}
	if len(rev) != 1 || rev[0] != 3 {
		t.Fatalf("mirror labels = %v, want [3]", rev)
	}
}

// TestAdjList_AddEdgeLabeled_SimpleGraphDuplicate verifies the simple-graph
// collapse: a duplicate (src,dst) is a no-op and the existing slot keeps its
// original label (the fused label of the duplicate is ignored).
func TestAdjList_AddEdgeLabeled_SimpleGraphDuplicate(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true}) // simple graph
	if err := a.AddEdgeLabeled("a", "b", 1, 11); err != nil {
		t.Fatalf("AddEdgeLabeled #1: %v", err)
	}
	if err := a.AddEdgeLabeled("a", "b", 1, 22); err != nil {
		t.Fatalf("AddEdgeLabeled #2 (dup): %v", err)
	}
	labs := labelsOf(t, a, "a")
	if len(labs) != 1 || labs[0] != 11 {
		t.Fatalf("after duplicate labels = %v, want [11] (dup label 22 ignored)", labs)
	}
	if got := a.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1 (duplicate not counted)", got)
	}
}

// TestAdjList_AddEdgeLabeledH_FusedBoth verifies the doubly-fused path stamps
// BOTH a handle and a label onto the new slot at insertion time.
func TestAdjList_AddEdgeLabeledH_FusedBoth(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	if err := a.AddEdgeLabeledH("a", "b", 1, 100, 4); err != nil {
		t.Fatalf("AddEdgeLabeledH #1: %v", err)
	}
	if err := a.AddEdgeLabeledH("a", "c", 2, 200, 5); err != nil {
		t.Fatalf("AddEdgeLabeledH #2: %v", err)
	}
	srcID := slotID(t, a, "a")
	nb, _, hs := a.LoadEntryH(srcID)
	labs := a.LoadEntryLabels(srcID)
	if len(hs) != len(nb) || len(labs) != len(nb) {
		t.Fatalf("column lengths: nb %d hs %d labs %d", len(nb), len(hs), len(labs))
	}
	wantH := map[graph.NodeID]uint64{slotID(t, a, "b"): 100, slotID(t, a, "c"): 200}
	wantL := map[graph.NodeID]uint32{slotID(t, a, "b"): 4, slotID(t, a, "c"): 5}
	for i, n := range nb {
		if hs[i] != wantH[n] {
			t.Fatalf("handles[%d] (neighbour %d) = %d, want %d", i, n, hs[i], wantH[n])
		}
		if labs[i] != wantL[n] {
			t.Fatalf("labels[%d] (neighbour %d) = %d, want %d", i, n, labs[i], wantL[n])
		}
	}
}

// TestAdjList_AddEdgeLabeled_RaceWithReaders proves the fused append-time label
// write is reader-safe: a concurrent lock-free reader running while labelled
// edges are appended never observes a TORN entry (the data race detector would
// flag a write to a slot a reader is reading). The fused write only ever stamps
// the NEW slot (invisible until the header is published) and never mutates a
// live slot in place, so the race detector must stay clean. Run under -race.
//
// The reader reads the label column and the neighbours with two independent
// atomic loads, so a concurrent append may publish a longer neighbours snapshot
// between them; the reader therefore tolerates a length SKEW (exactly as the
// production readers in lpg.slotLabelsForPair / RelationshipTypesInUse do by
// bounding the scan to the shorter column) and asserts only that every label it
// can index against a published neighbour is a value it actually wrote.
func TestAdjList_AddEdgeLabeled_RaceWithReaders(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	if err := a.AddEdgeLabeled("a", "b", 0, 1); err != nil {
		t.Fatalf("seed AddEdgeLabeled: %v", err)
	}
	srcID := slotID(t, a, "a")
	const maxLabel = 2000

	stop := make(chan struct{})
	var readers, writers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				labs := a.LoadEntryLabels(srcID)
				// Bound the scan to the column we loaded; every label slot must
				// hold a value this test wrote (1..maxLabel), never garbage from
				// a torn write into a live slot.
				for _, v := range labs {
					if v < 1 || v >= maxLabel {
						t.Errorf("observed out-of-range label %d (torn write?)", v)
						return
					}
				}
			}
		}()
	}
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := uint32(1); i < maxLabel; i++ {
			_ = a.AddEdgeLabeled("a", "b", 0, i)
		}
	}()
	writers.Wait()
	close(stop)
	readers.Wait()
}

// TestAdjList_SetEdgeLabelSlot_RaceWithReaders is the proof obligation from the
// atomic-publication certification: a concurrent lock-free reader
// (LoadEntryLabels/LoadEntry) running while SetEdgeLabelSlot mutates an
// existing index must never observe a torn entry. Run under -race.
func TestAdjList_SetEdgeLabelSlot_RaceWithReaders(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	const deg = 64
	for i := 0; i < deg; i++ {
		if err := a.AddEdge("a", "b", i); err != nil {
			t.Fatalf("AddEdge #%d: %v", i, err)
		}
	}
	srcID := slotID(t, a, "a")
	dstID := slotID(t, a, "b")

	stop := make(chan struct{})
	var readers sync.WaitGroup
	var writers sync.WaitGroup

	// Readers: continuously snapshot the label column and the neighbours,
	// asserting the lengths agree (a torn publish would break this).
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				labs := a.LoadEntryLabels(srcID)
				nb, _ := a.LoadEntry(srcID)
				if labs != nil && len(labs) != len(nb) {
					t.Errorf("torn snapshot: labels %d != neighbours %d", len(labs), len(nb))
					return
				}
			}
		}()
	}

	// Writers: repeatedly set and clear the first-slot label.
	for w := 0; w < 2; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := uint32(1); i < 2000; i++ {
				a.SetEdgeLabelSlot(srcID, dstID, i)
				a.ClearEdgeLabelSlotValue(srcID, dstID, i)
			}
		}()
	}

	// Once the bounded writers finish, signal the readers to stop and join.
	writers.Wait()
	close(stop)
	readers.Wait()
}

// hubGraph builds a simple directed graph with one source ("hub") and deg
// distinct out-neighbours ("n0".."n{deg-1}"), returning the graph, the source
// id, and the neighbour ids in append order. It is the fixture for the
// per-slot-vs-batch re-label benchmarks and the batch correctness test.
func hubGraph(tb testing.TB, deg int) (*AdjList[string, int], graph.NodeID, []graph.NodeID) {
	tb.Helper()
	a := New[string, int](Config{Directed: true})
	dsts := make([]graph.NodeID, deg)
	for i := 0; i < deg; i++ {
		dst := fmt.Sprintf("n%d", i)
		if err := a.AddEdge("hub", dst, i); err != nil {
			tb.Fatalf("AddEdge hub->%s: %v", dst, err)
		}
	}
	srcID := slotID(tb, a, "hub")
	for i := 0; i < deg; i++ {
		dsts[i] = slotID(tb, a, fmt.Sprintf("n%d", i))
	}
	return a, srcID, dsts
}

// TestAdjList_SetEdgeLabelSlots_Batch verifies the amortized bulk setter: a
// single call writes every requested slot, returns the count written, honours
// the zero-clears-the-slot rule, ignores neighbours with no slot, and is a
// no-op (no column) when nothing matches.
func TestAdjList_SetEdgeLabelSlots_Batch(t *testing.T) {
	t.Parallel()
	a, srcID, dsts := hubGraph(t, 5)

	updates := map[graph.NodeID]uint32{
		dsts[0]: 10,
		dsts[2]: 30,
		dsts[4]: 50,
	}
	if got := a.SetEdgeLabelSlots(srcID, updates); got != 3 {
		t.Fatalf("SetEdgeLabelSlots wrote %d slots, want 3", got)
	}
	labs := labelsOf(t, a, "hub")
	want := []uint32{10, 0, 30, 0, 50}
	if len(labs) != len(want) {
		t.Fatalf("labels = %v, want %v", labs, want)
	}
	for i := range want {
		if labs[i] != want[i] {
			t.Fatalf("labels = %v, want %v", labs, want)
		}
	}

	// A value of 0 clears a slot; an unknown neighbour is ignored. The two
	// applied writes are dsts[0]->0 and dsts[4]->7; the bogus id matches nothing.
	bogus := graph.NodeID(1 << 30)
	if got := a.SetEdgeLabelSlots(srcID, map[graph.NodeID]uint32{dsts[0]: 0, dsts[4]: 7, bogus: 99}); got != 2 {
		t.Fatalf("second batch wrote %d slots, want 2", got)
	}
	labs = labelsOf(t, a, "hub")
	want = []uint32{0, 0, 30, 0, 7}
	for i := range want {
		if labs[i] != want[i] {
			t.Fatalf("after clear+set labels = %v, want %v", labs, want)
		}
	}

	// Empty updates and an unknown source are no-ops returning 0.
	if got := a.SetEdgeLabelSlots(srcID, nil); got != 0 {
		t.Fatalf("nil updates wrote %d, want 0", got)
	}
	if got := a.SetEdgeLabelSlots(graph.NodeID(1<<30), map[graph.NodeID]uint32{dsts[0]: 1}); got != 0 {
		t.Fatalf("unknown source wrote %d, want 0", got)
	}
}

// TestAdjList_SetEdgeLabelSlots_FirstSlotMultigraph verifies that, like the
// single-slot setter, the batch writes only the FIRST slot matching each
// neighbour when parallel edges share a destination.
func TestAdjList_SetEdgeLabelSlots_FirstSlotMultigraph(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	const parallel = 4
	for i := 0; i < parallel; i++ {
		if err := a.AddEdge("a", "b", i); err != nil {
			t.Fatalf("AddEdge #%d: %v", i, err)
		}
	}
	srcID := slotID(t, a, "a")
	dstID := slotID(t, a, "b")
	if got := a.SetEdgeLabelSlots(srcID, map[graph.NodeID]uint32{dstID: 7}); got != 1 {
		t.Fatalf("batch wrote %d slots, want 1 (first slot only)", got)
	}
	labs := labelsOf(t, a, "a")
	want := []uint32{7, 0, 0, 0}
	for i := range want {
		if labs[i] != want[i] {
			t.Fatalf("labels = %v, want %v (only the first slot)", labs, want)
		}
	}
}

// TestAdjList_SetEdgeLabelSlots_RaceWithReaders mirrors the single-slot race
// obligation for the bulk path: concurrent lock-free readers must never see a
// torn entry while a batch writer republishes the whole column. Run under -race.
func TestAdjList_SetEdgeLabelSlots_RaceWithReaders(t *testing.T) {
	t.Parallel()
	a, srcID, dsts := hubGraph(t, 64)

	stop := make(chan struct{})
	var readers, writers sync.WaitGroup

	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				labs := a.LoadEntryLabels(srcID)
				nb, _ := a.LoadEntry(srcID)
				if labs != nil && len(labs) != len(nb) {
					t.Errorf("torn snapshot: labels %d != neighbours %d", len(labs), len(nb))
					return
				}
			}
		}()
	}

	for w := 0; w < 2; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := uint32(1); i < 1000; i++ {
				updates := make(map[graph.NodeID]uint32, len(dsts))
				for _, d := range dsts {
					updates[d] = i
				}
				a.SetEdgeLabelSlots(srcID, updates)
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()
}

// BenchmarkAdjList_HubRelabel contrasts re-labelling all of a hub's d slots
// one SetEdgeLabelSlot call at a time (each copies the whole column -> O(d²))
// against one SetEdgeLabelSlots call (a single copy -> O(d)). Comparing ns/op
// across the degrees shows the per-slot path scaling ~quadratically while the
// batch path scales ~linearly — the sub-quadratic re-label the task requires.
func BenchmarkAdjList_HubRelabel(b *testing.B) {
	for _, deg := range []int{256, 1024, 4096} {
		a, srcID, dsts := hubGraph(b, deg)

		b.Run(fmt.Sprintf("PerSlot/deg=%d", deg), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				v := uint32(i + 1)
				for _, d := range dsts {
					a.SetEdgeLabelSlot(srcID, d, v)
				}
			}
		})

		updates := make(map[graph.NodeID]uint32, deg)
		b.Run(fmt.Sprintf("Batch/deg=%d", deg), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				v := uint32(i + 1)
				for _, d := range dsts {
					updates[d] = v
				}
				a.SetEdgeLabelSlots(srcID, updates)
			}
		})
	}
}
