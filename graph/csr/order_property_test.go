package csr_test

// order_property_test.go — property and configuration coverage for the CSR
// within-source ordering (rmp #2144).
//
// order_test.go (in package csr) covers OrderRuns directly against hand-built
// arrays. This file covers the same invariants THROUGH BuildFromAdjList over a real
// adjacency, across every configuration the adjacency supports, and under rapid so
// the shapes are not just the ones I thought to write down.
//
// The invariants asserted are the three that make the reordering observationally
// invisible except in cost:
//
//  1. the neighbour MULTISET of every source is unchanged;
//  2. the handle-to-destination MAPPING is unchanged — a mis-permuted handle column
//     would preserve (1) while silently reassigning edge identity;
//  3. the WITHIN-RUN ORDINAL of each (destination, handle) pair is unchanged, which
//     is what cypher's buildEdgeTypeFilter reads to resolve a parallel edge's
//     relationship type.
//
// Layer: short.

import (
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// slotRec is one CSR slot, carrying everything a permutation must keep together.
type slotRec struct {
	src    uint64
	dst    graph.NodeID
	handle uint64
	// weight is what makes an equal-KEY permutation detectable. Slots that share
	// (dst, handle) — handle-0 slots, or any handle-less entry with repeated
	// destinations — are indistinguishable by the sort key, so recording only
	// (dst, handle, ordinal) leaves an unstable reorder INVISIBLE: the tuple set is
	// unchanged. A distinct per-slot weight is the payload that must travel with
	// its slot, so a stability break shows up as a weight paired with the wrong
	// ordinal. Verified by sabotage: making mergeRun take the right half on ties
	// leaves the weightless form of this test green and trips this one.
	weight  int64
	ordinal int // position of this slot within its (src, dst) run
}

// collectSlots walks a CSR and records every slot with its within-run ordinal.
func collectSlots(c *csr.CSR[int64]) []slotRec {
	verts, edges, handles := c.VerticesSlice(), c.EdgesSlice(), c.HandlesSlice()
	weights := c.WeightsSlice()
	var out []slotRec
	for s := 0; s+1 < len(verts); s++ {
		seen := map[graph.NodeID]int{}
		for p := verts[s]; p < verts[s+1]; p++ {
			var h uint64
			if handles != nil {
				h = handles[p]
			}
			var w int64
			if weights != nil {
				w = weights[p]
			}
			seen[edges[p]]++
			out = append(out, slotRec{uint64(s), edges[p], h, w, seen[edges[p]]})
		}
	}
	return out
}

// collectAdjSlots is the same record set read from the ADJACENCY, which the
// ordering never touches — the independent oracle.
func collectAdjSlots(a *adjlist.AdjList[int, int64]) []slotRec {
	var out []slotRec
	maxID := a.MaxNodeID()
	for id := uint64(0); id <= uint64(maxID); id++ {
		nb, ws, handles := a.LoadEntryH(graph.NodeID(id))
		seen := map[graph.NodeID]int{}
		for i, dst := range nb {
			var h uint64
			if handles != nil {
				h = handles[i]
			}
			var w int64
			if ws != nil {
				w = ws[i]
			}
			seen[dst]++
			out = append(out, slotRec{id, dst, h, w, seen[dst]})
		}
	}
	return out
}

// canonical sorts a slot set so two sets can be compared as multisets.
func canonical(rs []slotRec) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		switch {
		case a.src != b.src:
			return a.src < b.src
		case a.dst != b.dst:
			return a.dst < b.dst
		case a.handle != b.handle:
			return a.handle < b.handle
		case a.ordinal != b.ordinal:
			return a.ordinal < b.ordinal
		default:
			return a.weight < b.weight
		}
	})
}

// assertOrderedAndFaithful is the shared assertion: the CSR's runs are ordered, and
// its slot set matches the adjacency's as a multiset including handles and
// within-run ordinals.
func assertOrderedAndFaithful(t rapidOrT, a *adjlist.AdjList[int, int64], c *csr.CSR[int64], label string) {
	if !c.RunsOrdered() {
		t.Errorf("%s: CSR runs are not ordered", label)
		return
	}
	got, want := collectSlots(c), collectAdjSlots(a)
	canonical(got)
	canonical(want)
	if len(got) != len(want) {
		t.Errorf("%s: slot count %d, want %d", label, len(got), len(want))
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: slot %d = %+v, want %+v", label, i, got[i], want[i])
			return
		}
	}
}

// rapidOrT lets the helpers serve both *testing.T and *rapid.T.
type rapidOrT interface {
	Errorf(format string, args ...any)
}

