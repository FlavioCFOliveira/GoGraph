package search

import (
	"container/heap"
	"context"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
)

// EppsteinKShortest computes up to k loopless shortest paths from
// src to dst in c using a uniform-cost-style expansion over the
// implicit path-tree. Each priority-queue entry is a (cost, path)
// pair; popping the cheapest entry that ends at dst yields the next
// k-shortest path. Loopless mode (the default) excludes any
// expansion whose neighbour is already in the popped path.
//
// Relative to [YenKShortest] the implementation avoids the k spur-
// Dijkstra rounds: only one shortest-path tree is implicitly
// explored. On graphs where k is large the asymptotic improvement
// over Yen materialises.
func EppsteinKShortest[W Weight](c *csr.CSR[W], src, dst graph.NodeID, k int) []YenPath[W] {
	defer metrics.Time("search.EppsteinKShortest")()
	out, _ := EppsteinKShortestCtx(context.Background(), c, src, dst, k)
	return out
}

// EppsteinKShortestCtx is the context-aware variant of
// [EppsteinKShortest]. ctx.Err() is checked every 4096 heap pops;
// on cancellation returns (nil, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical heap-based k-shortest with loopless guard
func EppsteinKShortestCtx[W Weight](ctx context.Context, c *csr.CSR[W], src, dst graph.NodeID, k int) ([]YenPath[W], error) {
	defer metrics.Time("search.EppsteinKShortestCtx")()
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
	pq := &eppsteinPQ[W]{}
	heap.Init(pq)
	heap.Push(pq, eppsteinItem[W]{cost: 0, path: []graph.NodeID{src}})
	result := make([]YenPath[W], 0, k)
	tick := 0
	for pq.Len() > 0 && len(result) < k {
		tick++
		if tick&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.EppsteinKShortestCtx.errors", 1)
				return nil, err
			}
		}
		top := heap.Pop(pq).(eppsteinItem[W]) //nolint:errcheck // PQ types are statically known
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
			heap.Push(pq, eppsteinItem[W]{cost: top.cost + w, path: newPath})
		}
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// pathContains reports whether p contains v. Linear scan; for the
// short paths Eppstein produces (length << V) the constant beats a
// per-pop bitmap allocation.
func pathContains(p []graph.NodeID, v graph.NodeID) bool {
	for _, n := range p {
		if n == v {
			return true
		}
	}
	return false
}

// eppsteinItem is one priority-queue entry.
type eppsteinItem[W Weight] struct {
	cost W
	path []graph.NodeID
}

// eppsteinPQ is the heap-interface adapter — kept private so callers
// can't accidentally bypass the loopless guard in EppsteinKShortest.
type eppsteinPQ[W Weight] []eppsteinItem[W]

func (h eppsteinPQ[W]) Len() int           { return len(h) }
func (h eppsteinPQ[W]) Less(i, j int) bool { return h[i].cost < h[j].cost }
func (h eppsteinPQ[W]) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *eppsteinPQ[W]) Push(x any)        { *h = append(*h, x.(eppsteinItem[W])) } //nolint:errcheck // statically known
func (h *eppsteinPQ[W]) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
