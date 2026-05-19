package search

import (
	"context"
	"errors"

	"gograph/graph"
	"gograph/graph/csr"
)

// ErrNegativeCycle is returned by [BellmanFord] when the input graph
// contains a negative cycle reachable from the source. In that case
// shortest-path distances are not defined.
var ErrNegativeCycle = errors.New("search: negative cycle reachable from source")

// BellmanFord computes single-source shortest paths in c starting at
// src. Unlike [Dijkstra], it accepts edges with negative weights and
// detects negative cycles reachable from the source, returning
// [ErrNegativeCycle] in that case.
//
// The algorithm is the textbook O(V*E) variant: for each of V-1
// rounds it relaxes every edge once; an additional round detects
// negative cycles by reporting any further successful relaxation.
//
// The implementation reuses the [Distances] result type and the
// per-W pooled state of [Dijkstra]; the inner relaxation loop is
// zero-alloc.
func BellmanFord[W Weight](c *csr.CSR[W], src graph.NodeID) (*Distances[W], error) {
	return BellmanFordCtx(context.Background(), c, src)
}

// BellmanFordCtx is the context-aware variant of [BellmanFord].
// ctx.Err() is checked at every relaxation round boundary; on
// cancellation returns (nil, wrapped ctx.Err()).
func BellmanFordCtx[W Weight](ctx context.Context, c *csr.CSR[W], src graph.NodeID) (*Distances[W], error) {
	maxID := uint64(c.MaxNodeID())
	st := acquireDijkstra[W](maxID)
	defer releaseDijkstra(st)
	resetDijkstraState(st)

	if uint64(src)+1 >= uint64(len(c.VerticesSlice())) {
		return newDistancesCopy(st, src, maxID), nil
	}

	st.found[uint64(src)] = true
	st.parent[uint64(src)] = src

	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()

	for round := uint64(0); round < maxID-1; round++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !relaxOnce(st, verts, edges, weights, maxID) {
			break
		}
	}

	if relaxOnce(st, verts, edges, weights, maxID) {
		return nil, ErrNegativeCycle
	}
	return newDistancesCopy(st, src, maxID), nil
}

// resetDijkstraState clears the pooled buffers ahead of a fresh run.
func resetDijkstraState[W Weight](st *dijkstraState[W]) {
	var zero W
	for i := range st.dist {
		st.dist[i] = zero
		st.parent[i] = 0
		st.found[i] = false
	}
}

// relaxOnce performs one Bellman-Ford relaxation sweep over every
// edge whose source is already reachable. Returns true if any
// distance was improved.
func relaxOnce[W Weight](
	st *dijkstraState[W],
	verts []uint64,
	edges []graph.NodeID,
	weights []W,
	maxID uint64,
) bool {
	changed := false
	for from := uint64(0); from < maxID; from++ {
		if !st.found[from] {
			continue
		}
		start := verts[from]
		end := verts[from+1]
		for k := start; k < end; k++ {
			nb := uint64(edges[k])
			cand := st.dist[from] + weights[k]
			if !st.found[nb] || cand < st.dist[nb] {
				st.dist[nb] = cand
				st.parent[nb] = graph.NodeID(from)
				st.found[nb] = true
				changed = true
			}
		}
	}
	return changed
}
