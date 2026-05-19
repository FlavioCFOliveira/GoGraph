// Package centrality implements vertex importance metrics. v1
// carries Brandes' betweenness centrality and the PageRank family
// (T61/T62 sister tasks).
package centrality

import (
	"gograph/graph"
	"gograph/graph/csr"
)

// Betweenness computes the exact betweenness centrality of every
// node in c using Brandes' algorithm (2001). Returns a slice
// indexed by NodeID. Unweighted: O(V * E).
//
// The result is not normalised — callers can divide by
// (n-1)(n-2)/2 (undirected) or (n-1)(n-2) (directed) for the
// classical normalised score.
func Betweenness[W any](c *csr.CSR[W]) []float64 {
	maxID := int(c.MaxNodeID())
	cb := make([]float64, maxID)
	if maxID == 0 {
		return cb
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	sigma := make([]float64, maxID)
	dist := make([]int, maxID)
	delta := make([]float64, maxID)
	pred := make([][]int, maxID)

	for s := 0; s < maxID; s++ {
		brandesSource(s, maxID, verts, edges, sigma, dist, delta, pred, cb)
	}
	return cb
}

func brandesSource(s, maxID int, verts []uint64, edges []graph.NodeID, sigma []float64, dist []int, delta []float64, pred [][]int, cb []float64) {
	for i := 0; i < maxID; i++ {
		sigma[i] = 0
		dist[i] = -1
		delta[i] = 0
		pred[i] = pred[i][:0]
	}
	sigma[s] = 1
	dist[s] = 0
	queue := []int{s}
	stack := make([]int, 0, maxID)
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		stack = append(stack, v)
		for k := verts[v]; k < verts[v+1]; k++ {
			w := int(edges[k])
			if dist[w] < 0 {
				dist[w] = dist[v] + 1
				queue = append(queue, w)
			}
			if dist[w] == dist[v]+1 {
				sigma[w] += sigma[v]
				pred[w] = append(pred[w], v)
			}
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		w := stack[i]
		for _, v := range pred[w] {
			delta[v] += (sigma[v] / sigma[w]) * (1 + delta[w])
		}
		if w != s {
			cb[w] += delta[w]
		}
	}
}
