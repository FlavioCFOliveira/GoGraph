package search

import (
	"context"
	"runtime"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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
// negative edge weights and detects negative cycles via a post-DP
// diagonal scan; in the no-negative-cycle case the diagonal stays at 0.
//
// Reachability is tracked via a parallel found[] bitmap, not an
// in-band +Inf sentinel — an earlier sentinel constructed by 60
// iterations of v++/v+=v wrapped on integer types and silently
// corrupted distances. The bitmap is correct for every Weight type.
//
// For floating-point Weight types it validates that no edge weight
// is NaN or +/-Inf; the simple entry returns nil and the Ctx variant
// returns [ErrInvalidInput] otherwise. Integer Weight types skip
// that pass.
//
// When the graph contains a negative-weight cycle, the simple entry
// returns nil; the Ctx variant returns [ErrNegativeCycle] (the same
// sentinel used by [BellmanFord]). The check is the canonical CLRS
// §25.2 post-pass: any live vertex whose self-distance has been
// relaxed below zero lies on a negative cycle.
//
// Integer-Weight overflow precondition. The DP accumulates path
// distances in W's own arithmetic (dist[i,k] + dist[k,j]) with no
// overflow guard on the hot path. For an integer Weight type the caller
// must ensure that the longest shortest path's cumulative weight fits W;
// otherwise the addition wraps and both the relaxation and the post-DP
// negative-cycle scan compare wrapped values, yielding a silently
// incorrect matrix. The NaN/+-Inf gate covers only floating-point W.
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
// Errors surfaced: [ErrInvalidInput] (NaN/Inf float weight),
// [ErrNegativeCycle] (negative-weight cycle detected post-DP),
// or the underlying ctx.Err() on cancellation.
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
	out := floydInit[W](c, maxID, compact, live)
	if live == 0 {
		return out, nil
	}
	if err := floydRunDP[W](ctx, out, live); err != nil {
		metrics.IncCounter("search.FloydWarshallCtx.errors", 1)
		return nil, err
	}
	if floydHasNegativeCycle(out, live) {
		metrics.IncCounter("search.FloydWarshallCtx.errors", 1)
		return nil, ErrNegativeCycle
	}
	return out, nil
}

// floydInit allocates the compact APSP matrix, seeds the zero-distance
// diagonal, and ingests the CSR edges (taking the minimum weight on
// parallel edges). It is the shared prologue for the serial
// [FloydWarshallCtx] and the parallel [FloydWarshallParallelCtx].
func floydInit[W Weight](c *csr.CSR[W], maxID int, compact []int, live int) *APSP[W] {
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
	return out
}

// floydPivotRows relaxes destination rows [lo,hi) against pivot k using
// pre-snapshotted column k (colDist[i] = dist[i][k], colFound parallel)
// and row k (rowDist[j] = dist[k][j], rowFound parallel). This is the
// canonical CLRS recurrence d_k[i][j] = min(d_{k-1}[i][j],
// d_{k-1}[i][k] + d_{k-1}[k][j]) with both right-hand terms read from
// the pre-pivot snapshot rather than in place.
//
// Reading column k and row k from snapshot vectors — never from the
// live matrix — is what makes the i-loop a pure function of (lo,hi):
// the kernel reads only the two read-only snapshots plus the rows it
// owns, and writes only rows in [lo,hi). Distinct [lo,hi) ranges touch
// disjoint memory, so any partition of [0,live) across goroutines is
// race-free and produces a result independent of the partition — the
// foundation of the bit-identical parallel variant (rmp #1680). It also
// keeps the column read cache-hot (a contiguous vector rather than a
// strided dist[i][k] gather).
func floydPivotRows[W Weight](dist []W, found []bool, live, lo, hi int, colDist []W, colFound []bool, rowDist []W, rowFound []bool) {
	for i := lo; i < hi; i++ {
		if !colFound[i] {
			continue
		}
		ik := colDist[i]
		iRow := i * live
		for j := 0; j < live; j++ {
			if !rowFound[j] {
				continue
			}
			cand := ik + rowDist[j]
			idx := iRow + j
			if !found[idx] || cand < dist[idx] {
				dist[idx] = cand
				found[idx] = true
			}
		}
	}
}

// floydSnapshotPivot materialises column k and row k of out into the
// caller-owned scratch vectors before a pivot's relaxation. Splitting
// the snapshot from the relaxation lets the parallel variant publish
// both vectors once (under the coordinator) and then fan the disjoint
// destination-row ranges out to workers that read them race-free.
func floydSnapshotPivot[W Weight](out *APSP[W], live, k int, colDist []W, colFound []bool, rowDist []W, rowFound []bool) {
	kRow := k * live
	for i := 0; i < live; i++ {
		colIdx := i*live + k
		colDist[i] = out.dist[colIdx]
		colFound[i] = out.found[colIdx]
		rowDist[i] = out.dist[kRow+i]
		rowFound[i] = out.found[kRow+i]
	}
}

// floydRunDP runs the serial O(V^3) pivot loop over the prepared matrix,
// honouring ctx cancellation at every pivot. It snapshots column k and
// row k once per pivot and relaxes the whole [0,live) destination range
// through the shared [floydPivotRows] kernel, so its output is
// bit-identical to the parallel variant by construction.
func floydRunDP[W Weight](ctx context.Context, out *APSP[W], live int) error {
	colDist := make([]W, live)
	colFound := make([]bool, live)
	rowDist := make([]W, live)
	rowFound := make([]bool, live)
	for k := 0; k < live; k++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Cooperative yield every 64 k-pivots so concurrent short
		// queries scheduled on the same M see progress; the overhead
		// is under 1% on V=1024 inputs (one Gosched per ~262K cells).
		if k&0x3F == 0 {
			runtime.Gosched()
		}
		floydSnapshotPivot[W](out, live, k, colDist, colFound, rowDist, rowFound)
		floydPivotRows[W](out.dist, out.found, live, 0, live, colDist, colFound, rowDist, rowFound)
	}
	return nil
}

// floydHasNegativeCycle is the canonical CLRS §25.2 post-DP scan: any
// diagonal entry dist[i,i] strictly below zero proves vertex i lies on a
// negative-weight cycle. Without this check the matrix would silently
// report finite distances polluted by the cycle.
func floydHasNegativeCycle[W Weight](out *APSP[W], live int) bool {
	var zero W
	for i := 0; i < live; i++ {
		if out.dist[i*live+i] < zero {
			return true
		}
	}
	return false
}
