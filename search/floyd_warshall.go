package search

import (
	"context"
	"runtime"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
)

// APSP is an all-pairs shortest-paths distance matrix.
//
// APSP is allocated and indexed over the set of live NodeIDs (those
// with at least one incident edge in the source CSR). The public At
// method accepts arbitrary NodeIDs and reports the pair as
// unreachable when either endpoint lies in a ghost slot.
//
// APSP is safe for concurrent reads.
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
// For floating-point Weight types it validates that no edge weight
// is NaN or +/-Inf; the simple entry returns nil and the Ctx variant
// returns [ErrInvalidInput] otherwise. Integer Weight types skip
// that pass.
//
// Concurrency: safe to invoke from any number of goroutines on a
// shared CSR; allocates its own working buffers per call.
func FloydWarshall[W Weight](c *csr.CSR[W]) *APSP[W] {
	defer metrics.Time("search.FloydWarshall")()
	out, _ := FloydWarshallCtx(context.Background(), c)
	return out
}

// FloydWarshallCtx is the context-aware variant of [FloydWarshall].
// ctx.Err() is checked at every k-pivot iteration (the outermost
// loop); on cancellation returns (nil, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical Floyd-Warshall: NaN/Inf gate + live-mask compaction + matrix init + edge ingest + DP
func FloydWarshallCtx[W Weight](ctx context.Context, c *csr.CSR[W]) (*APSP[W], error) {
	defer metrics.Time("search.FloydWarshallCtx")()
	// Float Weight types: NaN / +/-Inf silently corrupts every
	// k-pivot relaxation. Fail fast at the public boundary; integer
	// W short-circuits in O(1).
	if anyFloatInvalid(c.WeightsSlice()) {
		metrics.IncCounter("search.FloydWarshallCtx.errors", 1)
		return nil, ErrInvalidInput
	}
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
		return out, nil
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
	// Materialise the k-th column into a contiguous scratch vector
	// once per k-pivot. The original k-i-j loop reads dist[i, k]
	// with stride live*sizeof(W), forcing a fresh cache line per i
	// (the live=2048 case lands in DRAM bandwidth territory). The
	// scratch vector turns that into a single hot row read in the
	// inner i-loop, recovering 2x+ on M4-class cores while
	// preserving the canonical k-i-j inner-loop block.
	ikCol := make([]W, live)
	ikFound := make([]bool, live)
	for k := 0; k < live; k++ {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.FloydWarshallCtx.errors", 1)
			return nil, err
		}
		// Cooperative yield every 64 k-pivots so concurrent short
		// queries scheduled on the same M see progress; the overhead
		// is under 1% on V=1024 inputs (one Gosched per ~262K cells).
		if k&0x3F == 0 {
			runtime.Gosched()
		}
		// Materialise dist[*, k] and found[*, k].
		for i := 0; i < live; i++ {
			idx := i*live + k
			ikCol[i] = out.dist[idx]
			ikFound[i] = out.found[idx]
		}
		kRow := k * live
		for i := 0; i < live; i++ {
			if !ikFound[i] {
				continue
			}
			ik := ikCol[i]
			iRow := i * live
			for j := 0; j < live; j++ {
				kjIdx := kRow + j
				if !out.found[kjIdx] {
					continue
				}
				cand := ik + out.dist[kjIdx]
				idx := iRow + j
				if !out.found[idx] || cand < out.dist[idx] {
					out.dist[idx] = cand
					out.found[idx] = true
				}
			}
		}
	}
	return out, nil
}
