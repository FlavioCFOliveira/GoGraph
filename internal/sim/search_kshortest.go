package sim

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// kshortestSeedMix is XORed with the tick to seed this check's [Seed], keeping
// its draw stream disjoint from every other per-tick check in the package (each
// uses its own distinct mix constant, e.g. matchingSeedMix). The value is the
// SECOND splitmix64 finaliser constant, chosen only because it is well-mixed
// and not used elsewhere in this package.
const kshortestSeedMix uint64 = 0xc4ceb9fe1a85ec53

// K-shortest fixture bounds. The graphs are kept tiny (n <= kshortestNMax) so
// the independent brute-force reference — which enumerates EVERY simple src->dst
// path by DFS — stays cheap on every tick, while still admitting many distinct
// src->dst paths (a denser-than-tree edge set guarantees several routes compete
// on cost). Weights are small positive integers so path costs compare EXACTLY.
const (
	kshortestNMin     = 4 // smallest graph: still has multiple 0->n-1 routes
	kshortestNMax     = 8 // largest graph: brute-force simple-path DFS stays cheap
	kshortestWeicMax  = 9 // edge weights drawn from [1, kshortestWeicMax]
	kshortestKMax     = 6 // request up to this many shortest paths
	kshortestFixtures = 4 // independent graphs generated per tick
)

// kshortestMaxPops is the MaxPops budget handed to the bounded loopless entry
// (KShortestPathsLooplessCtxWithOpts). The bare KShortestPathsLoopless is an
// UNBOUNDED-memory / worst-case-exponential DoS vector (its own godoc says so),
// so the checker MUST drive the bounded form. On these tiny graphs the full
// k-shortest enumeration pops far fewer than this many entries, so the budget
// is generous enough never to truncate a clean fixture in practice; the
// truncation path is still handled correctly (see ksp comparison below).
const kshortestMaxPops = 200_000

// kshortestViolations runs the K-SHORTEST-PATHS family on a handful of
// deterministic, seed-derived small weighted DIGRAPHS and cross-checks the
// library against an independent brute-force reference it implements here.
//
// For each fixture (src=0, dst=n-1, integer weights) it:
//
//   - enumerates ALL simple (loopless) src->dst paths by DFS, sums each one's
//     edge weights, sorts the costs ascending, and takes the first k — the
//     reference sorted-cost multiset;
//   - runs search.YenKShortest (loopless) and asserts its sorted COST multiset
//     equals the reference (paths are non-unique, costs are not), then VALIDATES
//     every returned YenPath (real edges, cost == sum of weights, no repeated
//     node, list sorted by cost ascending);
//   - runs the BOUNDED search.KShortestPathsLooplessCtxWithOpts (also loopless)
//     and asserts the same — except that when the MaxPops budget truncates it
//     (ErrResourceBudgetExceeded), only the returned PREFIX is required to be a
//     correct prefix of the reference, never an over- or mis-ordered set;
//   - runs search.EppsteinKShortest, which is a DEPRECATED ALIAS of
//     KShortestPathsLoopless (it forwards verbatim — see search/eppstein.go), so
//     it is LOOPLESS-EQUIVALENT, not loop-allowing: it is therefore compared to
//     the same loopless reference and validated identically.
//
// Each fixture is a pure function of tick (via [NewSeed]), so any divergence is
// reproducible from the tick alone. A clean check returns nil; each divergence
// appends a [ViolationSearchDivergence] (or [ViolationOracleDeviation] for an
// unexpected error) tagged with the algorithm's Op.
func kshortestViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ kshortestSeedMix)
	var vs []Violation
	for f := 0; f < kshortestFixtures; f++ {
		vs = append(vs, kshortestFixtureViolations(tick, seed)...)
	}
	return vs
}

