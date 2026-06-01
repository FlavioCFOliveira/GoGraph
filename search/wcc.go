package search

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/ds"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// WCC computes the weakly-connected components of c — equivalence
// classes induced by the symmetric closure of c's edge relation.
// Returns one int per live NodeID labelling its component in [0, K),
// where K is the number of components, plus K itself. Ghost slots
// (no incident edge) report -1.
//
// Internally WCC walks every edge once and unions the endpoints in a
// slice-backed Union-Find; final component IDs are compacted into
// [0, K) preserving order of first occurrence so the output is
// deterministic across runs of the same graph.
//
// Concurrency: WCC is safe to invoke concurrently on a shared CSR.
func WCC[W any](c *csr.CSR[W]) (component []int, k int, err error) {
	defer metrics.Time("search.WCC")()
	component, k, err = WCCCtx(context.Background(), c)
	if err != nil {
		metrics.IncCounter("search.WCC.errors", 1)
	}
	return component, k, err
}

// WCCCtx is the context-aware variant of [WCC]. ctx.Err() is checked
// once before the union sweep and once before the relabel sweep; on
// cancellation returns (nil, 0, wrapped ctx.Err()).
func WCCCtx[W any](ctx context.Context, c *csr.CSR[W]) (component []int, k int, err error) {
	defer metrics.Time("search.WCCCtx")()
	if cerr := ctx.Err(); cerr != nil {
		metrics.IncCounter("search.WCCCtx.errors", 1)
		return nil, 0, cerr
	}
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return nil, 0, nil
	}
	mask := c.LiveMask()
	uf := ds.NewSlice(maxID)
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	for u := 0; u < maxID; u++ {
		for k := verts[u]; k < verts[u+1]; k++ {
			uf.Union(u, int(edges[k]))
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		metrics.IncCounter("search.WCCCtx.errors", 1)
		return nil, 0, cerr
	}
	component = make([]int, maxID)
	for i := range component {
		component[i] = -1
	}
	// Compact root IDs into [0, K) preserving order of first occurrence.
	rootRemap := make(map[int]int, maxID)
	next := 0
	for i := 0; i < maxID; i++ {
		if !mask[i] {
			continue
		}
		r := uf.Find(i)
		id, ok := rootRemap[r]
		if !ok {
			id = next
			rootRemap[r] = id
			next++
		}
		component[i] = id
	}
	return component, next, nil
}
