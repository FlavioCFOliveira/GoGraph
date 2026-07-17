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
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
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

// buildGridCSR builds an n-by-n 4-directional grid with unit edge weights as a
// directed CSR (each undirected grid edge is emitted in both directions). Node
// id(r,c) = r*n + c; src is the (0,0) corner and dst the (n-1,n-1) corner. Every
// corner-to-corner shortest path costs 2*(n-1), so a cost-ordered best-first
// enumerator must pop every cheaper prefix — a super-polynomially large set of
// self-avoiding walks — before it can return even the first complete path.
func buildGridCSR(tb testing.TB, n int) (*csr.CSR[int64], graph.NodeID, graph.NodeID) {
	tb.Helper()
	id := func(r, c int) int { return r*n + c }
	var edges []weightedEdge
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			u := id(r, c)
			if c+1 < n {
				right := id(r, c+1)
				edges = append(edges, weightedEdge{u, right, 1}, weightedEdge{right, u, 1})
			}
			if r+1 < n {
				down := id(r+1, c)
				edges = append(edges, weightedEdge{u, down, 1}, weightedEdge{down, u, 1})
			}
		}
	}
	c, a := buildWeightedCSR(tb, edges)
	src, ok := a.Mapper().Lookup(id(0, 0))
	if !ok {
		tb.Fatal("grid src key not found")
	}
	dst, ok := a.Mapper().Lookup(id(n-1, n-1))
	if !ok {
		tb.Fatal("grid dst key not found")
	}
	return c, src, dst
}

// TestKShortestPathsLoopless_GridBlowup_vs_Yen pins the #1997/#2006 contrast: on
// an n-by-n grid the best-first loopless enumerator's pop count grows
// super-polynomially (measured ~3,218 pops at 6x6 rising to ~5,750,066 at 10x10),
// while YenKShortest answers the same k-shortest query in polynomial time
// (~150µs, essentially flat across those sizes). The blowup is OBSERVED SAFELY
// through the bounded entry KShortestPathsLooplessCtxWithOpts with a MaxPops cap:
// the enumerator hits ErrResourceBudgetExceeded long before it can enumerate even
// one complete path, so the test never actually runs the exponential loop —
// whereas Yen returns k valid paths on the same graph at a tiny fraction of that
// work. A regression that made Yen exponential (or the loopless entry falsely
// finish within the budget) would flip one of these assertions.
func TestKShortestPathsLoopless_GridBlowup_vs_Yen(t *testing.T) {
	t.Parallel()
	const (
		n       = 12    // 12x12 grid: cheapest corner-to-corner cost is 2*(n-1)=22
		k       = 4     // request 4 shortest paths from both algorithms
		maxPops = 50000 // budget far below the millions of pops the loopless entry needs
	)
	c, src, dst := buildGridCSR(t, n)

	// Polynomial side: Yen returns k valid loopless paths cheaply.
	yen := YenKShortest(c, src, dst, k)
	if len(yen) != k {
		t.Fatalf("YenKShortest returned %d paths, want %d", len(yen), k)
	}
	for i, p := range yen {
		if len(p.Nodes) < 2 || p.Nodes[0] != src || p.Nodes[len(p.Nodes)-1] != dst {
			t.Fatalf("Yen path %d has bad endpoints: %v", i, p.Nodes)
		}
	}

	// Exponential side: the loopless enumerator cannot reach k paths within
	// maxPops pops on this grid, so the bounded entry surfaces the blowup as
	// ErrResourceBudgetExceeded instead of running the exponential loop.
	if _, err := KShortestPathsLooplessCtxWithOpts(
		context.Background(), c, src, dst, k,
		KShortestPathsLooplessOpts{MaxPops: maxPops},
	); !errors.Is(err, ErrResourceBudgetExceeded) {
		t.Fatalf("expected ErrResourceBudgetExceeded from the loopless enumerator on a %dx%d grid within %d pops, got %v", n, n, maxPops, err)
	}
}
