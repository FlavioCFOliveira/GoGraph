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
	live    int    // number of live NodeIDs (compact matrix dimension)
	maxID   int    // CSR.MaxNodeID(); preserved for NodeID-space callers
	compact []int  // length maxID; compact[id] is the index in [0, live) or -1
	dist    []W    // length live*live; row-major in compact space
	found   []bool // parallel reachability bitmap; obviates an in-band Inf sentinel
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
	idx := ii*a.live + jj
	if !a.found[idx] {
		return zero, false
	}
	return a.dist[idx], true
}

// N returns the size of the underlying compact distance matrix
// (i.e. the count of live NodeIDs in the source CSR).
func (a *APSP[W]) N() int { return a.live }

// FloydWarshall computes APSP via the textbook O(V^3) DP, where V is
// the count of live NodeIDs (not the sparse MaxNodeID). It tolerates
// negative edge weights but assumes no negative cycle; the diagonal
// stays at 0.
//
// Reachability is tracked via a parallel found[] bitmap, not an
// in-band +Inf sentinel — the v1.0.0 sentinel constructed by 60
// iterations of v++/v+=v wrapped on integer types and silently
// corrupted distances. The bitmap is correct for every Weight type.
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
		found:   make([]bool, live*live),
	}
	if live == 0 {
		return out
	}
	for i := 0; i < live; i++ {
		idx := i*live + i
		out.found[idx] = true
		// dist[idx] already W's zero value.
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
			idx := ui*live + vi
			if !out.found[idx] || w < out.dist[idx] {
				out.dist[idx] = w
				out.found[idx] = true
			}
		}
	}
	for k := 0; k < live; k++ {
		for i := 0; i < live; i++ {
			ikIdx := i*live + k
			if !out.found[ikIdx] {
				continue
			}
			ik := out.dist[ikIdx]
			for j := 0; j < live; j++ {
				kjIdx := k*live + j
				if !out.found[kjIdx] {
					continue
				}
				kj := out.dist[kjIdx]
				cand := ik + kj
				idx := i*live + j
				if !out.found[idx] || cand < out.dist[idx] {
					out.dist[idx] = cand
					out.found[idx] = true
				}
			}
		}
	}
	return out
}
