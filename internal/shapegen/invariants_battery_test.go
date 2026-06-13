package shapegen_test

// This file is the wiring (#1446) that puts internal/invariants' checkers on
// real generator output. Before it, the five exported invariant checkers had
// zero external consumers — the documented "invariant checkers" leg of the
// test battery (docs/test-battery.md) verified nothing about the library's
// actual graphs. Here each checker runs against a shape with a KNOWN topology,
// so a generator that produced a disconnected "connected" family, a cyclic
// "DAG", or a non-bipartite "bipartite" shape would be caught.
//
// It also makes internal/shapegen a genuine external importer of
// internal/invariants, which TestInvariantsHasExternalImporter asserts.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/invariants"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// mustBuild builds a shape with the default (empty) config or fails the test.
// The shape constructors bake in their own directedness, so an empty
// adjlist.Config is sufficient.
func mustBuild(t *testing.T, s shapegen.Shape[int, int64]) *lpg.Graph[int, int64] {
	t.Helper()
	g, err := s.Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("build %s: %v", s.Name(), err)
	}
	return g
}

// TestInvariants_AssertConnected_OnShapegen runs AssertConnected on shapes
// that are connected by construction.
func TestInvariants_AssertConnected_OnShapegen(t *testing.T) {
	invariants.AssertConnected[int, int64](t, mustBuild(t, shapegen.Path(12, false)))
	invariants.AssertConnected[int, int64](t, mustBuild(t, shapegen.Cycle(10, false)))
	invariants.AssertConnected[int, int64](t, mustBuild(t, shapegen.Star(8, true)))
}

// TestInvariants_AssertDAG_OnShapegen runs AssertDAG on directed acyclic
// shapes (a directed path edges i->i+1 carries no cycle).
func TestInvariants_AssertDAG_OnShapegen(t *testing.T) {
	invariants.AssertDAG[int, int64](t, mustBuild(t, shapegen.Path(16, true)))
}

// TestInvariants_AssertBipartite_OnShapegen runs AssertBipartite on shapes
// that are 2-colourable: a path, an EVEN cycle, and a star.
func TestInvariants_AssertBipartite_OnShapegen(t *testing.T) {
	invariants.AssertBipartite[int, int64](t, mustBuild(t, shapegen.Path(12, false)))
	invariants.AssertBipartite[int, int64](t, mustBuild(t, shapegen.Cycle(8, false)))
	invariants.AssertBipartite[int, int64](t, mustBuild(t, shapegen.Star(9, false)))
}

// TestInvariants_AssertShapeEqual_OnShapegen runs AssertShapeEqual over two
// independent builds of the same deterministic shape; their topology must be
// identical.
func TestInvariants_AssertShapeEqual_OnShapegen(t *testing.T) {
	a := mustBuild(t, shapegen.Path(20, false))
	b := mustBuild(t, shapegen.Path(20, false))
	invariants.AssertShapeEqual[int, int64](t, a, b)

	c := mustBuild(t, shapegen.BalancedBinary(4))
	d := mustBuild(t, shapegen.BalancedBinary(4))
	invariants.AssertShapeEqual[int, int64](t, c, d)
}

// TestInvariants_AssertDistanceBound_OnUnitPath runs AssertDistanceBound and
// BuildBFSDepths over a unit-weighted path. The shapegen catalogue uses a
// sentinel edge weight of 0 (its shapes are topology fixtures, not weighted
// graphs), so the BFS-hop ≤ Dijkstra-distance property is exercised here on
// an explicit unit-weighted graph where the bound is meaningful.
func TestInvariants_AssertDistanceBound_OnUnitPath(t *testing.T) {
	const n = 8
	g := lpg.New[int, int64](adjlist.Config{Directed: false})
	for i := 0; i < n-1; i++ {
		if err := g.AddEdge(i, i+1, 1); err != nil {
			t.Fatalf("AddEdge(%d->%d): %v", i, i+1, err)
		}
	}

	c := csr.BuildFromAdjList(g.AdjList())
	src, ok := g.AdjList().Mapper().Lookup(0)
	if !ok {
		t.Fatal("source node 0 not found in mapper")
	}

	depths, err := invariants.BuildBFSDepths[int64](context.Background(), c, src)
	if err != nil {
		t.Fatalf("BuildBFSDepths: %v", err)
	}
	dij, err := search.Dijkstra[int64](c, src)
	if err != nil {
		t.Fatalf("Dijkstra: %v", err)
	}
	invariants.AssertDistanceBound[int64](t, depths, dij)

	// Sanity: BFS must have reached every node on the connected path so the
	// checker is not vacuously passing over an empty depth map.
	if len(depths) != n {
		t.Fatalf("BFS reached %d nodes, want %d", len(depths), n)
	}
}
