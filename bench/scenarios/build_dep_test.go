// Package scenarios_test contains fixture-based scenario tests that
// exercise realistic end-to-end pipelines against the gograph APIs.
package scenarios_test

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestBuildDep_TopoAndSCC builds a synthetic Linux-kernel-like make-dependency
// DAG (50 nodes, ~150 directed edges, deterministic), then verifies that
// TopologicalSort produces a valid linear extension and TarjanSCC returns
// only singleton components (a necessary property of any DAG).
func TestBuildDep_TopoAndSCC(t *testing.T) {
	t.Parallel()

	const (
		n    = 50
		seed = 42
	)

	// Build a random DAG: only add edge i→j when i < j, guaranteeing acyclicity.
	// Target ≈150 edges: accept each candidate pair with probability 150/(n*(n-1)/2) ≈ 12%.
	rng := rand.New(rand.NewPCG(seed, 0)) //nolint:gosec // deterministic PRNG for test-data generation, not cryptography
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})

	type edge struct{ u, v int }
	var edges []edge

	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			// Accept with probability ≈ 12/100 to hit ~150 edges.
			if rng.IntN(100) < 12 {
				if err := a.AddEdge(i, j, struct{}{}); err != nil {
					t.Fatalf("AddEdge(%d→%d): %v", i, j, err)
				}
				edges = append(edges, edge{i, j})
			}
		}
	}

	// Ensure the graph is non-trivial.
	if len(edges) == 0 {
		t.Fatal("no edges generated — check RNG parameters")
	}

	c := csr.BuildFromAdjList(a)

	// --- TopologicalSort ---
	order, err := search.TopologicalSort(c)
	if err != nil {
		t.Fatalf("TopologicalSort: %v", err)
	}

	// Build position map: user key → position in topo order.
	pos := make(map[int]int, len(order))
	for idx, id := range order {
		key, ok := a.Mapper().Resolve(id)
		if !ok {
			t.Fatalf("Resolve(%d): NodeID not found in mapper", id)
		}
		pos[key] = idx
	}

	// Verify linear extension: for every edge u→v, pos(u) < pos(v).
	for _, e := range edges {
		pu, uOK := pos[e.u]
		pv, vOK := pos[e.v]
		if !uOK {
			t.Errorf("node %d missing from topo order", e.u)
			continue
		}
		if !vOK {
			t.Errorf("node %d missing from topo order", e.v)
			continue
		}
		if pu >= pv {
			t.Errorf("edge %d→%d violates topo order: pos[%d]=%d, pos[%d]=%d",
				e.u, e.v, e.u, pu, e.v, pv)
		}
	}

	// --- TarjanSCC ---
	sccs := search.TarjanSCC(c)

	// Every SCC in a DAG must be a singleton.
	for i, comp := range sccs {
		if len(comp) != 1 {
			t.Errorf("SCC %d has %d nodes (want 1): DAG must produce only singletons", i, len(comp))
		}
	}
}
