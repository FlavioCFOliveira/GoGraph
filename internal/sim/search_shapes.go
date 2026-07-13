package sim

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// This file holds the small shaped-graph builders shared by the fixture-based
// search checks that need a specific structure (a DAG for topological sort, a
// symmetric CSR for diameter / direction-optimising BFS / undirected Eulerian
// trails). Every builder produces an immutable CSR over dense NodeIDs [0,n) via
// the canonical counting-then-scatter offset pass, so every source — including a
// pure sink with no out-edges — owns an offset slot (the off-by-one class of bug
// an undirected or sink-heavy fixture is especially prone to).

// dirCSRFromEdges builds a directed CSR[float64] (nil weights) from the given
// directed edge list over n dense NodeIDs. Edges are emitted in a per-source
// counting order, so the layout is a pure function of the edge slice.
func dirCSRFromEdges(n int, edges [][2]int) *csr.CSR[float64] {
	if n == 0 {
		return csr.FromArrays[float64]([]uint64{0}, nil, nil, 0, 0)
	}
	vertices := make([]uint64, n+1)
	for _, e := range edges {
		vertices[e[0]+1]++
	}
	for i := 1; i <= n; i++ {
		vertices[i] += vertices[i-1]
	}
	out := make([]graph.NodeID, len(edges))
	cursor := make([]uint64, n)
	for _, e := range edges {
		pos := vertices[e[0]] + cursor[e[0]]
		out[pos] = graph.NodeID(e[1])
		cursor[e[0]]++
	}
	return csr.FromArrays[float64](vertices, out, nil, uint64(n), uint64(len(edges)))
}

// symCSRFromEdges builds a symmetric directed CSR[float64] (nil weights) from an
// UNDIRECTED edge list over n dense NodeIDs: each {u,v} pair is materialised as
// both u->v and v->u. The caller is responsible for supplying a simple edge set
// (no self-loops, no duplicate pairs) where the consuming algorithm — e.g.
// CountTriangles — requires one; the diameter, direction-optimising BFS, and
// undirected-Eulerian checks that use this builder tolerate neither, so they
// pass de-duplicated, self-loop-free pairs.
func symCSRFromEdges(n int, undirected [][2]int) *csr.CSR[float64] {
	if n == 0 {
		return csr.FromArrays[float64]([]uint64{0}, nil, nil, 0, 0)
	}
	vertices := make([]uint64, n+1)
	for _, e := range undirected {
		vertices[e[0]+1]++
		vertices[e[1]+1]++
	}
	for i := 1; i <= n; i++ {
		vertices[i] += vertices[i-1]
	}
	out := make([]graph.NodeID, 2*len(undirected))
	cursor := make([]uint64, n)
	for _, e := range undirected {
		u, v := e[0], e[1]
		pu := vertices[u] + cursor[u]
		out[pu] = graph.NodeID(v)
		cursor[u]++
		pv := vertices[v] + cursor[v]
		out[pv] = graph.NodeID(u)
		cursor[v]++
	}
	return csr.FromArrays[float64](vertices, out, nil, uint64(n), uint64(2*len(undirected)))
}

// symAdjacency builds a plain undirected adjacency (both directions) from an
// undirected edge list, for the from-scratch BFS references. Parallel to
// symCSRFromEdges but as a slice-of-slices the reference walks directly.
func symAdjacency(n int, undirected [][2]int) [][]int {
	adj := make([][]int, n)
	for _, e := range undirected {
		adj[e[0]] = append(adj[e[0]], e[1])
		adj[e[1]] = append(adj[e[1]], e[0])
	}
	return adj
}

// bfsDistancesFromAdj returns per-node hop distances from src over adj, with -1
// for unreachable nodes. It is the independent BFS reference for the diameter
// and direction-optimising-BFS checks.
func bfsDistancesFromAdj(adj [][]int, n, src int) []int {
	dist := make([]int, n)
	for i := range dist {
		dist[i] = -1
	}
	if src < 0 || src >= n {
		return dist
	}
	dist[src] = 0
	queue := []int{src}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, v := range adj[u] {
			if dist[v] == -1 {
				dist[v] = dist[u] + 1
				queue = append(queue, v)
			}
		}
	}
	return dist
}
