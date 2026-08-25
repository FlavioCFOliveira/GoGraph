package sim

// This file contributes a self-contained algorithm-correctness check for the
// FLOW family of the search/flow package to the deterministic simulation
// harness. It exercises two algorithms against independent naive references
// that share no code with the production implementations:
//
//   - max-flow: search/flow's MaxFlow (Dinic) and EdmondsKarp are each compared,
//     by VALUE only, against an independent BFS augmenting-path (Edmonds-Karp)
//     max-flow written here from scratch. As a second, structurally-independent
//     invariant the max-flow value is also checked against the capacity of the
//     minimum s-t cut, derived from residual reachability on the reference's own
//     residual graph (max-flow min-cut theorem);
//   - global min cut: search/flow's StoerWagner is compared, by WEIGHT only,
//     against the global min cut computed independently as the minimum over every
//     sink t in {1..n-1} of the s-t min cut (s fixed at 0), each s-t min cut
//     obtained from the reference max-flow run on the symmetric capacities;
//   - min-cost max-flow: search/flow's MinCostMaxFlow / MinCostMaxFlowCtx are
//     compared, by (flow, cost), against an independent successive-shortest-path
//     reference that augments along Bellman-Ford (SPFA) cheapest paths. Both cost
//     regimes are driven on EVERY tick — all-positive costs (the zero-potential
//     fast path) and forced-negative costs (the Bellman-Ford potential bootstrap)
//     — plus two hand-built networks that plant a negative-cost CYCLE and must be
//     refused with flow.ErrNegativeCycle.
//
// Determinism is load-bearing: every fixture is a pure function of the tick via
// a single Seed draw stream; there is no time, no global rand, and no Go
// map-iteration order in any output path. Only integer capacities/weights are
// used, so every comparison is an exact integer equality with no tolerance.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/search/flow"
)

// flowSeedConst is XORed with the tick to derive this checker's seed. A distinct
// per-checker constant keeps the flow checker's draw stream independent of any
// other seed-derived stream in the harness for the same tick, so its fixtures do
// not correlate with another checker's.
const flowSeedConst uint64 = 0x1f0f10b1c0ffee11

// flowMaxFixtures is how many max-flow fixtures flowViolations generates per
// tick; flowCutFixtures is the same for Stoer-Wagner global-min-cut fixtures.
// Both are small and fixed so the per-tick cost stays bounded and the draw
// stream length is independent of any input.
const (
	flowMaxFixtures = 4
	flowCutFixtures = 4
	// flowMCMFFixtures is how many min-cost-max-flow fixtures flowViolations
	// generates per tick.
	flowMCMFFixtures = 4
	// flowMCMFNegFrom is the first min-cost-max-flow fixture INDEX that carries
	// negative arc costs. Fixtures [0, flowMCMFNegFrom) keep the all-positive
	// costs the checker has always used — the ZERO-POTENTIAL FAST PATH, where
	// search/flow's hasNegativeCost is false and the Bellman-Ford bootstrap is
	// skipped entirely — and fixtures [flowMCMFNegFrom, flowMCMFFixtures) are
	// forced-negative, which is the only way the DST drives that bootstrap.
	//
	// The flavour is selected by fixture INDEX and never by a seed draw. That is
	// deliberate: with an index both flavours fire on EVERY tick, so neither code
	// path can go unexercised because a tick drew unluckily.
	flowMCMFNegFrom = 2
)

// flowMaxCost bounds the per-edge integer cost the min-cost-max-flow fixtures
// emit. Kept small (like [flowMaxCap]) so the flow-times-cost products stay far
// below the int64 accumulation the algorithm guards, and the hand-reasoning
// about a fixture stays tractable.
const flowMaxCost = 10

// flowCapInf is the local "infinite push" sentinel for the reference BFS
// max-flow. It is the maximum bottleneck seeded into each augmenting search and
// must stay strictly above any capacity the fixtures can produce. Fixture
// capacities are bounded by flowMaxCap (a few tens), so a value this large
// cannot be confused with a real edge capacity. It is deliberately well below
// the production package's own sentinel (1<<62) so the reference never trips the
// engine's overflow guard either.
const flowCapInf = 1 << 40

// flowMaxCap bounds the per-edge integer capacity / per-edge undirected weight
// the fixtures emit. Kept small so sums of capacities cannot approach
// flowCapInf even on the densest fixture, and so the hand-reasoning about the
// fixtures stays tractable.
const flowMaxCap = 20

// flowViolations runs the FLOW-family algorithm-correctness checks for one
// simulation tick and returns one Violation per divergence found, or nil when
// every fixture agrees with its independent reference. The result is a pure
// function of tick: the same tick always produces the same fixtures and hence
// the same verdict.
func flowViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ flowSeedConst)
	var out []Violation

	for i := 0; i < flowMaxFixtures; i++ {
		out = append(out, flowCheckMaxFlow(tick, seed)...)
	}
	for i := 0; i < flowCutFixtures; i++ {
		out = append(out, flowCheckMinCut(tick, seed)...)
	}
	// Fixture INDEX (never a seed draw) selects the cost flavour, so the
	// all-positive fast path and the negative-cost Bellman-Ford bootstrap are
	// BOTH driven on every tick. See [flowMCMFNegFrom].
	for i := 0; i < flowMCMFFixtures; i++ {
		out = append(out, flowCheckMinCostMaxFlow(tick, seed, i >= flowMCMFNegFrom)...)
	}
	// Planted negative-cycle fixtures: hand-built constants that consume no
	// draws, so they perturb neither the stream above nor each other.
	out = append(out, flowCheckNegativeCycle(tick)...)
	return out
}

