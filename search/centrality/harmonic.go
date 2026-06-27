package centrality

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Harmonic computes harmonic centrality over the immutable snapshot c,
// returning a per-NodeID slice of length c.MaxNodeID().
//
// Harmonic centrality sums the reciprocal distance to every other node, with
// unreachable nodes contributing 0 (1/∞):
//
//	H(u) = ( Σ_{v ≠ u} 1/d(u,v) ) / (n-1)
//
// Unlike closeness it is well-defined on every graph — disconnected, with
// isolated nodes, or directed — because an unreachable pair contributes a
// finite 0 rather than forcing a division by an infinite total. The result is
// normalised by n-1 so it lies in [0,1].
//
// Orientation: distances are measured along OUTGOING edges (how quickly u can
// reach others). For the incoming convention pass c.BuildReverse(); on an
// undirected snapshot the two coincide. Self-loops and parallel edges do not
// affect shortest-path distances and are ignored.
//
// Complexity is O(V*(V+E)) — one breadth-first search per source. Concurrency:
// Harmonic allocates its own buffers per call and is safe for concurrent use on
// a snapshot CSR.
//
// Reference: Boldi & Vigna, "Axioms for Centrality", Internet Mathematics 10
// (2014) 222-262; Rochat (2009); Marchiori & Latora (2000).
func Harmonic[W any](c *csr.CSR[W]) []float64 {
	defer metrics.Time("search.centrality.Harmonic").Stop()
	out, _ := HarmonicCtx(context.Background(), c)
	return out
}

// HarmonicCtx is the context-aware variant of [Harmonic]. ctx.Err() is checked
// at every source-node boundary; on cancellation it returns (nil, wrapped err).
func HarmonicCtx[W any](ctx context.Context, c *csr.CSR[W]) ([]float64, error) {
	defer metrics.Time("search.centrality.HarmonicCtx").Stop()
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	slots := len(verts) - 1
	if slots <= 0 {
		return nil, nil
	}
	n := int(c.Order())
	out := make([]float64, slots)
	if n <= 1 {
		return out, nil
	}

	bfs := newBFSScratch(slots)
	norm := 1.0 / float64(n-1)
	for src := 0; src < slots; src++ {
		if src&0x3FF == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("harmonic: %w", err)
			}
		}
		if verts[src] == verts[src+1] {
			continue // no outgoing edges → reaches nobody → 0
		}
		out[src] = bfs.runHarmonic(verts, edges, src) * norm
	}
	return out, nil
}

// runHarmonic performs a BFS from src over outgoing edges and returns the sum
// of reciprocal hop distances to every reachable other node. It reuses the
// scratch buffers via the visit stamp, mirroring [bfsScratch.run].
func (b *bfsScratch) runHarmonic(verts []uint64, edges []graph.NodeID, src int) float64 {
	b.cur++
	stamp := b.cur
	b.queue = b.queue[:0]
	b.queue = append(b.queue, int32(src))
	b.stamp[src] = stamp
	b.dist[src] = 0
	var sum float64
	for head := 0; head < len(b.queue); head++ {
		u := b.queue[head]
		d := b.dist[u]
		for k := verts[u]; k < verts[u+1]; k++ {
			v := int32(edges[k])
			if b.stamp[v] == stamp {
				continue
			}
			b.stamp[v] = stamp
			b.dist[v] = d + 1
			sum += 1.0 / float64(d+1)
			b.queue = append(b.queue, v)
		}
	}
	return sum
}
