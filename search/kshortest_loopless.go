package search

import (
	"container/heap"
	"context"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
)

// KShortestPathsLoopless computes up to k loopless shortest paths
// from src to dst in c using a best-first expansion over the implicit
// loopless-path tree. Each priority-queue entry is a (cost, path)
// pair; popping the cheapest entry that ends at dst yields the next
// k-shortest path. The loopless guard excludes any expansion whose
// neighbour is already present in the popped path.
//
// # K bounds and memory growth
//
// Each pop expands one path of length up to V, so a worst-case
// run does O(k * V * Δ) work and stores O(k * V) words in the
// priority queue. Practical guidance:
//
//   - k <= 100: queue stays comfortably below a megabyte even on
//     million-edge graphs.
//   - k <= 10 000: queue grows to roughly k * diameter * word_size
//     bytes; budget accordingly and pre-allocate.
//   - k unbounded (`enumerate all simple paths until exhaustion`):
//     this implementation will exhaust because the path count can
//     be exponential in V (think dense graphs); cap k explicitly
//     to keep the queue bounded.
//
// For graphs where k is comparable to the number of simple paths
// the routine matches Yen's asymptotic behaviour without paying
// the k spur-Dijkstra rounds; on sparse graphs with few
// alternative routes [YenKShortest] is typically faster in
// practice.
//
// This is NOT the heap-of-heaps construction of Eppstein 1998 ("Finding
// the k Shortest Paths", SIAM J. Comput.). That algorithm builds an
// implicit graph D(G) over sidetrack edges of the shortest-path tree
// and achieves O(m + n log n + k); the implementation here is the
// simpler best-first enumeration that GoGraph has historically shipped
// under the EppsteinKShortest name. See [EppsteinKShortest] for the
// deprecated alias preserved for backwards compatibility.
//
// For floating-point Weight types it validates that no edge weight
// is NaN or +/-Inf and returns nil through the simple entry (and
// [ErrInvalidInput] through the Ctx variant) otherwise; integer
// Weight types skip that pass.
//
// Safe for concurrent use against an immutable CSR; the call holds no
// shared state across invocations.
func KShortestPathsLoopless[W Weight](c *csr.CSR[W], src, dst graph.NodeID, k int) []YenPath[W] {
	defer metrics.Time("search.KShortestPathsLoopless")()
	out, _ := KShortestPathsLooplessCtx(context.Background(), c, src, dst, k)
	return out
}

// KShortestPathsLooplessCtx is the context-aware variant of
// [KShortestPathsLoopless]. ctx.Err() is checked every 4096 heap pops;
// on cancellation returns (nil, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical best-first k-shortest with NaN/Inf gate + loopless guard
func KShortestPathsLooplessCtx[W Weight](ctx context.Context, c *csr.CSR[W], src, dst graph.NodeID, k int) ([]YenPath[W], error) {
	defer metrics.Time("search.KShortestPathsLooplessCtx")()
	if k <= 0 {
		return nil, nil
	}
	verts := c.VerticesSlice()
	if uint64(src)+1 >= uint64(len(verts)) || uint64(dst)+1 >= uint64(len(verts)) {
		return nil, nil
	}
	if src == dst {
		return []YenPath[W]{{Nodes: []graph.NodeID{src}, Cost: 0}}, nil
	}
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	// Float Weight types: NaN / +/-Inf silently corrupts cost-ordered
	// PQ comparisons. Fail fast; integer W short-circuits in O(1).
	if anyFloatInvalid(weights) {
		metrics.IncCounter("search.KShortestPathsLooplessCtx.errors", 1)
		return nil, ErrInvalidInput
	}
	pq := &looplessPQ[W]{}
	heap.Init(pq)
	heap.Push(pq, looplessItem[W]{cost: 0, path: []graph.NodeID{src}})
	result := make([]YenPath[W], 0, k)
	tick := 0
	for pq.Len() > 0 && len(result) < k {
		tick++
		if tick&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.KShortestPathsLooplessCtx.errors", 1)
				return nil, err
			}
		}
		top := heap.Pop(pq).(looplessItem[W]) //nolint:errcheck // PQ types are statically known
		last := top.path[len(top.path)-1]
		if last == dst {
			result = append(result, YenPath[W]{Nodes: top.path, Cost: top.cost})
			continue
		}
		start := verts[uint64(last)]
		end := verts[uint64(last)+1]
		for kk := start; kk < end; kk++ {
			nb := edges[kk]
			if pathContains(top.path, nb) {
				continue
			}
			var w W
			if weights != nil {
				w = weights[kk]
			}
			newPath := make([]graph.NodeID, len(top.path)+1)
			copy(newPath, top.path)
			newPath[len(newPath)-1] = nb
			heap.Push(pq, looplessItem[W]{cost: top.cost + w, path: newPath})
		}
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// pathContains reports whether p contains v. Linear scan; for the
// short paths this routine produces (length << V) the constant beats a
// per-pop bitmap allocation.
func pathContains(p []graph.NodeID, v graph.NodeID) bool {
	for _, n := range p {
		if n == v {
			return true
		}
	}
	return false
}

// looplessItem is one priority-queue entry.
type looplessItem[W Weight] struct {
	cost W
	path []graph.NodeID
}

// looplessPQ is the heap-interface adapter — kept private so callers
// can't accidentally bypass the loopless guard.
type looplessPQ[W Weight] []looplessItem[W]

func (h looplessPQ[W]) Len() int           { return len(h) }
func (h looplessPQ[W]) Less(i, j int) bool { return h[i].cost < h[j].cost }
func (h looplessPQ[W]) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *looplessPQ[W]) Push(x any)        { *h = append(*h, x.(looplessItem[W])) } //nolint:errcheck // statically known
func (h *looplessPQ[W]) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
