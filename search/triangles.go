package search

import (
	"context"
	"slices"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// CountTriangles returns the total number of triangles in the
// undirected graph c, plus a per-NodeID count of how many triangles
// each vertex participates in. c is expected to be a symmetric
// directed CSR (each undirected edge appears as both (u, v) and
// (v, u)) — typical of [adjlist.AdjList] with Directed=false.
//
// # Algorithm
//
// The implementation is the node-iterator algorithm with degree
// ordering: at every vertex v we examine pairs of neighbours
// (u, w) only when both have a strictly higher canonical rank than
// v. Canonical rank breaks ties on raw NodeID, so each triangle is
// counted exactly once.
//
// # Complexity
//
// Time:  O(sum over v of deg(v)^2 in the degree-ordered subgraph),
//
//	bounded by O(E * sqrt(E)) per Chiba & Nishizeki 1985.
//	For uniform-degree graphs this collapses to
//	O(E * d_avg); for power-law graphs the sqrt(E) bound
//	is tight.
//
// Space: O(V) for the per-node count array plus O(d_max) scratch
//
//	for the per-vertex neighbour scan (reused across
//	vertices).
//
// # Streaming variant
//
// CountTriangles requires the full CSR in memory: the inner loop
// random-accesses neighbour lists of arbitrary vertices. A
// streaming variant for graphs that do not fit in RAM is not
// provided — the implementation cost is non-trivial
// (reservoir sampling à la TRIEST, or a wedge-based estimator)
// and no production caller has asked for it. If such a caller
// surfaces, the natural seam is in search/extern/triangles.go
// alongside the existing semi-external BFS and PageRank.
//
// Concurrency: CountTriangles is safe to invoke concurrently on a
// shared CSR.
func CountTriangles[W any](c *csr.CSR[W]) (total int64, perNode []int64) {
	defer metrics.Time("search.CountTriangles").Stop()
	total, perNode, _ = CountTrianglesCtx(context.Background(), c)
	return total, perNode
}

// CountTrianglesCtx is the context-aware variant of [CountTriangles].
// ctx.Err() is checked every 4096 candidate-pair iterations of the
// inner neighbour-pair loop; on cancellation returns
// (0, nil, wrapped ctx.Err()). The counter advances on inner work
// rather than per outer vertex because the inner pair-loop is
// O(deg(v)^2): at a star-hub vertex (degree in the millions) a single
// outer iteration would otherwise run O(V^2) with no ctx check.
func CountTrianglesCtx[W any](ctx context.Context, c *csr.CSR[W]) (total int64, perNode []int64, err error) {
	defer metrics.Time("search.CountTrianglesCtx").Stop()
	if cerr := ctx.Err(); cerr != nil {
		metrics.IncCounter("search.CountTrianglesCtx.errors", 1)
		return 0, nil, cerr
	}
	n := int(c.MaxNodeID())
	if n == 0 {
		return 0, nil, nil
	}
	p := prepareTrianglePlan(c, n)
	perNode = make([]int64, n)
	loops := 0
	for v := 0; v < n; v++ {
		if cerr := countTrianglesVertex(ctx, p, v, &total, perNode, &loops); cerr != nil {
			metrics.IncCounter("search.CountTrianglesCtx.errors", 1)
			return 0, nil, cerr
		}
	}
	return total, perNode, nil
}

// trianglePlan bundles the source-independent products of the
// triangle-count setup: the per-vertex-sorted adjacency copy (so the
// inner edge test is a binary search), the CSR vertex-offset slice,
// and the canonical degree-ordered rank. Every field is read-only once
// [prepareTrianglePlan] returns, so the plan is safe to share by
// pointer across the per-vertex workers of [CountTrianglesParallel].
type trianglePlan struct {
	adjBuf []graph.NodeID
	verts  []uint64
	rank   []int
}

// prepareTrianglePlan computes the sorted adjacency copy and the
// degree-ordered canonical rank shared by the serial and parallel
// triangle counters. It never mutates c (the adjacency is sorted in a
// fresh copy).
func prepareTrianglePlan[W any](c *csr.CSR[W], n int) trianglePlan {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	// Sort each adjacency list ascending so the per-vertex inner
	// "is u-v an edge?" check is O(log deg) via binary search.
	// Allocate a fresh edges copy; we must not mutate c.
	adjBuf := make([]graph.NodeID, len(edges))
	copy(adjBuf, edges)
	for v := 0; v < n; v++ {
		slices.Sort(adjBuf[verts[v]:verts[v+1]])
	}
	deg := make([]int, n)
	for v := 0; v < n; v++ {
		deg[v] = int(verts[v+1] - verts[v])
	}
	// Canonical rank: degree-asc, ties broken by NodeID-asc. We
	// store the rank explicitly so the pair test below is
	// constant-time.
	rank := make([]int, n)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		if deg[order[i]] != deg[order[j]] {
			return deg[order[i]] < deg[order[j]]
		}
		return order[i] < order[j]
	})
	for r, v := range order {
		rank[v] = r
	}
	return trianglePlan{adjBuf: adjBuf, verts: verts, rank: rank}
}

// countTrianglesVertex counts the triangles whose lowest-ranked vertex
// is v, accumulating the count into *total and the per-participant
// counts into perNode. loops is a caller-owned running counter of
// inner candidate-pair iterations; ctx.Err() is polled every 4096 of
// them so cancellation latency stays bounded even at a high-degree hub.
//
// The triangle monoid is integer addition: counting only triangles
// rooted at each v (and giving every vertex of a found triangle one
// increment) means each triangle is tallied exactly once regardless of
// the order in which vertices are visited. Both *total and perNode are
// caller-private in the parallel path, so concurrent workers never
// share a counter; their partials are summed exactly in the reduce.
func countTrianglesVertex(ctx context.Context, p trianglePlan, v int, total *int64, perNode []int64, loops *int) error {
	verts, adjBuf, rank := p.verts, p.adjBuf, p.rank
	// Collect the higher-ranked neighbours of v.
	nb := adjBuf[verts[v]:verts[v+1]]
	// For each pair (u, w) where rank[u] > rank[v] and rank[w] > rank[v]
	// and u < w (in NodeID order to dedupe), check if u-w is an edge.
	for i := 0; i < len(nb); i++ {
		u := nb[i]
		if rank[u] <= rank[v] {
			continue
		}
		uAdj := adjBuf[verts[u]:verts[u+1]]
		for j := i + 1; j < len(nb); j++ {
			// Poll on inner work: the pair-loop is O(deg(v)^2), so
			// counting candidate pairs (not outer vertices) bounds
			// cancellation latency at a high-degree hub.
			if *loops&0xFFF == 0 {
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
			}
			*loops++
			w := nb[j]
			if rank[w] <= rank[v] {
				continue
			}
			idx := sort.Search(len(uAdj), func(k int) bool { return uAdj[k] >= w })
			if idx < len(uAdj) && uAdj[idx] == w {
				*total++
				perNode[v]++
				perNode[uint64(u)]++
				perNode[uint64(w)]++
			}
		}
	}
	return nil
}
