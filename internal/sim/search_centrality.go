package sim

import (
	"errors"
	"fmt"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// centralitySeedSalt is XORed into the tick before deriving this checker's Seed
// so the CENTRALITY battery draws an independent random stream from the other
// per-tick search checks (which salt the same tick with their own distinct
// constants). Sharing the raw tick would couple the fixtures of unrelated
// checks.
const centralitySeedSalt uint64 = 0xc3a7_1d05_9e6b_2f84

// centralityWorkers is the PINNED Brandes worker count the battery drives the
// parallel betweenness functions with. Pinning it makes the parallel float
// reduction reproducible for a given fixture (the library documents that its
// output is deterministic for a fixed numWorkers), and exercising numWorkers > 1
// covers the cross-source parallel reduction path rather than only the serial
// fallback.
const centralityWorkers = 4

// centralityAbsEps and centralityRelEps bound the disagreement the battery
// tolerates between the library's betweenness and the independent reference.
// The library's godoc states the parallel result may differ from the serial one
// by up to ~1e-12 per node because IEEE-754 addition is non-associative and the
// parallel path re-associates the cross-source dependency sum; the reference
// accumulates in yet another order again. The fixtures are tiny (n <= 12) so the
// true values are exactly representable small rationals, and a combined
// absolute+relative tolerance of 1e-9 sits far above the float-reassociation
// noise (~1e-12) yet far below the smallest meaningful gap between distinct
// betweenness values on these shapes (which differ by >= 0.5). Comparing with a
// tolerance — never exact equality — is mandatory: a parallel float reduction is
// not bit-identical to any single-threaded reference.
const (
	centralityAbsEps = 1e-9
	centralityRelEps = 1e-9
)

// centralityFixture is one deterministic graph the CENTRALITY battery probes. It
// carries the integer-NodeID adjacency the CSR builder consumes (already
// symmetrised for undirected fixtures, since the library treats the CSR as a
// directed adjacency and an undirected edge is two directed arcs), a parallel
// per-arc weight slice for the weighted check (positive, finite), a human label
// for diagnostics, and whether the graph is undirected (purely informational —
// the arcs are already materialised in both directions when so).
type centralityFixture struct {
	name     string
	order    int // number of distinct NodeIDs, 0..order-1
	arcs     []centralityArc
	directed bool
}

// centralityArc is a directed arc (Src -> Dst) with a strictly-positive weight,
// expressed in the integer NodeID space the CSR builder consumes. For an
// undirected fixture both arcs (u->v and v->u) appear, carrying the same weight.
type centralityArc struct {
	Src    graph.NodeID
	Dst    graph.NodeID
	Weight float64
}

// centralityViolations runs the CENTRALITY (betweenness) correctness battery for
// one simulation tick. It builds a fixed set of small deterministic graphs
// (n <= 12) plus one seed-derived bridged graph, and for each one cross-checks
// the library's parallel betweenness against an INDEPENDENT reference computed
// directly from the betweenness definition (Brandes' single-source dependency
// accumulation re-derived here, not the library's code):
//
//   - Unweighted: [centrality.BetweennessParallel] with a pinned worker count is
//     compared against centralityUnweightedReference, which runs a BFS from
//     every source to obtain shortest-path distances and counts sigma, then
//     accumulates the dependency of every vertex.
//   - Weighted: when the fixture's arc weights are all positive and finite,
//     [centrality.WeightedBetweennessParallel] is compared against
//     centralityWeightedReference, which runs Dijkstra from every source for the
//     same accumulation.
//
// Normalisation convention. The library returns the UNNORMALISED betweenness and
// accumulates cb[v] += delta_s(v) for EVERY source s in [0, n) (its source loop
// runs over all vertices and never halves the result for undirected input). The
// reference mirrors this exactly: it sums the dependency over all ORDERED source
// vertices s, so an undirected pair {s,t} is counted once as (s,t) and once as
// (t,s). No extra division or doubling is applied on either side, so the two are
// directly comparable.
//
// Each node's value is compared with a combined absolute+relative epsilon, never
// exact equality, because the parallel float reduction is not bit-identical to
// the reference's accumulation order. All randomness flows from a single Seed
// derived from tick, no map-iteration order influences any output, and the
// fixture count is fixed, so a violation at a given tick is reproducible
// bit-for-bit. A clean run returns nil; divergences are returned tagged
// ViolationSearchDivergence with Op "search:BetweennessParallel" or
// "search:WeightedBetweennessParallel".
func centralityViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ centralitySeedSalt)

	var vs []Violation
	for _, f := range centralityFixtures(seed) {
		c := centralityBuildCSR(f)

		// --- Unweighted betweenness ---
		got := centrality.BetweennessParallel[float64](c, centralityWorkers)
		want := centralityUnweightedReference(f)
		vs = append(vs, centralityCompare(tick, "search:BetweennessParallel", f, want, got)...)

		// --- Weighted betweenness ---
		// The library validates weights at its boundary (NaN/Inf -> ErrInvalidInput,
		// non-positive -> ErrNonPositiveWeight). Every fixture is built with finite,
		// strictly-positive weights, so a returned error is itself a divergence: the
		// well-formed input must be accepted.
		gotW, err := centrality.WeightedBetweennessParallel(c, centralityWorkers)
		if err != nil {
			vs = append(vs, centralityDiverge(tick, "search:WeightedBetweennessParallel", fmt.Sprintf(
				"%s: WeightedBetweennessParallel rejected a well-formed positive-weight graph: %v (ErrInvalidInput=%v, ErrNonPositiveWeight=%v)",
				f.name, err, errors.Is(err, centrality.ErrInvalidInput), errors.Is(err, centrality.ErrNonPositiveWeight)))...)
		} else {
			wantW := centralityWeightedReference(f)
			vs = append(vs, centralityCompare(tick, "search:WeightedBetweennessParallel", f, wantW, gotW)...)
		}
	}
	return vs
}

