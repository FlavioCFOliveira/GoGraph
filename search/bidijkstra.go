package search

import (
	"context"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
)

// BidirectionalDijkstra computes a shortest path from src to dst in c
// using bidirectional Dijkstra: a forward search from src on the
// original CSR and a simultaneous reverse search from dst on the
// transposed CSR ([csr.CSR.BuildReverse]). Both searches relax non-
// negative edges with the same heap invariant as [Dijkstra]; the
// algorithm terminates when the minimum-key sum across both heaps
// reaches or exceeds the best meeting-edge cost seen so far.
//
// For random-access graphs the asymptotic gain over one-way
// [Dijkstra] is roughly sqrt of the unidirectional ball, with
// concrete >2x speedups on road-network workloads (deep, low-degree
// shortest paths).
//
// The reverse CSR is rebuilt internally; callers that issue many
// point-to-point queries on the same graph should hoist the build
// out of the hot loop with [BidirectionalDijkstraOn].
//
// Returns the path (inclusive of src and dst) and its total cost,
// [ErrNoPath] when no s-t path exists, or [ErrNegativeWeight] if c
// contains any negative-weight edge.
func BidirectionalDijkstra[W Weight](c *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, W, error) {
	defer metrics.Time("search.BidirectionalDijkstra")()
	rev := c.BuildReverse()
	path, cost, err := BidirectionalDijkstraOnCtx(context.Background(), c, rev, src, dst)
	if err != nil {
		metrics.IncCounter("search.BidirectionalDijkstra.errors", 1)
	}
	return path, cost, err
}

// BidirectionalDijkstraOn is [BidirectionalDijkstra] with a pre-
// built reverse CSR. Use this when running many point-to-point
// queries on the same graph; the reverse-CSR construction is O(V+E)
// and is the only allocation outside the algorithm's working state.
func BidirectionalDijkstraOn[W Weight](c, rev *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, W, error) {
	defer metrics.Time("search.BidirectionalDijkstraOn")()
	path, cost, err := BidirectionalDijkstraOnCtx(context.Background(), c, rev, src, dst)
	if err != nil {
		metrics.IncCounter("search.BidirectionalDijkstraOn.errors", 1)
	}
	return path, cost, err
}

