package search

import (
	"context"
	"runtime"
	"sort"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/ds"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// wccParallelMinEdges is the smallest edge count for which
// [WCCParallelCtx] partitions the union sweep across the worker pool.
// Below it the per-worker union-find allocation plus the single-threaded
// merge cost more than the serial sweep saves (WCC is already among the
// cheapest analytics ops), so the call runs the serial union path.
const wccParallelMinEdges = 1 << 16

// WCCParallel computes the weakly-connected components of c with the
// edge-union sweep distributed across a bounded worker pool. It returns
// exactly the same partition as [WCC] — the same per-NodeID component
// labels in [0, K) and the same K — because connected components are a
// function of the edge set alone (the union relation is associative and
// commutative), and the deterministic ascending-NodeID relabel pass is
// shared verbatim with the serial path.
//
// The parallelism is Option-B sharding (rmp #1679): each worker unions a
// disjoint, edge-balanced slice of the edge list into its own private
// union-find, then a single-threaded pass merges the W private forests
// into one. This is race-free by construction — workers touch only their
// own forest and read the immutable CSR — at the cost of an O(W * live)
// serial merge, so the speedup is bounded (WCC is bandwidth- and
// merge-bound); concurrent atomic union-find would scale further but is
// unwarranted for the cheapest op.
//
// numWorkers <= 0 picks runtime.GOMAXPROCS(0); it is then treated as an
// upper bound and capped at the merge-optimal ~sqrt(edges/live), beyond
// which the serial merge dominates and extra workers regress the
// speedup. With an effective count of 1 — numWorkers == 1, a graph below
// [wccParallelMinEdges], or a graph too sparse for the merge to pay — the
// call runs the serial union sweep and skips the merge entirely.
//
// Concurrency: WCCParallel reads the immutable CSR without
// synchronisation and allocates its own working storage per call, so it
// is safe to invoke concurrently on a shared CSR.
func WCCParallel[W any](c *csr.CSR[W], numWorkers int) (component []int, k int, err error) {
	defer metrics.Time("search.WCCParallel")()
	component, k, err = WCCParallelCtx(context.Background(), c, numWorkers)
	if err != nil {
		metrics.IncCounter("search.WCCParallel.errors", 1)
	}
	return component, k, err
}

// WCCParallelCtx is the context-aware variant of [WCCParallel]. ctx.Err()
// is checked once before the union sweep and once before the relabel
// sweep; on cancellation returns (nil, 0, wrapped ctx.Err()).
func WCCParallelCtx[W any](ctx context.Context, c *csr.CSR[W], numWorkers int) (component []int, k int, err error) {
	defer metrics.Time("search.WCCParallelCtx")()
	if cerr := ctx.Err(); cerr != nil {
		metrics.IncCounter("search.WCCParallelCtx.errors", 1)
		return nil, 0, cerr
	}
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return nil, 0, nil
	}
	dense, live := wccBuildDense(c, maxID)
	if live == 0 {
		return wccGhostComponent(maxID), 0, nil
	}

	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	numEdges := len(edges)

	if numWorkers <= 0 {
		numWorkers = runtime.GOMAXPROCS(0)
	}
	if numWorkers > numEdges {
		numWorkers = numEdges
	}
	// The single-threaded merge costs O(numWorkers * live), so total work
	// is ~ numEdges/numWorkers (parallel union) + numWorkers*live (merge),
	// minimised near numWorkers = sqrt(numEdges/live). Past that crossover
	// the merge dominates and extra workers regress the speedup, so cap
	// the effective count there: requesting more cores never slows WCC.
	if opt := wccMergeOptimalWorkers(numEdges, live); numWorkers > opt {
		numWorkers = opt
	}

	var global *ds.UnionFindSlice
	if numWorkers <= 1 || numEdges < wccParallelMinEdges {
		// Serial union sweep — same partition, no merge.
		global = ds.NewSlice(live)
		wccUnionEdgeRange(global, dense, verts, edges, 0, numEdges, maxID)
	} else {
		// Each worker unions a disjoint edge-balanced range into its own
		// private forest; a single-threaded pass then merges them.
		locals := make([]*ds.UnionFindSlice, numWorkers)
		var wg sync.WaitGroup
		for w := 0; w < numWorkers; w++ {
			elo := w * numEdges / numWorkers
			ehi := (w + 1) * numEdges / numWorkers
			locals[w] = ds.NewSlice(live)
			wg.Add(1)
			go func(uf *ds.UnionFindSlice, elo, ehi int) {
				defer wg.Done()
				wccUnionEdgeRange(uf, dense, verts, edges, elo, ehi, maxID)
			}(locals[w], elo, ehi)
		}
		wg.Wait()
		global = ds.NewSlice(live)
		for _, local := range locals {
			wccMergeInto(global, local, live)
		}
	}

	if cerr := ctx.Err(); cerr != nil {
		metrics.IncCounter("search.WCCParallelCtx.errors", 1)
		return nil, 0, cerr
	}
	component, k = wccRelabel(global, dense, maxID, live)
	return component, k, nil
}

// wccUnionEdgeRange unions the endpoints of the edges in the contiguous
// edge-index range [elo, ehi) into uf, in the dense [0, live) space. The
// source vertex of an edge index is recovered by binary-searching the
// CSR row offsets for the node owning elo, then advancing as the index
// crosses each row boundary — so a worker needs no precomputed per-edge
// source array. A node that owns an edge necessarily has an incident
// edge and is therefore live (dense[u] >= 0).
func wccUnionEdgeRange(uf *ds.UnionFindSlice, dense []int, verts []uint64, edges []graph.NodeID, elo, ehi, maxID int) {
	if elo >= ehi {
		return
	}
	// u owns edge elo: the smallest node whose exclusive row end exceeds
	// elo (verts[u] <= elo < verts[u+1]).
	u := sort.Search(maxID, func(i int) bool { return verts[i+1] > uint64(elo) })
	for k := elo; k < ehi; k++ {
		for uint64(k) >= verts[u+1] {
			u++
		}
		dv := dense[int(edges[k])]
		if dv < 0 {
			// A live source's neighbour is live by the LiveMask
			// definition; this branch keeps the union total robust under
			// any future mask/edge skew.
			continue
		}
		uf.Union(dense[u], dv)
	}
}

// wccMergeOptimalWorkers returns floor(sqrt(numEdges/live)), the worker
// count that minimises the parallel-union-plus-merge total work (see
// [WCCParallelCtx]). It returns at least 1; a ratio below 4 (a graph too
// sparse for the merge to pay) collapses to the serial sweep.
func wccMergeOptimalWorkers(numEdges, live int) int {
	if live <= 0 {
		return 1
	}
	ratio := numEdges / live
	w := 1
	for (w+1)*(w+1) <= ratio {
		w++
	}
	return w
}

// wccMergeInto folds the private forest local into the shared forest
// global, both over the dense [0, live) space. Replaying each non-root
// node against its local representative reproduces local's equivalence
// classes inside global; doing this for every worker's forest yields the
// union of all partitions — the exact full-graph partition. Single-
// threaded: global is mutated by this pass alone.
func wccMergeInto(global, local *ds.UnionFindSlice, live int) {
	for n := 0; n < live; n++ {
		r := local.Find(n)
		if r != n {
			global.Union(n, r)
		}
	}
}
