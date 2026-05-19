package search

import (
	"context"
	"errors"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
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
//
// For hot loops, prefer the zero-allocation primitive [AStarInto].
func AStar[W Weight](
	c *csr.CSR[W],
	src, dst graph.NodeID,
	h func(graph.NodeID) W,
) ([]graph.NodeID, W, error) {
	defer metrics.Time("search.AStar")()
	path, cost, err := AStarCtx(context.Background(), c, src, dst, h)
	if err != nil {
		metrics.IncCounter("search.AStar.errors", 1)
	}
	return path, cost, err
}

// AStarCtx is the context-aware variant of [AStar]. ctx.Err() is
// checked every 4096 heap pops; on cancellation returns
// (nil, zero, wrapped ctx.Err()).
func AStarCtx[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	src, dst graph.NodeID,
	h func(graph.NodeID) W,
) ([]graph.NodeID, W, error) {
	defer metrics.Time("search.AStarCtx")()
	var zero W
	maxID := uint64(c.MaxNodeID())
	st := acquireDijkstra[W](maxID)
	defer releaseDijkstra(st)

	var path []graph.NodeID
	cost, err := aStarCore[W](ctx, c, src, dst, h, st.dist[:maxID], st.parent[:maxID], st.found[:maxID], &st.heap, &path)
	if err != nil {
		metrics.IncCounter("search.AStarCtx.errors", 1)
		return nil, zero, err
	}
	return path, cost, nil
}

// AStarInto is the zero-allocation primitive behind [AStar]. The
// caller provides the dist, parent, and found scratch slices (each
// at least c.MaxNodeID()) and a path destination slice that is
// truncated and appended to. The returned cost matches [AStar].
//
// If any scratch slice is too small, [ErrBufferTooSmall] is returned.
//
// Concurrency: the caller's buffers are written in-place; concurrent
// callers must supply separate buffers.
func AStarInto[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	src, dst graph.NodeID,
	h func(graph.NodeID) W,
	dist []W,
	parent []graph.NodeID,
	found []bool,
	path *[]graph.NodeID,
) (W, error) {
	defer metrics.Time("search.AStarInto")()
	var zero W
	maxID := uint64(c.MaxNodeID())
	if uint64(len(dist)) < maxID || uint64(len(parent)) < maxID || uint64(len(found)) < maxID {
		metrics.IncCounter("search.AStarInto.errors", 1)
		return zero, ErrBufferTooSmall
	}
	heap := acquireDijkHeap[W]()
	defer releaseDijkHeap(heap)
	cost, err := aStarCore[W](ctx, c, src, dst, h, dist[:maxID], parent[:maxID], found[:maxID], heap, path)
	if err != nil {
		metrics.IncCounter("search.AStarInto.errors", 1)
	}
	return cost, err
}

// aStarCore is the shared algorithm body. Pre-conditions:
//   - len(dist), len(parent), len(found) all equal c.MaxNodeID();
//   - heap has been reset to empty;
//   - *path may be non-nil; it is truncated to 0 length before use
//     and its underlying array is reused if cap permits.
//
//nolint:gocyclo // canonical A*: precondition checks + heap loop + reconstruction
func aStarCore[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	src, dst graph.NodeID,
	h func(graph.NodeID) W,
	dist []W,
	parent []graph.NodeID,
	found []bool,
	heap *dijkHeap[W],
	path *[]graph.NodeID,
) (W, error) {
	var zero W
	weights := c.WeightsSlice()
	for _, w := range weights {
		if w < zero {
			return zero, ErrNegativeWeight
		}
	}

	verts := c.VerticesSlice()
	if uint64(src)+1 >= uint64(len(verts)) ||
		uint64(dst)+1 >= uint64(len(verts)) {
		return zero, ErrNoPath
	}
	if src == dst {
		*path = appendSelf(*path, src)
		return zero, nil
	}

	for i := range dist {
		dist[i] = zero
		parent[i] = 0
		found[i] = false
	}
	heap.items = heap.items[:0]

	edges := c.EdgesSlice()

	found[uint64(src)] = true
	parent[uint64(src)] = src
	heap.push(h(src), src)

	popCount := 0
	for heap.len() > 0 {
		if popCount&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return zero, err
			}
		}
		popCount++
		top := heap.pop()
		if top.node == dst {
			reconstructPathInto(parent, src, dst, path)
			return dist[uint64(dst)], nil
		}
		gCurrent := dist[uint64(top.node)]
		if top.dist != gCurrent+h(top.node) {
			continue
		}
		start := verts[uint64(top.node)]
		end := verts[uint64(top.node)+1]
		for k := start; k < end; k++ {
			nb := edges[k]
			cand := gCurrent + weights[k]
			if !found[uint64(nb)] || cand < dist[uint64(nb)] {
				dist[uint64(nb)] = cand
				parent[uint64(nb)] = top.node
				found[uint64(nb)] = true
				heap.push(cand+h(nb), nb)
			}
		}
	}
	return zero, ErrNoPath
}

// appendSelf returns a slice containing exactly {src}, reusing the
// underlying array of the caller's buffer when possible.
func appendSelf(buf []graph.NodeID, src graph.NodeID) []graph.NodeID {
	if cap(buf) >= 1 {
		buf = buf[:1]
		buf[0] = src
		return buf
	}
	return []graph.NodeID{src}
}

// reconstructPathInto walks parents from dst back to src and writes
// the path into *path (truncating to 0 first; growing as needed).
func reconstructPathInto(parent []graph.NodeID, src, dst graph.NodeID, path *[]graph.NodeID) {
	length := 1
	for cur := dst; cur != src; {
		cur = parent[uint64(cur)]
		length++
	}
	if cap(*path) < length {
		*path = make([]graph.NodeID, length)
	} else {
		*path = (*path)[:length]
	}
	cur := dst
	for i := length - 1; i > 0; i-- {
		(*path)[i] = cur
		cur = parent[uint64(cur)]
	}
	(*path)[0] = src
}
