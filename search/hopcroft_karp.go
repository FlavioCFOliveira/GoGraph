package search

import (
	"context"

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
	out, _ := HopcroftKarpCtx(context.Background(), c, nLeft)
	return out
}

// HopcroftKarpCtx is the context-aware variant of [HopcroftKarp].
// ctx.Err() is checked at every phase boundary (BFS-layer + DFS-augment
// pair); on cancellation returns (zero Matching, wrapped ctx.Err()).
func HopcroftKarpCtx[W any](ctx context.Context, c *csr.CSR[W], nLeft int) (Matching, error) {
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
	stack := make([]hkFrame, 0, nLeft)
	size := 0
	for {
		if err := ctx.Err(); err != nil {
			return Matching{}, err
		}
		queue := bfsLayer(matchL, matchR, dist, verts, edges, nLeft)
		if !queue {
			break
		}
		for u := 0; u < nLeft; u++ {
			if matchL[u] == ^graph.NodeID(0) {
				var ok bool
				ok, stack = dfsAugment(graph.NodeID(u), matchL, matchR, dist, verts, edges, nLeft, stack)
				if ok {
					size++
				}
			}
		}
	}
	return Matching{MatchL: matchL, MatchR: matchR, Size: size}, nil
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

// hkFrame is one frame of the explicit DFS stack maintained by
// [dfsAugment]. u is a left-side vertex; eIdx is the index of the
// edge currently being tried at u.
type hkFrame struct {
	u    graph.NodeID
	eIdx uint64
}

// dfsAugment walks one augmenting path from src using an explicit
// stack — the recursive variant would grow the goroutine stack
// through several boundaries at V=1e7 bipartite scale (CLAUDE.md
// mandates iterative DFS on hot paths). On success the matches are
// applied along the entire stacked path before returning true; on
// failure dist[u] = -1 for every dead-ended left vertex (matches the
// recursive variant). stack is reused across calls — pass the
// previous slice and capture the returned header.
func dfsAugment(src graph.NodeID, matchL, matchR []graph.NodeID, dist []int, verts []uint64, edges []graph.NodeID, nLeft int, stack []hkFrame) (bool, []hkFrame) {
	stack = append(stack[:0], hkFrame{u: src, eIdx: verts[uint64(src)]})
	for {
		li := len(stack) - 1
		if li < 0 {
			return false, stack
		}
		u := stack[li].u
		eIdx := stack[li].eIdx
		end := verts[uint64(u)+1]
		// Walk usable edges at u without going through the heap on
		// every step.
		for eIdx < end {
			v := edges[eIdx]
			pair := matchR[uint64(v)]
			if pair == ^graph.NodeID(0) {
				// Free right vertex: apply matches along the stacked
				// path (including the current u -> v).
				stack[li].eIdx = eIdx
				for i := li; i >= 0; i-- {
					f := stack[i]
					vAt := edges[f.eIdx]
					matchL[uint64(f.u)] = vAt
					matchR[uint64(vAt)] = f.u
				}
				return true, stack
			}
			if int(pair) < nLeft && dist[uint64(pair)] == dist[uint64(u)]+1 {
				stack[li].eIdx = eIdx
				stack = append(stack, hkFrame{u: pair, eIdx: verts[uint64(pair)]})
				goto next
			}
			eIdx++
		}
		// Exhausted u — dead-end. Pop and advance parent's edge.
		dist[uint64(u)] = -1
		stack = stack[:li]
		if li > 0 {
			stack[li-1].eIdx++
		}
	next:
	}
}
