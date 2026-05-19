package search

import (
	"gograph/graph"
	"gograph/graph/csr"
)

// APSP is an all-pairs shortest-paths distance matrix.
type APSP[W Weight] struct {
	n    int
	dist []W
}

// At returns the shortest-path distance from i to j and a bool
// reporting reachability.
func (a *APSP[W]) At(i, j graph.NodeID) (W, bool) {
	var zero W
	if int(i) >= a.n || int(j) >= a.n {
		return zero, false
	}
	v := a.dist[int(i)*a.n+int(j)]
	if v == a.inf() {
		return zero, false
	}
	return v, true
}

// N returns the matrix size.
func (a *APSP[W]) N() int { return a.n }

// inf returns the in-band infinity sentinel used by the distance
// matrix. It is the smallest negative of W's zero plus a large
// constant — chosen so a comparison "value == inf" reliably marks
// unreachability without overflow during relaxation.
func (a *APSP[W]) inf() W {
	var zero W
	v := zero
	for i := 0; i < 60; i++ {
		v++
		v += v
	}
	return v
}

// FloydWarshall computes APSP via the textbook O(V^3) DP. It
// tolerates negative edge weights but assumes no negative cycle;
// the diagonal stays at 0.
func FloydWarshall[W Weight](c *csr.CSR[W]) *APSP[W] {
	maxID := int(c.MaxNodeID())
	out := &APSP[W]{n: maxID, dist: make([]W, maxID*maxID)}
	infV := out.inf()
	for i := range out.dist {
		out.dist[i] = infV
	}
	for i := 0; i < maxID; i++ {
		out.dist[i*maxID+i] = 0
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	for u := 0; u < maxID; u++ {
		start := verts[u]
		end := verts[u+1]
		for k := start; k < end; k++ {
			v := int(edges[k])
			var w W
			if weights != nil {
				w = weights[k]
			}
			if w < out.dist[u*maxID+v] {
				out.dist[u*maxID+v] = w
			}
		}
	}
	for k := 0; k < maxID; k++ {
		for i := 0; i < maxID; i++ {
			ik := out.dist[i*maxID+k]
			if ik == infV {
				continue
			}
			for j := 0; j < maxID; j++ {
				kj := out.dist[k*maxID+j]
				if kj == infV {
					continue
				}
				if cand := ik + kj; cand < out.dist[i*maxID+j] {
					out.dist[i*maxID+j] = cand
				}
			}
		}
	}
	return out
}
