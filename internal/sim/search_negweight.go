package sim

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// negWeightSalt keeps this checker's draw stream disjoint from every other
// per-tick search check.
const negWeightSalt uint64 = 0x2b7c_f4e1_09a6_d83e

// negWeightNoCycleFixtures / negWeightCycleFixtures are how many
// negative-weight-without-cycle and planted-negative-cycle fixtures the checker
// generates per tick. Kept small and fixed so the per-tick cost is bounded and
// the draw-stream length is input-independent.
const (
	negWeightNoCycleFixtures = 3
	negWeightCycleFixtures   = 2
)

// negWeightMag bounds the magnitude of the small INTEGER edge weights the
// fixtures emit; weights are drawn from [-negWeightMag, +negWeightMag] and held
// as integer-valued float64 so every path sum stays exact (well within 2^53).
const negWeightMag = 5

// negEdge is one directed weighted edge of a negative-weight fixture. The weight
// may be negative; it is an integer held as float64 for exact path sums.
type negEdge struct {
	src, dst int
	w        float64
}

// negWeightViolations drives the negative-weight shortest-path algorithms —
// [search.BellmanFord], [search.FloydWarshall], [search.JohnsonAPSP] — which the
// rest of the DST never exercises (every other weighted fixture uses strictly
// positive weights, so these three routines' whole reason to exist, negative
// edges and negative-cycle detection, was untested). Dijkstra / A* /
// BidirectionalDijkstra are excluded by contract (they require non-negative
// weights).
//
// Two fixture families, both deterministic functions of the tick:
//
//   - no-cycle: a directed acyclic graph (forward edges u->v, u<v) with some
//     negative edge weights. A DAG can hold no cycle, so a fortiori no negative
//     cycle, whatever the weights. BellmanFord (single source), FloydWarshall,
//     and JohnsonAPSP distance maps are each cross-checked against an INDEPENDENT
//     naive Bellman-Ford (V-1 full-edge-sweep) reference computed here from
//     scratch. Integer-valued weights make every comparison an exact equality.
//   - negative cycle: a graph with a planted cycle of strictly negative total
//     weight reachable from the source. BellmanFord must return
//     [search.ErrNegativeCycle]; FloydWarshallCtx and JohnsonAPSP must likewise
//     surface [search.ErrNegativeCycle] (the shared sentinel).
//
// Divergences are tagged ViolationSearchDivergence / ViolationOracleDeviation
// with the algorithm's Op.
func negWeightViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ negWeightSalt)
	var vs []Violation
	for i := 0; i < negWeightNoCycleFixtures; i++ {
		vs = append(vs, negWeightNoCycleCheck(tick, seed)...)
	}
	for i := 0; i < negWeightCycleFixtures; i++ {
		vs = append(vs, negWeightCycleCheck(tick, seed, i)...)
	}
	return vs
}

// negWeightNoCycleCheck builds one negative-weight DAG and cross-checks the
// negative-tolerant shortest-path algorithms against the naive reference.
func negWeightNoCycleCheck(tick int64, seed *Seed) []Violation {
	n, edges := negGenDAG(seed)
	c := negBuildCSR(n, edges)

	// Independent reference: naive Bellman-Ford from every source.
	refDist := make([][]float64, n)
	refReach := make([][]bool, n)
	for u := 0; u < n; u++ {
		refDist[u], refReach[u] = negRefBellmanFord(n, edges, u)
	}

	var vs []Violation

	// BellmanFord single-source from a few deterministic sources.
	for _, src := range negSources(n) {
		d, err := search.BellmanFord(c, graph.NodeID(src))
		if err != nil {
			vs = append(vs, searchDeviation(tick, "BellmanFord", err))
			continue
		}
		for v := 0; v < n; v++ {
			got, ok := d.Distance(graph.NodeID(v))
			if ok != refReach[src][v] {
				vs = append(vs, negDiverge(tick, "BellmanFord", fmt.Sprintf(
					"reachability %d->%d: got %v want %v (edges=%s)", src, v, ok, refReach[src][v], negFmtEdges(edges)))...)
				break
			}
			if ok && got != refDist[src][v] {
				vs = append(vs, negDiverge(tick, "BellmanFord", fmt.Sprintf(
					"distance %d->%d: got %v want %v (edges=%s)", src, v, got, refDist[src][v], negFmtEdges(edges)))...)
				break
			}
		}
	}

	// FloydWarshall and JohnsonAPSP all-pairs (both tolerate negative edges).
	fw := search.FloydWarshall(c)
	jo, jerr := search.JohnsonAPSP(c)
	if jerr != nil {
		vs = append(vs, searchDeviation(tick, "JohnsonAPSP", jerr))
	}
	for u := 0; u < n; u++ {
		for v := 0; v < n; v++ {
			vs = append(vs, negCompareAPSP(tick, "FloydWarshall", fw, u, v, refDist[u][v], refReach[u][v], edges)...)
			if jerr == nil {
				vs = append(vs, negCompareAPSP(tick, "JohnsonAPSP", jo, u, v, refDist[u][v], refReach[u][v], edges)...)
			}
		}
		if len(vs) > 0 {
			break // first divergence is enough; keep the report bounded
		}
	}
	return vs
}

