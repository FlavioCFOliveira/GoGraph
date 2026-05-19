package search

import (
	"context"
	"errors"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
)

// ErrNoEulerian is returned by [Hierholzer] when c does not admit
// an Eulerian circuit or path.
var ErrNoEulerian = errors.New("search: graph has no Eulerian circuit or path")

// Hierholzer computes an Eulerian circuit (or path) over a directed
// graph captured by c. The algorithm is the classical Hierholzer
// (1873) iterative form running in O(E).
//
// Returns the trail as a slice of NodeIDs (length = E + 1) or
// [ErrNoEulerian] when the necessary conditions (every vertex has
// in-degree == out-degree for a circuit, or at most one vertex
// with out-in==1 and one with in-out==1 for a path) are not met,
// or the graph is not connected through its non-zero degree
// vertices.
func Hierholzer[W any](c *csr.CSR[W]) ([]graph.NodeID, error) {
	defer metrics.Time("search.Hierholzer")()
	res, err := HierholzerCtx(context.Background(), c)
	if err != nil {
		metrics.IncCounter("search.Hierholzer.errors", 1)
	}
	return res, err
}

// HierholzerCtx is the context-aware variant of [Hierholzer]. ctx.Err()
// is checked every 4096 trail steps; on cancellation returns
// (nil, wrapped ctx.Err()).
func HierholzerCtx[W any](ctx context.Context, c *csr.CSR[W]) ([]graph.NodeID, error) {
	defer metrics.Time("search.HierholzerCtx")()
	maxID := int(c.MaxNodeID())
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	indeg := make([]int, maxID)
	for i := 0; i < maxID; i++ {
		for k := verts[i]; k < verts[i+1]; k++ {
			indeg[edges[k]]++
		}
	}

	start, ok := pickStart(verts, indeg, maxID)
	if !ok {
		metrics.IncCounter("search.HierholzerCtx.errors", 1)
		return nil, ErrNoEulerian
	}

	// nextEdge advances the per-vertex pointer; an edge is consumed
	// when the trail visits it.
	nextEdge := make([]uint64, maxID)
	for i := 0; i < maxID; i++ {
		nextEdge[i] = verts[i]
	}

	// The resulting trail has exactly E+1 entries; pre-sizing it
	// eliminates the O(log E) cascade of append doublings that the
	// v1.0 implementation incurred (and the ~80 MB copy at E=10^7).
	stack := []graph.NodeID{graph.NodeID(start)}
	trail := make([]graph.NodeID, 0, len(edges)+1)
	stepCount := 0
	for len(stack) > 0 {
		if stepCount&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.HierholzerCtx.errors", 1)
				return nil, err
			}
		}
		stepCount++
		v := stack[len(stack)-1]
		if nextEdge[uint64(v)] < verts[uint64(v)+1] {
			w := edges[nextEdge[uint64(v)]]
			nextEdge[uint64(v)]++
			stack = append(stack, w)
			continue
		}
		trail = append(trail, v)
		stack = stack[:len(stack)-1]
	}

	// Total edges = E; trail must have len = E + 1; otherwise the
	// graph is disconnected for the chosen start.
	if len(trail) != len(edges)+1 {
		metrics.IncCounter("search.HierholzerCtx.errors", 1)
		return nil, ErrNoEulerian
	}
	for i, j := 0, len(trail)-1; i < j; i, j = i+1, j-1 {
		trail[i], trail[j] = trail[j], trail[i]
	}
	return trail, nil
}

// pickStart returns the index of the starting vertex for the
// Eulerian trail. For a circuit (every vertex has indeg==outdeg)
// it picks the first vertex with non-zero out-degree. For a path
// it picks the unique vertex with outdeg-indeg==1.
func pickStart(verts []uint64, indeg []int, maxID int) (int, bool) {
	startCandidate := -1
	overflows := 0
	for i := 0; i < maxID; i++ {
		out := int(verts[i+1] - verts[i])
		switch out - indeg[i] {
		case 0:
			if out > 0 && startCandidate == -1 {
				startCandidate = i
			}
		case 1:
			startCandidate = i
			overflows++
		case -1:
			overflows++
		default:
			return -1, false
		}
	}
	if overflows != 0 && overflows != 2 {
		return -1, false
	}
	if startCandidate == -1 {
		return -1, false
	}
	return startCandidate, true
}
