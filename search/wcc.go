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
	defer metrics.Time("search.WCC").Stop()
	component, k, err = WCCCtx(context.Background(), c)
	if err != nil {
		metrics.IncCounter("search.WCC.errors", 1)
	}
	return component, k, err
}

// WCCCtx is the context-aware variant of [WCC]. ctx.Err() is checked
// once before the union sweep and once before the relabel sweep; on
// cancellation returns (nil, 0, wrapped ctx.Err()).
//
// The union-find universe and the working storage are sized on the live
// NodeID count, not on MaxNodeID(), so a sparse (shard-amplified) NodeID
// space cannot inflate the allocation beyond O(live) (rmp #1474). The
// returned component slice is still indexed by NodeID (length MaxNodeID)
// to preserve the public contract — ghost / isolated slots report -1 —
// but the disjoint-set arithmetic runs in the compact [0, live) space.
func WCCCtx[W any](ctx context.Context, c *csr.CSR[W]) (component []int, k int, err error) {
	defer metrics.Time("search.WCCCtx").Stop()
	if cerr := ctx.Err(); cerr != nil {
		metrics.IncCounter("search.WCCCtx.errors", 1)
		return nil, 0, cerr
	}
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return nil, 0, nil
	}
	dense, live := wccBuildDense(c, maxID)
	if live == 0 {
		// No incident edges anywhere: every slot is a ghost. Preserve
		// the documented all-(-1) NodeID-indexed output with K=0.
		return wccGhostComponent(maxID), 0, nil
	}
	uf := ds.NewSlice(live)
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	for u := 0; u < maxID; u++ {
		du := dense[u]
		if du < 0 {
			continue
		}
		for k := verts[u]; k < verts[u+1]; k++ {
			dv := dense[int(edges[k])]
			if dv < 0 {
				// A live source's neighbour is live by the LiveMask
				// definition; this branch keeps the union total under
				// any future mask/edge skew.
				continue
			}
			uf.Union(du, dv)
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		metrics.IncCounter("search.WCCCtx.errors", 1)
		return nil, 0, cerr
	}
	component, k = wccRelabel(uf, dense, maxID, live)
	return component, k, nil
}

// wccBuildDense compacts the live NodeID set into a dense [0, live) index
// space so the Union-Find covers only real nodes. dense[id] is the
// compact index of a live NodeID, or -1 for a ghost / isolated slot.
func wccBuildDense[W any](c *csr.CSR[W], maxID int) (dense []int, live int) {
	mask := c.LiveMask()
	dense = make([]int, maxID)
	for i := 0; i < maxID; i++ {
		if mask[i] {
			dense[i] = live
			live++
		} else {
			dense[i] = -1
		}
	}
	return dense, live
}

// wccGhostComponent returns the all-(-1) NodeID-indexed component slice
// for a graph with no incident edges anywhere.
func wccGhostComponent(maxID int) []int {
	component := make([]int, maxID)
	for i := range component {
		component[i] = -1
	}
	return component
}

// wccRelabel projects a settled union-find (over the dense [0, live)
// space) back onto the NodeID-indexed component slice, assigning
// component IDs [0, K) in ascending order of each component's minimum
// NodeID (the order of first occurrence as i sweeps upward). Ghost slots
// report -1.
//
// This pass is deterministic and a pure function of the partition: any
// union-find encoding the same equivalence classes — serial or the
// parallel merge of [WCCParallel] — yields byte-identical labels. The
// serial sweep must not be parallelised or reordered, or the
// min-first-occurrence labelling would change.
func wccRelabel(uf *ds.UnionFindSlice, dense []int, maxID, live int) (component []int, k int) {
	component = wccGhostComponent(maxID)
	// Compact root IDs into [0, K) preserving order of first occurrence.
	// rootRemap is keyed by compact root index, so it holds at most
	// `live` entries rather than O(MaxNodeID).
	rootRemap := make(map[int]int, live)
	next := 0
	for i := 0; i < maxID; i++ {
		di := dense[i]
		if di < 0 {
			continue
		}
		r := uf.Find(di)
		id, ok := rootRemap[r]
		if !ok {
			id = next
			rootRemap[r] = id
			next++
		}
		component[i] = id
	}
	return component, next
}
