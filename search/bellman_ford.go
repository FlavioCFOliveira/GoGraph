package search

import (
	"context"
	"errors"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
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
	defer metrics.Time("search.BellmanFord")()
	res, err := BellmanFordCtx(context.Background(), c, src)
	if err != nil {
		metrics.IncCounter("search.BellmanFord.errors", 1)
	}
	return res, err
}

// BellmanFordCtx is the context-aware variant of [BellmanFord].
// ctx.Err() is checked at every relaxation round boundary; on
// cancellation returns (nil, wrapped ctx.Err()).
func BellmanFordCtx[W Weight](ctx context.Context, c *csr.CSR[W], src graph.NodeID) (*Distances[W], error) {
	defer metrics.Time("search.BellmanFordCtx")()
	maxID := uint64(c.MaxNodeID())
	st := acquireDijkstra[W](maxID)
	defer releaseDijkstra(st)

	if err := bellmanFordCore[W](ctx, c, src, st.dist[:maxID], st.parent[:maxID], st.found[:maxID]); err != nil {
		metrics.IncCounter("search.BellmanFordCtx.errors", 1)
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
	defer metrics.Time("search.BellmanFordInto")()
	maxID := uint64(c.MaxNodeID())
	if uint64(len(dist)) < maxID || uint64(len(parent)) < maxID || uint64(len(found)) < maxID {
		metrics.IncCounter("search.BellmanFordInto.errors", 1)
		return ErrBufferTooSmall
	}
	err := bellmanFordCore[W](ctx, c, src, dist[:maxID], parent[:maxID], found[:maxID])
	if err != nil {
		metrics.IncCounter("search.BellmanFordInto.errors", 1)
	}
	return err
}

// bellmanFordCore is the shared algorithm body invoked by both
// [BellmanFordCtx] and [BellmanFordInto]. Pre-conditions:
//   - len(dist), len(parent), len(found) all equal c.MaxNodeID().
//
// The implementation is SPFA (Shortest Path Faster Algorithm,
// Bannister-Eppstein 2012 attribution): a worklist-driven relaxation
// that only revisits nodes whose tentative distance has changed,
// instead of the V-1 full-edge sweeps of textbook Bellman-Ford.
// SLF (Smallest Label First) deque ordering pushes nodes with
// smaller tentative distance to the front of the queue, which
// usually relaxes their downstream once instead of multiple times.
// Negative-cycle detection is preserved: any node enqueued more
// than V times closes a cycle and yields [ErrNegativeCycle].
//
// Postconditions identical to [DijkstraInto].
//
//nolint:gocyclo // SPFA with SLF + negative-cycle counter and ctx-yield path
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

	// SPFA with SLF on a circular deque. The buffer holds at most
	// maxID elements at any time (each NodeID can appear once);
	// power-of-two sizing lets the modular index arithmetic compile
	// to a bit-mask.
	bufSize := 1
	for bufSize < int(maxID)+1 {
		bufSize <<= 1
	}
	if bufSize < 8 {
		bufSize = 8
	}
	dq := make([]graph.NodeID, bufSize)
	mask := bufSize - 1
	head := 0
	tail := 0
	dq[tail] = src
	tail = (tail + 1) & mask
	inQueue := make([]bool, maxID)
	inQueue[uint64(src)] = true
	relaxes := make([]uint32, maxID)
	relaxes[uint64(src)] = 1
	yieldCtr := 0
	for head != tail {
		yieldCtr++
		if yieldCtr&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		v := dq[head]
		head = (head + 1) & mask
		inQueue[uint64(v)] = false
		dv := dist[uint64(v)]
		start := verts[uint64(v)]
		end := verts[uint64(v)+1]
		for k := start; k < end; k++ {
			nb := uint64(edges[k])
			cand := dv + weights[k]
			if !found[nb] || cand < dist[nb] {
				dist[nb] = cand
				parent[nb] = v
				found[nb] = true
				if !inQueue[nb] {
					relaxes[nb]++
					if uint64(relaxes[nb]) > maxID {
						return ErrNegativeCycle
					}
					// SLF (Smallest Label First): when the new tentative
					// distance is smaller than the front element's,
					// push to the front so the cheapest is dequeued
					// next. Otherwise push to the back.
					if head != tail && cand < dist[uint64(dq[head])] {
						head = (head - 1) & mask
						dq[head] = graph.NodeID(nb)
					} else {
						dq[tail] = graph.NodeID(nb)
						tail = (tail + 1) & mask
					}
					inQueue[nb] = true
				}
			}
		}
	}
	return nil
}