// BidirectionalDijkstraOnCtx is the context-aware variant of
// [BidirectionalDijkstraOn]. ctx.Err() is checked every 4096 heap
// pops; on cancellation returns (nil, zero, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical bidirectional Dijkstra: negative-weight scan + dual heap loop + meet bookkeeping + path stitching
func BidirectionalDijkstraOnCtx[W Weight](ctx context.Context, c, rev *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, W, error) {
	defer metrics.Time("search.BidirectionalDijkstraOnCtx")()
	var zero W
	weights := c.WeightsSlice()
	for _, w := range weights {
		if w < zero {
			metrics.IncCounter("search.BidirectionalDijkstraOnCtx.errors", 1)
			return nil, zero, ErrNegativeWeight
		}
	}
	verts := c.VerticesSlice()
	if uint64(src)+1 >= uint64(len(verts)) || uint64(dst)+1 >= uint64(len(verts)) {
		metrics.IncCounter("search.BidirectionalDijkstraOnCtx.errors", 1)
		return nil, zero, ErrNoPath
	}
	if src == dst {
		return []graph.NodeID{src}, zero, nil
	}

	maxID := uint64(c.MaxNodeID())

	// Forward and reverse working state — both use the existing
	// dijkstraState pool (one for the original graph, one for the
	// transposed graph; each carries its own heap and buffers).
	fSt := acquireDijkstra[W](maxID)
	defer releaseDijkstra(fSt)
	rSt := acquireDijkstra[W](maxID)
	defer releaseDijkstra(rSt)
	// Reset only found[]: dist and parent are read solely after
	// their found[] becomes true (set in the same statement as the
	// dist write), so stale values are harmless. Special-case the
	// seeds so the meet check on src/dst itself sees zero.
	for i := range fSt.found {
		fSt.found[i] = false
	}
	for i := range rSt.found {
		rSt.found[i] = false
	}
	fSt.heap.items = fSt.heap.items[:0]
	rSt.heap.items = rSt.heap.items[:0]

	fSt.found[uint64(src)] = true
	fSt.parent[uint64(src)] = src
	fSt.dist[uint64(src)] = zero
	fSt.heap.push(zero, src)
	rSt.found[uint64(dst)] = true
	rSt.parent[uint64(dst)] = dst
	rSt.dist[uint64(dst)] = zero
	rSt.heap.push(zero, dst)

	fSettled := make([]bool, maxID)
	rSettled := make([]bool, maxID)
	bestKnown := false
	var bestCost W
	var meetNode graph.NodeID
	revVerts := rev.VerticesSlice()
	revEdges := rev.EdgesSlice()
	revWeights := rev.WeightsSlice()
	edges := c.EdgesSlice()

	popCount := 0
	for fSt.heap.len() > 0 && rSt.heap.len() > 0 {
		if popCount&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.BidirectionalDijkstraOnCtx.errors", 1)
				return nil, zero, err
			}
		}
		popCount++
		// Termination: the smallest sum of the two frontiers cannot
		// improve on bestCost any further.
		if bestKnown && fSt.heap.items[0].dist+rSt.heap.items[0].dist >= bestCost {
			break
		}
		// Step the side whose heap top is smaller.
		if fSt.heap.items[0].dist <= rSt.heap.items[0].dist {
			top := fSt.heap.pop()
			if fSettled[uint64(top.node)] {
				continue
			}
			fSettled[uint64(top.node)] = true
			start := verts[uint64(top.node)]
			end := verts[uint64(top.node)+1]
			for k := start; k < end; k++ {
				nb := edges[k]
				cand := top.dist + weights[k]
				if !fSt.found[uint64(nb)] || cand < fSt.dist[uint64(nb)] {
					fSt.dist[uint64(nb)] = cand
					fSt.parent[uint64(nb)] = top.node
					fSt.found[uint64(nb)] = true
					fSt.heap.push(cand, nb)
				}
				if rSt.found[uint64(nb)] {
					pathCost := cand + rSt.dist[uint64(nb)]
					if !bestKnown || pathCost < bestCost {
						bestKnown = true
						bestCost = pathCost
						meetNode = nb
					}
				}
			}
		} else {
			top := rSt.heap.pop()
			if rSettled[uint64(top.node)] {
				continue
			}
			rSettled[uint64(top.node)] = true
			start := revVerts[uint64(top.node)]
			end := revVerts[uint64(top.node)+1]
			for k := start; k < end; k++ {
				nb := revEdges[k]
				cand := top.dist + revWeights[k]
				if !rSt.found[uint64(nb)] || cand < rSt.dist[uint64(nb)] {
					rSt.dist[uint64(nb)] = cand
					rSt.parent[uint64(nb)] = top.node
					rSt.found[uint64(nb)] = true
					rSt.heap.push(cand, nb)
				}
				if fSt.found[uint64(nb)] {
					pathCost := fSt.dist[uint64(nb)] + cand
					if !bestKnown || pathCost < bestCost {
						bestKnown = true
						bestCost = pathCost
						meetNode = nb
					}
				}
			}
		}
	}

	if !bestKnown {
		metrics.IncCounter("search.BidirectionalDijkstraOnCtx.errors", 1)
		return nil, zero, ErrNoPath
	}

	// Reconstruct the path: src ... fparent[mid] ... mid ... rparent[mid] ... dst.
	// The forward parent chain steps src-ward; the reverse parent chain steps
	// dst-ward (since rSt's parent[w] is w's predecessor in the reverse graph,
	// which is its successor in the original graph).
	prefixLen := 1
	for cur := meetNode; cur != src; {
		cur = fSt.parent[uint64(cur)]
		prefixLen++
	}
	suffixLen := 0
	for cur := meetNode; cur != dst; {
		cur = rSt.parent[uint64(cur)]
		suffixLen++
	}
	out := make([]graph.NodeID, prefixLen+suffixLen)
	cur := meetNode
	for i := prefixLen - 1; i > 0; i-- {
		out[i] = cur
		cur = fSt.parent[uint64(cur)]
	}
	out[0] = src
	cur = rSt.parent[uint64(meetNode)]
	for i := prefixLen; i < prefixLen+suffixLen; i++ {
		out[i] = cur
		if cur == dst {
			break
		}
		cur = rSt.parent[uint64(cur)]
	}
	return out, bestCost, nil
}
