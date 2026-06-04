package search

import (
	"context"
	"errors"
	"runtime"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrNegativeEdgeAPSP is returned by [DijkstraAPSP] when the input
// CSR contains a strictly-negative edge weight. DijkstraAPSP does not
// reweight negative edges; callers with mixed-sign weights and no
// negative cycles should use [JohnsonAPSP] (which reweights via
// Bellman-Ford) or [FloydWarshall].
var ErrNegativeEdgeAPSP = errors.New("search: DijkstraAPSP requires non-negative edge weights")

// DijkstraAPSP computes APSP on c by running [Dijkstra] from every
// live vertex. It accepts only non-negative edge weights.
//
// For graphs with negative edges (but no negative cycle), use
// [JohnsonAPSP] which prefixes a Bellman-Ford reweighting pass and
// runs in O(V * (V + E) * log V), or [FloydWarshall] which tolerates
// them at the cost of O(V^3) work.
//
// For floating-point Weight types it validates that no edge weight
// is NaN or +/-Inf and returns [ErrInvalidInput] otherwise; integer
// Weight types skip that pass.
//
// Complexity: O(V * (V + E) * log V).
func DijkstraAPSP[W Weight](c *csr.CSR[W]) (*APSP[W], error) {
	defer metrics.Time("search.DijkstraAPSP")()
	res, err := DijkstraAPSPCtx(context.Background(), c)
	if err != nil {
		metrics.IncCounter("search.DijkstraAPSP.errors", 1)
	}
	return res, err
}

// DijkstraAPSPCtx is the context-aware variant of [DijkstraAPSP].
// ctx.Err() is checked once per source vertex; on cancellation
// returns (nil, wrapped ctx.Err()).
func DijkstraAPSPCtx[W Weight](ctx context.Context, c *csr.CSR[W]) (*APSP[W], error) {
	defer metrics.Time("search.DijkstraAPSPCtx")()
	// Float Weight types: NaN / +/-Inf in any edge silently breaks
	// every per-source Dijkstra. Fail fast at the public boundary
	// (each inner Dijkstra would re-detect it but at higher cost);
	// integer W short-circuits in O(1).
	if anyFloatInvalid(c.WeightsSlice()) {
		metrics.IncCounter("search.DijkstraAPSPCtx.errors", 1)
		return nil, ErrInvalidInput
	}
	maxID := int(c.MaxNodeID())
	mask := c.LiveMask()
	compact := make([]int, maxID)
	live := 0
	for i := 0; i < maxID; i++ {
		if mask[i] {
			compact[i] = live
			live++
		} else {
			compact[i] = -1
		}
	}
	out := &APSP[W]{
		live:    live,
		maxID:   maxID,
		compact: compact,
		dist:    make([]W, live*live),
		found:   make([]bool, live*live),
	}
	if live == 0 {
		return out, nil
	}
	for i := 0; i < live; i++ {
		idx := i*live + i
		out.found[idx] = true
	}
	for src := 0; src < maxID; src++ {
		si := compact[src]
		if si < 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.DijkstraAPSPCtx.errors", 1)
			return nil, err
		}
		if src&0x3F == 0 {
			runtime.Gosched()
		}
		d, err := DijkstraCtx(ctx, c, graph.NodeID(src))
		if err != nil {
			metrics.IncCounter("search.DijkstraAPSPCtx.errors", 1)
			if errors.Is(err, ErrNegativeWeight) {
				return nil, ErrNegativeEdgeAPSP
			}
			return nil, err
		}
		for dst := 0; dst < maxID; dst++ {
			di := compact[dst]
			if di < 0 {
				continue
			}
			if v, ok := d.Distance(graph.NodeID(dst)); ok {
				idx := si*live + di
				out.dist[idx] = v
				out.found[idx] = true
			}
		}
	}
	return out, nil
}