// flowCheckMaxFlow builds one directed capacity network from the seed, then
// asserts that:
//
//   - flow.MaxFlow (Dinic) equals the independent BFS reference max-flow value;
//   - flow.EdmondsKarp equals the same reference value;
//   - the reference max-flow value equals the capacity of the minimum s-t cut
//     derived from residual reachability on the reference residual graph
//     (max-flow min-cut, a check that shares no augmenting-path code with the
//     value computation's outer loop).
//
// Each fixture uses a freshly-built Network per algorithm call because the
// package's MaxFlow/EdmondsKarp mutate the network's residual capacities in
// place; reusing one network across calls would feed a drained residual graph
// to the next algorithm. Comparisons are exact integer equalities; on any
// mismatch a ViolationSearchDivergence is appended and the others are still
// evaluated so a single fixture can report every way it diverged.
func flowCheckMaxFlow(tick int64, seed *Seed) []Violation {
	const op = "search:MaxFlow"
	n, edges := flowGenNetwork(seed)
	src, sink := 0, n-1

	// Independent reference: max-flow value plus the min-cut capacity from its
	// own residual graph. The reference operates on its own residual arrays and
	// never touches a flow.Network.
	refFlow, refCut := flowRefMaxFlowAndMinCut(n, edges, src, sink)

	var out []Violation

	// flow.MaxFlow (Dinic) — fresh network, value-only comparison.
	gotDinic := flow.MaxFlow(flowBuildNetwork(n, edges), src, sink)
	if gotDinic != refFlow {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"MaxFlow (Dinic) value diverged from independent reference: got=%d ref=%d (n=%d, src=%d, sink=%d, edges=%s)",
				gotDinic, refFlow, n, src, sink, flowFmtEdges(edges)),
		})
	}

	// flow.EdmondsKarp — fresh network, value-only comparison.
	gotEK := flow.EdmondsKarp(flowBuildNetwork(n, edges), src, sink)
	if gotEK != refFlow {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"EdmondsKarp value diverged from independent reference: got=%d ref=%d (n=%d, src=%d, sink=%d, edges=%s)",
				gotEK, refFlow, n, src, sink, flowFmtEdges(edges)),
		})
	}

	// flow.PushRelabelMaxFlow — fresh network, value-only comparison. A third,
	// algorithmically-distinct max-flow (FIFO push-relabel with the gap
	// heuristic) held to the same independent reference value as Dinic/EK.
	gotPR := flow.PushRelabelMaxFlow(flowBuildNetwork(n, edges), src, sink)
	if gotPR != refFlow {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"PushRelabelMaxFlow value diverged from independent reference: got=%d ref=%d (n=%d, src=%d, sink=%d, edges=%s)",
				gotPR, refFlow, n, src, sink, flowFmtEdges(edges)),
		})
	}

	// Second invariant: max-flow == min-cut capacity (max-flow min-cut theorem),
	// both from the reference. This catches a reference that augments wrongly in
	// a way that happens to match a buggy engine, because the cut is read from
	// residual reachability rather than the augmenting loop's running total.
	if refFlow != refCut {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"reference max-flow/min-cut self-inconsistency: maxflow=%d mincut=%d (n=%d, src=%d, sink=%d, edges=%s)",
				refFlow, refCut, n, src, sink, flowFmtEdges(edges)),
		})
	}

	return out
}

// flowCheckMinCut builds one connected undirected symmetric weight matrix from
// the seed, then asserts that flow.StoerWagner's global-min-cut WEIGHT equals
// the global min cut computed independently as
//
//	min over t in {1..n-1} of  (s=0, t) s-t min cut
//
// where each s-t min cut is the reference max-flow on the symmetric capacities
// (an undirected edge {i,j} of weight w becomes a directed arc i->j and j->i
// each of capacity w). Fixing s=0 and ranging t over all other vertices is
// sufficient for the GLOBAL min cut: the global min cut separates 0 from at
// least one vertex t, and that (0,t) cut is then among the minimised set; any
// (0,t) cut is in turn an upper bound on the global min cut, so the minimum over
// t equals the global min cut exactly. The comparison is an exact integer
// equality on the cut weight only (the A/B partition is not compared).
func flowCheckMinCut(tick int64, seed *Seed) []Violation {
	const op = "search:StoerWagner"
	n, w := flowGenWeightMatrix(seed)

	got := flow.StoerWagner(w, n)
	ref := flowRefGlobalMinCut(n, w)

	if got.Weight != ref {
		return []Violation{{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"StoerWagner global-min-cut weight diverged from independent reference: got=%d ref=%d (n=%d, weights=%s)",
				got.Weight, ref, n, flowFmtMatrix(w, n)),
		}}
	}
	return nil
}

// flowEdge is one directed capacity arc of a generated max-flow fixture.
type flowEdge struct {
	src, dst, cap int
}

// flowGenNetwork derives a directed capacity network from seed: n nodes
// (6..10), src=0, sink=n-1, with seed-chosen positive-capacity arcs. It first
// lays a deterministic forward "spine" 0->1->...->(n-1) so the network is always
// connected and the max-flow is positive (avoiding a degenerate all-zero
// fixture), then adds extra forward-biased arcs to create alternative augmenting
// paths and bottlenecks. All capacities are in [1, flowMaxCap]. Edges are
// emitted in a fixed, index-ordered sequence so the output never depends on map
// iteration order.
func flowGenNetwork(seed *Seed) (int, []flowEdge) {
	n := 6 + seed.IntN(5) // 6..10
	edges := make([]flowEdge, 0, n*2)

	// Connected spine guaranteeing a positive max-flow.
	for i := 0; i < n-1; i++ {
		edges = append(edges, flowEdge{src: i, dst: i + 1, cap: 1 + seed.IntN(flowMaxCap)})
	}

	// Extra forward-biased arcs (src index < dst index keeps the network a DAG,
	// which keeps the reference's correctness easy to reason about while still
	// exercising multiple augmenting paths and shared bottlenecks). The number
	// of extra arcs is seed-chosen but bounded.
	extra := seed.IntN(n + 2)
	for k := 0; k < extra; k++ {
		a := seed.IntN(n)
		b := seed.IntN(n)
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		edges = append(edges, flowEdge{src: a, dst: b, cap: 1 + seed.IntN(flowMaxCap)})
	}
	return n, edges
}

// flowBuildNetwork constructs a fresh flow.Network for one algorithm call. A new
// network is required per call because the package's max-flow routines mutate
// the residual capacities in place.
func flowBuildNetwork(n int, edges []flowEdge) *flow.Network {
	g := flow.NewNetwork(n)
	for _, e := range edges {
		g.AddEdge(e.src, e.dst, e.cap)
	}
	return g
}