// centralityFixtures returns the deterministic battery for one tick: a fixed set
// of small shapes whose betweenness is well understood (an isolated graph with
// no shortest paths, a path, a star, an undirected and a directed bridged
// two-cluster graph, a complete graph, a directed acyclic diamond) plus one
// seed-derived bridged graph. The order is fixed and independent of any map
// iteration, so the draw stream — and therefore the whole battery — is
// reproducible from the seed alone. Only the final fixture consumes random
// draws.
func centralityFixtures(seed *Seed) []centralityFixture {
	return []centralityFixture{
		centralityIsolated("isolated-no-edges", 4),
		centralityPath("path5", 5),
		centralityStar("star6", 6),
		centralityBridgedClusters("bridged-clusters-3-3"),
		centralityDirectedChain("directed-chain5", 5),
		centralityCompleteUndirected("complete-k4", 4),
		centralityDirectedDiamond("directed-diamond"),
		centralityRandomBridged(seed),
	}
}

// --- Fixed fixtures ------------------------------------------------------------

// centralityIsolated builds n vertices with no arcs. No vertex lies on any
// shortest path, so every betweenness score is exactly 0 — a baseline that
// catches a reference or library that fabricates flow on an empty graph.
func centralityIsolated(name string, n int) centralityFixture {
	return centralityFixture{name: name, order: n, arcs: nil}
}

// centralityPath builds the undirected path 0-1-2-...-(n-1) with unit weights.
// Interior vertices carry strictly increasing-then-decreasing betweenness; the
// two endpoints carry 0. For n=5 the unnormalised ordered-pair scores are
// [0, 6, 8, 6, 0] (vertex i lies between the i*(n-1-i) ordered front pairs in
// each of the two directions: 2*i*(n-1-i)). n must be >= 2.
func centralityPath(name string, n int) centralityFixture {
	arcs := make([]centralityArc, 0, 2*(n-1))
	for i := 0; i < n-1; i++ {
		arcs = append(arcs,
			centralityArc{Src: graph.NodeID(i), Dst: graph.NodeID(i + 1), Weight: 1},
			centralityArc{Src: graph.NodeID(i + 1), Dst: graph.NodeID(i), Weight: 1},
		)
	}
	return centralityFixture{name: name, order: n, arcs: arcs}
}