// JohnsonAPSP computes APSP on c using Johnson's algorithm: a
// Bellman-Ford reweighting pass from a virtual zero-weight source
// computes a potential h(v); the original edge weights are reweighted
// to w'(u,v) = w(u,v) + h(u) - h(v); a Dijkstra from every live
// vertex on the reweighted (non-negative) graph yields d'(u,v); the
// original distances are recovered via d(u,v) = d'(u,v) - h[u] + h[v].
//
// Compared to [DijkstraAPSP] which rejects negative edges, JohnsonAPSP
// accepts arbitrary signed edge weights and reports a negative cycle
// reachable from any source via [ErrNegativeCycle]. Compared to
// [FloydWarshall] which is O(V^3), Johnson is O(V * (V + E) * log V)
// — strictly better on sparse graphs (E = O(V)).
//
// For floating-point Weight types it validates that no edge weight
// is NaN or +/-Inf and returns [ErrInvalidInput] otherwise; integer
// Weight types skip that pass. The gate sits at Johnson's public
// boundary so callers get a single point of failure (the inner
// Bellman-Ford reweighting would otherwise re-detect it at higher
// cost).
//
// Floating-point caveat: when W is a floating-point type, the
// reweight/recover arithmetic w(u,v) + h(u) - h(v) followed by
// d'(u,v) - h[u] + h[v] can accumulate ULP-level rounding error,
// so the recovered d(u,v) may differ from [FloydWarshall]'s output
// by a small tolerance. Integer Weight types reproduce
// [FloydWarshall] exactly.
//
// Integer-Weight overflow precondition. Johnson is the most
// overflow-exposed shortest-path routine here: the potential h(v) is a
// cumulative shortest-path distance, the reweight w(u,v) + h(u) - h(v)
// and the recover d'(u,v) - h[u] + h[v] each combine three such terms,
// and the inner Dijkstra accumulates over the reweighted graph. For an
// integer Weight type the caller must ensure that every intermediate
// (the deepest potential, the reweighted-path distance, and the
// recovered distance) fits W; otherwise the arithmetic wraps and the
// reported distances are silently wrong. The NaN/+-Inf gate covers only
// floating-point W. A development build with -tags gograph_debug adds an
// assertion to both the Bellman-Ford reweighting pass and the reweighted
// per-source Dijkstra that panics on a cumulative-distance wraparound;
// the production hot path carries no such check.
//
// Concurrency: JohnsonAPSP is safe for any number of concurrent
// invocations on a shared, immutable CSR.
//
// Complexity: O(V * (V + E) * log V) for the Dijkstra pass plus
// O(V * E) for the Bellman-Ford pass (SPFA worst-case bound).
func JohnsonAPSP[W Weight](c *csr.CSR[W]) (*APSP[W], error) {
	defer metrics.Time("search.JohnsonAPSP")()
	res, err := JohnsonAPSPCtx(context.Background(), c)
	if err != nil {
		metrics.IncCounter("search.JohnsonAPSP.errors", 1)
	}
	return res, err
}

// JohnsonAPSPCtx is the context-aware variant of [JohnsonAPSP].
// ctx.Err() is checked once per source vertex during the Dijkstra
// pass and at every relaxation-round boundary during the
// Bellman-Ford pass; on cancellation returns (nil, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical Johnson: NaN/Inf gate + live-mask compaction + virtual-source BF + reweight + per-source Dijkstra + recover
func JohnsonAPSPCtx[W Weight](ctx context.Context, c *csr.CSR[W]) (*APSP[W], error) {
	defer metrics.Time("search.JohnsonAPSPCtx")()
	// Float Weight types: NaN / +/-Inf in any edge silently corrupts
	// both the BF reweighting potential h and every per-source
	// Dijkstra. Fail fast at the public boundary so callers see a
	// single point of failure; integer W short-circuits in O(1).
	if anyFloatInvalid(c.WeightsSlice()) {
		metrics.IncCounter("search.JohnsonAPSPCtx.errors", 1)
		return nil, ErrInvalidInput
	}
	maxID := int(c.MaxNodeID())
	mask := c.LiveMask()
	compact := make([]int, maxID)
	live := 0
	for i := 0; i < maxID; i++ {
		if mask[i] {
			compact[i] = live
			live++
		} else {
			compact[i] = -1
		}
	}
	out := &APSP[W]{
		live:    live,
		maxID:   maxID,
		compact: compact,
		dist:    make([]W, live*live),
		found:   make([]bool, live*live),
	}
	if live == 0 {
		return out, nil
	}
	for i := 0; i < live; i++ {
		idx := i*live + i
		out.found[idx] = true
	}

	// Bellman-Ford reweighting pass. Conceptually we add a synthetic
	// source s with zero-weight edges to every vertex and compute the
	// shortest-path potential h(v) = delta(s, v). Equivalently, we
	// seed every vertex with dist=0 and run SPFA: any vertex that
	// can be relaxed to a strictly negative value drags its
	// reachable component down; a negative cycle is detected when
	// any vertex enters the worklist more than V times.
	h := make([]W, maxID)
	if err := bellmanFordVirtualSource[W](ctx, c, h); err != nil {
		metrics.IncCounter("search.JohnsonAPSPCtx.errors", 1)
		return nil, err
	}

	// Build the reweighted edge-weights view. By Johnson's lemma all
	// w'(u,v) = w(u,v) + h(u) - h(v) are non-negative when there is
	// no negative cycle reachable from the virtual source.
	weights := c.WeightsSlice()
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	reweighted := make([]W, len(weights))
	for u := 0; u < maxID; u++ {
		start := verts[u]
		end := verts[u+1]
		for k := start; k < end; k++ {
			v := uint64(edges[k])
			reweighted[k] = weights[k] + h[u] - h[v]
		}
	}

	// Reweighted Dijkstra from every live vertex. We use the same
	// pooled state machinery as Dijkstra to keep per-source allocation
	// down to the heap items themselves (which the per-W sync.Pool
	// amortises). dijkstraCoreWithWeights reads the reweighted slice
	// in place of c.WeightsSlice().
	st := acquireDijkstra[W](uint64(maxID))
	defer releaseDijkstra(st)
	for src := 0; src < maxID; src++ {
		si := compact[src]
		if si < 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.JohnsonAPSPCtx.errors", 1)
			return nil, err
		}
		if src&0x3F == 0 {
			runtime.Gosched()
		}
		if err := dijkstraCoreWithWeights[W](
			ctx, c, reweighted, graph.NodeID(src),
			st.dist[:maxID], st.parent[:maxID], st.found[:maxID], &st.heap,
		); err != nil {
			metrics.IncCounter("search.JohnsonAPSPCtx.errors", 1)
			return nil, err
		}
		// Recover original distances: d(src, dst) = d'(src, dst) - h[src] + h[dst].
		hsrc := h[src]
		for dst := 0; dst < maxID; dst++ {
			di := compact[dst]
			if di < 0 {
				continue
			}
			if !st.found[dst] {
				continue
			}
			idx := si*live + di
			out.dist[idx] = st.dist[dst] - hsrc + h[dst]
			out.found[idx] = true
		}
	}
	return out, nil
}

