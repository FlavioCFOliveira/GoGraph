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
//
// For hot loops where the caller can amortise buffer allocation,
// prefer the zero-allocation primitive [BellmanFordInto].
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

	if err := bellmanFordCore[W](ctx, c, src, st.dist[:maxID], st.parent[:maxID], st.found[:maxID]); err != nil {
		return nil, err
	}
	return newDistancesCopy(st, src, maxID), nil
}

// BellmanFordInto is the zero-allocation primitive behind [BellmanFord].
// It writes single-source shortest-path results directly into the
// caller-provided slices, each of which must have length at least
// c.MaxNodeID(); otherwise it returns [ErrBufferTooSmall]. The slices
// are reset in-place before the traversal.
//
// Concurrency: the caller's slices are written in-place; concurrent
// callers must supply separate buffers.
func BellmanFordInto[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	src graph.NodeID,
	dist []W,
	parent []graph.NodeID,
	found []bool,
) error {
	maxID := uint64(c.MaxNodeID())
	if uint64(len(dist)) < maxID || uint64(len(parent)) < maxID || uint64(len(found)) < maxID {
		return ErrBufferTooSmall
	}
	return bellmanFordCore[W](ctx, c, src, dist[:maxID], parent[:maxID], found[:maxID])
}

// bellmanFordCore is the shared algorithm body invoked by both
// [BellmanFordCtx] and [BellmanFordInto]. Pre-conditions:
//   - len(dist), len(parent), len(found) all equal c.MaxNodeID().
//
// Postconditions identical to [DijkstraInto].
func bellmanFordCore[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	src graph.NodeID,
	dist []W,
	parent []graph.NodeID,
	found []bool,
) error {
	maxID := uint64(c.MaxNodeID())
	var zero W
	for i := range dist {
		dist[i] = zero
		parent[i] = 0
		found[i] = false
	}

	verts := c.VerticesSlice()
	if uint64(src)+1 >= uint64(len(verts)) {
		return nil
	}

	found[uint64(src)] = true
	parent[uint64(src)] = src

	edges := c.EdgesSlice()
	weights := c.WeightsSlice()

	for round := uint64(0); round < maxID-1; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !relaxOnceBuf(dist, parent, found, verts, edges, weights, maxID) {
			break
		}
	}

	if relaxOnceBuf(dist, parent, found, verts, edges, weights, maxID) {
		return ErrNegativeCycle
	}
	return nil
}

// relaxOnceBuf performs one Bellman-Ford relaxation sweep over every
// edge whose source is already reachable, operating on caller buffers.
// Returns true if any distance was improved.
func relaxOnceBuf[W Weight](
	dist []W,
	parent []graph.NodeID,
	found []bool,
	verts []uint64,
	edges []graph.NodeID,
	weights []W,
	maxID uint64,
) bool {
	changed := false
	for from := uint64(0); from < maxID; from++ {
		if !found[from] {
			continue
		}
		start := verts[from]
		end := verts[from+1]
		for k := start; k < end; k++ {
			nb := uint64(edges[k])
			cand := dist[from] + weights[k]
			if !found[nb] || cand < dist[nb] {
				dist[nb] = cand
				parent[nb] = graph.NodeID(from)
				found[nb] = true
				changed = true
			}
		}
	}
	return changed
}
