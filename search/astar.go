package search

import (
	"context"
	"errors"

	"gograph/graph"
	"gograph/graph/csr"
)

// ErrNoPath is returned by point-to-point search algorithms when no
// path exists between the requested endpoints.
var ErrNoPath = errors.New("search: no path between endpoints")

// AStar computes a shortest path from src to dst on c, guided by the
// caller-supplied heuristic h. h must be admissible (h(n) is a lower
// bound on the true cost from n to dst) for optimality; if h is also
// consistent (h(u) <= w(u,v) + h(v) for every edge u->v) the search
// never re-expands a node. Admissibility and consistency are caller
// contracts; AStar does not validate them.
//
// Returns the path from src to dst (inclusive) with the corresponding
// total cost, or [ErrNoPath] when dst is unreachable. Returns
// [ErrNegativeWeight] if any edge weight in c is strictly negative.
func AStar[W Weight](
	c *csr.CSR[W],
	src, dst graph.NodeID,
	h func(graph.NodeID) W,
) ([]graph.NodeID, W, error) {
	return AStarCtx(context.Background(), c, src, dst, h)
}

// AStarCtx is the context-aware variant of [AStar]. ctx.Err() is
// checked every 4096 heap pops; on cancellation returns
// (nil, zero, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical A*: precondition checks + pool acquire + heap loop + reconstruction
func AStarCtx[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	src, dst graph.NodeID,
	h func(graph.NodeID) W,
) ([]graph.NodeID, W, error) {
	var zero W
	weights := c.WeightsSlice()
	for _, w := range weights {
		if w < zero {
			return nil, zero, ErrNegativeWeight
		}
	}

	if uint64(src)+1 >= uint64(len(c.VerticesSlice())) ||
		uint64(dst)+1 >= uint64(len(c.VerticesSlice())) {
		return nil, zero, ErrNoPath
	}
	if src == dst {
		return []graph.NodeID{src}, zero, nil
	}

	maxID := uint64(c.MaxNodeID())
	st := acquireDijkstra[W](maxID)
	defer releaseDijkstra(st)
	resetDijkstraState(st)

	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	st.found[uint64(src)] = true
	st.parent[uint64(src)] = src
	st.heap.push(h(src), src)

	popCount := 0
	for st.heap.len() > 0 {
		if popCount&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return nil, zero, err
			}
		}
		popCount++
		top := st.heap.pop()
		if top.node == dst {
			return reconstructPath(st, src, dst), st.dist[uint64(dst)], nil
		}
		gCurrent := st.dist[uint64(top.node)]
		if top.dist != gCurrent+h(top.node) {
			continue
		}
		start := verts[uint64(top.node)]
		end := verts[uint64(top.node)+1]
		for k := start; k < end; k++ {
			nb := edges[k]
			cand := gCurrent + weights[k]
			if !st.found[uint64(nb)] || cand < st.dist[uint64(nb)] {
				st.dist[uint64(nb)] = cand
				st.parent[uint64(nb)] = top.node
				st.found[uint64(nb)] = true
				st.heap.push(cand+h(nb), nb)
			}
		}
	}
	return nil, zero, ErrNoPath
}

// reconstructPath walks parents from dst back to src and returns the
// path in source-to-destination order. Caller must have already
// verified that dst is reachable (st.found[dst] is true).
func reconstructPath[W Weight](st *dijkstraState[W], src, dst graph.NodeID) []graph.NodeID {
	length := 1
	for cur := dst; cur != src; {
		cur = st.parent[uint64(cur)]
		length++
	}
	out := make([]graph.NodeID, length)
	cur := dst
	for i := length - 1; i > 0; i-- {
		out[i] = cur
		cur = st.parent[uint64(cur)]
	}
	out[0] = src
	return out
}
