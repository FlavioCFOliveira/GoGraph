package centrality

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Closeness computes closeness centrality over the immutable snapshot c,
// returning a per-NodeID slice of length c.MaxNodeID().
//
// The score uses the Wasserman-Faust normalisation, which is well-behaved on
// disconnected graphs (the classic 1/Σd form over-rewards nodes trapped in a
// small component). For a node u that reaches r other nodes at total distance
// Σd over the n nodes of the graph:
//
//	C(u) = (r / (n-1)) * (r / Σd)
//
// A node that reaches no other node (Σd == 0, including every node with no
// outgoing edges and every isolated node) scores exactly 0 — never NaN/Inf.
//
// Orientation: distances are measured along OUTGOING edges (how quickly u can
// reach the rest of the graph). For the incoming convention (how quickly u is
// reached, the NetworkX default) pass c.BuildReverse(). On an undirected
// (symmetric) snapshot the two are identical. Self-loops and parallel edges do
// not affect any shortest-path distance and are therefore ignored.
//
// Complexity is O(V*(V+E)) — one breadth-first search per source. Concurrency:
// Closeness allocates its own buffers per call and is safe to invoke from any
// number of goroutines on a snapshot CSR.
//
// Reference: Wasserman & Faust, Social Network Analysis (1994), ch. 5;
// Freeman, Social Networks 1 (1978/79) 215-239.
func Closeness[W any](c *csr.CSR[W]) []float64 {
	defer metrics.Time("search.centrality.Closeness").Stop()
	out, _ := ClosenessCtx(context.Background(), c)
	return out
}

// ClosenessCtx is the context-aware variant of [Closeness]. ctx.Err() is
// checked at every source-node boundary; on cancellation it returns
// (nil, wrapped ctx.Err()).
func ClosenessCtx[W any](ctx context.Context, c *csr.CSR[W]) ([]float64, error) {
	defer metrics.Time("search.centrality.ClosenessCtx").Stop()
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	slots := len(verts) - 1
	if slots <= 0 {
		return nil, nil
	}
	n := int(c.Order()) // true node count (excludes sharded ghost slots)
	out := make([]float64, slots)
	if n <= 1 {
		return out, nil // a lone node reaches nobody
	}

	bfs := newBFSScratch(slots)
	for src := 0; src < slots; src++ {
		if src&0x3FF == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("closeness: %w", err)
			}
		}
		// A node with no outgoing edges reaches nobody → score 0; skip BFS.
		if verts[src] == verts[src+1] {
			continue
		}
		reach, sumDist := bfs.run(verts, edges, src)
		if reach == 0 || sumDist == 0 {
			continue
		}
		r := float64(reach)
		out[src] = (r / float64(n-1)) * (r / float64(sumDist))
	}
	return out, nil
}

// bfsScratch holds reusable breadth-first-search buffers so a per-source sweep
// over the whole graph allocates once rather than per source.
type bfsScratch struct {
	dist  []int32 // hop distance from the current source; -1 = unvisited
	queue []int32 // FIFO ring buffer of NodeIDs
	stamp []uint32
	cur   uint32
}

func newBFSScratch(slots int) *bfsScratch {
	return &bfsScratch{
		dist:  make([]int32, slots),
		queue: make([]int32, 0, slots),
		stamp: make([]uint32, slots),
	}
}

// run performs a BFS from src over outgoing edges and returns the number of
// reachable other nodes and the sum of their hop distances. Buffers are reused
// across calls via a monotonically increasing visit stamp, avoiding an O(V)
// reset per source.
func (b *bfsScratch) run(verts []uint64, edges []graph.NodeID, src int) (reach int, sumDist int64) {
	b.cur++
	stamp := b.cur
	b.queue = b.queue[:0]
	b.queue = append(b.queue, int32(src))
	b.stamp[src] = stamp
	b.dist[src] = 0
	for head := 0; head < len(b.queue); head++ {
		u := b.queue[head]
		d := b.dist[u]
		for k := verts[u]; k < verts[u+1]; k++ {
			v := int32(edges[k])
			if b.stamp[v] == stamp {
				continue // already visited this sweep (handles self-loops/parallel edges)
			}
			b.stamp[v] = stamp
			b.dist[v] = d + 1
			reach++
			sumDist += int64(d + 1)
			b.queue = append(b.queue, v)
		}
	}
	return reach, sumDist
}