// kshortestFixtureViolations generates one deterministic weighted digraph and
// runs every K-shortest cross-check on it.
func kshortestFixtureViolations(tick int64, seed *Seed) []Violation {
	n := kshortestNMin + seed.IntN(kshortestNMax-kshortestNMin+1)
	k := 1 + seed.IntN(kshortestKMax) // k in [1, kshortestKMax]
	g := kshortestGenGraph(seed, n)
	src, dst := graph.NodeID(0), graph.NodeID(n-1)

	// Reference: ALL simple src->dst path costs, sorted ascending, first k.
	refCosts := kshortestRefSortedCosts(g, 0, n-1, k)

	c := kshortestBuildCSR(g)

	var vs []Violation

	// --- Yen: loopless k-shortest. Compare sorted-cost multiset + validate. ---
	yen := search.YenKShortest(c, src, dst, k)
	vs = append(vs, kshortestCompareExact(tick, "YenKShortest", g, src, dst, k, refCosts, yen)...)
	vs = append(vs, kshortestValidatePaths(tick, "YenKShortest", g, src, dst, yen)...)

	// --- Bounded loopless best-first. Compare with budget-truncation tolerance. ---
	ksp, kspErr := search.KShortestPathsLooplessCtxWithOpts(
		context.Background(), c, src, dst, k,
		search.KShortestPathsLooplessOpts{MaxPops: kshortestMaxPops},
	)
	truncated := errors.Is(kspErr, search.ErrResourceBudgetExceeded)
	if kspErr != nil && !truncated {
		vs = append(vs, kshortestDiverge(tick, "KShortestPathsLoopless",
			fmt.Sprintf("unexpected error on n=%d k=%d: %v", n, k, kspErr))...)
	} else {
		vs = append(vs, kshortestCompareBounded(tick, "KShortestPathsLoopless", g, src, dst, k, refCosts, ksp, truncated)...)
		vs = append(vs, kshortestValidatePaths(tick, "KShortestPathsLoopless", g, src, dst, ksp)...)
	}

	// --- Eppstein: deprecated alias of the loopless entry => loopless-equivalent. ---
	// The DST deliberately exercises this deprecated entry point while it remains
	// in the public API, so a regression in the alias is still caught.
	epp := search.EppsteinKShortest(c, src, dst, k) //nolint:staticcheck // intentionally exercising the deprecated public alias
	vs = append(vs, kshortestCompareExact(tick, "EppsteinKShortest", g, src, dst, k, refCosts, epp)...)
	vs = append(vs, kshortestValidatePaths(tick, "EppsteinKShortest", g, src, dst, epp)...)

	return vs
}

// ---------------------------------------------------------------------------
// Deterministic fixture generation.
// ---------------------------------------------------------------------------

// kshortestEdge is one directed weighted edge in a fixture graph.
type kshortestEdge struct {
	to     int
	weight int
}

// kshortestGraph is a tiny adjacency-list digraph keyed by dense node index
// [0,n). adj[u] is u's out-edges in ascending-target order (built by index, so
// iteration is never map-ordered). It is the single ground-truth structure both
// the CSR builder and the brute-force reference read.
type kshortestGraph struct {
	n   int
	adj [][]kshortestEdge
}

// kshortestGenGraph builds a deterministic weighted digraph on n nodes that is
// GUARANTEED to have multiple distinct src(0)->dst(n-1) paths:
//
//   - a forward "spine" 0->1->...->(n-1) ensures at least one path exists and
//     that dst is reachable from src;
//   - extra forward "skip" edges u->v (u < v) are added with ~1/2 probability,
//     creating alternative, differently-costed routes that compete on cost;
//   - every edge carries a weight in [1, kshortestWeicMax].
//
// All edges go strictly forward (u < v), so the graph is a DAG and every walk is
// automatically loopless — this keeps the brute force a finite DFS while still
// exercising the k-shortest cost ordering across many simple paths. Parallel
// edges are never produced (at most one u->v), matching simple-graph CSR
// expectations. Determinism: every choice draws from seed in a fixed order.
func kshortestGenGraph(seed *Seed, n int) *kshortestGraph {
	g := &kshortestGraph{n: n, adj: make([][]kshortestEdge, n)}
	for u := 0; u < n; u++ {
		// Spine edge u -> u+1 (skipped for the last node).
		if u+1 < n {
			g.adj[u] = append(g.adj[u], kshortestEdge{to: u + 1, weight: kshortestWeight(seed)})
		}
		// Skip edges u -> v for v in [u+2, n) with probability ~1/2 each.
		for v := u + 2; v < n; v++ {
			if seed.IntN(2) == 0 {
				g.adj[u] = append(g.adj[u], kshortestEdge{to: v, weight: kshortestWeight(seed)})
			}
		}
	}
	return g
}

// kshortestWeight draws an edge weight in [1, kshortestWeicMax]. Strictly
// positive so Dijkstra/best-first cost ordering is well-defined and the costs
// are exact integers.
func kshortestWeight(seed *Seed) int { return 1 + seed.IntN(kshortestWeicMax) }

