package csr

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// BuildSymmetricFromAdjList builds the symmetric (undirected) projection of
// adj in one pass over the adjacency. Its result is identical, column by
// column, to BuildFromAdjList(adj).BuildSymmetric(), and follows every rule
// [CSR.BuildSymmetric] documents; it differs only in cost: no intermediate
// directed CSR is allocated.
//
// The result is marked symmetric by construction ([CSR.KnownSymmetric]), as
// a [CSR.BuildSymmetric] result is.
//
// # Trade-off against the two-step build
//
// It trades CPU for memory. Measured with BenchmarkSymmetricProjection_OnePass and _TwoStep (50k
// vertices, 400k weighted arcs, Apple M4, benchstat n=10), against
// BuildFromAdjList(adj).BuildSymmetric(): 32% fewer bytes allocated
// (19.10 MiB against 28.27 MiB) and 31% fewer allocations, at 28% more time
// (22.63 ms against 17.69 ms). The time goes to reading the scattered
// adjacency entries twice — once to count the mirrored degrees, once to
// scatter — where the two-step build reads them once and then works on
// contiguous arrays. Prefer it where peak memory matters more than build time.
//
// Concurrency: like [BuildFromAdjList], it reads the CURRENT entry of every
// node twice (count, then scatter), so the caller must exclude writers for
// the duration. If an entry grows between the passes, the surplus arcs are
// dropped rather than written outside their run. The returned CSR is
// immutable and safe for concurrent reads.
//
// Complexity: O(V + E log d) time, where d is the longest run (O(V + E) when
// no run exceeds the insertion-sort cutoff of [OrderRuns]). Memory is the
// result's own O(V + E) arrays plus one O(V) cursor array and at most one
// [OrderRuns] scratch buffer per column.
func BuildSymmetricFromAdjList[N comparable, W any](adj *adjlist.AdjList[N, W]) *CSR[W] {
	maxID := uint64(adj.MaxNodeID())
	if maxID == 0 {
		return &CSR[W]{vertices: []uint64{0}, symmetric: true}
	}

	// Pass 1: count each node's symmetric degree into verts[x+1] — every arc
	// once at its source, and every non-loop arc once more at its destination
	// — then prefix-sum, so verts[x] is the START of x's run.
	verts := make([]uint64, maxID+1)
	var anyHandles bool
	for id := range maxID {
		nb, _, h := adj.LoadEntryH(graph.NodeID(id))
		verts[id+1] += uint64(len(nb))
		for _, dst := range nb {
			if uint64(dst) != id {
				verts[uint64(dst)+1]++
			}
		}
		if h != nil {
			anyHandles = true
		}
	}
	for x := uint64(1); x <= maxID; x++ {
		verts[x] += verts[x-1]
	}
	total := verts[maxID]

	edges := make([]graph.NodeID, total)
	var weights []W
	// Same gates as the directed build: a payload-carrying W and a source
	// adjacency not in weightless mode.
	if hasWeights[W]() && !adj.Weightless() {
		weights = make([]W, total)
	}
	var handles []uint64
	if anyHandles {
		handles = make([]uint64, total)
	}

	// Pass 2: an ascending scan emitting, per arc, the forward arc and then
	// its mirror — the emission order BuildSymmetric produces from the
	// directed snapshot, which is what makes the two builds identical after
	// the stable ordering of pass 3.
	cursor := make([]uint64, maxID)
	copy(cursor, verts[:maxID])
	emit := func(src, dst graph.NodeID, w W, h uint64) {
		pos := cursor[src]
		if pos >= verts[uint64(src)+1] {
			return // entry grew after pass 1: no room in this run
		}
		cursor[src] = pos + 1
		edges[pos] = dst
		if weights != nil {
			weights[pos] = w
		}
		if handles != nil {
			handles[pos] = h
		}
	}
	for id := range maxID {
		nb, ws, h := adj.LoadEntryH(graph.NodeID(id))
		src := graph.NodeID(id)
		for j, dst := range nb {
			var w W
			if weights != nil && j < len(ws) {
				w = ws[j]
			}
			var hj uint64 // 0 is the "no handle" sentinel, as in the directed build
			if j < len(h) {
				hj = h[j]
			}
			emit(src, dst, w, hj)
			if dst != src {
				emit(dst, src, w, hj)
			}
		}
	}

	// Pass 3: order each run by (destination, handle). See order.go.
	OrderRuns(verts, edges, weights, handles)

	return &CSR[W]{
		vertices:  verts,
		edges:     edges,
		weights:   weights,
		handles:   handles,
		order:     uint64(adj.Order()),
		size:      total,
		symmetric: true,
	}
}