// centralityStar builds the undirected star with hub 0 and leaves 1..n-1, unit
// weights. The hub lies on the unique shortest path between every pair of
// distinct leaves; each leaf lies on none. For n leaves+hub the hub's
// unnormalised ordered-pair score is (n-1)*(n-2) and every leaf is 0. n must be
// >= 2.
func centralityStar(name string, n int) centralityFixture {
	arcs := make([]centralityArc, 0, 2*(n-1))
	for i := 1; i < n; i++ {
		arcs = append(arcs,
			centralityArc{Src: 0, Dst: graph.NodeID(i), Weight: 1},
			centralityArc{Src: graph.NodeID(i), Dst: 0, Weight: 1},
		)
	}
	return centralityFixture{name: name, order: n, arcs: arcs}
}

// centralityBridgedClusters builds two triangles {0,1,2} and {3,4,5} joined by a
// single bridge edge 2-3, undirected, unit weights. The bridge endpoints 2 and 3
// are articulation points carrying high betweenness (every cross-cluster
// shortest path traverses the bridge), giving the non-trivial articulated
// structure the brief asks for.
func centralityBridgedClusters(name string) centralityFixture {
	und := func(a, b int) []centralityArc {
		return []centralityArc{
			{Src: graph.NodeID(a), Dst: graph.NodeID(b), Weight: 1},
			{Src: graph.NodeID(b), Dst: graph.NodeID(a), Weight: 1},
		}
	}
	// Triangle A: 0-1-2.
	arcs := und(0, 1)
	arcs = append(arcs, und(1, 2)...)
	arcs = append(arcs, und(0, 2)...)
	// Triangle B: 3-4-5.
	arcs = append(arcs, und(3, 4)...)
	arcs = append(arcs, und(4, 5)...)
	arcs = append(arcs, und(3, 5)...)
	// Bridge.
	arcs = append(arcs, und(2, 3)...)
	return centralityFixture{name: name, order: 6, arcs: arcs}
}

// centralityDirectedChain builds the directed chain 0->1->2->...->(n-1) with
// unit weights (NOT symmetrised). Only forward shortest paths exist, so each
// interior vertex i lies on the i*(n-1-i) ordered source-target pairs whose path
// runs forward through it — exactly half the undirected path's score because the
// reverse direction is unreachable. n must be >= 2.
func centralityDirectedChain(name string, n int) centralityFixture {
	arcs := make([]centralityArc, 0, n-1)
	for i := 0; i < n-1; i++ {
		arcs = append(arcs, centralityArc{Src: graph.NodeID(i), Dst: graph.NodeID(i + 1), Weight: 1})
	}
	return centralityFixture{name: name, order: n, directed: true, arcs: arcs}
}

// centralityCompleteUndirected builds the complete graph K_n, undirected, unit
// weights. Every pair is directly adjacent, so no vertex lies strictly between
// any other pair: every betweenness score is exactly 0. This catches a reference
// that miscounts when many equal-length shortest paths exist (here every
// shortest path has length 1 and passes through no intermediate vertex). n must
// be >= 2.
func centralityCompleteUndirected(name string, n int) centralityFixture {
	var arcs []centralityArc
	for a := 0; a < n; a++ {
		for b := a + 1; b < n; b++ {
			arcs = append(arcs,
				centralityArc{Src: graph.NodeID(a), Dst: graph.NodeID(b), Weight: 1},
				centralityArc{Src: graph.NodeID(b), Dst: graph.NodeID(a), Weight: 1},
			)
		}
	}
	return centralityFixture{name: name, order: n, arcs: arcs}
}

