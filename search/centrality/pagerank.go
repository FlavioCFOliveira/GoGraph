package centrality

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"runtime/pprof"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// pageRankParallelThreshold is the live-node count below which the
// sequential push-based SpMV is used and the reverse-CSR transpose is
// skipped entirely. Below this size the one-time O(V+E) transpose build
// plus the goroutine fan-out/join overhead dominates the per-iteration
// SpMV, so the serial path is strictly faster. The threshold was chosen
// empirically (see BenchmarkPageRank_* in pagerank_parallel_test.go):
// the parallel pull path overtakes the serial push path on graphs of a
// few thousand live nodes and upward.
const pageRankParallelThreshold = 2048

// ErrInvalidInput is returned by centrality algorithms when their
// float options contain an invalid value: NaN, +/-Inf, or an
// out-of-range parameter (e.g. Damping outside (0,1), negative
// Tolerance/Epsilon). Non-finite or out-of-range inputs propagate
// through the power-iteration and push loops and silently corrupt
// the rank vector; validating once at entry is mandatory.
var ErrInvalidInput = errors.New("centrality: input option is invalid (NaN, Inf, or out of range)")

// ErrMaxStepsExceeded is returned by [PersonalisedPushPageRank] and
// [PersonalisedPushPageRankCtx] when the MaxSteps budget is reached
// before the residue converges to Epsilon. The returned rank vector
// is the partial result accumulated so far and does NOT satisfy the
// ε-approximation guarantee.
var ErrMaxStepsExceeded = errors.New("centrality: MaxSteps budget exhausted before convergence")

// ErrNonPositiveWeight is returned by [WeightedBetweenness] and
// [WeightedBetweennessCtx] when any edge weight is zero or negative.
// Brandes' weighted variant builds a predecessor DAG ordered by
// shortest-path distance using Dijkstra; zero-weight edges connect
// two nodes at equal distance, which can cause a node to be settled
// (and its σ consumed downstream) before a later-settled equal-distance
// predecessor has accumulated its contribution — making σ inconsistent
// and silently corrupting the centrality values. Strictly positive
// weights are therefore required.
var ErrNonPositiveWeight = errors.New("centrality: edge weight must be strictly positive")

// hasInvalidFloat returns true when any of values is NaN or +/-Inf.
func hasInvalidFloat(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return true
		}
	}
	return false
}

// PageRankOptions configures [PageRank].
type PageRankOptions struct {
	Damping       float64
	MaxIterations int
	Tolerance     float64
}

// DefaultPageRankOptions returns the classic Brin-Page parameters
// (damping 0.85, max 100 iterations, tolerance 1e-6).
func DefaultPageRankOptions() PageRankOptions {
	return PageRankOptions{Damping: 0.85, MaxIterations: 100, Tolerance: 1e-6}
}

// PageRank runs the in-memory power-iteration form of PageRank
// over c and returns the per-NodeID rank slice plus the iteration
// count to convergence (capped at MaxIterations).
//
// The returned slice has length c.MaxNodeID(); only NodeIDs that
// participate in at least one edge (live nodes) carry non-zero rank.
// The sum over the slice equals 1.0 within numerical tolerance.
//
// Concurrency: PageRank is safe to invoke from any number of
// goroutines on a snapshot CSR; the function allocates its working
// buffers per call and does not share state.
//
// Algorithm. The implementation is the textbook power-iteration form
// with proper handling of dangling nodes (nodes with out-degree 0):
// at each iteration the mass currently held by dangling nodes is
// redistributed uniformly across all live nodes, modelling them as
// teleporting their entire share back into the system. This ensures
// total mass is conserved and the result is a true stationary
// distribution.
//
// Parallelism. On graphs with at least pageRankParallelThreshold live
// nodes and when GOMAXPROCS > 1, the per-iteration sparse mat-vec runs
// the pull formulation (next[v] = baseShare + d·Σ_{u∈in(v)} cur[u]/
// outdeg[u]) over a reverse-CSR, partitioned across a persistent worker
// pool by approximately equal in-edge count. Each next[v] is computed
// independently with no write contention, and every vertex sums its
// in-edges in the fixed reverse-CSR order, so the result is bit-for-bit
// identical to the serial path regardless of GOMAXPROCS or worker
// scheduling. Smaller graphs use the serial push form unchanged and pay
// neither the reverse-CSR transpose nor any goroutine overhead.
func PageRank[W any](c *csr.CSR[W], opts PageRankOptions) (ranks []float64, iterations int, err error) {
	defer metrics.Time("search.centrality.PageRank")()
	ranks, iterations, err = PageRankCtx(context.Background(), c, opts)
	if err != nil {
		metrics.IncCounter("search.centrality.PageRank.errors", 1)
	}
	return ranks, iterations, err
}

