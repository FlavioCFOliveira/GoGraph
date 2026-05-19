package search

import (
	"context"

	"gograph/graph/csr"
)

// KCore computes the coreness number of every vertex in c via the
// Batagelj-Zaversnik 2003 linear-time peeling algorithm. The result
// is a NodeID-indexed slice; ghost slots (no incident edge) report 0
// — the same coreness as a live but degree-0 vertex.
//
// The coreness of v is the largest k such that v belongs to a
// connected k-core of c. The algorithm proceeds by repeatedly
// peeling the lowest-degree vertex, recording its current degree as
// its coreness, and decrementing the remaining degree of each of
// its neighbours. Total time is O(V + E) via bucket-list peeling.
//
// Concurrency: KCore is safe to invoke concurrently on a shared CSR.
func KCore[W any](c *csr.CSR[W]) []int {
	out, _ := KCoreCtx(context.Background(), c)
	return out
}

// KCoreCtx is the context-aware variant of [KCore]. ctx.Err() is
// checked every 4096 peeled vertices; on cancellation returns
// (nil, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical Batagelj-Zaversnik bucket-peel
func KCoreCtx[W any](ctx context.Context, c *csr.CSR[W]) ([]int, error) {
	n := int(c.MaxNodeID())
	if n == 0 {
		return nil, nil
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	deg := make([]int, n)
	maxDeg := 0
	for v := 0; v < n; v++ {
		d := int(verts[v+1] - verts[v])
		deg[v] = d
		if d > maxDeg {
			maxDeg = d
		}
	}
	// Bucket sort. bin[d] holds the starting offset of bucket d in
	// vert; vert is a permutation of [0, n) ordered ascending by
	// current degree; pos[v] is v's current index in vert. The
	// classical B-Z technique: count, prefix-sum to obtain starts,
	// fill (incrementing bin[d] as each vertex lands), then back-
	// shift bin to recover the start-of-bucket invariant used by
	// the peel loop.
	bin := make([]int, maxDeg+2)
	for v := 0; v < n; v++ {
		bin[deg[v]+1]++
	}
	for d := 1; d <= maxDeg+1; d++ {
		bin[d] += bin[d-1]
	}
	vert := make([]int, n)
	pos := make([]int, n)
	for v := 0; v < n; v++ {
		p := bin[deg[v]]
		vert[p] = v
		pos[v] = p
		bin[deg[v]]++
	}
	for d := maxDeg; d > 0; d-- {
		bin[d] = bin[d-1]
	}
	bin[0] = 0
	coreness := make([]int, n)
	peelCount := 0
	for i := 0; i < n; i++ {
		peelCount++
		if peelCount&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		v := vert[i]
		coreness[v] = deg[v]
		// Decrement each neighbour u with strictly higher current
		// degree, sliding u one slot earlier in vert.
		for k := verts[v]; k < verts[v+1]; k++ {
			u := int(edges[k])
			if deg[u] > deg[v] {
				du := deg[u]
				// Swap u with the vertex at bin[du] (the start of
				// u's current degree bucket).
				w := vert[bin[du]]
				if u != w {
					vert[pos[u]] = w
					vert[bin[du]] = u
					pos[w] = pos[u]
					pos[u] = bin[du]
				}
				bin[du]++
				deg[u]--
			}
		}
	}
	return coreness, nil
}