// flowGenWeightMatrix derives a connected undirected symmetric integer weight
// matrix from seed: n nodes (5..7), dense row-major n*n. It first lays a
// deterministic spanning path 0-1-...-(n-1) so the graph is always connected
// (every global cut is then positive), then adds seed-chosen extra undirected
// edges. Every weight is in [1, flowMaxCap]; the matrix is symmetric and its
// diagonal is zero (no self-loops). Edges are written in a fixed (i<j) order, so
// the matrix is a pure function of the draw stream with no map ordering.
func flowGenWeightMatrix(seed *Seed) (int, []int) {
	n := 5 + seed.IntN(3) // 5..7
	w := make([]int, n*n)
	set := func(i, j, v int) {
		w[i*n+j] = v
		w[j*n+i] = v
	}

	// Spanning path guaranteeing connectivity.
	for i := 0; i < n-1; i++ {
		set(i, i+1, 1+seed.IntN(flowMaxCap))
	}

	// Extra undirected edges over distinct (i<j) pairs, seed-chosen but bounded.
	extra := seed.IntN(n + 2)
	for k := 0; k < extra; k++ {
		a := seed.IntN(n)
		b := seed.IntN(n)
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		// Overwrite (rather than accumulate) so a repeated pair stays within
		// [1, flowMaxCap]; the value is still a deterministic function of the
		// draw order.
		set(a, b, 1+seed.IntN(flowMaxCap))
	}
	return n, w
}

// flowRefMaxFlowAndMinCut is the independent reference for the max-flow fixtures.
// It computes the s-t max-flow value with a BFS augmenting-path (Edmonds-Karp)
// loop on its own residual arrays — sharing no code with search/flow — and then
// reads the capacity of the minimum s-t cut directly from the residual graph
// (the set S of vertices reachable from src on positive-residual arcs defines
// the cut; its capacity is the sum of ORIGINAL capacities of arcs from S to V\S).
// Returning both lets the caller assert max-flow == min-cut as a second
// invariant. Capacities are integers, so the arithmetic is exact.
func flowRefMaxFlowAndMinCut(n int, edges []flowEdge, src, sink int) (maxFlow, minCut int) {
	// Build a residual graph as dense adjacency over arc records. Each undirected
	// residual pair is (forward, backward); the backward arc starts at zero
	// capacity. We keep the ORIGINAL forward capacity separately for the cut sum.
	type arc struct {
		to      int
		cap     int // residual capacity (mutated by augmentation)
		origCap int // original capacity (forward arcs only; 0 for back-arcs)
		rev     int // index into adj[to] of the paired reverse arc
		isFwd   bool
	}
	adj := make([][]arc, n)
	addArc := func(u, v, c int) {
		fwd := arc{to: v, cap: c, origCap: c, rev: len(adj[v]), isFwd: true}
		bwd := arc{to: u, cap: 0, origCap: 0, rev: len(adj[u]), isFwd: false}
		adj[u] = append(adj[u], fwd)
		adj[v] = append(adj[v], bwd)
	}
	for _, e := range edges {
		addArc(e.src, e.dst, e.cap)
	}

	// BFS augmenting-path max-flow (Edmonds-Karp). parentNode/parentArc record
	// the predecessor and the arc used to reach each node so the bottleneck path
	// can be walked back.
	parentNode := make([]int, n)
	parentArc := make([]int, n)
	for {
		for i := range parentNode {
			parentNode[i] = -1
		}
		parentNode[src] = src
		queue := []int{src}
		for qh := 0; qh < len(queue) && parentNode[sink] == -1; qh++ {
			u := queue[qh]
			for ai := range adj[u] {
				a := adj[u][ai]
				if a.cap > 0 && parentNode[a.to] == -1 {
					parentNode[a.to] = u
					parentArc[a.to] = ai
					queue = append(queue, a.to)
				}
			}
		}
		if parentNode[sink] == -1 {
			break // no augmenting path remains
		}
		// Bottleneck along the discovered path.
		push := flowCapInf
		for v := sink; v != src; v = parentNode[v] {
			a := adj[parentNode[v]][parentArc[v]]
			if a.cap < push {
				push = a.cap
			}
		}
		// Apply the push: decrement forward residual, increment paired reverse.
		for v := sink; v != src; v = parentNode[v] {
			u := parentNode[v]
			ai := parentArc[v]
			adj[u][ai].cap -= push
			rev := adj[u][ai].rev
			adj[v][rev].cap += push
		}
		maxFlow += push
	}

	// Min cut from residual reachability: S = nodes reachable from src on
	// positive-residual arcs after the max-flow saturates the cut.
	inS := make([]bool, n)
	inS[src] = true
	stack := []int{src}
	for len(stack) > 0 {
		u := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for ai := range adj[u] {
			a := adj[u][ai]
			if a.cap > 0 && !inS[a.to] {
				inS[a.to] = true
				stack = append(stack, a.to)
			}
		}
	}
	// Cut capacity = sum of ORIGINAL capacities of forward arcs from S to V\S.
	for u := 0; u < n; u++ {
		if !inS[u] {
			continue
		}
		for ai := range adj[u] {
			a := adj[u][ai]
			if a.isFwd && !inS[a.to] {
				minCut += a.origCap
			}
		}
	}
	return maxFlow, minCut
}

// flowRefGlobalMinCut is the independent reference for the Stoer-Wagner fixtures.
// It computes the GLOBAL min cut of the undirected symmetric weight matrix as the
// minimum over every sink t in {1..n-1} of the (s=0, t) s-t min cut, where each
// s-t min cut equals the reference max-flow on the symmetric capacities. For
// n <= 1 the global min cut is 0 by convention. The arithmetic is exact integer.
func flowRefGlobalMinCut(n int, w []int) int {
	if n <= 1 {
		return 0
	}
	// The symmetric weight matrix maps to a directed capacity network with arc
	// i->j and j->i each of capacity w[i*n+j]; this directed s-t max-flow equals
	// the undirected s-t min cut.
	edges := make([]flowEdge, 0, n*n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i != j && w[i*n+j] > 0 {
				edges = append(edges, flowEdge{src: i, dst: j, cap: w[i*n+j]})
			}
		}
	}
	best := -1
	for t := 1; t < n; t++ {
		f, _ := flowRefMaxFlowAndMinCut(n, edges, 0, t)
		if best < 0 || f < best {
			best = f
		}
	}
	if best < 0 {
		// n >= 2 with no edges at all: the graph is disconnected and the global
		// min cut is 0. (The fixtures always lay a spanning path, so this is a
		// defensive default rather than a reachable branch.)
		return 0
	}
	return best
}

