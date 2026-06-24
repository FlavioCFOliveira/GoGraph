package search

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// PrimMST computes a minimum spanning tree of c rooted at src using
// Prim's algorithm with a binary-heap priority queue. The returned
// parent slice maps each NodeID to its predecessor in the tree
// (parent[src] == src; nodes unreachable from src have parent[i] == 0
// but found[i] == false — distinguishable by a parallel found bitmap,
// returned as the third value).
//
// For a connected undirected graph PrimMST and [KruskalMST] return
// trees of identical total weight (the MST weight is unique for
// distinct-weighted inputs, and tied inputs may yield different but
// equally-weighted trees).
//
// For floating-point Weight types it validates that no edge weight
// is NaN or +/-Inf and returns [ErrInvalidInput] otherwise; integer
// Weight types skip that pass.
//
// Concurrency: PrimMST is safe to invoke concurrently on a shared
// CSR — it allocates its own working storage.
func PrimMST[W Weight](c *csr.CSR[W], src graph.NodeID) (parent []graph.NodeID, found []bool, totalWeight W, err error) {
	defer metrics.Time("search.PrimMST").Stop()
	parent, found, totalWeight, err = PrimMSTCtx(context.Background(), c, src)
	if err != nil {
		metrics.IncCounter("search.PrimMST.errors", 1)
	}
	return parent, found, totalWeight, err
}

// PrimMSTCtx is the context-aware variant of [PrimMST]. ctx.Err() is
// checked every 4096 heap pops; on cancellation returns
// (nil, nil, zero, wrapped ctx.Err()).
func PrimMSTCtx[W Weight](ctx context.Context, c *csr.CSR[W], src graph.NodeID) (parent []graph.NodeID, found []bool, totalWeight W, err error) {
	defer metrics.Time("search.PrimMSTCtx").Stop()
	verts := c.VerticesSlice()
	if uint64(src)+1 >= uint64(len(verts)) {
		return nil, nil, totalWeight, nil
	}
	// Float Weight types: NaN / +/-Inf in an edge weight silently
	// breaks the minEdge comparator. Fail fast at the public
	// boundary; integer W short-circuits in O(1).
	if anyFloatInvalid(c.WeightsSlice()) {
		metrics.IncCounter("search.PrimMSTCtx.errors", 1)
		return nil, nil, totalWeight, ErrInvalidInput
	}
	maxID := uint64(c.MaxNodeID())
	parent = make([]graph.NodeID, maxID)
	found = make([]bool, maxID)
	inTree := make([]bool, maxID)
	// minEdge[v] is the weight of the cheapest known edge connecting
	// v to a tree node; meaningful only when found[v] is true.
	minEdge := make([]W, maxID)
	h := acquireDijkHeap[W]()
	defer releaseDijkHeap(h)
	var zero W
	parent[uint64(src)] = src
	found[uint64(src)] = true
	h.push(zero, src)
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	popCount := 0
	for h.len() > 0 {
		if popCount&0xFFF == 0 {
			if cerr := ctx.Err(); cerr != nil {
				metrics.IncCounter("search.PrimMSTCtx.errors", 1)
				return nil, nil, totalWeight, cerr
			}
		}
		popCount++
		top := h.pop()
		if inTree[uint64(top.node)] {
			continue
		}
		inTree[uint64(top.node)] = true
		totalWeight += top.dist
		start := verts[uint64(top.node)]
		end := verts[uint64(top.node)+1]
		for k := start; k < end; k++ {
			nb := edges[k]
			if inTree[uint64(nb)] {
				continue
			}
			var w W
			if weights != nil {
				w = weights[k]
			}
			if !found[uint64(nb)] || w < minEdge[uint64(nb)] {
				minEdge[uint64(nb)] = w
				parent[uint64(nb)] = top.node
				found[uint64(nb)] = true
				h.push(w, nb)
			}
		}
	}
	return parent, found, totalWeight, nil
}