// centralityDirectedDiamond builds the directed acyclic "diamond"
// 0->1, 0->2, 1->3, 2->3 with unit weights. Between source 0 and sink 3 there
// are TWO shortest paths of length 2 (via 1 and via 2), so vertices 1 and 2 each
// carry the fractional dependency 1/2 from the single ordered pair (0,3). It
// exercises the sigma > 1 (multiple shortest paths) accumulation specifically.
func centralityDirectedDiamond(name string) centralityFixture {
	arcs := []centralityArc{
		{Src: 0, Dst: 1, Weight: 1},
		{Src: 0, Dst: 2, Weight: 1},
		{Src: 1, Dst: 3, Weight: 1},
		{Src: 2, Dst: 3, Weight: 1},
	}
	return centralityFixture{name: name, order: 4, directed: true, arcs: arcs}
}

// --- Seed-derived fixture ------------------------------------------------------

// centralityRandomBridged builds two cliques of seed-chosen sizes joined by a
// single bridge, undirected, with seed-chosen small integer weights on every
// edge. Both bridge endpoints are articulation points, so the graph always has
// the non-trivial articulated structure the brief requires while varying tick to
// tick. The total order is capped at 12. Weights are drawn in [1, 4] so the
// weighted shortest paths are non-degenerate yet the values stay exactly
// representable.
func centralityRandomBridged(seed *Seed) centralityFixture {
	// Two cliques of size in [2, 5]; total order = a+b <= 10 (< 12 cap).
	a := 2 + seed.IntN(4) // [2,5]
	b := 2 + seed.IntN(4) // [2,5]
	order := a + b

	// Deterministic per-edge weight in [1,4]. The draw order is fixed by the
	// nested loops below, so the whole fixture replays from the seed.
	weight := func() float64 { return float64(1 + seed.IntN(4)) }

	und := func(u, v int, w float64) []centralityArc {
		return []centralityArc{
			{Src: graph.NodeID(u), Dst: graph.NodeID(v), Weight: w},
			{Src: graph.NodeID(v), Dst: graph.NodeID(u), Weight: w},
		}
	}

	var arcs []centralityArc
	// Clique A over [0, a).
	for u := 0; u < a; u++ {
		for v := u + 1; v < a; v++ {
			arcs = append(arcs, und(u, v, weight())...)
		}
	}
	// Clique B over [a, a+b).
	for u := a; u < order; u++ {
		for v := u + 1; v < order; v++ {
			arcs = append(arcs, und(u, v, weight())...)
		}
	}
	// Bridge: last vertex of A (a-1) to first of B (a).
	arcs = append(arcs, und(a-1, a, weight())...)

	return centralityFixture{name: fmt.Sprintf("random-bridged-%d-%d", a, b), order: order, arcs: arcs}
}

// --- CSR construction ----------------------------------------------------------

// centralityBuildCSR materialises f as an immutable directed CSR[float64],
// carrying the arc weights in the parallel weights slice for the weighted check.
//
// It computes the length-(order+1) offsets array and the source-grouped flat
// edge+weight arrays exactly as csr.BuildFromAdjList would, via a counting pass
// over the out-degrees followed by a scatter. Building the offsets
// programmatically — rather than hand-writing them per fixture — eliminates the
// off-by-one class of bug a sink vertex (no out-edges) is especially prone to:
// every vertex, including a pure sink, must own an offset slot or the
// in-degree/source loops index out of range. order must be strictly greater than
// every NodeID that appears as a source OR a destination.
func centralityBuildCSR(f centralityFixture) *csr.CSR[float64] {
	order := f.order
	vertices := make([]uint64, order+1)
	for _, a := range f.arcs {
		vertices[int(a.Src)+1]++ // tally out-degree into the next slot
	}
	for i := 1; i <= order; i++ {
		vertices[i] += vertices[i-1] // prefix sum -> offsets
	}
	edges := make([]graph.NodeID, len(f.arcs))
	weights := make([]float64, len(f.arcs))
	cursor := make([]uint64, order)
	for _, a := range f.arcs {
		s := int(a.Src)
		pos := vertices[s] + cursor[s]
		edges[pos] = a.Dst
		weights[pos] = a.Weight
		cursor[s]++
	}
	return csr.FromArrays[float64](vertices, edges, weights, uint64(order), uint64(len(edges)))
}