// kshortestBuildCSR assembles the directed CSR the search package expects from a
// fixture graph, by hand (no adjlist dependency). vertices has length n+1;
// vertices[id] is the start of id's out-edges; weights is parallel to edges. The
// edge order within a source is the fixture's ascending-target order.
func kshortestBuildCSR(g *kshortestGraph) *csr.CSR[int] {
	order := uint64(g.n)
	vertices := make([]uint64, order+1)
	var size uint64
	for u := 0; u < g.n; u++ {
		size += uint64(len(g.adj[u]))
	}
	edges := make([]graph.NodeID, 0, size)
	weights := make([]int, 0, size)
	var pos uint64
	for u := 0; u < g.n; u++ {
		vertices[u] = pos
		for _, e := range g.adj[u] {
			edges = append(edges, graph.NodeID(e.to))
			weights = append(weights, e.weight)
			pos++
		}
	}
	vertices[g.n] = pos // terminal offset = total edge count
	return csr.FromArrays[int](vertices, edges, weights, order, pos)
}

// ---------------------------------------------------------------------------
// Independent brute-force reference.
// ---------------------------------------------------------------------------

// kshortestRefSortedCosts enumerates EVERY simple (loopless) src->dst path by
// DFS, computes each path's total edge weight, sorts the costs ascending, and
// returns the first k. This is the independent oracle the library is judged
// against: it shares no code with Yen or the best-first enumerator, so agreement
// is genuine evidence rather than a tautology. It returns the FULL sorted cost
// list truncated to k (fewer than k entries when fewer than k paths exist).
//
// The cost MULTISET (not the paths) is the comparison target because distinct
// paths routinely share a cost, so the paths are non-unique while the sorted
// costs are a well-defined invariant of the k-shortest answer.
func kshortestRefSortedCosts(g *kshortestGraph, src, dst, k int) []int {
	var costs []int
	visited := make([]bool, g.n)
	visited[src] = true
	kshortestDFS(g, src, dst, 0, visited, &costs)
	sort.Ints(costs)
	if len(costs) > k {
		costs = costs[:k]
	}
	return costs
}

// kshortestDFS recursively walks every simple path from cur to dst, accumulating
// the running cost, and appends each completed path's total to costs. visited
// enforces looplessness (no node repeats on a path); it is set on entry by the
// caller for src and maintained here for every deeper node.
func kshortestDFS(g *kshortestGraph, cur, dst, cost int, visited []bool, costs *[]int) {
	if cur == dst {
		*costs = append(*costs, cost)
		return
	}
	for _, e := range g.adj[cur] {
		if visited[e.to] {
			continue
		}
		visited[e.to] = true
		kshortestDFS(g, e.to, dst, cost+e.weight, visited, costs)
		visited[e.to] = false
	}
}

// ---------------------------------------------------------------------------
// Comparisons.
// ---------------------------------------------------------------------------

// kshortestCompareExact asserts the algorithm returned EXACTLY the reference
// sorted-cost multiset: same length and element-wise-equal costs. Used for the
// algorithms that never truncate (Yen, and Eppstein's loopless alias) on these
// tiny fixtures.
func kshortestCompareExact(tick int64, algo string, g *kshortestGraph, src, dst graph.NodeID, k int, ref []int, got []search.YenPath[int]) []Violation {
	gotCosts := kshortestCosts(got)
	if !kshortestIntsEqual(gotCosts, ref) {
		return kshortestDiverge(tick, algo,
			fmt.Sprintf("sorted-cost multiset %v disagrees with the brute-force reference %v (n=%d k=%d, src=%d dst=%d)",
				gotCosts, ref, g.n, k, src, dst))
	}
	return nil
}

// kshortestCompareBounded asserts the bounded loopless result agrees with the
// reference, tolerating a budget truncation: when truncated, the returned
// PREFIX must be a correct prefix of the reference (same costs in order, no
// extra/misordered entry); when not truncated, it must equal the reference
// exactly. Either way the returned costs must already be in ascending order
// (the algorithm pops cheapest-first) — kshortestValidatePaths enforces that
// separately, so here we only compare against the reference.
func kshortestCompareBounded(tick int64, algo string, g *kshortestGraph, src, dst graph.NodeID, k int, ref []int, got []search.YenPath[int], truncated bool) []Violation {
	gotCosts := kshortestCosts(got)
	if !truncated {
		if !kshortestIntsEqual(gotCosts, ref) {
			return kshortestDiverge(tick, algo,
				fmt.Sprintf("sorted-cost multiset %v disagrees with the brute-force reference %v (n=%d k=%d, src=%d dst=%d)",
					gotCosts, ref, g.n, k, src, dst))
		}
		return nil
	}
	// Truncated: the prefix must match the reference position-for-position and
	// must not be longer than the reference (it can only be a shorter prefix).
	if len(gotCosts) > len(ref) {
		return kshortestDiverge(tick, algo,
			fmt.Sprintf("budget-truncated result has MORE paths (%d) than the reference (%d) (n=%d k=%d)",
				len(gotCosts), len(ref), g.n, k))
	}
	for i := range gotCosts {
		if gotCosts[i] != ref[i] {
			return kshortestDiverge(tick, algo,
				fmt.Sprintf("budget-truncated prefix %v is not a prefix of the reference %v (n=%d k=%d)",
					gotCosts, ref, g.n, k))
		}
	}
	return nil
}

