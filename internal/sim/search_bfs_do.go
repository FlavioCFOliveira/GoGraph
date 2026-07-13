package sim

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// bfsDoSalt keeps the direction-optimising-BFS check's draw stream disjoint from
// every other per-tick search check.
const bfsDoSalt uint64 = 0xb75d_0a13_c9e2_46f8

// bfsDoViolations exercises [search.BFSDirectionOpt] on a scale-free / supernode
// shape (a hub adjacent to every other vertex, plus a ring among the rest),
// which the rest of the DST never builds. The direction-optimising algorithm
// switches between top-down and bottom-up expansion based on frontier density,
// so a supernode's giant single-hop frontier is exactly the structure that
// drives the bottom-up phase — but the check asserts CORRECTNESS regardless of
// which phase runs.
//
// BFSDirectionOpt expects a symmetric (undirected) CSR, which [symCSRFromEdges]
// builds. For each of a few deterministic sources the traversal's reachable set
// and per-node depths are compared against a from-scratch BFS reference
// ([bfsDistancesFromAdj]) that shares no code with the algorithm under test:
// every reached node must carry the true shortest hop distance, and no
// unreachable node may be visited (nor any reachable node skipped).
func bfsDoViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ bfsDoSalt)
	n, edges := bfsDoGenScaleFree(seed)
	c := symCSRFromEdges(n, edges)
	adj := symAdjacency(n, edges)

	var vs []Violation
	for _, src := range bfsDoSources(n) {
		// Collect the algorithm's (node -> depth) map. The visit callback always
		// continues; a node visited more than once would be an algorithm bug.
		depth := make([]int, n)
		visited := make([]bool, n)
		var revisit bool
		search.BFSDirectionOpt(c, graph.NodeID(src), func(node graph.NodeID, d int) bool {
			id := int(node)
			if id < 0 || id >= n {
				return true
			}
			if visited[id] {
				revisit = true
			}
			visited[id] = true
			depth[id] = d
			return true
		})
		if revisit {
			vs = append(vs, bfsDoDiverge(tick, fmt.Sprintf("src=%d: a node was visited more than once", src)))
		}

		ref := bfsDistancesFromAdj(adj, n, src)
		for v := 0; v < n; v++ {
			reachable := ref[v] >= 0
			if visited[v] != reachable {
				vs = append(vs, bfsDoDiverge(tick, fmt.Sprintf(
					"src=%d node=%d: visited=%v but plain-BFS reachable=%v (n=%d)", src, v, visited[v], reachable, n)))
				break
			}
			if reachable && depth[v] != ref[v] {
				vs = append(vs, bfsDoDiverge(tick, fmt.Sprintf(
					"src=%d node=%d: BFS-DO depth %d != plain-BFS distance %d (n=%d)", src, v, depth[v], ref[v], n)))
				break
			}
		}
	}
	return vs
}

// bfsDoSources returns a small deterministic set of sources: the hub (0), a
// leaf (1), and a mid vertex, exercising expansion both outward from the hub and
// inward toward it.
func bfsDoSources(n int) []int {
	switch {
	case n <= 0:
		return nil
	case n == 1:
		return []int{0}
	case n == 2:
		return []int{0, 1}
	default:
		return []int{0, 1, n / 2}
	}
}

// bfsDoGenScaleFree builds a deterministic supernode/scale-free undirected
// graph: n nodes (48..96), a hub (0) adjacent to every other vertex, plus a ring
// 1-2-...-(n-1)-1 among the non-hub vertices. The hub's degree is n-1 (a genuine
// supernode) while every other vertex has degree 3, giving the skewed degree
// distribution that drives the direction-optimising phase switch. Edges are
// emitted in a fixed order with no duplicates, so the fixture is a pure function
// of the single size draw.
func bfsDoGenScaleFree(seed *Seed) (int, [][2]int) {
	n := 48 + seed.IntN(49) // 48..96
	edges := make([][2]int, 0, 2*n)
	// Hub adjacency: 0 - i for every i in [1, n).
	for i := 1; i < n; i++ {
		edges = append(edges, [2]int{0, i})
	}
	// Ring among the non-hub vertices [1, n): i - (i+1), closing (n-1) - 1.
	for i := 1; i < n-1; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	if n >= 4 {
		// Close the ring. Distinct from every (i,i+1) edge only when there are at
		// least three non-hub vertices (n >= 4); n is always >= 48 here.
		edges = append(edges, [2]int{n - 1, 1})
	}
	return n, edges
}

// bfsDoDiverge builds a single direction-optimising-BFS divergence violation.
func bfsDoDiverge(tick int64, msg string) Violation {
	return Violation{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:BFSDirectionOpt", Message: msg}
}