// --- Independent betweenness references ----------------------------------------
//
// Both references re-derive Brandes' single-source dependency accumulation from
// the betweenness DEFINITION rather than calling the library's code, so they are
// a genuine independent oracle. For every source s they:
//
//  1. compute, for every vertex w, the shortest-path distance d(s,w) and the
//     count sigma(s,w) of distinct shortest paths from s to w, recording the set
//     of shortest-path predecessors of w;
//  2. accumulate the dependency delta_s(v) = sum over w of
//     (sigma(s,v)/sigma(s,w)) * (1 + delta_s(w)) in reverse-distance order,
//     summing over each w that has v as a shortest-path predecessor;
//  3. add delta_s(v) to cb[v] for every v != s.
//
// Summing over all ORDERED sources s reproduces the library's unnormalised
// ordered-pair convention exactly. The unweighted reference orders vertices by
// BFS layer; the weighted reference orders them by Dijkstra settle order. Both
// are O(V * (V + E)) which is trivial at n <= 12.

// centralityAdjacency rebuilds, from a fixture, a plain out-adjacency in NodeID
// order plus the per-arc weight, so the references can traverse without going
// through the CSR (keeping them fully independent of the CSR code path too).
func centralityAdjacency(f centralityFixture) (adj [][]int, w [][]float64) {
	adj = make([][]int, f.order)
	w = make([][]float64, f.order)
	for _, a := range f.arcs {
		s := int(a.Src)
		adj[s] = append(adj[s], int(a.Dst))
		w[s] = append(w[s], a.Weight)
	}
	return adj, w
}

// centralityUnweightedReference computes unnormalised betweenness for every
// vertex using BFS-based single-source dependency accumulation, summed over all
// ordered sources. It treats every arc as unit-length (the unweighted contract).
func centralityUnweightedReference(f centralityFixture) []float64 {
	n := f.order
	adj, _ := centralityAdjacency(f)
	cb := make([]float64, n)

	for s := 0; s < n; s++ {
		dist := make([]int, n)
		sigma := make([]float64, n)
		preds := make([][]int, n)
		for i := range dist {
			dist[i] = -1
		}
		dist[s] = 0
		sigma[s] = 1

		// BFS, recording shortest-path predecessors. The stack records settle
		// order (BFS layer order), so iterating it in reverse visits vertices in
		// non-increasing distance — the order the dependency accumulation needs.
		queue := []int{s}
		var stack []int
		for qh := 0; qh < len(queue); qh++ {
			v := queue[qh]
			stack = append(stack, v)
			for _, w := range adj[v] {
				if dist[w] < 0 {
					dist[w] = dist[v] + 1
					queue = append(queue, w)
				}
				if dist[w] == dist[v]+1 {
					sigma[w] += sigma[v]
					preds[w] = append(preds[w], v)
				}
			}
		}

		centralityAccumulate(stack, preds, sigma, cb, s)
	}
	return cb
}

