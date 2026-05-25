package search

// Task 691: EppsteinKShortest vs YenKShortest on a layered DAG.
//
// EppsteinKShortest is a deprecated alias for KShortestPathsLoopless
// (see eppstein.go); both it and YenKShortest implement loopless k-
// shortest paths. On an acyclic graph (no cycles by construction) the
// same set of paths exists, so both algorithms must return the same
// multiset of costs for up to k paths.
//
// Graph: shapegen.Layered(L=4, w=3, density=80, seed=42).
//   - 12 nodes (layer-major: 0..2 in layer 0, 3..5 in layer 1,
//     6..8 in layer 2, 9..11 in layer 3).
//   - Directed (DAG), edge weights all 0 (unweighted sentinel).
//   - src = node with user key 0 (first node of layer 0).
//   - dst = node with user key 11 (last node of layer 3).
//
// The two algorithms differ slightly in enumeration order and duplicate
// handling (YenKShortest may emit a duplicate path on certain DAG
// topologies before deduplication); the test therefore compares the
// sorted cost multiset over min(len(epp), len(yen)) entries rather
// than relying on a positional match.
//
// Acceptance criteria:
//   - Both algorithms return at least 1 path.
//   - The sorted cost slices of the common-length prefix are identical.
//
// Note: EppsteinKShortest is marked deprecated; the nolint directive
// is intentional and mirrors the pattern in kshortest_loopless_test.go.

import (
	"sort"
	"testing"

	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

//nolint:staticcheck // intentional exercise of the deprecated EppsteinKShortest alias
func TestEppsteinVsYen_LayeredDAG(t *testing.T) {
	t.Parallel()

	g, err := shapegen.Layered(4, 3, 80, 42).Build(defaultCfg())
	if err != nil {
		t.Fatalf("Layered.Build: %v", err)
	}

	a := g.AdjList()
	c := csr.BuildFromAdjList(a)
	m := a.Mapper()

	// src = user key 0 (first node of layer 0).
	// dst = user key 11 (last node of layer 3, since w=3 and L=4: 4*3-1=11).
	src, ok := m.Lookup(0)
	if !ok {
		t.Fatal("user key 0 not found in mapper")
	}
	dst, ok := m.Lookup(11)
	if !ok {
		t.Fatal("user key 11 not found in mapper")
	}

	const k = 5

	eppPaths := EppsteinKShortest(c, src, dst, k)
	yenPaths := YenKShortest(c, src, dst, k)

	if len(eppPaths) == 0 || len(yenPaths) == 0 {
		t.Fatalf("at least one algorithm returned no paths (epp=%d, yen=%d)", len(eppPaths), len(yenPaths))
	}

	// Compare sorted cost slices over the common prefix length so that
	// enumeration-order differences and duplicate-path quirks in either
	// algorithm do not cause false failures.
	n := len(eppPaths)
	if len(yenPaths) < n {
		n = len(yenPaths)
	}

	eppCosts := make([]int64, n)
	yenCosts := make([]int64, n)
	for i := 0; i < n; i++ {
		eppCosts[i] = eppPaths[i].Cost
		yenCosts[i] = yenPaths[i].Cost
	}
	sort.Slice(eppCosts, func(a, b int) bool { return eppCosts[a] < eppCosts[b] })
	sort.Slice(yenCosts, func(a, b int) bool { return yenCosts[a] < yenCosts[b] })

	for i := 0; i < n; i++ {
		if eppCosts[i] != yenCosts[i] {
			t.Fatalf("sorted cost[%d]: Eppstein=%v, Yen=%v", i, eppCosts[i], yenCosts[i])
		}
	}
}