// negCompareAPSP compares one APSP cell against the naive reference.
func negCompareAPSP(tick int64, algo string, a *search.APSP[float64], u, v int, want float64, wantReach bool, edges []negEdge) []Violation {
	if a == nil {
		return negDiverge(tick, algo, fmt.Sprintf("nil APSP result (edges=%s)", negFmtEdges(edges)))
	}
	got, ok := a.At(graph.NodeID(u), graph.NodeID(v))
	if ok != wantReach {
		return negDiverge(tick, algo, fmt.Sprintf("APSP reach %d->%d: got %v want %v (edges=%s)", u, v, ok, wantReach, negFmtEdges(edges)))
	}
	if ok && got != want {
		return negDiverge(tick, algo, fmt.Sprintf("APSP dist %d->%d: got %v want %v (edges=%s)", u, v, got, want, negFmtEdges(edges)))
	}
	return nil
}

// negWeightCycleCheck builds a graph with a planted negative-weight cycle
// reachable from the source and asserts each negative-tolerant algorithm
// reports it via [search.ErrNegativeCycle]. idx selects a fixed fixture (idx 0)
// or a seed-derived one so the check varies tick to tick while always planting a
// genuine negative cycle.
func negWeightCycleCheck(tick int64, seed *Seed, idx int) []Violation {
	n, edges := negGenNegativeCycle(seed, idx)
	c := negBuildCSR(n, edges)
	var vs []Violation

	// BellmanFord from node 0 (on the planted cycle) must detect it.
	if _, err := search.BellmanFord(c, 0); !errors.Is(err, search.ErrNegativeCycle) {
		vs = append(vs, negDiverge(tick, "BellmanFord", fmt.Sprintf(
			"planted negative cycle not detected: err=%v want ErrNegativeCycle (edges=%s)", err, negFmtEdges(edges)))...)
	}

	// FloydWarshallCtx must flag the negative cycle (the simple entry returns nil
	// by contract, so the Ctx variant is used to observe the sentinel).
	if _, err := search.FloydWarshallCtx(context.Background(), c); !errors.Is(err, search.ErrNegativeCycle) {
		vs = append(vs, negDiverge(tick, "FloydWarshall", fmt.Sprintf(
			"planted negative cycle not flagged: err=%v want ErrNegativeCycle (edges=%s)", err, negFmtEdges(edges)))...)
	}

	// JohnsonAPSP must surface the same sentinel from its Bellman-Ford reweighting.
	if _, err := search.JohnsonAPSP(c); !errors.Is(err, search.ErrNegativeCycle) {
		vs = append(vs, negDiverge(tick, "JohnsonAPSP", fmt.Sprintf(
			"planted negative cycle not surfaced: err=%v want ErrNegativeCycle (edges=%s)", err, negFmtEdges(edges)))...)
	}
	return vs
}

// negGenDAG derives a directed acyclic graph with signed integer weights from
// seed: n nodes (5..9), a forward spine 0->1->...->(n-1) guaranteeing every
// higher-index node is reachable from every lower one, plus seed-chosen forward
// skip edges u->v (u < v). All edges go strictly forward, so the graph is a DAG
// and holds no cycle (hence no negative cycle) regardless of the signs. Weights
// are integers in [-negWeightMag, +negWeightMag].
func negGenDAG(seed *Seed) (int, []negEdge) {
	n := 5 + seed.IntN(5) // 5..9
	edges := make([]negEdge, 0, n*2)
	for i := 0; i < n-1; i++ {
		edges = append(edges, negEdge{src: i, dst: i + 1, w: negWeight(seed)})
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
		// (a < b) keeps the graph a DAG; the pair may repeat but a repeated
		// (a,b) is a parallel edge FloydWarshall/Johnson resolve to the minimum,
		// which the naive reference also relaxes to, so it stays consistent.
		edges = append(edges, negEdge{src: a, dst: b, w: negWeight(seed)})
	}
	return n, edges
}