// bellmanFordVirtualSource computes the Johnson potential h(v) for
// every NodeID v in c. Conceptually we add a synthetic source s
// connected to every vertex with a zero-weight edge and run Bellman-
// Ford from s; the resulting h satisfies h(v) <= h(u) + w(u, v) for
// every edge (u, v), which is exactly the inequality Johnson's lemma
// needs.
//
// Implementation: SPFA seeded with every vertex in the worklist and
// dist[*] = 0. A negative cycle reachable from any vertex (and
// therefore from the virtual source) is detected by the
// relaxes[v] > maxID guard inherited from [bellmanFordCore].
//
// Pre-condition: len(h) == int(c.MaxNodeID()).
//
//nolint:gocyclo // virtual-source SPFA with SLF + negative-cycle counter and ctx-yield path
func bellmanFordVirtualSource[W Weight](ctx context.Context, c *csr.CSR[W], h []W) error {
	maxID := uint64(c.MaxNodeID())
	if maxID == 0 {
		return nil
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()

	var zero W
	for i := range h {
		h[i] = zero
	}

	// SPFA with SLF on a circular deque, primed with every vertex.
	bufSize := 1
	for bufSize < int(maxID)+1 {
		bufSize <<= 1
	}
	if bufSize < 8 {
		bufSize = 8
	}
	dq := make([]graph.NodeID, bufSize)
	mask := bufSize - 1
	head := 0
	tail := 0
	inQueue := make([]bool, maxID)
	relaxes := make([]uint32, maxID)
	for v := uint64(0); v < maxID; v++ {
		dq[tail] = graph.NodeID(v)
		tail = (tail + 1) & mask
		inQueue[v] = true
		relaxes[v] = 1
	}

	yieldCtr := 0
	for head != tail {
		yieldCtr++
		if yieldCtr&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		v := dq[head]
		head = (head + 1) & mask
		inQueue[uint64(v)] = false
		dv := h[uint64(v)]
		start := verts[uint64(v)]
		end := verts[uint64(v)+1]
		for k := start; k < end; k++ {
			nb := uint64(edges[k])
			cand := dv + weights[k]
			// Debug builds (-tags gograph_debug) trap an integer
			// cumulative-distance overflow in Johnson's reweighting
			// pass here; a no-op otherwise.
			assertNoRelaxOverflow(dv, weights[k], cand)
			if cand < h[nb] {
				h[nb] = cand
				if !inQueue[nb] {
					relaxes[nb]++
					if uint64(relaxes[nb]) > maxID {
						return ErrNegativeCycle
					}
					// SLF: cheaper tentative goes to the front so we
					// pop it sooner and minimise re-relaxation downstream.
					if head != tail && cand < h[uint64(dq[head])] {
						head = (head - 1) & mask
						dq[head] = graph.NodeID(nb)
					} else {
						dq[tail] = graph.NodeID(nb)
						tail = (tail + 1) & mask
					}
					inQueue[nb] = true
				}
			}
		}
	}
	return nil
}