// flowCostEdge is one directed arc of a generated min-cost-max-flow fixture,
// carrying a capacity (always a positive integer) and a per-unit cost. The cost
// is strictly positive on the all-positive fixture flavour and may be negative,
// zero, or positive on the forced-negative flavour (see [flowGenCostNetwork]).
type flowCostEdge struct {
	src, dst, cap, cost int
}

// flowResidualArc is one arc of the reference min-cost-max-flow's own residual
// graph. Each generated edge contributes a forward arc (cap = capacity,
// cost = cost) and a paired reverse arc (cap = 0, cost = -cost); rev indexes the
// partner inside the head node's arc slice so an augmentation can move capacity
// between them. Naming the residual as a type is what lets the optimality
// certificate ([flowResidualHasNegativeCycle]) read the reference's FINAL
// residual directly, without re-running any shortest-path search.
type flowResidualArc struct {
	to   int
	cap  int
	cost int
	rev  int
}

// errFlowRefRelaxBudget is returned by [flowRefMinCostMaxFlow] when its SPFA
// exhausts the relaxation budget.
//
// SPFA has no negative-cycle detector. On a residual graph that contains a
// negative-cost cycle its integer distances decrease without bound and nodes
// requeue forever, so the reference does not FAIL on such a graph — it HANGS,
// which in a deterministic simulation is the worst failure mode there is (a
// hung tick reports nothing at all). The budget converts that hang into a named
// violation, and it defends against any future route to a negative cycle, not
// only the one anticipated when it was written.
var errFlowRefRelaxBudget = errors.New("sim: reference SSP exhausted its relaxation budget (negative-cost cycle in the residual graph)")

// flowCheckMinCostMaxFlow builds one directed capacity+cost network from the
// seed — all-positive costs when negative is false, forced-negative costs when
// it is true — and hands it to [flowCheckCostNetwork], which holds every
// assertion. The split exists so a test can drive the exact production predicate
// with a hand-built network and watch each clause fire.
func flowCheckMinCostMaxFlow(tick int64, seed *Seed, negative bool) []Violation {
	n, edges := flowGenCostNetwork(seed, negative)
	return flowCheckCostNetwork(tick, n, edges, 0, n-1)
}

// flowCheckCostNetwork asserts every min-cost-max-flow invariant on one
// WELL-FORMED capacity+cost network — one whose arcs all run strictly from a low
// index to a high index, as [flowGenCostNetwork] guarantees.
//
// The clauses, in order:
//
//  0. STRUCTURAL PRECONDITION — every arc satisfies src < dst. Under negative
//     costs this is not a convenience but a soundness AND a liveness
//     precondition (both spelled out on [flowGenCostNetwork]). A violation is
//     reported and the reference is NOT run, because a backward arc can close a
//     negative cycle the reference cannot survive.
//  1. [flow.MinCostMaxFlowCtx] returns a NIL ERROR, and the error is named when
//     it does not. The non-context [flow.MinCostMaxFlow] wrapper discards its
//     error, so driving only the wrapper silently swallows three distinct
//     failures: flow.ErrNegativeCycle, flow.ErrCapacityOverflow, and — most
//     valuable of all — the internal rc<0 invariant violation, which returns a
//     PARTIAL flow alongside its error. That rc<0 guard is the exact tripwire
//     for a broken potential bootstrap: it is unreachable while every cost is
//     non-negative, and it is the single most informative signal the moment
//     negative costs exist. Naming the error is the difference between "the
//     bootstrap is broken" and a generic "(flow,cost) diverged".
//  2. the non-context wrapper agrees with the context entry point on
//     (flow, cost), which pins the wrapper's own documented contract.
//  3. (flow, cost) equals an INDEPENDENT successive-shortest-path reference
//     ([flowRefMinCostMaxFlow]) that shares no code with the production
//     Dijkstra-with-potentials SSP — it augments along Bellman-Ford (SPFA)
//     cheapest-cost paths on its own residual arrays. Agreement on BOTH numbers
//     certifies the cost is minimal among all max-flows.
//  4. the flow VALUE equals the plain Dinic max-flow ([flow.MaxFlow]) on the
//     same capacity topology with the costs ignored, proving MinCostMaxFlow
//     ships the MAXIMUM flow and not merely a cheap sub-maximal one.
//  5. OPTIMALITY CERTIFICATE — the reference's own FINAL residual graph holds no
//     negative-cost cycle. By Ahuja-Magnanti-Orlin Thm 9.1 a flow is min-cost
//     for its value iff its residual graph has no negative cycle, so clause (4)
//     and clause (5) together certify "min-cost MAX-flow" with no shortest-path
//     search of any kind. It is also the only clause here able to catch a
//     misconception SHARED by production and reference: the two implement the
//     same SSP schema and differ only in the shortest-path engine, so an error
//     in the schema itself would simply agree with itself in clause (3).
//
// Every algorithm call receives a freshly-built network because the flow
// routines mutate residual capacities in place; reusing one would feed a drained
// residual graph to the next call. All comparisons are exact integer equalities.
func flowCheckCostNetwork(tick int64, n int, edges []flowCostEdge, src, sink int) []Violation {
	const op = "search:MinCostMaxFlow"

	// (0) Structural precondition, asserted BEFORE the reference is called.
	if bad := flowFirstNonDAGArc(edges); bad >= 0 {
		e := edges[bad]
		return []Violation{{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"min-cost-max-flow fixture broke its DAG precondition: arc #%d is %d->%d, but every arc must satisfy src < dst (n=%d, edges=%s)",
				bad, e.src, e.dst, n, flowFmtCostEdges(edges)),
		}}
	}

	refFlow, refCost, refResidual, refErr := flowRefMinCostMaxFlow(n, edges, src, sink)
	if refErr != nil {
		return []Violation{{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"independent SSP reference failed on a well-formed fixture: %v (n=%d, src=%d, sink=%d, edges=%s)",
				refErr, n, src, sink, flowFmtCostEdges(edges)),
		}}
	}

	var out []Violation

	// (1) The context entry point must succeed, and must say so.
	gotFlow, gotCost, err := flow.MinCostMaxFlowCtx(
		context.Background(), flowBuildCostNetwork(n, edges), src, sink)
	if err != nil {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"MinCostMaxFlowCtx returned a non-nil error on a well-formed fixture: %v (partial=(%d,%d), n=%d, src=%d, sink=%d, edges=%s)",
				err, gotFlow, gotCost, n, src, sink, flowFmtCostEdges(edges)),
		})
	}

	// (2) The non-context wrapper must agree with the context entry point.
	plainFlow, plainCost := flow.MinCostMaxFlow(flowBuildCostNetwork(n, edges), src, sink)
	if plainFlow != gotFlow || plainCost != gotCost {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"MinCostMaxFlow wrapper disagreed with MinCostMaxFlowCtx: plain=(%d,%d) ctx=(%d,%d) (n=%d, src=%d, sink=%d, edges=%s)",
				plainFlow, plainCost, gotFlow, gotCost, n, src, sink, flowFmtCostEdges(edges)),
		})
	}

	// (3) Both numbers must match the independent SPFA-driven SSP reference.
	if gotFlow != refFlow || gotCost != refCost {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"MinCostMaxFlow (flow,cost) diverged from independent SSP reference: got=(%d,%d) ref=(%d,%d) (n=%d, src=%d, sink=%d, edges=%s)",
				gotFlow, gotCost, refFlow, refCost, n, src, sink, flowFmtCostEdges(edges)),
		})
	}

	// (4) The min-cost max-flow VALUE must equal the plain Dinic max-flow on the
	// same capacities (cost ignored): MinCostMaxFlow ships the maximum flow.
	capEdges := make([]flowEdge, len(edges))
	for i, e := range edges {
		capEdges[i] = flowEdge{src: e.src, dst: e.dst, cap: e.cap}
	}
	dinic := flow.MaxFlow(flowBuildNetwork(n, capEdges), src, sink)
	if gotFlow != dinic {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"MinCostMaxFlow value %d != Dinic max-flow %d on the same capacity network (n=%d, edges=%s)",
				gotFlow, dinic, n, flowFmtCostEdges(edges)),
		})
	}

	// (5) Optimality certificate on the REFERENCE's own final residual.
	if flowResidualHasNegativeCycle(refResidual) {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"reference min-cost max-flow failed its optimality certificate: its final residual graph contains a negative-cost cycle, so the flow of value %d at cost %d is NOT min-cost (n=%d, src=%d, sink=%d, edges=%s)",
				refFlow, refCost, n, src, sink, flowFmtCostEdges(edges)),
		})
	}

	return out
}

