package scenarios_test

import (
	"context"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// TestRecommendation_PersonalisedPageRank builds a 10-node graph with two
// dense 5-node undirected cliques connected by a single bridge edge, runs
// PersonalisedPushPageRankCtx seeded at node 0 (inside clique A), and verifies:
//
//  1. The rank vector sums to ≈ 1.0 (within 1e-3).
//  2. scores[srcID] is the highest score in the vector.
//  3. Every cluster-A node (nodes 0–4) scores strictly higher than every
//     cluster-B node (nodes 5–9).
//
// Topology:
//
//	Clique A:  0–1, 0–2, 0–3, 0–4, 1–2, 1–3, 1–4, 2–3, 2–4, 3–4  (undirected)
//	Clique B:  5–6, 5–7, 5–8, 5–9, 6–7, 6–8, 6–9, 7–8, 7–9, 8–9  (undirected)
//	Bridge:    4–5  (single undirected edge)
func TestRecommendation_PersonalisedPageRank(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})

	addClique := func(start, end int) {
		for i := start; i <= end; i++ {
			for j := i + 1; j <= end; j++ {
				if err := a.AddEdge(i, j, struct{}{}); err != nil {
					t.Fatalf("AddEdge(%d–%d): %v", i, j, err)
				}
			}
		}
	}

	addClique(0, 4) // clique A
	addClique(5, 9) // clique B

	// Single bridge between the two cliques.
	if err := a.AddEdge(4, 5, struct{}{}); err != nil {
		t.Fatalf("AddEdge bridge (4–5): %v", err)
	}

	c := csr.BuildFromAdjList(a)

	srcID, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatal("node 0 not interned")
	}

	scores, err := centrality.PersonalisedPushPageRankCtx(context.Background(), c, srcID, centrality.DefaultPPRPushOptions())
	if err != nil {
		t.Fatalf("PersonalisedPushPageRankCtx: %v", err)
	}
	if scores == nil {
		t.Fatal("scores is nil")
	}

	// --- Rank sum ≈ 1.0 ---
	var sum float64
	for _, s := range scores {
		sum += s
	}
	if math.Abs(sum-1.0) > 1e-3 {
		t.Errorf("rank sum = %.6f, want ≈ 1.0 (±1e-3)", sum)
	}

	// --- scores[src] must be the maximum ---
	maxScore := -1.0
	maxIdx := -1
	for i, s := range scores {
		if s > maxScore {
			maxScore = s
			maxIdx = i
		}
	}
	if graph.NodeID(maxIdx) != srcID {
		t.Errorf("highest rank at NodeID %d (score=%.6f), want src NodeID %d (score=%.6f)",
			maxIdx, maxScore, srcID, scores[srcID])
	}

	// --- Cluster A > Cluster B (all pairs) ---
	nodeID := func(key int) graph.NodeID {
		id, _ := a.Mapper().Lookup(key)
		return id
	}

	minA := math.Inf(1)
	for i := 0; i < 5; i++ {
		if s := scores[nodeID(i)]; s < minA {
			minA = s
		}
	}
	maxB := math.Inf(-1)
	for i := 5; i < 10; i++ {
		if s := scores[nodeID(i)]; s > maxB {
			maxB = s
		}
	}
	if minA <= maxB {
		t.Errorf("cluster A min score (%.6f) not strictly greater than cluster B max score (%.6f)",
			minA, maxB)
	}
}
