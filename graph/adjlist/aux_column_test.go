package adjlist

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// fakeAux is a minimal in-test [AuxColumn] used to verify that adjlist drives the
// lifecycle hooks correctly: it records one int per slot plus a per-slot present
// flag, and reproduces the GrowSlot (append-absent) / CompactSlot (splice)
// transforms an opaque column must support. It is intentionally a different,
// simpler representation than lpg's edgePropCols so the test exercises only the
// adjlist seam contract, not lpg internals.
type fakeAux struct {
	vals         []int
	present      []bool
	compactCalls int // number of times Compact was invoked (for the Compact-seam test)
}

func (f *fakeAux) GrowSlot(oldLen int) AuxColumn {
	out := &fakeAux{
		vals:    make([]int, oldLen+1),
		present: make([]bool, oldLen+1),
	}
	copy(out.vals, f.vals)
	copy(out.present, f.present)
	// The new slot at oldLen is ABSENT (present stays false, val stays 0).
	return out
}

// GrowSlotWithValue is the value-carrying analogue: the new slot at oldLen is
// PRESENT and carries the payload (an int in this test double).
func (f *fakeAux) GrowSlotWithValue(oldLen int, payload any) AuxColumn {
	out := &fakeAux{
		vals:    make([]int, oldLen+1),
		present: make([]bool, oldLen+1),
	}
	copy(out.vals, f.vals)
	copy(out.present, f.present)
	out.vals[oldLen] = payload.(int)
	out.present[oldLen] = true
	return out
}

func (f *fakeAux) CompactSlot(idx int) AuxColumn {
	n := len(f.vals)
	out := &fakeAux{
		vals:    make([]int, n-1),
		present: make([]bool, n-1),
	}
	copy(out.vals, f.vals[:idx])
	copy(out.present[:idx], f.present[:idx])
	copy(out.vals[idx:], f.vals[idx+1:])
	copy(out.present[idx:], f.present[idx+1:])
	return out
}

// Compact reclaims backing slack. fakeAux records compactCalls so a test can
// assert adjlist drives the hook; it returns a re-allocated exact-length copy
// when its backing carries slack, else the receiver unchanged.
func (f *fakeAux) Compact() AuxColumn {
	f.compactCalls++
	if cap(f.vals) == len(f.vals) && cap(f.present) == len(f.present) {
		return f
	}
	out := &fakeAux{
		vals:         make([]int, len(f.vals)),
		present:      make([]bool, len(f.present)),
		compactCalls: f.compactCalls,
	}
	copy(out.vals, f.vals)
	copy(out.present, f.present)
	return out
}

// setOn returns a copy with value v recorded (present) on slot.
func (f *fakeAux) setOn(slot, v int) *fakeAux {
	out := &fakeAux{
		vals:    append([]int(nil), f.vals...),
		present: append([]bool(nil), f.present...),
	}
	out.vals[slot] = v
	out.present[slot] = true
	return out
}

func auxOf(a *AdjList[string, int], src graph.NodeID) *fakeAux {
	c := a.LoadEntryAux(src)
	if c == nil {
		return nil
	}
	return c.(*fakeAux)
}

