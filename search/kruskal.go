package search

import (
	"context"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/ds"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// MSTEdge identifies one edge in a minimum-spanning-tree result.
// From and To are the endpoints in CSR NodeID space; Weight is the
// edge's weight.
type MSTEdge[W Weight] struct {
	From   graph.NodeID
	To     graph.NodeID
	Weight W
}

// KruskalMST computes a minimum spanning forest of c using Kruskal's
// algorithm (1956): sort all edges by weight ascending, then iterate
// adding each edge that connects two distinct components (tracked
// via a slice-backed Union-Find).
//
// For a connected graph the result has exactly live-1 edges, where
// live is the number of NodeIDs with at least one incident edge; for a
// disconnected graph it has live - numComponents edges. (The count is
// expressed over the live node set rather than the sparse MaxNodeID()
// because ghost padding slots in a Mapper-sharded NodeID space carry no
// edges and cannot join the spanning forest.)
//
// c is expected to be an undirected graph encoded as a symmetric
// directed CSR (every {u, v} edge appears as both (u, v) and (v, u));
// only one direction of each pair is added to the result, chosen
// canonically as u <= v.
//
// For floating-point Weight types it validates that no edge weight
// is NaN or +/-Inf and returns [ErrInvalidInput] otherwise; integer
// Weight types skip that pass.
//
// Concurrency: KruskalMST is safe to invoke from any number of
// goroutines on a shared CSR — it allocates its own working storage.
func KruskalMST[W Weight](c *csr.CSR[W]) (edges []MSTEdge[W], totalWeight W, err error) {
	defer metrics.Time("search.KruskalMST")()
	edges, totalWeight, err = KruskalMSTCtx(context.Background(), c)
	if err != nil {
		metrics.IncCounter("search.KruskalMST.errors", 1)
	}
	return edges, totalWeight, err
}

// KruskalMSTCtx is the context-aware variant of [KruskalMST]. ctx.Err()
// is checked once before the sort and every 4096 edges during the
// scan; on cancellation returns (nil, zero, wrapped ctx.Err()).
//
// The Union-Find universe is sized on the live NodeID count, not on
// MaxNodeID(), so a sparse (shard-amplified) NodeID space cannot inflate
// the disjoint-set storage beyond O(live) (rmp #1474). Result edges are
// reported in the original NodeID space; only the internal union
// arithmetic runs in the compact [0, live) index space.
func KruskalMSTCtx[W Weight](ctx context.Context, c *csr.CSR[W]) (edges []MSTEdge[W], totalWeight W, err error) {
	defer metrics.Time("search.KruskalMSTCtx")()
	if cerr := ctx.Err(); cerr != nil {
		metrics.IncCounter("search.KruskalMSTCtx.errors", 1)
		return nil, totalWeight, cerr
	}
	maxID := int(c.MaxNodeID())
	if maxID <= 1 {
		return nil, totalWeight, nil
	}
	// Float Weight types: NaN / +/-Inf in an edge weight silently
	// breaks the sort comparator (every NaN comparison is false).
	// Fail fast at the public boundary; integer W short-circuits.
	if anyFloatInvalid(c.WeightsSlice()) {
		metrics.IncCounter("search.KruskalMSTCtx.errors", 1)
		return nil, totalWeight, ErrInvalidInput
	}
	// Compact the live NodeID set into a dense [0, live) index space so
	// the Union-Find and the spanning-forest edge budget track the real
	// node count rather than the shard-amplified MaxNodeID().
	mask := c.LiveMask()
	dense := make([]int, maxID)
	live := 0
	for i := 0; i < maxID; i++ {
		if mask[i] {
			dense[i] = live
			live++
		} else {
			dense[i] = -1
		}
	}
	if live <= 1 {
		return nil, totalWeight, nil
	}
	verts := c.VerticesSlice()
	edgeDst := c.EdgesSlice()
	weights := c.WeightsSlice()
	// Collect the upper-triangle of every (u, v) pair.
	cand := make([]MSTEdge[W], 0, len(edgeDst)/2)
	for u := 0; u < maxID; u++ {
		if dense[u] < 0 {
			continue
		}
		for k := verts[u]; k < verts[u+1]; k++ {
			v := int(edgeDst[k])
			if uint64(v) < uint64(u) {
				continue // mirror; emitted when we visited v
			}
			var w W
			if weights != nil {
				w = weights[k]
			}
			cand = append(cand, MSTEdge[W]{From: graph.NodeID(u), To: graph.NodeID(v), Weight: w})
		}
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].Weight < cand[j].Weight })
	uf := ds.NewSlice(live)
	// A spanning forest over `live` nodes has at most live-1 edges.
	out := make([]MSTEdge[W], 0, live-1)
	for i, e := range cand {
		if i&0xFFF == 0 {
			if cerr := ctx.Err(); cerr != nil {
				metrics.IncCounter("search.KruskalMSTCtx.errors", 1)
				return nil, totalWeight, cerr
			}
		}
		// e.From / e.To are live (only live sources contribute candidates,
		// and an undirected edge's other endpoint is live too), so dense[]
		// yields a valid compact index for both.
		if uf.Union(dense[int(e.From)], dense[int(e.To)]) {
			out = append(out, e)
			totalWeight += e.Weight
			if len(out) == live-1 {
				break
			}
		}
	}
	return out, totalWeight, nil
}