// TestOrdering_PreservesSlotIdentity_Rapid draws random multigraphs — including
// degrees on both sides of the insertion-sort cutoff and destination spaces small
// enough to force parallel edges — and asserts all three invariants.
func TestOrdering_PreservesSlotIdentity_Rapid(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		nSrc := rapid.IntRange(1, 12).Draw(rt, "sources")
		maxDeg := rapid.IntRange(0, 70).Draw(rt, "maxDegree")
		dstSpace := rapid.IntRange(1, 12).Draw(rt, "dstSpace")
		useHandles := rapid.Bool().Draw(rt, "useHandles")

		a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
		h := uint64(0)
		for s := 0; s < nSrc; s++ {
			deg := rapid.IntRange(0, maxDeg).Draw(rt, "degree")
			for k := 0; k < deg; k++ {
				dst := rapid.IntRange(0, dstSpace).Draw(rt, "dst")
				var err error
				if useHandles {
					h++
					err = a.AddEdgeH(s, 1000+dst, int64(k), h)
				} else {
					err = a.AddEdge(s, 1000+dst, int64(k))
				}
				if err != nil {
					rt.Fatalf("AddEdge: %v", err)
				}
			}
		}
		assertOrderedAndFaithful(rt, a, csr.BuildFromAdjList(a), "rapid")
	})
}

// TestOrdering_ConfigurationMatrix covers every adjacency configuration that
// changes which parallel columns exist, since the ordering must permute exactly the
// ones present: undirected (mirrored edges), weightless (nil weights column),
// non-multigraph (no parallel edges), and the handle-less variant of each.
func TestOrdering_ConfigurationMatrix(t *testing.T) {
	t.Parallel()
	for _, cfg := range []struct {
		name string
		cfg  adjlist.Config
	}{
		{"directed-multigraph", adjlist.Config{Directed: true, Multigraph: true}},
		{"directed-simple", adjlist.Config{Directed: true}},
		{"undirected-multigraph", adjlist.Config{Multigraph: true}},
		{"undirected-simple", adjlist.Config{}},
		{"directed-multigraph-weightless", adjlist.Config{Directed: true, Multigraph: true, Weightless: true}},
		{"undirected-weightless", adjlist.Config{Weightless: true}},
	} {
		for _, withHandles := range []bool{false, true} {
			name := cfg.name
			if withHandles {
				name += "/handles"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				a := adjlist.New[int, int64](adjlist.Config(cfg.cfg))
				h := uint64(0)
				// Descending destinations past the cutoff so ordering must work,
				// plus repeated destinations to exercise parallel runs where the
				// configuration permits them.
				for s := 0; s < 4; s++ {
					for d := 60; d >= 0; d-- {
						reps := 1
						if cfg.cfg.Multigraph && d%7 == 0 {
							reps = 3
						}
						for r := 0; r < reps; r++ {
							var err error
							if withHandles {
								h++
								err = a.AddEdgeH(s, 100+d, int64(d), h)
							} else {
								err = a.AddEdge(s, 100+d, int64(d))
							}
							if err != nil {
								t.Fatalf("AddEdge: %v", err)
							}
						}
					}
				}
				assertOrderedAndFaithful(t, a, csr.BuildFromAdjList(a), name)
			})
		}
	}
}

// TestOrdering_SurvivesCompaction covers the removal and compaction path: a
// surviving slot must keep its ORIGINAL handle (handles are never renumbered or
// reused), and the CSR built afterwards must still be ordered and faithful.
func TestOrdering_SurvivesCompaction(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	// Three parallel edges to each of 40 destinations, inserted descending.
	handleOf := map[[2]int][]uint64{}
	h := uint64(0)
	for d := 39; d >= 0; d-- {
		for r := 0; r < 3; r++ {
			h++
			if err := a.AddEdgeH(0, 100+d, int64(d), h); err != nil {
				t.Fatal(err)
			}
			handleOf[[2]int{0, 100 + d}] = append(handleOf[[2]int{0, 100 + d}], h)
		}
	}
	assertOrderedAndFaithful(t, a, csr.BuildFromAdjList(a), "before removal")

	// Remove the MIDDLE parallel instance of every other destination by handle, so
	// compaction shifts the tail down and a surviving slot must keep its handle.
	removed := map[uint64]bool{}
	for d := 0; d < 40; d += 2 {
		hs := handleOf[[2]int{0, 100 + d}]
		if !a.RemoveEdgeByHandle(0, 100+d, hs[1]) {
			t.Fatalf("RemoveEdgeByHandle(%d, %d) reported nothing removed", 0, 100+d)
		}
		removed[hs[1]] = true
	}
	c := csr.BuildFromAdjList(a)
	assertOrderedAndFaithful(t, a, c, "after removal")

	// No removed handle may survive, and every surviving handle must be one that
	// was originally issued — never renumbered.
	issued := map[uint64]bool{}
	for _, hs := range handleOf {
		for _, x := range hs {
			issued[x] = true
		}
	}
	for _, got := range c.HandlesSlice() {
		if removed[got] {
			t.Errorf("removed handle %d survived compaction", got)
		}
		if !issued[got] {
			t.Errorf("handle %d was never issued; handles must not be renumbered", got)
		}
	}
	if want := 40*3 - 20; len(c.EdgesSlice()) != want {
		t.Errorf("edge count after removal = %d, want %d", len(c.EdgesSlice()), want)
	}
}
