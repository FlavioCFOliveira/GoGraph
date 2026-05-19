package search

import (
	"gograph/graph"
	"gograph/graph/csr"
)

// Matching is the result of [HopcroftKarp].
type Matching struct {
	// MatchL maps each left vertex to its matched right vertex or
	// -1 if unmatched.
	MatchL []graph.NodeID
	// MatchR is the symmetric map from right to left.
	MatchR []graph.NodeID
	// Size is the number of matched edges.
	Size int
}

// HopcroftKarp computes a maximum-cardinality matching on the
// bipartite graph captured by c. The first nLeft NodeIDs are
// assumed to form the left partition; the rest form the right
// partition. Edges should point from left to right (the algorithm
// follows c's adjacency for the left vertices).
//
// Complexity O(E * sqrt(V)).
func HopcroftKarp[W any](c *csr.CSR[W], nLeft int) Matching {
	maxID := int(c.MaxNodeID())
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	matchL := make([]graph.NodeID, nLeft)
	matchR := make([]graph.NodeID, maxID)
	for i := range matchL {
		matchL[i] = ^graph.NodeID(0)
	}
	for i := range matchR {
		matchR[i] = ^graph.NodeID(0)
	}
	dist := make([]int, nLeft)
	size := 0
	for {
		queue := bfsLayer(matchL, matchR, dist, verts, edges, nLeft)
		if !queue {
			break
		}
		for u := 0; u < nLeft; u++ {
			if matchL[u] == ^graph.NodeID(0) {
				if dfsAugment(graph.NodeID(u), matchL, matchR, dist, verts, edges, nLeft) {
					size++
				}
			}
		}
	}
	return Matching{MatchL: matchL, MatchR: matchR, Size: size}
}

// bfsLayer assigns dist[u] to every free left vertex u and to every
// left vertex reachable via an unmatched -> matched -> unmatched ...
// chain of edges of length-2. Returns true when at least one
// augmenting path exists.
func bfsLayer(matchL, matchR []graph.NodeID, dist []int, verts []uint64, edges []graph.NodeID, nLeft int) bool {
	queue := make([]int, 0, nLeft)
	for u := 0; u < nLeft; u++ {
		if matchL[u] == ^graph.NodeID(0) {
			dist[u] = 0
			queue = append(queue, u)
		} else {
			dist[u] = -1
		}
	}
	found := false
	for k := 0; k < len(queue); k++ {
		u := queue[k]
		for e := verts[u]; e < verts[u+1]; e++ {
			v := edges[e]
			pair := matchR[uint64(v)]
			if pair == ^graph.NodeID(0) {
				found = true
				continue
			}
			if dist[uint64(pair)] == -1 {
				dist[uint64(pair)] = dist[u] + 1
				queue = append(queue, int(pair))
			}
		}
	}
	return found
}

// dfsAugment recursively augments along a shortest path from u.
func dfsAugment(u graph.NodeID, matchL, matchR []graph.NodeID, dist []int, verts []uint64, edges []graph.NodeID, nLeft int) bool {
	for e := verts[uint64(u)]; e < verts[uint64(u)+1]; e++ {
		v := edges[e]
		pair := matchR[uint64(v)]
		if pair == ^graph.NodeID(0) || (int(pair) < nLeft && dist[uint64(pair)] == dist[uint64(u)]+1 &&
			dfsAugment(pair, matchL, matchR, dist, verts, edges, nLeft)) {
			matchL[uint64(u)] = v
			matchR[uint64(v)] = u
			return true
		}
	}
	dist[uint64(u)] = -1
	return false
}
