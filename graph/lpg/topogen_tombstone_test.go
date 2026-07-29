package lpg

// topogen_tombstone_test.go — tombstone transitions must advance the edge-topology
// generation (rmp #2143).
//
// TopoGeneration is the documented invalidation epoch for "any CSR-position-keyed
// cache". Tombstoning changes the LIVE topology those caches are derived from,
// because csr.BuildFromAdjListLive omits arcs incident to a tombstoned node
// (#1790) — yet RemoveNode used to record the tombstone without advancing the
// counter, so a cache built beforehand kept describing arcs that were no longer
// live. cypher's CSR pair cache made that observable as wrong query results.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestTopoGeneration_RemoveNodeAdvances(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatal(err)
	}

	before := g.TopoGeneration()
	g.RemoveNode("b")
	after := g.TopoGeneration()
	if after == before {
		t.Errorf("RemoveNode left TopoGeneration at %d; a tombstone changes the live "+
			"topology, so every CSR-position-keyed cache must be invalidated", before)
	}

	// Idempotent: removing an already-tombstoned node changes nothing, so it must
	// NOT advance the epoch and needlessly invalidate every cache.
	again := g.TopoGeneration()
	g.RemoveNode("b")
	if g.TopoGeneration() != again {
		t.Error("removing an already-tombstoned node advanced TopoGeneration; the " +
			"no-op must not invalidate caches")
	}
}

func TestTopoGeneration_ReviveAdvances(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatal(err)
	}
	g.RemoveNode("b")

	before := g.TopoGeneration()
	if err := g.AddNode("b"); err != nil { // revives
		t.Fatal(err)
	}
	if g.TopoGeneration() == before {
		t.Errorf("reviving a tombstoned node left TopoGeneration at %d", before)
	}
	if g.TombstoneCount() != 0 {
		t.Errorf("TombstoneCount = %d after reviving, want 0", g.TombstoneCount())
	}
}

// TestTopoGeneration_TombstoneCountIsNotASoundKey demonstrates WHY the generation
// has to advance on both transitions rather than a cache keying on the tombstone
// count: removing one node and reviving another leaves the count identical while
// the live set differs, so a count-keyed cache would wrongly hit. The generation
// must differ across that pair.
func TestTopoGeneration_TombstoneCountIsNotASoundKey(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"b", "c"} {
		if err := g.AddEdge("a", n, 1); err != nil {
			t.Fatal(err)
		}
	}

	g.RemoveNode("b")
	countAfterFirst, genAfterFirst := g.TombstoneCount(), g.TopoGeneration()

	if err := g.AddNode("b"); err != nil { // revive b
		t.Fatal(err)
	}
	g.RemoveNode("c") // tombstone c instead

	if g.TombstoneCount() != countAfterFirst {
		t.Fatalf("fixture broken: tombstone count %d != %d, so this does not model "+
			"the ABA case", g.TombstoneCount(), countAfterFirst)
	}
	if g.TopoGeneration() == genAfterFirst {
		t.Error("TopoGeneration is unchanged across a remove-then-revive-then-remove " +
			"sequence whose live set differs; it is therefore not a sound cache key")
	}
}
