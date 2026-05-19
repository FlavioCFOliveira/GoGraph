package centrality

import (
	"context"
	"math"
	"runtime"

	"gograph/graph"
	"gograph/graph/csr"
)

// WeightedBetweenness computes the weighted betweenness centrality
// of every NodeID in c using Dijkstra-augmented Brandes (Brandes
// 2001 §3, weighted variant). Edge weights must be non-negative.
//
// Complexity is O(V * (E log V)) for binary-heap-backed Dijkstra
// per source. The result is not normalised; callers wanting the
// classical 1 / ((n-1)(n-2)) factor can divide externally.
//
// Concurrency: WeightedBetweenness is safe to invoke concurrently
// on a shared CSR.
func WeightedBetweenness(c *csr.CSR[float64]) []float64 {
	out, _ := WeightedBetweennessCtx(context.Background(), c)
	return out
}

// WeightedBetweennessCtx is the context-aware variant of
// [WeightedBetweenness]. ctx.Err() is checked once per source vertex;
// on cancellation returns (nil, wrapped ctx.Err()).
func WeightedBetweennessCtx(ctx context.Context, c *csr.CSR[float64]) ([]float64, error) {
	n := int(c.MaxNodeID())
	cb := make([]float64, n)
	if n == 0 {
		return cb, nil
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	sigma := make([]float64, n)
	dist := make([]float64, n)
	delta := make([]float64, n)
	pred := make([][]int, n)
	stack := make([]int, 0, n)
	h := newWeightedHeap(n)

	for s := 0; s < n; s++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s&0x3F == 0 {
			runtime.Gosched()
		}
		weightedBrandesSource(s, n, verts, edges, weights, sigma, dist, delta, pred, cb, &stack, h)
	}
	return cb, nil
}

func weightedBrandesSource(s, n int, verts []uint64, edges []graph.NodeID, weights, sigma, dist, delta []float64, pred [][]int, cb []float64, stack *[]int, h *weightedHeap) {
	for i := 0; i < n; i++ {
		sigma[i] = 0
		dist[i] = math.Inf(1)
		delta[i] = 0
		pred[i] = pred[i][:0]
	}
	sigma[s] = 1
	dist[s] = 0
	*stack = (*stack)[:0]
	h.reset()
	h.push(s, 0)
	for h.len() > 0 {
		v, dv := h.pop()
		if dv > dist[v] {
			continue
		}
		*stack = append(*stack, v)
		for k := verts[v]; k < verts[v+1]; k++ {
			w := int(edges[k])
			ew := weights[k]
			cand := dv + ew
			if cand < dist[w] {
				dist[w] = cand
				sigma[w] = sigma[v]
				pred[w] = append(pred[w][:0], v)
				h.push(w, cand)
			} else if cand == dist[w] {
				sigma[w] += sigma[v]
				pred[w] = append(pred[w], v)
			}
		}
	}
	for i := len(*stack) - 1; i >= 0; i-- {
		w := (*stack)[i]
		for _, v := range pred[w] {
			delta[v] += (sigma[v] / sigma[w]) * (1 + delta[w])
		}
		if w != s {
			cb[w] += delta[w]
		}
	}
}

// weightedHeap is a small binary min-heap keyed by float64
// distance, used exclusively by [WeightedBetweennessCtx]. Inline
// pair storage avoids pointer chasing in the hot loop.
type weightedHeap struct {
	items []weightedHeapItem
}

type weightedHeapItem struct {
	node int
	dist float64
}

func newWeightedHeap(cap0 int) *weightedHeap {
	return &weightedHeap{items: make([]weightedHeapItem, 0, cap0)}
}

func (h *weightedHeap) reset()   { h.items = h.items[:0] }
func (h *weightedHeap) len() int { return len(h.items) }

func (h *weightedHeap) push(node int, dist float64) {
	h.items = append(h.items, weightedHeapItem{node: node, dist: dist})
	i := len(h.items) - 1
	for i > 0 {
		p := (i - 1) / 2
		if h.items[p].dist <= h.items[i].dist {
			break
		}
		h.items[p], h.items[i] = h.items[i], h.items[p]
		i = p
	}
}

func (h *weightedHeap) pop() (node int, dist float64) {
	top := h.items[0]
	last := len(h.items) - 1
	h.items[0] = h.items[last]
	h.items = h.items[:last]
	if len(h.items) == 0 {
		return top.node, top.dist
	}
	i := 0
	for {
		l := 2*i + 1
		r := 2*i + 2
		smallest := i
		if l < len(h.items) && h.items[l].dist < h.items[smallest].dist {
			smallest = l
		}
		if r < len(h.items) && h.items[r].dist < h.items[smallest].dist {
			smallest = r
		}
		if smallest == i {
			break
		}
		h.items[smallest], h.items[i] = h.items[i], h.items[smallest]
		i = smallest
	}
	return top.node, top.dist
}
