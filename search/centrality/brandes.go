// Package centrality implements vertex importance metrics. v1
// carries Brandes' betweenness centrality and the PageRank family
// (T61/T62 sister tasks).
package centrality

import (
	"context"
	"runtime"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Betweenness computes the exact betweenness centrality of every
// node in c using Brandes' algorithm (2001). Returns a slice
// indexed by NodeID. Unweighted: O(V * E).
//
// The result is not normalised — callers can divide by (n-1)(n-2) for the
// classical normalised score. This same divisor applies whether c is
// directed or undirected: Brandes' source loop runs over every vertex
// regardless of orientation, so the raw output is already on an
// ordered-pair basis for both (an undirected edge is walked in both
// directions during the shortest-path search). Dividing an undirected
// result by (n-1)(n-2)/2 instead double-counts and can push the
// normalised score above 1.0 — see [WeightedBetweenness], which uses the
// same uniform 1/((n-1)(n-2)) factor.
func Betweenness[W any](c *csr.CSR[W]) []float64 {
	defer metrics.Time("search.centrality.Betweenness").Stop()
	out, _ := BetweennessCtx(context.Background(), c)
	return out
}

// BetweennessCtx is the context-aware variant of [Betweenness].
// ctx.Err() is checked once per source vertex; on cancellation
// returns (nil, wrapped ctx.Err()).
func BetweennessCtx[W any](ctx context.Context, c *csr.CSR[W]) ([]float64, error) {
	defer metrics.Time("search.centrality.BetweennessCtx").Stop()
	maxID := int(c.MaxNodeID())
	cb := make([]float64, maxID)
	if maxID == 0 {
		return cb, nil
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	sigma := make([]float64, maxID)
	dist := make([]int, maxID)
	delta := make([]float64, maxID)
	pred := newPredArena(maxID, computeInDegrees(maxID, verts, edges))
	queue := make([]int, 0, maxID)
	stack := make([]int, 0, maxID)

	for s := 0; s < maxID; s++ {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.centrality.BetweennessCtx.errors", 1)
			return nil, err
		}
		if s&0x3F == 0 {
			runtime.Gosched()
		}
		queue, stack = brandesSource(s, maxID, verts, edges, sigma, dist, delta, pred, cb, queue, stack)
	}
	return cb, nil
}

// predArena is a flat, cache-friendly predecessor store for Brandes'
// betweenness accumulation, replacing a slice-of-slices
// (`[][]int`). The classical layout allocates one growable inner
// slice per vertex, and those inner slices re-grow (re-allocate)
// across the V single-source runs whenever a later source gives a
// vertex more shortest-path predecessors than any earlier source did
// — ≈1.9k allocs/op on the guard-band graph.
//
// The arena replaces every inner slice with one contiguous backing
// array (flat) partitioned into a fixed per-vertex region. Each
// region is pre-sized to the vertex's in-degree, which is a tight
// upper bound on how many shortest-path predecessors it can ever have
// in any single-source BFS (a predecessor edge u→w only exists if the
// in-edge u→w exists). off[w] is the fixed start of w's region and
// len[w] its current fill count; the accumulation pass walks
// flat[off[w] : off[w]+len[w]] as a flat sequential scan — the same
// cache pattern as the old contiguous pred[w] slice, with zero
// pointer chasing and zero added passes.
//
// Because every region already has capacity = in-degree, an append
// never reallocates; a per-source reset only zeroes the len counters.
// The single backing array is allocated once and reused across all V
// sources, so steady-state per-source heap allocation is zero.
//
// Appends fill each region left-to-right in BFS-discovery order — the
// exact order the [][]int version produced — so the non-associative
// dependency sums are bit-identical, a hard requirement of the guard
// band. predArena is not safe for concurrent use; the parallel
// Brandes path gives each worker its own arena.
type predArena struct {
	off  []int // length n+1; off[w] is the fixed start of w's region
	pos  []int // length n; running write cursor for w, in [off[w], off[w+1]]
	flat []int // length off[n]; predecessor vertices, grouped by w
}

// newPredArena builds a predecessor arena whose per-vertex regions
// are pre-sized to each vertex's in-degree. indeg[w] is the number of
// edges pointing at w (the upper bound on w's shortest-path
// predecessors). The backing array is sized to the total edge count
// and never reallocated thereafter.
func newPredArena(n int, indeg []int) *predArena {
	off := make([]int, n+1)
	for w := 0; w < n; w++ {
		off[w+1] = off[w] + indeg[w]
	}
	return &predArena{
		off:  off,
		pos:  make([]int, n),
		flat: make([]int, off[n]),
	}
}

// add appends predecessor v to w's region, preserving insertion
// order. pos[w] is the running write cursor (initialised to off[w] on
// the per-source reset); the region capacity equals w's in-degree, so
// this never overflows and never reallocates. A single cursor array is
// read instead of off+cnt, keeping the hottest loop's index work to one
// slice access plus the flat store. O(1).
func (a *predArena) add(w, v int) {
	p := a.pos[w]
	a.flat[p] = v
	a.pos[w] = p + 1
}

// reset clears w's predecessor region by returning its write cursor to the
// region base. The weighted (Dijkstra) Brandes uses this when a strictly
// shorter path to w is found, discarding the predecessors recorded at the
// previous, longer distance; the unweighted BFS never needs it (BFS distances
// are monotonic so a vertex's predecessors are never invalidated). Because each
// in-neighbour relaxes w at most once per source, the total adds to w between
// resets never exceed w's in-degree, so the region never overflows. O(1).
func (a *predArena) reset(w int) { a.pos[w] = a.off[w] }

// computeInDegrees returns, for every vertex id in [0,n), the number
// of edges that point at it. For an undirected CSR this equals the
// out-degree; computing it directly from the edge targets is correct
// for both directed and undirected graphs. O(E).
func computeInDegrees(n int, verts []uint64, edges []graph.NodeID) []int {
	indeg := make([]int, n)
	end := verts[n]
	for k := uint64(0); k < end; k++ {
		indeg[int(edges[k])]++
	}
	return indeg
}

// brandesSource runs one single-source BFS-based betweenness
// accumulation. queue and stack are caller-owned scratch slices —
// the function truncates them to zero length before use and returns
// the (possibly grown) headers so the caller keeps the larger
// capacity across iterations.
func brandesSource(s, maxID int, verts []uint64, edges []graph.NodeID, sigma []float64, dist []int, delta []float64, pred *predArena, cb []float64, queue, stack []int) (queueOut, stackOut []int) {
	// Fold the per-vertex predecessor-cursor reset into the same O(n)
	// sweep that re-initialises sigma/dist/delta, so the arena costs no
	// extra pass per source. Each region's write cursor returns to its
	// fixed base off[i].
	off := pred.off
	pos := pred.pos
	for i := 0; i < maxID; i++ {
		sigma[i] = 0
		dist[i] = -1
		delta[i] = 0
		pos[i] = off[i]
	}
	sigma[s] = 1
	dist[s] = 0
	queue = append(queue[:0], s)
	stack = stack[:0]
	for qh := 0; qh < len(queue); qh++ {
		v := queue[qh]
		stack = append(stack, v)
		// dist[v]+1 is invariant across the inner loop and was recomputed
		// per edge (rmp #2868). v is dequeued, so dist[v] is already >= 0
		// and the dist[w] < 0 arm can never write dist[v] — not even
		// through a self-loop — so hoisting the sum out of the loop is
		// exact. It is an integer operation, so sigma and the predecessor
		// insertion order, and therefore the dependency sums, stay
		// bit-identical; TestBrandesDistanceHoist_BitIdentical asserts that
		// against the unchanged legacy reference.
		//
		// Measured, never assumed — interleaved A/B/A2 against 8affe124's
		// brandes.go in the otherwise-current tree, 7 samples, go1.27.1
		// darwin/arm64, noise floor (A against the byte-identical A2)
		// geomean -0.02% with every row ~: -3.52% on Brandes_Scale/5k_deg16,
		// -2.01% on 2k_deg16, ~ on every other row of the ladder including
		// the two small sparse rows (Betweenness_Serial,
		// Brandes_RandomGraph), 8-row geomean -0.70%, allocations
		// unchanged. The gain is degree-dependent: below roughly six
		// neighbours per vertex the per-vertex setup is not repaid per edge.
		//
		// Caching the neighbour's distance in a second local alongside the
		// hoist — read dist[w] once, refresh that local in the discovered
		// arm, and drop the reload in the equal-distance test — was tried,
		// measured in a 7-sample single-variable split, and DROPPED. It is
		// equally bit-identical, but in that split it bought only
		// -2.73%/-0.63% on the two deg16 rows against the hoist's
		// -4.41%/-2.08%, and it carried essentially the whole sparse-row
		// cost: the two together measured +1.52%/+1.35% on
		// Betweenness_Serial and Brandes_RandomGraph for a 4-row geomean of
		// -1.54%, the hoist alone +0.20%/+0.38% for -1.50%. Same win, and
		// the shipped arm's own re-run leaves both sparse rows at ~, so the
		// second dist[w] load stays.
		dv1 := dist[v] + 1
		for k := verts[v]; k < verts[v+1]; k++ {
			w := int(edges[k])
			if dist[w] < 0 {
				dist[w] = dv1
				queue = append(queue, w)
			}
			if dist[w] == dv1 {
				sigma[w] += sigma[v]
				pred.add(w, v)
			}
		}
	}
	flat := pred.flat
	for i := len(stack) - 1; i >= 0; i-- {
		w := stack[i]
		// Walk w's predecessors as a contiguous flat scan in their
		// original insertion order, matching the former `range pred[w]`
		// so the dependency sum is accumulated in an identical order and
		// stays bit-identical. The region spans [off[w], pos[w]).
		coef := 1 + delta[w]
		sw := sigma[w]
		region := flat[off[w]:pos[w]:pos[w]]
		for _, v := range region {
			delta[v] += (sigma[v] / sw) * coef
		}
		if w != s {
			cb[w] += delta[w]
		}
	}
	return queue, stack
}
