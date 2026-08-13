package lpg

// mvcc_reclaim_test.go — MVCC P6 (rmp #2284): reclamation must free the
// unreachable and nothing else.
//
// Layer: short.
//
// The asymmetry is the point. Freeing too little wastes memory; freeing too
// much leaves a live reader unable to reconstruct the version it is entitled
// to, which is silent data loss rather than a fault anyone would notice. Every
// test below therefore checks the second direction as well as the first.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

func reclaimGraph(t *testing.T) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.EnableLabelDeltas()
	g.EnablePropDeltas()
	// THE GRAPH'S OWN SWEEPER IS SUSPENDED FOR THE TEST'S DURATION (rmp #2424).
	//
	// Every test on this fixture is a WHITE-BOX test of a reclaimer as a function of
	// the watermark it is GIVEN: it counts deltas and calls ReclaimVersions with a
	// watermark it computes itself, frequently from a private mvcc.Horizon the graph
	// knows nothing about. A background pass is entitled to free those same records —
	// the graph's own horizon has no reader in it — so a concurrent sweep makes the
	// counts non-deterministic. Two of these tests failed exactly that way ("three
	// label writes left 2 deltas, want 3") the moment a sub-threshold charge began
	// starting a sweeper, and they had been passing only because a handful of direct
	// writes left none alive.
	//
	// EnterHolding claims a slot WITHOUT publishing an instant, which is the documented
	// hold-everything-back state: Horizon.Oldest reports zero, so a pass that does run
	// frees nothing. That suspends the sweeper's EFFECT rather than its existence,
	// which is what these tests need and all they need.
	slot := g.horizon.EnterHolding()
	t.Cleanup(func() { g.horizon.Leave(slot) })
	return g
}

func TestReclaimVersions_FreesOnlyTheUnreachable(t *testing.T) {
	g := reclaimGraph(t)
	id, ok := g.adj.Mapper().Lookup("a")
	if !ok {
		t.Fatal("a not interned")
	}

	if err := g.SetNodeLabel("a", "L1"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	afterL1 := g.readTS()
	if err := g.SetNodeLabel("a", "L2"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	afterL2 := g.readTS()
	if err := g.SetNodeLabel("a", "L3"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if g.LabelDeltaCount() != 3 {
		t.Fatalf("three label writes left %d deltas, want 3", g.LabelDeltaCount())
	}

	if n := g.ReclaimVersions(0); n != 0 {
		t.Fatalf("ReclaimVersions(0) freed %d; zero means reclaim NOTHING", n)
	}

	// Watermark at afterL2: no reader can be older, so the versions behind
	// L2 are unreachable and L3's is not.
	freed := g.ReclaimVersions(afterL2)
	if freed == 0 {
		t.Fatal("nothing was freed with a watermark past two of three versions")
	}
	l1, l2, l3 := g.reg.Intern("L1"), g.reg.Intern("L2"), g.reg.Intern("L3")

	// The version AT the watermark must survive: it is what a reader that
	// started then is entitled to see.
	atWM := g.labelBagAsOf(id, afterL2, 0)
	if !atWM.has(l1) || !atWM.has(l2) || atWM.has(l3) {
		t.Fatalf("a reader at the watermark sees L1=%v L2=%v L3=%v, want true true false — "+
			"reclamation freed a version a live reader is entitled to",
			atWM.has(l1), atWM.has(l2), atWM.has(l3))
	}
	// The current version is untouched.
	now := g.labelBagAsOf(id, g.readTS(), 0)
	if !now.has(l1) || !now.has(l2) || !now.has(l3) {
		t.Fatal("reclamation disturbed the current version")
	}
	_ = afterL1
}

func TestReclaimVersions_PropertiesToo(t *testing.T) {
	g := reclaimGraph(t)
	id, _ := g.adj.Mapper().Lookup("a")
	keyID := g.propKeys().Intern("w")

	for i := int64(1); i <= 3; i++ {
		if err := g.SetNodeProperty("a", "w", Int64Value(i)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if g.PropDeltaCount() != 3 {
		t.Fatalf("three property writes left %d deltas, want 3", g.PropDeltaCount())
	}
	wm := g.readTS()
	if n := g.ReclaimVersions(wm); n != 3 {
		t.Fatalf("freed %d of 3 with the watermark at the present", n)
	}
	if g.PropDeltaCount() != 0 {
		t.Fatalf("%d property deltas remain after full reclamation", g.PropDeltaCount())
	}
	bag := g.propBagAsOf(id, wm, 0)
	if v, had := bag.get(keyID); !had || v.v != int64(3) {
		t.Fatalf("the current value reads %v/%v after reclamation, want 3", v.v, had)
	}
}

// TestReclaimVersions_HeldBackByAReader wires reclamation to the horizon, which
// is the only way it is ever meant to be driven.
func TestReclaimVersions_HeldBackByAReader(t *testing.T) {
	g := reclaimGraph(t)
	id, _ := g.adj.Mapper().Lookup("a")
	old := g.readTS()
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	lid := g.reg.Intern("L")

	var h mvcc.Horizon
	slot := h.Enter(old) // a reader that began before the write
	if n := g.ReclaimVersions(h.Oldest(g.readTS())); n != 0 {
		t.Fatalf("freed %d records while a reader older than them was registered", n)
	}
	bag := g.labelBagAsOf(id, old, 0)
	if bag.has(lid) {
		t.Fatal("the long reader can see a label committed after it started")
	}
	h.Leave(slot)

	if n := g.ReclaimVersions(h.Oldest(g.readTS())); n != 1 {
		t.Fatalf("freed %d once the reader left, want 1", n)
	}
	if g.LabelDeltaCount() != 0 {
		t.Fatalf("%d deltas remain with no reader active", g.LabelDeltaCount())
	}
}

// TestReclaimVersions_ShrinksTheShardIndex pins that a fully reclaimed shard
// drops its side map, so a graph that churned once does not pay a map lookup
// for ever afterwards.
func TestReclaimVersions_ShrinksTheShardIndex(t *testing.T) {
	g := reclaimGraph(t)
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	id, _ := g.adj.Mapper().Lookup("a")
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	present := sh.d != nil
	sh.mu.RUnlock()
	if !present {
		t.Fatal("the shard has no side map after a versioned write")
	}
	g.ReclaimVersions(g.readTS())
	sh.mu.RLock()
	present = sh.d != nil
	sh.mu.RUnlock()
	if present {
		t.Fatal("the shard kept its side map after every version was reclaimed")
	}
}
