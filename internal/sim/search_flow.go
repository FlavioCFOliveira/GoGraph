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
//     obtained from the reference max-flow run on the symmetric capacities.
//
// Determinism is load-bearing: every fixture is a pure function of the tick via
// a single Seed draw stream; there is no time, no global rand, and no Go
// map-iteration order in any output path. Only integer capacities/weights are
// used, so every comparison is an exact integer equality with no tolerance.

import (
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
)

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
