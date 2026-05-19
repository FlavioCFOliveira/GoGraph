package search

import (
	"gograph/graph"
	"gograph/graph/csr"
)

// APSP is an all-pairs shortest-paths distance matrix.
//
// APSP is allocated and indexed over the set of live NodeIDs (those
// with at least one incident edge in the source CSR). The public At
// method accepts arbitrary NodeIDs and reports the pair as
// unreachable when either endpoint lies in a ghost slot.
type APSP[W Weight] struct {
	live    int   // number of live NodeIDs (compact matrix dimension)
	maxID   int   // CSR.MaxNodeID(); preserved for NodeID-space callers
	compact []int // length maxID; compact[id] is the index in [0, live) or -1
	dist    []W   // length live*live; row-major in compact space
}

// At returns the shortest-path distance from i to j and a bool
// reporting reachability. NodeIDs not present in the source CSR
// (ghost slots) always report unreachable.
func (a *APSP[W]) At(i, j graph.NodeID) (W, bool) {
	var zero W
	if int(i) >= a.maxID || int(j) >= a.maxID {
		return zero, false
	}
	ii := a.compact[int(i)]
	jj := a.compact[int(j)]
	if ii < 0 || jj < 0 {
		return zero, false
	}
	v := a.dist[ii*a.live+jj]
	if v == a.inf() {
		return zero, false
	}
	return v, true
}

// N returns the size of the underlying compact distance matrix
// (i.e. the count of live NodeIDs in the source CSR).
func (a *APSP[W]) N() int { return a.live }

// inf returns the in-band infinity sentinel used by the distance
// matrix.
func (a *APSP[W]) inf() W {
	var zero W
	v := zero
	for i := 0; i < 60; i++ {
		v++
		v += v
	}
	return v
}

// FloydWarshall computes APSP via the textbook O(V^3) DP, where V is
// the count of live NodeIDs (not the sparse MaxNodeID). It tolerates
// negative edge weights but assumes no negative cycle; the diagonal
// stays at 0.
//
// Concurrency: safe to invoke from any number of goroutines on a
// shared CSR; allocates its own working buffers per call.
//
//nolint:gocyclo // canonical Floyd-Warshall: live-mask compaction + matrix init + edge ingest + DP
func FloydWarshall[W Weight](c *csr.CSR[W]) *APSP[W] {
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
	}
	if live == 0 {
		return out
	}
	infV := out.inf()
	for i := range out.dist {
		out.dist[i] = infV
	}
	for i := 0; i < live; i++ {
		out.dist[i*live+i] = 0
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	for u := 0; u < maxID; u++ {
		ui := compact[u]
		if ui < 0 {
			continue
		}
		start := verts[u]
		end := verts[u+1]
		for k := start; k < end; k++ {
			v := int(edges[k])
			vi := compact[v]
			if vi < 0 {
				continue
			}
			var w W
			if weights != nil {
				w = weights[k]
			}
			if w < out.dist[ui*live+vi] {
				out.dist[ui*live+vi] = w
			}
		}
	}
	for k := 0; k < live; k++ {
		for i := 0; i < live; i++ {
			ik := out.dist[i*live+k]
			if ik == infV {
				continue
			}
			for j := 0; j < live; j++ {
				kj := out.dist[k*live+j]
				if kj == infV {
					continue
				}
				if cand := ik + kj; cand < out.dist[i*live+j] {
					out.dist[i*live+j] = cand
				}
			}
		}
	}
	return out
}