// centralityWeightedReference computes unnormalised weighted betweenness for
// every vertex using Dijkstra-based single-source dependency accumulation,
// summed over all ordered sources. Weights are strictly positive (the library's
// contract and every fixture's guarantee), so Dijkstra is exact. It uses an
// O(V^2) settle loop (no heap) which is trivially correct at n <= 12 and avoids
// any dependence on the library's priority-queue code.
func centralityWeightedReference(f centralityFixture) []float64 {
	n := f.order
	adj, wts := centralityAdjacency(f)
	cb := make([]float64, n)

	for s := 0; s < n; s++ {
		dist := make([]float64, n)
		sigma := make([]float64, n)
		preds := make([][]int, n)
		settled := make([]bool, n)
		for i := range dist {
			dist[i] = math.Inf(1)
		}
		dist[s] = 0
		sigma[s] = 1

		// settleOrder records vertices in the order Dijkstra finalises them
		// (non-decreasing distance). Reversed, it is the non-increasing-distance
		// order the dependency accumulation requires.
		var settleOrder []int
		for {
			// Select the unsettled vertex of minimum tentative distance. Ties are
			// broken by lowest index, a deterministic rule independent of any map
			// ordering. A vertex unreachable from s (dist == +Inf) is never
			// selected, so it contributes no dependency, matching the library.
			u := -1
			best := math.Inf(1)
			for v := 0; v < n; v++ {
				if !settled[v] && dist[v] < best {
					best = dist[v]
					u = v
				}
			}
			if u < 0 {
				break // all reachable vertices settled
			}
			settled[u] = true
			settleOrder = append(settleOrder, u)
			for i, v := range adj[u] {
				nd := dist[u] + wts[u][i]
				switch {
				case nd < dist[v]-centralityAbsEps:
					// Strictly shorter path: reset v's count and predecessors.
					dist[v] = nd
					sigma[v] = sigma[u]
					preds[v] = []int{u}
				case math.Abs(nd-dist[v]) <= centralityAbsEps:
					// Equal-length alternative shortest path: accumulate.
					sigma[v] += sigma[u]
					preds[v] = append(preds[v], u)
				}
			}
		}

		centralityAccumulate(settleOrder, preds, sigma, cb, s)
	}
	return cb
}

// centralityAccumulate performs Brandes' back-propagation of dependencies for one
// source. order is the per-source settle order (BFS layer or Dijkstra finalise
// order); walking it in reverse visits vertices by non-increasing distance.
// preds[w] holds w's shortest-path predecessors, sigma[w] its shortest-path
// count. It adds the resulting dependency of every vertex v != s into cb.
func centralityAccumulate(order []int, preds [][]int, sigma, cb []float64, s int) {
	delta := make([]float64, len(cb))
	for i := len(order) - 1; i >= 0; i-- {
		w := order[i]
		coef := (1 + delta[w]) / sigma[w]
		for _, v := range preds[w] {
			delta[v] += sigma[v] * coef
		}
		if w != s {
			cb[w] += delta[w]
		}
	}
}

// --- Comparison ----------------------------------------------------------------

// centralityCompare checks the library's per-node betweenness (got) against the
// independent reference (want) for one fixture, returning a violation per node
// whose values disagree beyond the combined absolute+relative epsilon. It first
// guards length parity (a wrong-length result is itself a divergence). Nodes are
// walked in ascending NodeID order so the first reported divergence is
// deterministic.
func centralityCompare(tick int64, op string, f centralityFixture, want, got []float64) []Violation {
	if len(want) != len(got) {
		return centralityDiverge(tick, op, fmt.Sprintf(
			"%s: betweenness result length = %d, want %d (one value per NodeID)", f.name, len(got), len(want)))
	}
	var vs []Violation
	for v := range want {
		if !centralityApproxEqual(want[v], got[v]) {
			vs = append(vs, centralityDiverge(tick, op, fmt.Sprintf(
				"%s: betweenness[%d] = %.17g, reference = %.17g (|diff| = %.3g exceeds abs %g + rel %g)",
				f.name, v, got[v], want[v], math.Abs(got[v]-want[v]), centralityAbsEps, centralityRelEps))...)
		}
	}
	return vs
}

// centralityApproxEqual reports whether a and b agree within the combined
// absolute+relative tolerance. The absolute term covers values near zero (where
// a relative test is meaningless); the relative term scales the tolerance with
// the magnitude of the larger value for the bigger scores on denser fixtures.
func centralityApproxEqual(a, b float64) bool {
	diff := math.Abs(a - b)
	if diff <= centralityAbsEps {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return diff <= centralityRelEps*scale
}

// centralityDiverge builds a single betweenness divergence violation tagged with
// the given op ("search:BetweennessParallel" or
// "search:WeightedBetweennessParallel").
func centralityDiverge(tick int64, op, msg string) []Violation {
	return []Violation{{Kind: ViolationSearchDivergence, Tick: tick, Op: op, Message: msg}}
}
