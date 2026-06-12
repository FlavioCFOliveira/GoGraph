package search

import (
	"container/heap"
	"context"
	"errors"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrResourceBudgetExceeded is returned by [KShortestPathsLooplessCtxWithOpts]
// when the MaxPops or MaxQueueBytes limit is reached before k paths are found.
var ErrResourceBudgetExceeded = errors.New("search: resource budget exceeded")

// KShortestPathsLooplessOpts configures optional resource guards for
// [KShortestPathsLooplessCtxWithOpts]. Zero values mean no limit.
type KShortestPathsLooplessOpts struct {
	// MaxPops is the maximum number of priority-queue pops allowed.
	// When reached, the call returns (partial, ErrResourceBudgetExceeded).
	MaxPops int
	// MaxQueueBytes is the approximate maximum bytes the priority queue
	// may hold. Approximated as sum(len(path)) * 8 across all live
	// entries; when exceeded the call returns (partial, ErrResourceBudgetExceeded).
	MaxQueueBytes int64
}

// KShortestPathsLoopless computes up to k loopless shortest paths
// from src to dst in c using a best-first expansion over the implicit
// loopless-path tree. Each priority-queue entry is a (cost, path)
// pair; popping the cheapest entry that ends at dst yields the next
// k-shortest path. The loopless guard excludes any expansion whose
// neighbour is already present in the popped path.
//
// # Complexity and resource budget
//
// The algorithm expands the implicit loopless-path tree in best-first
// order. In the worst case it must pop every prefix path cheaper than
// the k-th s-t shortest path; that set can be exponential in V.
// Concrete example: a diamond-chain of depth D plus a single expensive
// s-t shortcut forces 2^D pops even for k=2.
//
// Use ctx cancellation or [KShortestPathsLooplessCtxWithOpts] with a
// MaxPops/MaxQueueBytes bound when operating on arbitrary graphs.
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
// For an explicit resource cap use [KShortestPathsLooplessCtxWithOpts].
func KShortestPathsLooplessCtx[W Weight](ctx context.Context, c *csr.CSR[W], src, dst graph.NodeID, k int) ([]YenPath[W], error) {
	defer metrics.Time("search.KShortestPathsLooplessCtx")()
	return KShortestPathsLooplessCtxWithOpts(ctx, c, src, dst, k, KShortestPathsLooplessOpts{})
}

// KShortestPathsLooplessCtxWithOpts is the context-aware, resource-bounded
// variant of [KShortestPathsLoopless].
//
// ctx.Err() is checked every 4096 heap pops; on cancellation returns
// (partial, wrapped ctx.Err()).
//
// opts.MaxPops, when positive, caps the total number of priority-queue
// pops. opts.MaxQueueBytes, when positive, caps the approximate memory
// held by the priority queue (estimated as sum of path lengths × 8 bytes).
// When either limit is reached the call returns any paths found so far
// together with [ErrResourceBudgetExceeded].
//
//nolint:gocyclo // canonical best-first k-shortest with NaN/Inf gate + loopless guard + resource caps
func KShortestPathsLooplessCtxWithOpts[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	src, dst graph.NodeID,
	k int,
	opts KShortestPathsLooplessOpts,
) ([]YenPath[W], error) {
	defer metrics.Time("search.KShortestPathsLooplessCtxWithOpts")()
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
		metrics.IncCounter("search.KShortestPathsLooplessCtxWithOpts.errors", 1)
		return nil, ErrInvalidInput
	}
	pq := &looplessPQ[W]{}
	heap.Init(pq)
	seedPath := []graph.NodeID{src}
	heap.Push(pq, looplessItem[W]{cost: 0, path: seedPath})
	pq.totalPathLen += int64(len(seedPath))
	result := make([]YenPath[W], 0, k)
	tick := 0
	for pq.Len() > 0 && len(result) < k {
		tick++
		if tick&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.KShortestPathsLooplessCtxWithOpts.errors", 1)
				return partialOrNil(result), err
			}
		}
		// MaxPops guard: checked after each pop.
		if opts.MaxPops > 0 && tick > opts.MaxPops {
			return partialOrNil(result), ErrResourceBudgetExceeded
		}
		top := heap.Pop(pq).(looplessItem[W]) //nolint:errcheck // PQ types are statically known
		pq.totalPathLen -= int64(len(top.path))
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
			item := looplessItem[W]{cost: top.cost + w, path: newPath}
			heap.Push(pq, item)
			pq.totalPathLen += int64(len(newPath))
		}
		// MaxQueueBytes guard: checked after the expansion of each pop.
		if opts.MaxQueueBytes > 0 && pq.totalPathLen*8 > opts.MaxQueueBytes {
			return partialOrNil(result), ErrResourceBudgetExceeded
		}
	}
	return partialOrNil(result), nil
}

// partialOrNil returns results if non-empty, nil otherwise.
// Callers that find no paths should return nil, not an empty slice.
func partialOrNil[W Weight](results []YenPath[W]) []YenPath[W] {
	if len(results) == 0 {
		return nil
	}
	return results
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
//
// totalPathLen tracks the sum of path lengths across all live items;
// used by KShortestPathsLooplessCtxWithOpts to estimate queue memory.
// The caller is responsible for updating totalPathLen around Push/Pop
// calls (done in KShortestPathsLooplessCtxWithOpts directly so the
// heap.Interface delegates remain allocation-free).
type looplessPQ[W Weight] struct {
	items        []looplessItem[W]
	totalPathLen int64
}

func (h looplessPQ[W]) Len() int           { return len(h.items) }
func (h looplessPQ[W]) Less(i, j int) bool { return h.items[i].cost < h.items[j].cost }
func (h looplessPQ[W]) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *looplessPQ[W]) Push(x any)        { h.items = append(h.items, x.(looplessItem[W])) } //nolint:errcheck // statically known
func (h *looplessPQ[W]) Pop() any {
	old := h.items
	n := len(old)
	item := old[n-1]
	h.items = old[:n-1]
	return item
}