// negGenNegativeCycle builds a graph with a planted negative-weight cycle
// reachable from node 0. idx 0 is a fixed hand-checked 3-cycle
// (0->1->2->0 summing to -3); other idx values append a seed-chosen negative
// back edge onto a forward spine so the cycle length and the extra structure
// vary while the total cycle weight is always guaranteed negative.
func negGenNegativeCycle(seed *Seed, idx int) (int, []negEdge) {
	if idx == 0 {
		// 0->1 (+1), 1->2 (+1), 2->0 (-5): cycle sum = -3 < 0, reachable from 0.
		return 3, []negEdge{
			{0, 1, 1}, {1, 2, 1}, {2, 0, -5},
		}
	}
	n := 4 + seed.IntN(4) // 4..7
	edges := make([]negEdge, 0, n+1)
	var forwardSum float64
	for i := 0; i < n-1; i++ {
		w := float64(1 + seed.IntN(negWeightMag)) // positive forward weight [1, negWeightMag]
		forwardSum += w
		edges = append(edges, negEdge{src: i, dst: i + 1, w: w})
	}
	// Back edge (n-1)->0 whose weight makes the whole ring sum strictly negative.
	back := -(forwardSum + 1)
	edges = append(edges, negEdge{src: n - 1, dst: 0, w: back})
	return n, edges
}

// negWeight draws an integer weight in [-negWeightMag, +negWeightMag] as a
// float64 (exact). The sign is drawn from the same stream so the fixture replays
// from the seed.
func negWeight(seed *Seed) float64 {
	return float64(seed.IntN(2*negWeightMag+1) - negWeightMag)
}

// negSources returns a small deterministic set of source vertices for the
// single-source Bellman-Ford check: first, middle, last.
func negSources(n int) []int {
	switch {
	case n <= 0:
		return nil
	case n == 1:
		return []int{0}
	case n == 2:
		return []int{0, 1}
	default:
		return []int{0, n / 2, n - 1}
	}
}

// negBuildCSR materialises the signed-weight edge list as a directed
// CSR[float64] over dense NodeIDs [0,n) via the counting-then-scatter offset
// build (every source, including a pure sink, owns an offset slot).
func negBuildCSR(n int, edges []negEdge) *csr.CSR[float64] {
	if n == 0 {
		return csr.FromArrays[float64]([]uint64{0}, nil, nil, 0, 0)
	}
	vertices := make([]uint64, n+1)
	for _, e := range edges {
		vertices[e.src+1]++
	}
	for i := 1; i <= n; i++ {
		vertices[i] += vertices[i-1]
	}
	out := make([]graph.NodeID, len(edges))
	weights := make([]float64, len(edges))
	cursor := make([]uint64, n)
	for _, e := range edges {
		pos := vertices[e.src] + cursor[e.src]
		out[pos] = graph.NodeID(e.dst)
		weights[pos] = e.w
		cursor[e.src]++
	}
	return csr.FromArrays[float64](vertices, out, weights, uint64(n), uint64(len(edges)))
}

// negRefBellmanFord is the independent naive Bellman-Ford reference: V-1
// full-edge-sweep relaxations from src over the signed-weight edge set. It
// shares no code with search/, so agreement is genuine evidence. On the DAG
// fixtures no negative cycle exists, so V-1 sweeps converge to the true
// distances. Returns per-node distances and a reachability bitmap.
func negRefBellmanFord(n int, edges []negEdge, src int) (dist []float64, reach []bool) {
	dist = make([]float64, n)
	reach = make([]bool, n)
	if src < 0 || src >= n {
		return dist, reach
	}
	reach[src] = true
	for iter := 0; iter < n-1; iter++ {
		changed := false
		for _, e := range edges {
			if !reach[e.src] {
				continue
			}
			cand := dist[e.src] + e.w
			if !reach[e.dst] || cand < dist[e.dst] {
				reach[e.dst] = true
				dist[e.dst] = cand
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return dist, reach
}

// negFmtEdges renders a signed-weight edge list deterministically (input order).
func negFmtEdges(edges []negEdge) string {
	s := "["
	for i, e := range edges {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%d->%d:%g", e.src, e.dst, e.w)
	}
	return s + "]"
}

// negDiverge builds a single negative-weight divergence violation.
func negDiverge(tick int64, algo, msg string) []Violation {
	return []Violation{{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo, Message: msg}}
}
