package flow

import (
	"math"
	"math/rand/v2"
	"testing"
)

// bruteForceMinAssignment computes the minimum-cost perfect matching
// in an n×n cost matrix by exhaustive permutation enumeration.
// Suitable only for small n (n ≤ 8 before factorial blowup).
func bruteForceMinAssignment(cost []int, n int) int {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	best := math.MaxInt
	var enumerate func(k int)
	enumerate = func(k int) {
		if k == n {
			sum := 0
			for i, j := range perm {
				sum += cost[i*n+j]
			}
			if sum < best {
				best = sum
			}
			return
		}
		for i := k; i < n; i++ {
			perm[k], perm[i] = perm[i], perm[k]
			enumerate(k + 1)
			perm[k], perm[i] = perm[i], perm[k]
		}
	}
	enumerate(0)
	return best
}

// TestMinCostMaxFlow_Assignment verifies that MinCostMaxFlow on a
// bipartite-assignment flow network finds the minimum-cost perfect
// matching. The cost of the computed flow must equal the value
// returned by exhaustive brute-force enumeration (feasible for n=5).
//
// Network layout (2n+2 nodes, n=5):
//   - Node 0:        source
//   - Nodes 1..n:    left  (workers)
//   - Nodes n+1..2n: right (jobs)
//   - Node 2n+1:     sink
//
// Edges:
//   - source  -> each left node:  cap=1, cost=0
//   - left[i] -> right[j]:        cap=1, cost=costMatrix[i][j]
//   - each right node -> sink:    cap=1, cost=0
//
// A perfect matching sends exactly n units of flow; MinCostMaxFlow
// must minimise the total cost over all such matchings.
func TestMinCostMaxFlow_Assignment(t *testing.T) {
	t.Parallel()

	const n = 5
	r := rand.New(rand.NewPCG(997, 1009)) //nolint:gosec // deterministic

	cost := make([]int, n*n)
	for i := range cost {
		cost[i] = int(r.IntN(50)) + 1 // costs in [1, 50]
	}

	src := 0
	snk := 2*n + 1

	g := NewCostNetwork(2*n + 2)
	for i := 0; i < n; i++ {
		g.AddCostEdge(src, 1+i, 1, 0)   // source -> left[i]
		g.AddCostEdge(n+1+i, snk, 1, 0) // right[i] -> sink
		for j := 0; j < n; j++ {
			g.AddCostEdge(1+i, n+1+j, 1, cost[i*n+j]) // left[i] -> right[j]
		}
	}

	totalFlow, mcmfCost := MinCostMaxFlow(g, src, snk)

	if totalFlow != n {
		t.Fatalf("flow = %d, want %d (perfect matching)", totalFlow, n)
	}

	wantCost := bruteForceMinAssignment(cost, n)
	if mcmfCost != wantCost {
		t.Fatalf("cost = %d, want %d (minimum assignment)", mcmfCost, wantCost)
	}
}
