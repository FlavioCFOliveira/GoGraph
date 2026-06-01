package search

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// HierholzerUndirected computes an Eulerian circuit (or path) over
// the undirected graph captured by c. c is expected to be a
// symmetric directed CSR (every {u, v} edge appears as both (u, v)
// and (v, u)) — typical of [adjlist.AdjList] with Directed=false.
//
// Returns the trail as a slice of NodeIDs (length = E + 1 where E is
// the number of distinct undirected edges) or [ErrNoEulerian] when
// the preconditions are not met. For an Eulerian circuit every
// non-isolated vertex must have even degree; for an Eulerian path,
// exactly two vertices may have odd degree (these become the path's
// endpoints).
func HierholzerUndirected[W any](c *csr.CSR[W]) ([]graph.NodeID, error) {
	defer metrics.Time("search.HierholzerUndirected")()
	res, err := HierholzerUndirectedCtx(context.Background(), c)
	if err != nil {
		metrics.IncCounter("search.HierholzerUndirected.errors", 1)
	}
	return res, err
}

// HierholzerUndirectedCtx is the context-aware variant of
// [HierholzerUndirected]. ctx.Err() is checked every 4096 trail
// steps; on cancellation returns (nil, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical Hierholzer with twin-edge consumption
func HierholzerUndirectedCtx[W any](ctx context.Context, c *csr.CSR[W]) ([]graph.NodeID, error) {
	defer metrics.Time("search.HierholzerUndirectedCtx")()
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return nil, nil
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	if len(edges) == 0 {
		return nil, nil
	}
	if len(edges)%2 != 0 {
		// A symmetric CSR must have an even total edge count; an odd
		// count means c isn't an undirected encoding.
		metrics.IncCounter("search.HierholzerUndirectedCtx.errors", 1)
		return nil, ErrNoEulerian
	}
	// Build the twin index: twin[k] is the position in edges[] of
	// the reverse-direction copy of edge k. For each directed pair
	// (u, v) at edges[k]=v we look in v's adjacency for the first
	// not-yet-twinned slot with destination u.
	twin := buildUndirectedTwin(verts, edges, maxID)
	// Degree (each undirected edge counts once for each endpoint).
	deg := make([]int, maxID)
	for u := 0; u < maxID; u++ {
		deg[u] = int(verts[u+1] - verts[u])
	}
	// Pick start vertex: any odd-degree vertex if exactly two exist;
	// otherwise any non-isolated vertex. Reject more than two
	// odd-degree vertices.
	start, ok := pickUndirectedStart(deg, maxID)
	if !ok {
		metrics.IncCounter("search.HierholzerUndirectedCtx.errors", 1)
		return nil, ErrNoEulerian
	}

	nextEdge := make([]uint64, maxID)
	for i := 0; i < maxID; i++ {
		nextEdge[i] = verts[i]
	}
	consumed := make([]bool, len(edges))
	stack := make([]graph.NodeID, 0, len(edges)/2+1)
	stack = append(stack, graph.NodeID(start))
	trail := make([]graph.NodeID, 0, len(edges)/2+1)
	stepCount := 0
	for len(stack) > 0 {
		if stepCount&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.HierholzerUndirectedCtx.errors", 1)
				return nil, err
			}
		}
		stepCount++
		v := stack[len(stack)-1]
		// Skip already-consumed edges out of v.
		for nextEdge[uint64(v)] < verts[uint64(v)+1] && consumed[nextEdge[uint64(v)]] {
			nextEdge[uint64(v)]++
		}
		if nextEdge[uint64(v)] < verts[uint64(v)+1] {
			k := nextEdge[uint64(v)]
			w := edges[k]
			consumed[k] = true
			consumed[twin[k]] = true
			nextEdge[uint64(v)]++
			stack = append(stack, w)
			continue
		}
		trail = append(trail, v)
		stack = stack[:len(stack)-1]
	}
	expectedLen := len(edges)/2 + 1
	if len(trail) != expectedLen {
		metrics.IncCounter("search.HierholzerUndirectedCtx.errors", 1)
		return nil, ErrNoEulerian
	}
	for i, j := 0, len(trail)-1; i < j; i, j = i+1, j-1 {
		trail[i], trail[j] = trail[j], trail[i]
	}
	return trail, nil
}

// buildUndirectedTwin returns a per-edge-index slice mapping each
// directed CSR edge to its undirected twin (the slot in the other
// endpoint's adjacency that names this edge in reverse). Build is
// O(V + E); the algorithm walks every (u, v) once and pairs it with
// the first not-yet-paired (v, u) slot.
func buildUndirectedTwin(verts []uint64, edges []graph.NodeID, maxID int) []uint64 {
	twin := make([]uint64, len(edges))
	// claimed[k] tracks whether edges[k] has been paired yet.
	claimed := make([]bool, len(edges))
	for u := 0; u < maxID; u++ {
		for k := verts[u]; k < verts[u+1]; k++ {
			if claimed[k] {
				continue
			}
			v := edges[k]
			// Find the first unclaimed slot in v's adjacency that
			// points back to u.
			for l := verts[uint64(v)]; l < verts[uint64(v)+1]; l++ {
				if claimed[l] {
					continue
				}
				if int(edges[l]) == u {
					twin[k] = l
					twin[l] = k
					claimed[k] = true
					claimed[l] = true
					break
				}
			}
		}
	}
	return twin
}

// pickUndirectedStart returns the start vertex for the Eulerian
// trail: any odd-degree vertex when exactly two such vertices exist,
// otherwise any non-isolated vertex. Reject more than two odd-degree
// vertices.
func pickUndirectedStart(deg []int, maxID int) (int, bool) {
	startCandidate := -1
	oddCount := 0
	for i := 0; i < maxID; i++ {
		if deg[i] == 0 {
			continue
		}
		if deg[i]%2 == 1 {
			startCandidate = i
			oddCount++
		} else if startCandidate == -1 {
			startCandidate = i
		}
	}
	if oddCount != 0 && oddCount != 2 {
		return -1, false
	}
	if startCandidate == -1 {
		return -1, false
	}
	return startCandidate, true
}