// PageRankCtx is the context-aware variant of [PageRank]. ctx.Err()
// is checked at every iteration boundary; on cancellation returns
// (nil, 0, wrapped ctx.Err()).
//
// This is the one-shot hot path: its computation is kept as a single
// monolithic function (rather than routed through the [PageRanker] state
// machinery) so the compiler keeps every working buffer in one frame —
// extracting it behind a method boundary measurably regressed the
// parallel SpMV by ~3%. [PageRanker.Run] carries the reusable variant for
// repeated queries.
//
//nolint:gocyclo // canonical power-iteration: defaults + live detection + iteration loop
func PageRankCtx[W any](ctx context.Context, c *csr.CSR[W], opts PageRankOptions) (ranks []float64, iterations int, err error) {
	defer metrics.Time("search.centrality.PageRankCtx")()
	opts, err = validatePageRankOptions(opts)
	if err != nil {
		metrics.IncCounter("search.centrality.PageRankCtx.errors", 1)
		return nil, 0, err
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	n := len(verts) - 1
	if n <= 0 {
		return nil, 0, nil
	}

	// A node is "live" if it has at least one incident edge (in or out).
	// Dangling sinks (out-degree 0, in-degree > 0) count as live;
	// totally isolated ghost slots from sharded NodeID packing do not.
	isLive := make([]bool, n)
	// outdeg is logically an unsigned integer count; storing it as
	// uint32 halves its memory footprint (n nodes saved 4 bytes each
	// vs the v1.0 float64) and doubles its L1 lane density. The
	// per-source float64 conversion at use site is a single FCVT
	// instruction with no measurable cost on M4-class cores.
	outdeg := make([]uint32, n)
	for i := 0; i < n; i++ {
		deg := verts[i+1] - verts[i]
		if deg > 0 {
			outdeg[i] = uint32(deg)
			isLive[i] = true
			for k := verts[i]; k < verts[i+1]; k++ {
				isLive[int(edges[k])] = true
			}
		}
	}
	live := 0
	for i := 0; i < n; i++ {
		if isLive[i] {
			live++
		}
	}
	if live == 0 {
		return make([]float64, n), 0, nil
	}

	cur := make([]float64, n)
	next := make([]float64, n)
	initRank := 1.0 / float64(live)
	for i := 0; i < n; i++ {
		if isLive[i] {
			cur[i] = initRank
		}
	}
	teleport := (1 - opts.Damping) / float64(live)

	// Decide once whether to run the parallel pull-formulation SpMV. The
	// pull path requires the reverse-CSR (in-edges); it is built once and
	// reused across every iteration. For graphs below the threshold (or
	// when GOMAXPROCS == 1) the serial push path is used unchanged, so
	// tiny graphs never pay the transpose nor the goroutine overhead.
	workers := runtime.GOMAXPROCS(0)
	useParallel := workers > 1 && live >= pageRankParallelThreshold
	var engine *pageRankEngine
	if useParallel {
		// Build only the reverse-CSR structure (offsets + in-neighbour
		// ids). The full csr.BuildReverse also transposes the weight and
		// handle columns, which PageRank never reads — for an int64- or
		// otherwise-weighted CSR that is a large wasted allocation and an
		// extra serial pass that dominates the per-iteration win when the
		// iteration count is small. The lean structure-only transpose
		// keeps the one-time O(V+E) cost minimal.
		revVerts, revEdges := pageRankBuildReverseStructure(verts, edges, n)
		if workers > n {
			workers = n
		}
		engine = newPageRankEngine(ctx, workers, revVerts, revEdges, n)
		defer engine.close()
	}

	for iter := 1; iter <= opts.MaxIterations; iter++ {
		if cerr := ctx.Err(); cerr != nil {
			metrics.IncCounter("search.centrality.PageRankCtx.errors", 1)
			return nil, 0, cerr
		}
		// Dangling mass: sum of cur[i] for live nodes with no out-edges.
		// Redistributed uniformly across all live nodes (canonical PageRank).
		// This O(V) reduction stays sequential (cheap, and its fixed
		// increasing-i order keeps the float sum deterministic).
		var danglingMass float64
		for i := 0; i < n; i++ {
			if isLive[i] && outdeg[i] == 0 {
				danglingMass += cur[i]
			}
		}
		baseShare := teleport + opts.Damping*danglingMass/float64(live)

		var delta float64
		if useParallel {
			// Cancellation is honoured at iteration granularity: a worker
			// runs its whole O(E/workers) range without polling ctx, then
			// ctx.Err() is checked here and at the top of the loop. One
			// iteration's SpMV is sub-millisecond per worker at the
			// parallel threshold, so cancellation latency stays bounded by
			// a single step plus the barrier.
			delta = engine.iterate(next, cur, isLive, outdeg, baseShare, opts.Damping)
			if cerr := ctx.Err(); cerr != nil {
				metrics.IncCounter("search.centrality.PageRankCtx.errors", 1)
				return nil, 0, cerr
			}
		} else {
			// Seed every live node with teleport + dangling redistribution.
			for i := 0; i < n; i++ {
				if isLive[i] {
					next[i] = baseShare
				} else {
					next[i] = 0
				}
			}

			// Distribute outgoing mass from non-dangling sources (push SpMV).
			for src := 0; src < n; src++ {
				if outdeg[src] == 0 {
					continue
				}
				share := opts.Damping * cur[src] / float64(outdeg[src])
				for k := verts[src]; k < verts[src+1]; k++ {
					next[int(edges[k])] += share
				}
			}

			// L1 delta.
			for i := 0; i < n; i++ {
				d := next[i] - cur[i]
				if d < 0 {
					d = -d
				}
				delta += d
			}
		}

		cur, next = next, cur
		if delta < opts.Tolerance {
			return cur, iter, nil
		}
	}
	return cur, opts.MaxIterations, nil
}

// validatePageRankOptions rejects invalid float options and fills in the
// zero-value defaults, returning the canonicalised options. It is the
// single validation site shared by [PageRankCtx] and [PageRanker.Run].
func validatePageRankOptions(opts PageRankOptions) (PageRankOptions, error) {
	if hasInvalidFloat(opts.Damping, opts.Tolerance) {
		return opts, ErrInvalidInput
	}
	// Zero is the Go zero-value sentinel meaning "use the default".
	// Only explicitly out-of-range values are rejected.
	if opts.Damping != 0 && (opts.Damping <= 0 || opts.Damping >= 1) {
		return opts, ErrInvalidInput
	}
	if opts.Tolerance < 0 {
		return opts, ErrInvalidInput
	}
	if opts.Damping == 0 {
		opts.Damping = 0.85
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 100
	}
	if opts.Tolerance <= 0 {
		opts.Tolerance = 1e-6
	}
	return opts, nil
}

// pageRankState bundles the CSR-derived working storage for one PageRank
// computation: the live/out-degree topology (a pure function of the CSR)
// and the two rank vectors. A fresh state is built per one-shot call; a
// [PageRanker] reuses one state across repeated runs to amortise the
// allocations and the reverse-CSR transpose.
type pageRankState[W any] struct {
	n        int
	live     int
	isLive   []bool
	outdeg   []uint32
	cur      []float64
	next     []float64
	revInit  bool     // whether revVerts/revEdges have been built
	revVerts []uint64 // reverse-CSR offsets (lazy, cached on a PageRanker)
	revEdges []graph.NodeID
}

// newPageRankState builds the CSR-derived topology (isLive, outdeg, live
// count) and allocates the two rank vectors. Returns nil when the graph
// is empty (n <= 0). The reverse-CSR structure is NOT built here; it is
// materialised lazily by run when the parallel path is selected.
func newPageRankState[W any](c *csr.CSR[W]) *pageRankState[W] {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	n := len(verts) - 1
	if n <= 0 {
		return nil
	}
	// A node is "live" if it has at least one incident edge (in or out).
	// Dangling sinks (out-degree 0, in-degree > 0) count as live;
	// totally isolated ghost slots from sharded NodeID packing do not.
	isLive := make([]bool, n)
	// outdeg is logically an unsigned integer count; storing it as
	// uint32 halves its memory footprint (n nodes saved 4 bytes each
	// vs the v1.0 float64) and doubles its L1 lane density. The
	// per-source float64 conversion at use site is a single FCVT
	// instruction with no measurable cost on M4-class cores.
	outdeg := make([]uint32, n)
	for i := 0; i < n; i++ {
		deg := verts[i+1] - verts[i]
		if deg > 0 {
			outdeg[i] = uint32(deg)
			isLive[i] = true
			for k := verts[i]; k < verts[i+1]; k++ {
				isLive[int(edges[k])] = true
			}
		}
	}
	live := 0
	for i := 0; i < n; i++ {
		if isLive[i] {
			live++
		}
	}
	return &pageRankState[W]{
		n:      n,
		live:   live,
		isLive: isLive,
		outdeg: outdeg,
		cur:    make([]float64, n),
		next:   make([]float64, n),
	}
}

// run executes the power iteration over the prepared state. The cur/next
// vectors are re-seeded each call so a reused state yields a result
// identical to a one-shot call. opts must already be validated/defaulted.
//
//nolint:gocyclo // canonical power-iteration: live re-seed + parallel/serial dispatch + iteration loop
func (st *pageRankState[W]) run(ctx context.Context, c *csr.CSR[W], opts PageRankOptions) (ranks []float64, iterations int, err error) {
	// Hoist every field the hot loop touches into locals so the loop body
	// reads stack/registers rather than chasing the *pageRankState pointer
	// each iteration — matching the locals-only form of the original
	// monolithic implementation.
	n := st.n
	live := st.live
	isLive := st.isLive
	outdeg := st.outdeg
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	// Re-seed cur from scratch (a reused state may hold a previous run's
	// values; the one-shot path's cur is already fresh zeros). next is not
	// re-seeded here: both the serial seed loop and the parallel runRange
	// fully overwrite every next[i] at the start of each iteration before
	// any read, so a stale next from a previous run is never observed.
	cur := st.cur
	next := st.next
	initRank := 1.0 / float64(live)
	for i := 0; i < n; i++ {
		if isLive[i] {
			cur[i] = initRank
		} else {
			cur[i] = 0
		}
	}
	teleport := (1 - opts.Damping) / float64(live)

	// Decide once whether to run the parallel pull-formulation SpMV. The
	// pull path requires the reverse-CSR (in-edges); it is built once and
	// reused across every iteration (and, on a PageRanker, across every
	// query). For graphs below the threshold (or when GOMAXPROCS == 1) the
	// serial push path is used unchanged, so tiny graphs never pay the
	// transpose nor the goroutine overhead.
	workers := runtime.GOMAXPROCS(0)
	useParallel := workers > 1 && live >= pageRankParallelThreshold
	var engine *pageRankEngine
	if useParallel {
		// Build only the reverse-CSR structure (offsets + in-neighbour
		// ids), and cache it on the state. The full csr.BuildReverse also
		// transposes the weight and handle columns, which PageRank never
		// reads — for an int64- or otherwise-weighted CSR that is a large
		// wasted allocation and an extra serial pass that dominates the
		// per-iteration win when the iteration count is small. The lean
		// structure-only transpose keeps the one-time O(V+E) cost minimal,
		// and a reused state pays it only on the first parallel run.
		if !st.revInit {
			st.revVerts, st.revEdges = pageRankBuildReverseStructure(verts, edges, n)
			st.revInit = true
		}
		if workers > n {
			workers = n
		}
		engine = newPageRankEngine(ctx, workers, st.revVerts, st.revEdges, n)
		defer engine.close()
	}

	for iter := 1; iter <= opts.MaxIterations; iter++ {
		if cerr := ctx.Err(); cerr != nil {
			metrics.IncCounter("search.centrality.PageRankCtx.errors", 1)
			return nil, 0, cerr
		}
		// Dangling mass: sum of cur[i] for live nodes with no out-edges.
		// Redistributed uniformly across all live nodes (canonical PageRank).
		// This O(V) reduction stays sequential (cheap, and its fixed
		// increasing-i order keeps the float sum deterministic).
		var danglingMass float64
		for i := 0; i < n; i++ {
			if isLive[i] && outdeg[i] == 0 {
				danglingMass += cur[i]
			}
		}
		baseShare := teleport + opts.Damping*danglingMass/float64(live)

		var delta float64
		if useParallel {
			// Cancellation is honoured at iteration granularity: a worker
			// runs its whole O(E/workers) range without polling ctx, then
			// ctx.Err() is checked here and at the top of the loop. One
			// iteration's SpMV is sub-millisecond per worker at the
			// parallel threshold, so cancellation latency stays bounded by
			// a single step plus the barrier.
			delta = engine.iterate(next, cur, isLive, outdeg, baseShare, opts.Damping)
			if cerr := ctx.Err(); cerr != nil {
				metrics.IncCounter("search.centrality.PageRankCtx.errors", 1)
				return nil, 0, cerr
			}
		} else {
			// Seed every live node with teleport + dangling redistribution.
			for i := 0; i < n; i++ {
				if isLive[i] {
					next[i] = baseShare
				} else {
					next[i] = 0
				}
			}

			// Distribute outgoing mass from non-dangling sources (push SpMV).
			for src := 0; src < n; src++ {
				if outdeg[src] == 0 {
					continue
				}
				share := opts.Damping * cur[src] / float64(outdeg[src])
				for k := verts[src]; k < verts[src+1]; k++ {
					next[int(edges[k])] += share
				}
			}

			// L1 delta.
			for i := 0; i < n; i++ {
				d := next[i] - cur[i]
				if d < 0 {
					d = -d
				}
				delta += d
			}
		}

		cur, next = next, cur
		if delta < opts.Tolerance {
			st.cur, st.next = cur, next
			return cur, iter, nil
		}
	}
	st.cur, st.next = cur, next
	return cur, opts.MaxIterations, nil
}

// PageRanker is a stateful, reusable PageRank computer bound to one
// immutable CSR snapshot. It caches the CSR-derived working storage —
// the live/out-degree topology and, on the parallel path, the reverse-CSR
// transpose — so that repeated [PageRanker.Run] calls on the same
// snapshot skip those one-time allocations. It mirrors the stateless
// [PageRank] / stateful split used elsewhere in the package (e.g.
// search.Dijkstra vs search.DijkstraInto).
//
// Use it for repeated-query workloads (parameter sweeps over Damping or
// MaxIterations, convergence studies, A/B comparisons) on a single graph.
// For a single computation, prefer the one-shot [PageRank].
//
// # Concurrency
//
// A PageRanker owns mutable working buffers and is therefore NOT safe for
// concurrent use: a single PageRanker must not have Run invoked from more
// than one goroutine at a time. To run PageRank concurrently over a shared
// CSR, give each goroutine its own PageRanker (or call the one-shot
// [PageRank]); because the underlying CSR is immutable and read-only,
// independent PageRankers over the same snapshot are race-free.
//
// # Result aliasing
//
// The []float64 returned by Run aliases an internal buffer and is
// invalidated by the next Run call on the same PageRanker. Callers that
// need the rank vector to outlive the next Run must copy it.
type PageRanker[W any] struct {
	c     *csr.CSR[W]
	state *pageRankState[W]
}

// NewPageRanker builds a reusable PageRanker over the immutable snapshot
// c. The CSR-derived topology is built eagerly; the reverse-CSR transpose
// (needed only by the parallel path) is built lazily on the first Run
// that selects it and then cached for subsequent runs.
func NewPageRanker[W any](c *csr.CSR[W]) *PageRanker[W] {
	return &PageRanker[W]{c: c, state: newPageRankState[W](c)}
}

// Run computes PageRank over the bound snapshot with opts and returns the
// per-NodeID rank slice plus the iteration count to convergence. The
// returned slice aliases an internal buffer (see the type's Result
// aliasing note) and is invalidated by the next Run.
//
// The result is bit-for-bit identical to the equivalent one-shot
// [PageRankCtx] call: Run re-seeds the rank vectors from scratch on every
// invocation and consumes the same shared core, so reusing cached state
// changes only the allocation profile, never the output.
func (p *PageRanker[W]) Run(ctx context.Context, opts PageRankOptions) (ranks []float64, iterations int, err error) {
	defer metrics.Time("search.centrality.PageRanker.Run")()
	opts, err = validatePageRankOptions(opts)
	if err != nil {
		metrics.IncCounter("search.centrality.PageRanker.Run.errors", 1)
		return nil, 0, err
	}
	if p.state == nil {
		// n <= 0: empty graph.
		return nil, 0, nil
	}
	if p.state.live == 0 {
		return make([]float64, p.state.n), 0, nil
	}
	return p.state.run(ctx, p.c, opts)
}

// pageRankBuildReverseStructure transposes the forward CSR (verts,
// edges) into a structure-only reverse-CSR: revVerts are the in-edge
// prefix-sum offsets (length n+1) and revEdges lists, for every vertex
// v, its in-neighbours u. Weights and edge handles are deliberately not
// transposed — PageRank reads neither, and on a weighted CSR copying
// them would add a large allocation and a serial pass.
//
// Order. In-neighbours are scattered in increasing source-id order (the
// outer loop visits u = 0..n-1 ascending), so revEdges[revVerts[v]:
// revVerts[v+1]] lists v's in-neighbours in the same order the serial
// push path accumulates contributions into next[v]. This is the
// property the bit-identity determinism argument relies on.
//
// Complexity: O(V + E) time, O(V + E) space (two integer arrays only).
func pageRankBuildReverseStructure(verts []uint64, edges []graph.NodeID, n int) (revVerts []uint64, revEdges []graph.NodeID) {
	revVerts = make([]uint64, n+1)
	// Pass 1: count in-degree per destination into revVerts[v+1].
	for u := 0; u < n; u++ {
		for k := verts[u]; k < verts[u+1]; k++ {
			revVerts[int(edges[k])+1]++
		}
	}
	// Prefix sum -> offsets.
	for i := 1; i <= n; i++ {
		revVerts[i] += revVerts[i-1]
	}
	revEdges = make([]graph.NodeID, revVerts[n])
	// Pass 2: scatter sources into reversed slots, advancing a per-dest
	// cursor. The cursor array reuses no extra type; it is a temporary.
	cursor := make([]uint64, n)
	for u := 0; u < n; u++ {
		for k := verts[u]; k < verts[u+1]; k++ {
			v := int(edges[k])
			revEdges[revVerts[v]+cursor[v]] = graph.NodeID(u)
			cursor[v]++
		}
	}
	return revVerts, revEdges
}

// pageRankEngine is a persistent worker pool that runs one PageRank
// power-iteration step per call to iterate, using the pull formulation
// over a reverse-CSR. The pool is created once (newPageRankEngine) and
// reused across every iteration, so the per-iteration goroutine spawn
// and pprof-label cost is paid once rather than per iteration — the
// dominant overhead the naive fan-out/join design suffered on the
// short, memory-bound SpMV step.
//
// Pull formulation. For every live vertex v,
//
//	next[v] = baseShare + damping * Σ_{u ∈ in(v)} cur[u] / outdeg[u]
//
// where in(v) are the in-neighbours of v listed in revEdges. Each v
// writes only next[v], so workers operate on disjoint output ranges
// with zero write contention.
//
// Determinism. The summation order for each v is the fixed reverse-CSR
// order revEdges[revVerts[v]:revVerts[v+1]], independent of the worker
// that handles v and of the worker count. Because
// pageRankBuildReverseStructure scatters in-neighbours in increasing
// source-id order — the same order in which the serial push path
// accumulates them — the per-vertex float sum is identical to the
// serial push result down to the last bit. The
// per-worker partial L1 deltas are reduced in fixed worker-id order, so
// the returned delta is likewise deterministic across worker counts.
//
// Load balancing. Vertex ranges are partitioned by approximately equal
// in-edge count, not equal vertex count. On power-law graphs a handful
// of hub vertices hold a large fraction of all in-edges, so equal-
// vertex chunks would be grossly imbalanced and the hub-heavy worker
// would gate the barrier. Edge-balanced contiguous ranges keep the
// per-worker work even while preserving the increasing-v order that the
// determinism argument relies on.
//
// Complexity: O(V + E) work per iterate call, O(workers) extra space.
type pageRankEngine struct {
	ctx      context.Context
	workers  int
	revVerts []uint64
	revEdges []graph.NodeID
	bounds   []int           // edge-balanced chunk boundaries, len workers+1
	deltas   []float64       // per-worker partial L1 delta
	start    []chan struct{} // fan-out: one private signal channel per worker
	done     chan int        // fan-in: each worker reports its index on completion
	quit     chan struct{}

	// Per-iteration shared inputs, set by iterate before releasing workers.
	next, cur []float64
	isLive    []bool
	outdeg    []uint32
	baseShare float64
	damping   float64
}

// newPageRankEngine builds the edge-balanced partition and launches the
// persistent worker goroutines. close must be called to terminate them.
func newPageRankEngine(ctx context.Context, workers int, revVerts []uint64, revEdges []graph.NodeID, n int) *pageRankEngine {
	e := &pageRankEngine{
		ctx:      ctx,
		workers:  workers,
		revVerts: revVerts,
		revEdges: revEdges,
		deltas:   make([]float64, workers),
		start:    make([]chan struct{}, workers),
		done:     make(chan int, workers),
		quit:     make(chan struct{}),
	}
	e.bounds = edgeBalancedBounds(revVerts, n, workers)
	for w := 0; w < workers; w++ {
		e.start[w] = make(chan struct{})
		go e.worker(w)
	}
	return e
}

// edgeBalancedBounds partitions [0, n) into workers contiguous vertex
// ranges with approximately equal in-edge counts, returning the
// workers+1 boundary offsets (bounds[0]=0, bounds[workers]=n). The
// partition uses the reverse-CSR prefix sums (revVerts) so each range's
// in-edge total is revVerts[hi]-revVerts[lo].
//
// The boundary scan advances a single cursor v monotonically, so the
// returned offsets are non-decreasing. On a graph dominated by one
// very-high-in-degree hub, several boundaries may collapse to the same
// v, leaving some workers an empty range (lo == hi); that is harmless —
// an empty range writes no next[v] and contributes zero delta.
func edgeBalancedBounds(revVerts []uint64, n, workers int) []int {
	bounds := make([]int, workers+1)
	bounds[workers] = n
	if n == 0 || workers <= 1 {
		return bounds
	}
	totalEdges := revVerts[n]
	v := 0
	for w := 1; w < workers; w++ {
		// Target cumulative in-edges at this boundary. The cursor v only
		// advances (revVerts is non-decreasing), so bounds stay sorted.
		target := uint64(w) * totalEdges / uint64(workers)
		for v < n && revVerts[v] < target {
			v++
		}
		bounds[w] = v
	}
	return bounds
}

// worker is the persistent loop: park on start, run the assigned range,
// report on done; exit on quit.
func (e *pageRankEngine) worker(w int) {
	pprof.Do(e.ctx, pprof.Labels("component", "pagerank-pull", "worker", fmt.Sprintf("%d", w)), func(context.Context) {
		for {
			select {
			case <-e.quit:
				return
			case <-e.start[w]:
				e.runRange(w)
				e.done <- w
			}
		}
	})
}

// runRange computes next[v] for every v in worker w's edge-balanced
// range and accumulates the partial L1 delta into e.deltas[w].
func (e *pageRankEngine) runRange(w int) {
	lo, hi := e.bounds[w], e.bounds[w+1]
	next, cur := e.next, e.cur
	isLive, outdeg := e.isLive, e.outdeg
	revVerts, revEdges := e.revVerts, e.revEdges
	baseShare, damping := e.baseShare, e.damping
	var localDelta float64
	for v := lo; v < hi; v++ {
		if !isLive[v] {
			// next[v] becomes 0; its delta contribution is |0 - cur[v]|.
			d := cur[v]
			if d < 0 {
				d = -d
			}
			localDelta += d
			next[v] = 0
			continue
		}
		sum := baseShare
		for k := revVerts[v]; k < revVerts[v+1]; k++ {
			u := int(revEdges[k])
			sum += damping * cur[u] / float64(outdeg[u])
		}
		next[v] = sum
		d := sum - cur[v]
		if d < 0 {
			d = -d
		}
		localDelta += d
	}
	e.deltas[w] = localDelta
}

// iterate runs one power-iteration step across the worker pool and
// returns the deterministic L1 delta ||next - cur||_1.
func (e *pageRankEngine) iterate(next, cur []float64, isLive []bool, outdeg []uint32, baseShare, damping float64) float64 {
	e.next, e.cur = next, cur
	e.isLive, e.outdeg = isLive, outdeg
	e.baseShare, e.damping = baseShare, damping
	// Release every worker on its private channel, then wait for all to
	// report. The per-worker start[w]/done rendezvous is the per-iteration
	// barrier; private start channels pin each fixed range to its worker
	// so no worker can steal a second range within one iteration.
	for w := 0; w < e.workers; w++ {
		e.start[w] <- struct{}{}
	}
	for w := 0; w < e.workers; w++ {
		<-e.done
	}
	// Deterministic reduction of partial deltas in fixed worker-id order.
	var delta float64
	for w := 0; w < e.workers; w++ {
		delta += e.deltas[w]
	}
	return delta
}

// close terminates the worker pool. It is safe to call exactly once.
func (e *pageRankEngine) close() { close(e.quit) }
