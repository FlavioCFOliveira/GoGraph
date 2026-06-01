package centrality

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestPageRank_Dangling runs PageRank on a directed web graph where
// 30% of nodes (140..199) are dangling sinks with no outgoing edges.
// The test verifies mass conservation, convergence, and positivity of
// live-node ranks.
func TestPageRank_Dangling(t *testing.T) {
	t.Parallel()

	const n = 200
	const nNonSink = 140 // nodes 0..139: have outgoing edges
	const nSink = 60     // nodes 140..199: dangling (no outgoing edges)

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	r := rand.New(rand.NewPCG(42, 43)) //nolint:gosec // deterministic

	// Non-sink nodes each get ~3 random outgoing edges.
	for i := 0; i < nNonSink; i++ {
		for k := 0; k < 3; k++ {
			dst := int(r.IntN(n))
			if err := a.AddEdge(i, dst, 1); err != nil {
				t.Fatalf("AddEdge(%d->%d): %v", i, dst, err)
			}
		}
	}
	// Nodes 140..199 intentionally have no outgoing edges (dangling sinks).
	// We do add them as edge destinations so they are live nodes.
	for i := nNonSink; i < n; i++ {
		// Point at least one non-sink at each sink so the sink is live.
		src := int(r.IntN(nNonSink))
		if err := a.AddEdge(src, i, 1); err != nil {
			t.Fatalf("AddEdge(%d->%d): %v", src, i, err)
		}
	}

	c := csr.BuildFromAdjList(a)

	opts := PageRankOptions{
		Damping:       0.85,
		MaxIterations: 200,
		Tolerance:     1e-9,
	}
	ranks, iters, err := PageRank(c, opts)

	// 1. No error.
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}

	// 2. Mass conservation over live nodes.
	liveMask := c.LiveMask()
	var totalMass float64
	for i, live := range liveMask {
		if live {
			totalMass += ranks[i]
		}
	}
	if math.Abs(totalMass-1.0) > 1e-9 {
		t.Fatalf("mass sum = %.15f, want 1.0 (delta %.3g)", totalMass, math.Abs(totalMass-1.0))
	}

	// 3. All live nodes carry strictly positive rank.
	for i, live := range liveMask {
		if live && ranks[i] <= 0 {
			t.Fatalf("live node %d has non-positive rank %.15f", i, ranks[i])
		}
	}

	// 4. Convergence within 100 iterations on a 200-node graph.
	if iters > 100 {
		t.Fatalf("iterations = %d, want <= 100", iters)
	}

	// 5. Ghost slots (nodes beyond liveMask) must be zero.
	for i := len(liveMask); i < len(ranks); i++ {
		if ranks[i] != 0 {
			t.Fatalf("ghost slot %d has non-zero rank %.15f", i, ranks[i])
		}
	}

	// Smoke check: dangling sink nodes should have higher average rank
	// than they would if they were unreachable. We only verify the mass
	// conservation invariant above; structural assertions are limited to
	// what the power-iteration algorithm guarantees.
	_ = nSink // used in const block above; silence potential lint
}
