package search

import (
	"context"
	"errors"

	"gograph/graph"
	"gograph/graph/csr"
)

// ErrNegativeEdgeAPSP is returned by [DijkstraAPSP] when the input
// CSR contains a strictly-negative edge weight. DijkstraAPSP does not
// reweight negative edges; callers with mixed-sign weights and no
// negative cycles should use [FloydWarshall].
var ErrNegativeEdgeAPSP = errors.New("search: DijkstraAPSP requires non-negative edge weights")

// DijkstraAPSP computes APSP on c by running [Dijkstra] from every
// live vertex. It accepts only non-negative edge weights.
//
// For graphs with negative edges (but no negative cycle), use
// [FloydWarshall] which tolerates them at the cost of O(V^3) work.
//
// Complexity: O(V * (V + E) * log V).
//
// Naming note: the v1.0.0 export "JohnsonAPSP" was misnamed — true
// Johnson's algorithm prefixes a Bellman-Ford reweighting pass to
// handle negative edges. The reweighting pass is deferred to a
// future release; this function is the actually-implemented
// Dijkstra-from-every-vertex variant. JohnsonAPSP is preserved as a
// deprecated alias for backward compatibility.
func DijkstraAPSP[W Weight](c *csr.CSR[W]) (*APSP[W], error) {
	return DijkstraAPSPCtx(context.Background(), c)
}

// DijkstraAPSPCtx is the context-aware variant of [DijkstraAPSP].
// ctx.Err() is checked once per source vertex; on cancellation
// returns (nil, wrapped ctx.Err()).
func DijkstraAPSPCtx[W Weight](ctx context.Context, c *csr.CSR[W]) (*APSP[W], error) {
	maxID := int(c.MaxNodeID())
	mask := c.LiveMask()
	compact := make([]int, maxID)
	live := 0
	for i := 0; i < maxID; i++ {
		if mask[i] {
			compact[i] = live
			live++
		} else {
			compact[i] = -1
		}
	}
	out := &APSP[W]{
		live:    live,
		maxID:   maxID,
		compact: compact,
		dist:    make([]W, live*live),
		found:   make([]bool, live*live),
	}
	if live == 0 {
		return out, nil
	}
	for i := 0; i < live; i++ {
		idx := i*live + i
		out.found[idx] = true
	}
	for src := 0; src < maxID; src++ {
		si := compact[src]
		if si < 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d, err := DijkstraCtx(ctx, c, graph.NodeID(src))
		if err != nil {
			if errors.Is(err, ErrNegativeWeight) {
				return nil, ErrNegativeEdgeAPSP
			}
			return nil, err
		}
		for dst := 0; dst < maxID; dst++ {
			di := compact[dst]
			if di < 0 {
				continue
			}
			if v, ok := d.Distance(graph.NodeID(dst)); ok {
				idx := si*live + di
				out.dist[idx] = v
				out.found[idx] = true
			}
		}
	}
	return out, nil
}

// JohnsonAPSP is a deprecated alias for [DijkstraAPSP]; the original
// v1.0.0 export was misnamed as it did not implement Bellman-Ford
// reweighting.
//
// Deprecated: use [DijkstraAPSP]. JohnsonAPSP will be removed in a
// future major release.
func JohnsonAPSP[W Weight](c *csr.CSR[W]) (*APSP[W], error) {
	return DijkstraAPSP(c)
}