// TestAux_GrowKeepsAlignmentAndClearsNewSlot drives real AddEdge appends through
// an entry that already carries an aux column and asserts that (a) existing slots
// keep their value, (b) the freshly-appended slot is ABSENT, and (c) the column
// stays the same length as neighbours.
func TestAux_GrowAcrossAppends(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	if err := a.AddEdge("s", "d0", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	srcID, _ := a.Mapper().Lookup("s")

	// Attach an aux column and set the value on slot 0.
	a.UpdateEntryAux(srcID, func(cur AuxColumn, nbs []graph.NodeID) (AuxColumn, bool) {
		base := &fakeAux{vals: make([]int, len(nbs)), present: make([]bool, len(nbs))}
		return base.setOn(0, 100), true
	})

	// Append three more parallel/distinct edges. Each append must grow the aux
	// column by one absent slot via GrowSlot.
	for i, d := range []string{"d1", "d2", "d3"} {
		if err := a.AddEdge("s", d, i+1); err != nil {
			t.Fatalf("AddEdge %s: %v", d, err)
		}
	}

	nbs, _ := a.LoadEntry(srcID)
	aux := auxOf(a, srcID)
	if aux == nil {
		t.Fatalf("aux column lost after appends")
	}
	if len(aux.vals) != len(nbs) {
		t.Fatalf("aux length %d != neighbours %d", len(aux.vals), len(nbs))
	}
	// Slot 0 keeps its value; the appended slots are absent.
	if !aux.present[0] || aux.vals[0] != 100 {
		t.Fatalf("slot 0 lost its value: present=%v val=%d", aux.present[0], aux.vals[0])
	}
	for i := 1; i < len(aux.present); i++ {
		if aux.present[i] {
			t.Fatalf("appended slot %d is present (should be absent)", i)
		}
	}
}

// TestAux_FusedAppendViaFactoryAndGrow drives the fused property-carrying append
// (AddEdgeLabeledWithProp): the FIRST edge of a node must build its aux column via
// the registered factory (value present at slot 0), and every subsequent fused
// append must add a PRESENT slot at oldLen via GrowSlotWithValue, all length-
// aligned with neighbours.
func TestAux_FusedAppendViaFactoryAndGrow(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	// The factory builds a single-present-slot fakeAux of the requested length
	// (the new slot is the last one, length-1) carrying the payload int.
	a.SetAuxFactory(func(length int, payload any) AuxColumn {
		f := &fakeAux{vals: make([]int, length), present: make([]bool, length)}
		f.vals[length-1] = payload.(int)
		f.present[length-1] = true
		return f
	})

	dsts := []string{"d0", "d1", "d2", "d3"}
	for i, d := range dsts {
		if err := a.AddEdgeLabeledWithProp("s", d, i, uint32(7), 100+i); err != nil {
			t.Fatalf("AddEdgeLabeledWithProp %s: %v", d, err)
		}
	}
	srcID, _ := a.Mapper().Lookup("s")
	nbs, _ := a.LoadEntry(srcID)
	aux := auxOf(a, srcID)
	if aux == nil {
		t.Fatalf("aux column never built by the fused path")
	}
	if len(aux.vals) != len(nbs) {
		t.Fatalf("aux length %d != neighbours %d", len(aux.vals), len(nbs))
	}
	// Every slot is present and carries 100+slot (the value fused at append time).
	for i := range nbs {
		if !aux.present[i] || aux.vals[i] != 100+i {
			t.Fatalf("slot %d: present=%v val=%d want %d", i, aux.present[i], aux.vals[i], 100+i)
		}
	}
	// The labels column was fused too (every slot carries label 7).
	labs := labelsOf(t, a, "s")
	if len(labs) != len(nbs) {
		t.Fatalf("fused labels column length %d != neighbours %d", len(labs), len(nbs))
	}
	for i, l := range labs {
		if l != 7 {
			t.Fatalf("fused label not stamped on slot %d: got %d want 7", i, l)
		}
	}
}

// TestAux_FusedAppendAfterAbsentGrow asserts a fused payload arriving on an entry
// whose earlier edges were added WITHOUT a payload (so the aux column is still
// nil) builds the column at the right length via the factory, with only the new
// slot present.
func TestAux_FusedAppendAfterAbsentGrow(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	a.SetAuxFactory(func(length int, payload any) AuxColumn {
		f := &fakeAux{vals: make([]int, length), present: make([]bool, length)}
		f.vals[length-1] = payload.(int)
		f.present[length-1] = true
		return f
	})
	// First two edges carry no payload: aux stays nil.
	if err := a.AddEdge("s", "d0", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("s", "d1", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	srcID, _ := a.Mapper().Lookup("s")
	if a.LoadEntryAux(srcID) != nil {
		t.Fatalf("aux non-nil before any payload")
	}
	// Third edge fuses a payload: the column is born at length 3 with only slot 2
	// present.
	if err := a.AddEdgeLabeledWithProp("s", "d2", 2, uint32(1), 999); err != nil {
		t.Fatalf("AddEdgeLabeledWithProp: %v", err)
	}
	aux := auxOf(a, srcID)
	nbs, _ := a.LoadEntry(srcID)
	if aux == nil || len(aux.vals) != len(nbs) {
		t.Fatalf("aux not length-aligned: %v vs %d neighbours", aux, len(nbs))
	}
	if aux.present[0] || aux.present[1] {
		t.Fatalf("earlier payload-free slots are present: %v", aux.present)
	}
	if !aux.present[2] || aux.vals[2] != 999 {
		t.Fatalf("fused slot 2: present=%v val=%d want 999", aux.present[2], aux.vals[2])
	}
}

// TestAux_FusedPayloadIsDirectional asserts the fused payload is stamped only on
// the FORWARD (src) entry, not the undirected MIRROR (dst,src) entry: edge
// properties are stored directionally (matching UpdateEntryAux / the higher
// layer's per-source SetEdgeProperty), while the label IS symmetric. This keeps a
// fused undirected write observationally identical to the two-step build.
func TestAux_FusedPayloadIsDirectional(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: false, Multigraph: true})
	a.SetAuxFactory(func(length int, payload any) AuxColumn {
		f := &fakeAux{vals: make([]int, length), present: make([]bool, length)}
		f.vals[length-1] = payload.(int)
		f.present[length-1] = true
		return f
	})
	if err := a.AddEdgeLabeledWithProp("a", "b", 0, uint32(3), 42); err != nil {
		t.Fatalf("AddEdgeLabeledWithProp: %v", err)
	}
	// Forward a->b carries the payload.
	aID, _ := a.Mapper().Lookup("a")
	fwd := auxOf(a, aID)
	if fwd == nil || len(fwd.vals) != 1 || !fwd.present[0] || fwd.vals[0] != 42 {
		t.Fatalf("forward entry missing fused payload: %v", fwd)
	}
	// Mirror b->a carries NO aux column (the payload is directional).
	bID, _ := a.Mapper().Lookup("b")
	if mir := a.LoadEntryAux(bID); mir != nil {
		t.Fatalf("mirror entry carries an aux column (payload must be directional): %v", mir)
	}
	// The label IS symmetric: both directions carry label 3.
	if labs := labelsOf(t, a, "a"); len(labs) != 1 || labs[0] != 3 {
		t.Fatalf("forward label = %v, want [3]", labs)
	}
	if labs := labelsOf(t, a, "b"); len(labs) != 1 || labs[0] != 3 {
		t.Fatalf("mirror label = %v, want [3] (label is symmetric)", labs)
	}
}

// TestAux_CompactAcrossRemoval drives a real RemoveEdge and asserts CompactSlot
// excised the right slot, preserving the surviving slots' values and binding.
func TestAux_CompactAcrossRemoval(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	dsts := []string{"d0", "d1", "d2", "d3"}
	for i, d := range dsts {
		if err := a.AddEdge("s", d, i); err != nil {
			t.Fatalf("AddEdge %s: %v", d, err)
		}
	}
	srcID, _ := a.Mapper().Lookup("s")
	// Set a distinct value on each slot.
	a.UpdateEntryAux(srcID, func(cur AuxColumn, nbs []graph.NodeID) (AuxColumn, bool) {
		f := &fakeAux{vals: make([]int, len(nbs)), present: make([]bool, len(nbs))}
		for i := range nbs {
			f = f.setOn(i, 10+i)
		}
		return f, true
	})

	// Remove the middle edge s->d1 (slot 1).
	a.RemoveEdge("s", "d1")

	nbs, _ := a.LoadEntry(srcID)
	aux := auxOf(a, srcID)
	if len(aux.vals) != len(nbs) {
		t.Fatalf("aux length %d != neighbours %d after compact", len(aux.vals), len(nbs))
	}
	// Surviving neighbours are d0, d2, d3 with values 10, 12, 13.
	d1ID, _ := a.Mapper().Lookup("d1")
	for i, nb := range nbs {
		if nb == d1ID {
			t.Fatalf("d1 still present at slot %d", i)
		}
		dName, _ := a.Mapper().Resolve(nb)
		var want int
		switch dName {
		case "d0":
			want = 10
		case "d2":
			want = 12
		case "d3":
			want = 13
		}
		if !aux.present[i] || aux.vals[i] != want {
			t.Fatalf("slot %d (%s): present=%v val=%d want %d", i, dName, aux.present[i], aux.vals[i], want)
		}
	}
}

// TestAux_NilUntilSet asserts an entry that never had an aux column attached
// keeps a nil aux through appends and removals (a property-free graph pays
// nothing).
func TestAux_NilUntilSet(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	if err := a.AddEdge("s", "d0", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("s", "d1", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	srcID, _ := a.Mapper().Lookup("s")
	if aux := a.LoadEntryAux(srcID); aux != nil {
		t.Fatalf("aux is non-nil on a graph that never set one: %v", aux)
	}
	a.RemoveEdge("s", "d0")
	if aux := a.LoadEntryAux(srcID); aux != nil {
		t.Fatalf("aux became non-nil after a removal on a property-free graph")
	}
}

// TestAux_CompactCarriedByTrim asserts Compact() carries the aux column verbatim
// (it has no slack notion) while trimming the topology arrays.
func TestAux_CarriedByTrim(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	// Build with growth slack: several appends over-allocate the backing arrays.
	for i := 0; i < 5; i++ {
		if err := a.AddEdge("s", "d", i); err != nil { // parallel edges to same dst
			t.Fatalf("AddEdge: %v", err)
		}
	}
	srcID, _ := a.Mapper().Lookup("s")
	a.UpdateEntryAux(srcID, func(cur AuxColumn, nbs []graph.NodeID) (AuxColumn, bool) {
		f := &fakeAux{vals: make([]int, len(nbs)), present: make([]bool, len(nbs))}
		for i := range nbs {
			f = f.setOn(i, 50+i)
		}
		return f, true
	})
	before := auxOf(a, srcID)

	a.Compact(context.Background())

	after := auxOf(a, srcID)
	if after == nil {
		t.Fatalf("aux dropped by Compact")
	}
	if len(after.vals) != len(before.vals) {
		t.Fatalf("aux length changed by Compact: %d -> %d", len(before.vals), len(after.vals))
	}
	for i := range after.vals {
		if !after.present[i] || after.vals[i] != 50+i {
			t.Fatalf("aux slot %d changed by Compact: present=%v val=%d", i, after.present[i], after.vals[i])
		}
	}
}

// TestAux_UpdateNoEntry asserts UpdateEntryAux returns false (and does not call
// fn in a way that publishes) for a source with no adjacency entry.
func TestAux_UpdateNoEntry(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	a.Mapper().Intern("lonely") // interned but no edge → no entry
	srcID, _ := a.Mapper().Lookup("lonely")
	called := false
	ok := a.UpdateEntryAux(srcID, func(cur AuxColumn, nbs []graph.NodeID) (AuxColumn, bool) {
		called = true
		return cur, true
	})
	if ok {
		t.Fatalf("UpdateEntryAux returned true for a source with no entry")
	}
	if called {
		t.Fatalf("fn was called for a source with no entry")
	}
}