// flowFirstNonDAGArc returns the index of the first arc that breaks the
// generator's structural invariant src < dst, or -1 when every arc respects it.
// The invariant is what makes the reference's DAG precondition structural rather
// than lucky: see the DAG invariant section on [flowGenCostNetwork].
func flowFirstNonDAGArc(edges []flowCostEdge) int {
	for i, e := range edges {
		if e.src >= e.dst {
			return i
		}
	}
	return -1
}

// flowNegCycleFixture is one hand-built network that plants a negative-cost
// cycle in the INITIAL residual graph, so search/flow's Bellman-Ford bootstrap
// must refuse it with [flow.ErrNegativeCycle].
type flowNegCycleFixture struct {
	name  string
	edges []flowCostEdge
	n     int
	src   int
	sink  int
	// dinic is the hand-computed plain max-flow on the SAME capacities with the
	// costs ignored. It is what makes the (0,0) assertions evidential instead of
	// vacuous: it proves the (0,0) is a REFUSAL and not simply "there was no
	// augmenting path to begin with".
	dinic int
}

// flowNegCycleFixtures returns the planted-negative-cycle fixtures. They are
// hand-built constants rather than seed-derived, and they must never reach
// [flowRefMinCostMaxFlow]: each contains a backward arc, so the SPFA reference
// would spin on them (see [errFlowRefRelaxBudget]). They therefore travel a
// separate checker path, [flowCheckNegCycleFixture], which compares against
// hand-computed values only.
//
// Both fixtures are verified by hand:
//
//	(a) "disjoint-cycle" — n=4, src=0, sink=3
//	    0->1 c4 $2 ; 1->2 c4 $-5 ; 2->1 c4 $1 ; 0->3 c4 $1
//	    The cycle 1->2->1 is worth -4 and is reachable from src while touching
//	    NEITHER endpoint. Arc 0->3 is load-bearing: without it the network has no
//	    src->sink path at all, (0,0) would be indistinguishable from "no
//	    augmenting path", and the assertion would be vacuous. With it, plain
//	    Dinic ships 4 units.
//	(b) "cycle-through-src" — n=2, src=0, sink=1
//	    0->1 c1 $-3 ; 1->0 c1 $1
//	    The cycle 0->1->0 is worth -2 and CONTAINS src, matching the wording of
//	    [flow.ErrNegativeCycle]'s own documentation. Plain Dinic ships 1 unit.
//
// Neither the cap=0 paired reverse arcs nor search/flow's validation can
// manufacture a false positive here. validateCostCapacities constrains only
// capacity magnitudes — never cost sign, never cycles — and runs BEFORE the
// bootstrap, so it cannot mask ErrNegativeCycle; bellmanFordBootstrap skips
// cap<=0 arcs in both its relaxation loop and its detection pass; and
// hasNegativeCost requires cap>0 && cost<0, which no reverse arc satisfies at
// rest.
func flowNegCycleFixtures() []flowNegCycleFixture {
	return []flowNegCycleFixture{
		{
			name:  "disjoint-cycle",
			n:     4,
			src:   0,
			sink:  3,
			dinic: 4,
			edges: []flowCostEdge{
				{src: 0, dst: 1, cap: 4, cost: 2},
				{src: 1, dst: 2, cap: 4, cost: -5},
				{src: 2, dst: 1, cap: 4, cost: 1},
				{src: 0, dst: 3, cap: 4, cost: 1},
			},
		},
		{
			name:  "cycle-through-src",
			n:     2,
			src:   0,
			sink:  1,
			dinic: 1,
			edges: []flowCostEdge{
				{src: 0, dst: 1, cap: 1, cost: -3},
				{src: 1, dst: 0, cap: 1, cost: 1},
			},
		},
	}
}

