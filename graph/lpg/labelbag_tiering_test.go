package lpg

// labelbag_tiering_test.go — regression gate for sprint 221 / #1629: the
// tiered labelBag (singleton -> small slice -> map) must be membership- and
// iteration-equivalent to the prior per-node map across every tier transition,
// and the nodeIdx label bitmap must stay in lockstep with the bag through
// set / remove / promotion / collapse. Companion to label_bitmap_tombstone_test.go
// (which covers the tombstone/revive lockstep at the 1-2-label tier).
//
// Layer: short.

import (
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestLabelBag_TierTransitions_LockstepWithIndex drives a single node across
// the singleton, small-slice, and promoted-map tiers and back down, asserting
// at every step that HasNodeLabel, the NodeLabels set, and the nodeIdx bitmap
// all agree (the load-bearing invariant of the #1629 refactor).
func TestLabelBag_TierTransitions_LockstepWithIndex(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	id, _ := g.AdjList().Mapper().Lookup("n")

	// Add 12 labels: 1 -> singleton, 2..8 -> small slice, 9..12 -> promoted map.
	const total = 12
	names := make([]string, total)
	for i := range names {
		names[i] = fmt.Sprintf("L%02d", i)
	}
	for i, name := range names {
		if err := g.SetNodeLabel("n", name); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", name, err)
		}
		want := i + 1
		assertLabelState(t, g, "n", id, names[:want])
	}

	// Idempotent re-add must not change the set or the index.
	if err := g.SetNodeLabel("n", names[0]); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	assertLabelState(t, g, "n", id, names)

	// Remove down: map tier never demotes, but membership must stay exact.
	// Remove every other label, then the rest, checking lockstep each time.
	remaining := append([]string(nil), names...)
	for _, name := range []string{"L11", "L09", "L07", "L05", "L03", "L01"} {
		g.RemoveNodeLabel("n", name)
		remaining = deleteStr(remaining, name)
		assertLabelState(t, g, "n", id, remaining)
	}
	// Now 6 labels remain (small tier). Drain to 1 (singleton collapse) and 0.
	for _, name := range append([]string(nil), remaining...) {
		g.RemoveNodeLabel("n", name)
		remaining = deleteStr(remaining, name)
		assertLabelState(t, g, "n", id, remaining)
	}
	if got := g.NodeLabels("n"); len(got) != 0 {
		t.Fatalf("expected no labels after draining, got %v", got)
	}
}

// assertLabelState checks that HasNodeLabel, NodeLabels, and the nodeIdx
// bitmap all report exactly the want set for node key/id.
func assertLabelState(t *testing.T, g *Graph[string, float64], key string, id graph.NodeID, want []string) {
	t.Helper()
	got := g.NodeLabels(key)
	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if fmt.Sprint(got) != fmt.Sprint(wantSorted) {
		t.Fatalf("NodeLabels = %v, want %v", got, wantSorted)
	}
	wantSet := make(map[string]bool, len(want))
	for _, name := range want {
		wantSet[name] = true
		if !g.HasNodeLabel(key, name) {
			t.Fatalf("HasNodeLabel(%s) = false, want true", name)
		}
		lid := uint32(g.Registry().Intern(name))
		if !g.NodeIndex().Has(lid, id) {
			t.Fatalf("nodeIdx missing %s for id %d (bag/index out of lockstep)", name, id)
		}
	}
	// Every label ever interned that is NOT wanted must be absent from both
	// the bag and the index for this id.
	for _, name := range []string{"L00", "L01", "L02", "L03", "L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11"} {
		if wantSet[name] {
			continue
		}
		if g.HasNodeLabel(key, name) {
			t.Fatalf("HasNodeLabel(%s) = true, want false", name)
		}
		if lid, ok := g.Registry().Lookup(name); ok {
			if g.NodeIndex().Has(uint32(lid), id) {
				t.Fatalf("nodeIdx still has %s for id %d after removal (stale index entry)", name, id)
			}
		}
	}
}

func deleteStr(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// TestLabelBag_Unit exercises the labelBag state machine directly across all
// tier transitions, independent of the Graph wiring.
func TestLabelBag_Unit(t *testing.T) {
	t.Parallel()
	var b labelBag
	if b.len() != 0 || b.has(1) {
		t.Fatal("zero value must be empty")
	}
	// singleton
	b.add(7)
	if b.len() != 1 || !b.has(7) || b.has(8) {
		t.Fatalf("singleton state wrong: len=%d", b.len())
	}
	b.add(7) // idempotent
	if b.len() != 1 {
		t.Fatalf("idempotent add changed len: %d", b.len())
	}
	// small tier
	for lid := LabelID(8); lid <= 14; lid++ { // total now 8 (7..14)
		b.add(lid)
	}
	if b.len() != 8 || b.m != nil {
		t.Fatalf("expected small tier len 8 (m nil), got len=%d m!=nil=%v", b.len(), b.m != nil)
	}
	// promotion to map at the 9th distinct label
	b.add(99)
	if b.len() != 9 || b.m == nil {
		t.Fatalf("expected promotion to map at 9, got len=%d m==nil=%v", b.len(), b.m == nil)
	}
	if !b.has(99) || !b.has(7) {
		t.Fatal("map tier lost a member")
	}
	// collect via forEach
	seen := map[LabelID]bool{}
	b.forEach(func(lid LabelID) { seen[lid] = true })
	if len(seen) != 9 {
		t.Fatalf("forEach visited %d, want 9", len(seen))
	}
	// del from map tier — never demotes
	if b.del(99) {
		t.Fatal("del should not report empty")
	}
	if b.m == nil {
		t.Fatal("map tier must not demote after del")
	}
	if b.has(99) || b.len() != 8 {
		t.Fatalf("del did not remove from map: len=%d", b.len())
	}
	// drain fully
	all := []LabelID{}
	b.forEach(func(lid LabelID) { all = append(all, lid) })
	for i, lid := range all {
		empty := b.del(lid)
		wantEmpty := i == len(all)-1
		if empty != wantEmpty {
			t.Fatalf("del(%d) nowEmpty=%v, want %v", lid, empty, wantEmpty)
		}
	}
	if b.len() != 0 {
		t.Fatalf("expected empty after drain, len=%d", b.len())
	}

	// small -> singleton collapse on del.
	var c labelBag
	c.add(1)
	c.add(2)
	c.add(3)      // small slice of 3
	if c.del(2) { // -> 2 left, still small
		t.Fatal("unexpected empty")
	}
	if c.del(1) { // -> 1 left, must collapse to singleton
		t.Fatal("unexpected empty")
	}
	if c.len() != 1 || c.ids != nil || c.count != 1 || !c.has(3) {
		t.Fatalf("small->singleton collapse failed: len=%d ids=%v count=%d", c.len(), c.ids, c.count)
	}
}