// kshortestValidatePaths checks every returned YenPath is self-consistent and
// the list as a whole is correctly ordered, independent of the reference:
//
//   - the path begins at src and ends at dst;
//   - each consecutive (u,v) pair is a real directed edge of g;
//   - the reported Cost equals the sum of those edges' weights;
//   - the path is loopless (no node repeats);
//   - successive paths are sorted by Cost ascending.
//
// This catches a path that is internally inconsistent even if its cost happened
// to land in the right multiset.
func kshortestValidatePaths(tick int64, algo string, g *kshortestGraph, src, dst graph.NodeID, paths []search.YenPath[int]) []Violation {
	var vs []Violation
	prevCost := 0
	for pi, p := range paths {
		if len(p.Nodes) == 0 {
			vs = append(vs, kshortestDiverge(tick, algo, fmt.Sprintf("path %d is empty", pi))...)
			continue
		}
		if p.Nodes[0] != src || p.Nodes[len(p.Nodes)-1] != dst {
			vs = append(vs, kshortestDiverge(tick, algo,
				fmt.Sprintf("path %d endpoints (%d..%d) are not src=%d dst=%d", pi, p.Nodes[0], p.Nodes[len(p.Nodes)-1], src, dst))...)
		}
		// Edge-existence + cost recomputation + looplessness.
		seen := make(map[graph.NodeID]struct{}, len(p.Nodes))
		var sum int
		ok := true
		for i, node := range p.Nodes {
			if _, dup := seen[node]; dup {
				vs = append(vs, kshortestDiverge(tick, algo,
					fmt.Sprintf("path %d repeats node %d (not loopless): %v", pi, node, p.Nodes))...)
				ok = false
				break
			}
			seen[node] = struct{}{}
			if i == 0 {
				continue
			}
			w, exists := kshortestEdgeWeight(g, p.Nodes[i-1], node)
			if !exists {
				vs = append(vs, kshortestDiverge(tick, algo,
					fmt.Sprintf("path %d uses a non-existent edge %d->%d: %v", pi, p.Nodes[i-1], node, p.Nodes))...)
				ok = false
				break
			}
			sum += w
		}
		if ok && sum != p.Cost {
			vs = append(vs, kshortestDiverge(tick, algo,
				fmt.Sprintf("path %d reported Cost %d != recomputed edge-weight sum %d: %v", pi, p.Cost, sum, p.Nodes))...)
		}
		if pi > 0 && p.Cost < prevCost {
			vs = append(vs, kshortestDiverge(tick, algo,
				fmt.Sprintf("paths not sorted ascending: path %d cost %d < previous %d", pi, p.Cost, prevCost))...)
		}
		prevCost = p.Cost
	}
	return vs
}

// kshortestEdgeWeight returns the weight of the directed edge from->to and
// whether it exists in g. Linear over from's out-edges (degree is tiny here).
func kshortestEdgeWeight(g *kshortestGraph, from, to graph.NodeID) (int, bool) {
	u := int(from)
	if u < 0 || u >= g.n {
		return 0, false
	}
	for _, e := range g.adj[u] {
		if graph.NodeID(e.to) == to {
			return e.weight, true
		}
	}
	return 0, false
}

// kshortestCosts projects the cost field out of a path slice, preserving order.
func kshortestCosts(paths []search.YenPath[int]) []int {
	out := make([]int, len(paths))
	for i, p := range paths {
		out[i] = p.Cost
	}
	return out
}

// kshortestIntsEqual reports whether two int slices are element-wise identical.
func kshortestIntsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// kshortestDiverge builds a single K-shortest-family divergence violation.
func kshortestDiverge(tick int64, algo, msg string) []Violation {
	return []Violation{{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo, Message: msg}}
}