// flowCheckNegativeCycle drives every planted-negative-cycle fixture for one
// tick. The fixtures are constants, so this consumes no seed draws and cannot
// perturb the tick's stream.
func flowCheckNegativeCycle(tick int64) []Violation {
	fixtures := flowNegCycleFixtures()
	out := make([]Violation, 0, len(fixtures))
	for i := range fixtures {
		out = append(out, flowCheckNegCycleFixture(tick, fixtures[i])...)
	}
	return out
}

// flowCheckNegCycleFixture asserts the negative-cycle contract on one planted
// fixture:
//
//   - [flow.MinCostMaxFlowCtx] returns (0, 0) together with an error that
//     satisfies errors.Is(err, [flow.ErrNegativeCycle]);
//   - the non-context [flow.MinCostMaxFlow] returns exactly (0, 0) — the
//     documented behaviour of the wrapper that discards the error;
//   - NON-VACUITY: plain Dinic on the SAME capacities ships f.dinic units, which
//     is non-zero. Without this last clause the (0,0) assertions could be
//     satisfied by a network that simply had no augmenting path, and an oracle
//     that cannot fail proves nothing.
func flowCheckNegCycleFixture(tick int64, f flowNegCycleFixture) []Violation {
	op := "search:MinCostMaxFlow/negcycle:" + f.name
	var out []Violation

	gotFlow, gotCost, err := flow.MinCostMaxFlowCtx(
		context.Background(), flowBuildCostNetwork(f.n, f.edges), f.src, f.sink)
	if !errors.Is(err, flow.ErrNegativeCycle) {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"MinCostMaxFlowCtx did not report ErrNegativeCycle on a planted negative cycle: err=%v got=(%d,%d) (n=%d, src=%d, sink=%d, edges=%s)",
				err, gotFlow, gotCost, f.n, f.src, f.sink, flowFmtCostEdges(f.edges)),
		})
	}
	if gotFlow != 0 || gotCost != 0 {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"MinCostMaxFlowCtx augmented a network holding a negative cycle: got=(%d,%d), want (0,0) (n=%d, src=%d, sink=%d, edges=%s)",
				gotFlow, gotCost, f.n, f.src, f.sink, flowFmtCostEdges(f.edges)),
		})
	}

	plainFlow, plainCost := flow.MinCostMaxFlow(flowBuildCostNetwork(f.n, f.edges), f.src, f.sink)
	if plainFlow != 0 || plainCost != 0 {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"MinCostMaxFlow wrapper returned %v on a negative cycle, want (0,0) (n=%d, src=%d, sink=%d, edges=%s)",
				[2]int{plainFlow, plainCost}, f.n, f.src, f.sink, flowFmtCostEdges(f.edges)),
		})
	}

	// NON-VACUITY: the same capacities must carry a non-zero plain max-flow, so
	// the (0,0) above is a refusal rather than an empty network.
	capEdges := make([]flowEdge, len(f.edges))
	for i, e := range f.edges {
		capEdges[i] = flowEdge{src: e.src, dst: e.dst, cap: e.cap}
	}
	if d := flow.MaxFlow(flowBuildNetwork(f.n, capEdges), f.src, f.sink); d != f.dinic {
		out = append(out, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: op,
			Message: fmt.Sprintf(
				"planted-negative-cycle fixture lost its non-vacuity witness: Dinic max-flow on the same capacities = %d, want %d (n=%d, src=%d, sink=%d, edges=%s)",
				d, f.dinic, f.n, f.src, f.sink, flowFmtCostEdges(f.edges)),
		})
	}
	return out
}

