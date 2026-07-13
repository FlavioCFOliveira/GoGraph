package sim

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/search"
)

// triangleViolations cross-checks the search package's triangle counters against
// an independent definition-based reference over the SIMPLE undirected view of
// the live graph, and asserts the serial and parallel counters agree EXACTLY.
//
// The undirected view is the de-duplicated, self-loop-free symmetric edge set
// [undirectedEdges] already builds for the MST check; materialised as the
// symmetric CSR [undirectedWeightedCSR], it satisfies CountTriangles' simple-graph
// precondition (no self-loops, no parallel edges — a parallel edge would silently
// over-count). The reference ([naiveTriangleCounts]) enumerates each triangle
// exactly once by its minimum-NodeID vertex over adjacency sets it builds from
// scratch, sharing no code with the node-iterator/degree-rank algorithm under
// test, so agreement is genuine evidence. Both the total (a single integer) and
// the per-node counts (one integer per NodeID) are exact-equality comparisons —
// triangle counts are integers, so no tolerance applies. Serial vs parallel is
// likewise exact: the triangle monoid is integer addition, order-independent, so
// a pinned worker count must reproduce the serial answer bit-for-bit.
func triangleViolations(tick int64, g *nameGraph) []Violation {
	n := len(g.names)
	if n == 0 {
		return nil
	}
	edges := undirectedEdges(g)
	c := undirectedWeightedCSR(g, edges)

	total, perNode := search.CountTriangles(c)
	totalPar, perNodePar := search.CountTrianglesParallel(c, searchParallelWorkers)
	refTotal, refPerNode := naiveTriangleCounts(n, edges)

	var vs []Violation
	if total != refTotal {
		vs = append(vs, triangleDiverge(tick, "CountTriangles",
			fmt.Sprintf("total triangle count got %d want %d", total, refTotal)))
	}
	if vio := triangleComparePerNode(tick, "CountTriangles", g, perNode, refPerNode); vio != nil {
		vs = append(vs, vio...)
	}

	// Serial vs parallel must be bit-identical (integer counts).
	if totalPar != total {
		vs = append(vs, triangleDiverge(tick, "CountTrianglesParallel",
			fmt.Sprintf("parallel total %d != serial total %d", totalPar, total)))
	}
	if vio := triangleComparePerNode(tick, "CountTrianglesParallel", g, perNodePar, perNode); vio != nil {
		vs = append(vs, vio...)
	}
	return vs
}

// triangleComparePerNode compares a per-node triangle-count slice (got) against
// the reference (want), returning a violation for the first NodeID that
// disagrees (walked in ascending id so the report is deterministic) or a
// length-parity violation when the result is the wrong shape.
func triangleComparePerNode(tick int64, algo string, g *nameGraph, got, want []int64) []Violation {
	if len(got) != len(want) {
		return []Violation{triangleDiverge(tick, algo,
			fmt.Sprintf("per-node result length %d, want %d", len(got), len(want)))}
	}
	for v := range want {
		if got[v] != want[v] {
			return []Violation{triangleDiverge(tick, algo,
				fmt.Sprintf("per-node triangle count for %q got %d want %d", g.names[v], got[v], want[v]))}
		}
	}
	return nil
}

// naiveTriangleCounts is the independent triangle-count reference over the
// undirected edge set. It builds per-vertex neighbour sets and enumerates every
// triangle exactly once — as the ordered triple (a < b < c) where a is the
// minimum vertex — checking membership b-c directly. It returns the total count
// and a per-NodeID count (each vertex of a triangle receives one increment),
// matching CountTriangles' contract. Complexity is O(V * d^2) with O(1) set
// membership.
func naiveTriangleCounts(n int, edges []mstEdge) (total int64, perNode []int64) {
	adjSet := make([]map[int]struct{}, n)
	adj := make([][]int, n)
	for i := range adjSet {
		adjSet[i] = make(map[int]struct{})
	}
	for _, e := range edges {
		if _, ok := adjSet[e.u][e.v]; ok {
			continue
		}
		adjSet[e.u][e.v] = struct{}{}
		adjSet[e.v][e.u] = struct{}{}
		adj[e.u] = append(adj[e.u], e.v)
		adj[e.v] = append(adj[e.v], e.u)
	}
	perNode = make([]int64, n)
	for a := 0; a < n; a++ {
		nbrs := adj[a]
		for i := 0; i < len(nbrs); i++ {
			b := nbrs[i]
			if b <= a {
				continue
			}
			for j := i + 1; j < len(nbrs); j++ {
				cc := nbrs[j]
				if cc <= a {
					continue
				}
				// a < b and a < c; require b-c adjacent to close the triangle
				// {a,b,c}. Each triangle is enumerated once (a is its minimum).
				if _, ok := adjSet[b][cc]; ok {
					total++
					perNode[a]++
					perNode[b]++
					perNode[cc]++
				}
			}
		}
	}
	return total, perNode
}

// triangleDiverge builds a single triangle-count divergence violation.
func triangleDiverge(tick int64, algo, msg string) Violation {
	return Violation{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo, Message: msg}
}