// flowGenCostNetwork derives a directed capacity+cost network from seed: n nodes
// (6..10), src=0, sink=n-1, a connected forward spine guaranteeing a positive
// flow, plus seed-chosen extra forward arcs. Every arc carries a capacity in
// [1, flowMaxCap]. Arcs are emitted in a fixed index-ordered sequence, so the
// output never depends on map iteration order.
//
// # Cost flavour
//
// negative=false emits every cost in [1, flowMaxCost]. This is the checker's
// original behaviour and the ZERO-POTENTIAL FAST PATH: search/flow's
// hasNegativeCost is false, so the Bellman-Ford bootstrap is skipped entirely.
//
// negative=true draws each cost from the SYMMETRIC interval
// [-flowMaxCost, +flowMaxCost] and FORCES the first spine arc (0->1) strictly
// negative, in [-flowMaxCost, -1]. Forcing is required, not cosmetic: with an
// unforced symmetric draw, 80 of 20,000 measured fixtures contained no negative
// arc at all, so hasNegativeCost would not fire and the bootstrap would not run.
// Forcing gives 20,000 of 20,000. The symmetric interval is chosen because it
// makes roughly 48% of the costs it draws negative (10 of its 21 values), which
// produces multi-arc negative shortest-path trees rather than the trivially-right
// single-negative-arc case; counting the always-negative forced arc as well,
// 52.4% of all arcs on this flavour are negative in a measured 5,000-tick sweep.
// It also INCLUDES 0, which lands a reduced cost of exactly 0 on the boundary of
// the production rc<0 guard — 9,472 zero-cost arcs were measured across a 20,000-
// fixture forced sweep, and 4,743 across the 10,000-fixture sweep the tests run.
//
// The magnitude is deliberately not widened. The source cut is at most 260 (one
// spine arc plus at most n+2 extra arcs out of node 0, each capped by
// flowMaxCap) and the largest absolute cost is flowMaxCost, so their product
// stays seven orders of magnitude below search/flow's capInf: ErrCapacityOverflow
// can never be entered by accident, and a reported fixture stays hand-
// reproducible from [flowFmtCostEdges], which already renders a negative cost as
// e.g. "$-7".
//
// # Draw count
//
// Every flavour spends EXACTLY ONE IntN draw per cost (see [flowDrawCost]),
// identical to the original 1+IntN(flowMaxCost). The per-fixture draw count is
// therefore unchanged and the flavours stay in lockstep on the shared per-tick
// Seed. Preserving that is load-bearing for determinism, and it is also what
// keeps the all-positive fixtures bit-identical to what they were before the
// negative-cost flavour existed.
//
// # DAG invariant
//
// Every arc runs strictly from a low index to a high index: the spine is
// i -> i+1, and every extra arc is normalised by swapping its endpoints. Under
// purely positive costs that was a convenience. Under negative costs it is a
// PRECONDITION, in two distinct ways:
//
//   - SOUNDNESS. The SPFA reference is a valid min-cost oracle only if the zero
//     flow is min-cost of value 0, which (Ahuja-Magnanti-Orlin Thm 9.1) holds
//     iff the arc set with cap>=1 has no negative-cost directed cycle. Because
//     every arc goes low -> high, the generated network has no directed cycle of
//     ANY sign, so the condition holds structurally rather than by luck. It keeps
//     holding after every augmentation: augmenting along a shortest path leaves
//     every residual reduced cost >= 0, and potentials telescope to 0 around any
//     cycle — a path arc is tight, so the reverse arc it newly opens carries a
//     reduced cost of exactly 0 and the resulting 2-cycle is worth 0, never
//     negative.
//   - LIVENESS. SPFA has no negative-cycle detector; on a negative cycle it hangs
//     rather than fails. [flowRefMinCostMaxFlow]'s relaxation budget is the
//     second line of defence, and [flowFirstNonDAGArc] asserts the invariant
//     itself before the reference is ever called.
func flowGenCostNetwork(seed *Seed, negative bool) (int, []flowCostEdge) {
	n := 6 + seed.IntN(5) // 6..10
	edges := make([]flowCostEdge, 0, n*2)
	for i := 0; i < n-1; i++ {
		edges = append(edges, flowCostEdge{
			src: i, dst: i + 1,
			cap: 1 + seed.IntN(flowMaxCap),
			// The FIRST spine arc is forced strictly negative on the negative
			// flavour, so hasNegativeCost is guaranteed to fire and the
			// Bellman-Ford bootstrap is guaranteed to run.
			cost: flowDrawCost(seed, negative, negative && i == 0),
		})
	}
	extra := seed.IntN(n + 2)
	for k := 0; k < extra; k++ {
		a := seed.IntN(n)
		b := seed.IntN(n)
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		edges = append(edges, flowCostEdge{
			src: a, dst: b,
			cap:  1 + seed.IntN(flowMaxCap),
			cost: flowDrawCost(seed, negative, false),
		})
	}
	return n, edges
}

// flowDrawCost draws EXACTLY ONE integer from seed and maps it into this
// fixture's cost interval:
//
//	forceNegative : [-flowMaxCost, -1]           strictly negative
//	negative      : [-flowMaxCost, +flowMaxCost] symmetric, includes 0
//	otherwise     : [1, flowMaxCost]             strictly positive
//
// Holding every flavour to a single IntN call is what makes the positive and
// negative flavours consume the same number of draws, so changing a fixture's
// flavour cannot shift the rest of the tick's stream. Each branch performs its
// own single draw; never compute one interval and then overwrite it with
// another, because that would spend two draws.
func flowDrawCost(seed *Seed, negative, forceNegative bool) int {
	switch {
	case forceNegative:
		return -1 - seed.IntN(flowMaxCost)
	case negative:
		return -flowMaxCost + seed.IntN(2*flowMaxCost+1)
	default:
		return 1 + seed.IntN(flowMaxCost)
	}
}

// flowBuildCostNetwork constructs a fresh flow.CostNetwork for one algorithm
// call (the routines mutate residual capacities in place, so each call needs a
// clean network).
func flowBuildCostNetwork(n int, edges []flowCostEdge) *flow.CostNetwork {
	g := flow.NewCostNetwork(n)
	for _, e := range edges {
		g.AddCostEdge(e.src, e.dst, e.cap, e.cost)
	}
	return g
}

// flowRefBuildResidual lays out the reference's residual graph: one forward arc
// per generated edge (cap = capacity, cost = cost) paired with a reverse arc
// (cap = 0, cost = -cost). The reverse cost is the NEGATION of the forward cost,
// which is already correct when the forward cost is itself negative — cancelling
// a unit of flow refunds what it cost — so no change was needed here to support
// the negative-cost flavour.
func flowRefBuildResidual(n int, edges []flowCostEdge) [][]flowResidualArc {
	adj := make([][]flowResidualArc, n)
	for _, e := range edges {
		u, v := e.src, e.dst
		adj[u] = append(adj[u], flowResidualArc{to: v, cap: e.cap, cost: e.cost, rev: len(adj[v])})
		adj[v] = append(adj[v], flowResidualArc{to: u, cap: 0, cost: -e.cost, rev: len(adj[u]) - 1})
	}
	return adj
}

// flowRefMinCostMaxFlow is the independent reference for the min-cost-max-flow
// fixtures. It runs Successive Shortest Paths where each augmenting path is the
// CHEAPEST (minimum total cost) src->sink path with positive residual capacity,
// found by a Bellman-Ford / SPFA relaxation on its own residual arrays — sharing
// no code with the production Dijkstra-with-potentials implementation. Because
// every augmentation is along a globally cheapest residual path, the loop
// terminates at the maximum flow whose cost is minimal (the SSP optimality
// theorem). Capacities and costs are small integers, so the arithmetic is exact.
//
// SSP is a valid oracle here only because the zero flow is min-cost of value 0,
// which holds because the generated arc set is acyclic; see the DAG invariant
// section on [flowGenCostNetwork]. Negative arc costs are otherwise handled with
// no special case: SPFA relaxes them directly, and dist[sink] is simply allowed
// to be negative.
//
// It returns its FINAL residual arrays alongside the answer so the caller can
// run the optimality certificate ([flowResidualHasNegativeCycle]) on them, and
// it returns [errFlowRefRelaxBudget] rather than spinning if a single SPFA
// exceeds (n+1)*arcs successful relaxations. That bound cannot be reached on a
// graph free of negative cycles: SPFA settles each node at most n-1 times, so
// its successful relaxations are at most (n-1)*arcs.
func flowRefMinCostMaxFlow(n int, edges []flowCostEdge, src, sink int) (maxFlow, minCost int, residual [][]flowResidualArc, err error) {
	adj := flowRefBuildResidual(n, edges)
	arcs := 0
	for u := range adj {
		arcs += len(adj[u])
	}
	budget := (n + 1) * arcs

	const inf = flowCapInf
	for {
		// SPFA: shortest-cost path from src over positive-residual arcs.
		dist := make([]int, n)
		inQueue := make([]bool, n)
		parentNode := make([]int, n)
		parentArc := make([]int, n)
		for i := range dist {
			dist[i] = inf
			parentNode[i] = -1
		}
		dist[src] = 0
		queue := []int{src}
		inQueue[src] = true
		relaxations := 0
		for len(queue) > 0 {
			u := queue[0]
			queue = queue[1:]
			inQueue[u] = false
			du := dist[u]
			for ai := range adj[u] {
				a := adj[u][ai]
				if a.cap <= 0 {
					continue
				}
				if cand := du + a.cost; cand < dist[a.to] {
					relaxations++
					if relaxations > budget {
						return maxFlow, minCost, adj, errFlowRefRelaxBudget
					}
					dist[a.to] = cand
					parentNode[a.to] = u
					parentArc[a.to] = ai
					if !inQueue[a.to] {
						inQueue[a.to] = true
						queue = append(queue, a.to)
					}
				}
			}
		}
		if dist[sink] >= inf {
			break // no augmenting path remains
		}
		// Bottleneck along the cheapest path.
		push := inf
		for v := sink; v != src; v = parentNode[v] {
			if a := adj[parentNode[v]][parentArc[v]]; a.cap < push {
				push = a.cap
			}
		}
		// Apply the push: decrement forward residual, increment paired reverse.
		for v := sink; v != src; v = parentNode[v] {
			u := parentNode[v]
			ai := parentArc[v]
			adj[u][ai].cap -= push
			adj[v][adj[u][ai].rev].cap += push
		}
		maxFlow += push
		minCost += push * dist[sink]
	}
	return maxFlow, minCost, adj, nil
}

// flowResidualHasNegativeCycle reports whether the residual graph — its arcs
// with cap > 0 — contains a negative-cost directed cycle. By
// Ahuja-Magnanti-Orlin Thm 9.1 a flow is min-cost for its own value iff its
// residual graph has no negative cycle, so a false verdict on a max-flow's final
// residual is an OPTIMALITY CERTIFICATE that shares no machinery with the
// shortest-path search that produced the flow.
//
// Two details are load-bearing and were both got wrong before they were got
// right:
//
//   - dist starts ALL-ZERO, which is Bellman-Ford from a virtual super-source
//     joined to every node by a zero-cost arc. That is what makes cycles
//     unreachable from src detectable too; seeding only src would miss them.
//   - the relaxation rounds and the detection pass are SEPARATE. With the
//     virtual super-source the graph has n+1 nodes, so a shortest simple path
//     spans at most n arcs and n full rounds are needed; "something still changed
//     on the last round" is NOT detection, because a legitimate late improvement
//     is indistinguishable from an unbounded one. Only a further improvement
//     found AFTER convergence proves a negative cycle.
//
// Cost is negligible at fixture scale: n <= 10 and roughly 30 arcs give at most
// a few hundred integer relaxations.
func flowResidualHasNegativeCycle(adj [][]flowResidualArc) bool {
	n := len(adj)
	dist := make([]int, n) // all-zero: virtual super-source over every node
	for round := 0; round < n; round++ {
		changed := false
		for u := 0; u < n; u++ {
			for ai := range adj[u] {
				a := adj[u][ai]
				if a.cap <= 0 {
					continue
				}
				if cand := dist[u] + a.cost; cand < dist[a.to] {
					dist[a.to] = cand
					changed = true
				}
			}
		}
		if !changed {
			// Converged: no further improvement is possible, so the detection
			// pass below cannot find one either.
			break
		}
	}
	// Separate detection pass: any improvement still available after n full
	// rounds can only come from a negative-cost cycle.
	for u := 0; u < n; u++ {
		for ai := range adj[u] {
			a := adj[u][ai]
			if a.cap <= 0 {
				continue
			}
			if dist[u]+a.cost < dist[a.to] {
				return true
			}
		}
	}
	return false
}

// flowFmtCostEdges renders a directed capacity+cost edge list deterministically
// (input order) for a violation message.
func flowFmtCostEdges(edges []flowCostEdge) string {
	s := "["
	for i, e := range edges {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%d->%d:c%d$%d", e.src, e.dst, e.cap, e.cost)
	}
	return s + "]"
}

// flowFmtEdges renders a directed edge list deterministically (input order) for
// a violation message, so a reported divergence can be reproduced exactly.
func flowFmtEdges(edges []flowEdge) string {
	s := "["
	for i, e := range edges {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%d->%d:%d", e.src, e.dst, e.cap)
	}
	return s + "]"
}

// flowFmtMatrix renders the upper triangle of a symmetric weight matrix
// deterministically (row-major i<j) for a violation message.
func flowFmtMatrix(w []int, n int) string {
	s := "["
	first := true
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if v := w[i*n+j]; v != 0 {
				if !first {
					s += " "
				}
				s += fmt.Sprintf("%d-%d:%d", i, j, v)
				first = false
			}
		}
	}
	return s + "]"
}
