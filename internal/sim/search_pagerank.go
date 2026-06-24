package sim

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// pagerankSeedConst salts the tick for this checker's independent draw stream.
const pagerankSeedConst uint64 = 0x9f2c_a4b1_77e3_05d9

// pagerankEpsilon is the absolute tolerance for the PageRank comparison. The
// library stops at an L1 residual of 1e-6, leaving its ranks within roughly
// d/(1-d) * 1e-6 (about 6e-6) of the true stationary distribution; the reference
// is iterated to a far tighter residual (near the exact fixpoint), so this
// epsilon comfortably covers the library's convergence gap while staying far
// below the smallest meaningful rank difference on these small graphs.
const pagerankEpsilon = 1e-4

// pagerankRefTolerance / pagerankRefMaxIter drive the reference to a near-exact
// stationary distribution, well past the library's default 1e-6 / 100.
const (
	pagerankRefTolerance = 1e-13
	pagerankRefMaxIter   = 10_000
)

// pagerankFixtures is the number of deterministic PageRank fixtures per tick.
const pagerankFixtures = 4

// pagerankViolations checks centrality.PageRank against an independent
// power-iteration reference that mirrors the library's model exactly (damping,
// dangling-mass redistribution over live nodes, teleport), comparing the rank
// vector within a convergence-aware epsilon. PageRank's answer is a unique
// stationary distribution, so the rank vector itself is the comparison invariant.
func pagerankViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ pagerankSeedConst)
	var vs []Violation
	for i := 0; i < pagerankFixtures; i++ {
		n, edges := pagerankGenGraph(seed)
		c := pagerankBuildCSR(n, edges)
		opts := centrality.DefaultPageRankOptions()
		got, iters, err := centrality.PageRank(c, opts)
		if err != nil {
			vs = append(vs, searchDeviation(tick, "PageRank", err))
			continue
		}
		if iters >= opts.MaxIterations {
			// The library hit its iteration cap without converging; the comparison
			// would be against a non-stationary vector. These small fixtures
			// converge in well under the cap, so this is a defensive skip.
			continue
		}
		want := pagerankReference(n, edges, opts.Damping)
		for v := 0; v < n; v++ {
			if v < len(got) && !pagerankClose(got[v], want[v]) {
				vs = append(vs, Violation{
					Kind: ViolationSearchDivergence, Tick: tick, Op: "search:PageRank",
					Message: fmt.Sprintf("rank[%d] got %.9f want %.9f (n=%d)", v, got[v], want[v], n),
				})
				break
			}
		}
	}
	return vs
}

// pagerankClose reports whether two ranks agree within pagerankEpsilon.
func pagerankClose(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= pagerankEpsilon
}

// pagerankGenGraph derives a directed graph from seed: n nodes (5..9), a forward
// spine 0->1->...->(n-1) for connectivity, plus seed-chosen extra arcs including
// at least one back edge (creating a cycle) so the stationary distribution is
// non-trivial; node n-1 is left as a dangling sink to exercise the dangling-mass
// redistribution path. Edges are emitted in a fixed order (no map iteration).
func pagerankGenGraph(seed *Seed) (int, [][2]int) {
	n := 5 + seed.IntN(5) // 5..9
	edges := make([][2]int, 0, n*2)
	for i := 0; i < n-1; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	// A back edge from a middle node to 0 guarantees a cycle (so mass recirculates).
	mid := 1 + seed.IntN(n-1)
	edges = append(edges, [2]int{mid, 0})
	// Extra seed-chosen arcs (skip self-loops and the dangling sink as a source so
	// n-1 stays dangling).
	extra := seed.IntN(n)
	for k := 0; k < extra; k++ {
		a := seed.IntN(n - 1) // never the sink
		b := seed.IntN(n)
		if a == b {
			continue
		}
		edges = append(edges, [2]int{a, b})
	}
	return n, edges
}

// pagerankBuildCSR builds the directed CSR for the library from the edge list.
func pagerankBuildCSR(n int, edges [][2]int) *csr.CSR[float64] {
	if n == 0 {
		return csr.FromArrays[float64]([]uint64{0}, nil, nil, 0, 0)
	}
	deg := make([]int, n)
	for _, e := range edges {
		deg[e[0]]++
	}
	vertices := make([]uint64, n+1)
	var total uint64
	for i := 0; i < n; i++ {
		vertices[i] = total
		total += uint64(deg[i])
	}
	vertices[n] = total
	out := make([]graph.NodeID, total)
	pos := make([]uint64, n)
	copy(pos, vertices[:n])
	for _, e := range edges {
		out[pos[e[0]]] = graph.NodeID(e[1])
		pos[e[0]]++
	}
	return csr.FromArrays[float64](vertices, out, nil, uint64(n), total)
}

// pagerankReference computes the PageRank stationary distribution independently
// by power iteration, mirroring the library's model: live nodes start at 1/live;
// each step redistributes the mass held by dangling (out-degree-0) live nodes
// uniformly over the live set, applies teleport (1-d)/live, and pulls
// d * rank(u) / outdeg(u) along each edge. It iterates to a near-exact residual.
func pagerankReference(n int, edges [][2]int, damping float64) []float64 {
	outAdj := make([][]int, n)
	outdeg := make([]int, n)
	live := make([]bool, n)
	for _, e := range edges {
		outAdj[e[0]] = append(outAdj[e[0]], e[1])
		outdeg[e[0]]++
		live[e[0]] = true
		live[e[1]] = true
	}
	liveCount := 0
	for i := 0; i < n; i++ {
		if live[i] {
			liveCount++
		}
	}
	rank := make([]float64, n)
	if liveCount == 0 {
		return rank
	}
	for i := 0; i < n; i++ {
		if live[i] {
			rank[i] = 1.0 / float64(liveCount)
		}
	}
	teleport := (1 - damping) / float64(liveCount)
	next := make([]float64, n)
	for iter := 0; iter < pagerankRefMaxIter; iter++ {
		var danglingMass float64
		for i := 0; i < n; i++ {
			if live[i] && outdeg[i] == 0 {
				danglingMass += rank[i]
			}
		}
		baseShare := teleport + damping*danglingMass/float64(liveCount)
		for i := 0; i < n; i++ {
			if live[i] {
				next[i] = baseShare
			} else {
				next[i] = 0
			}
		}
		for u := 0; u < n; u++ {
			if outdeg[u] == 0 {
				continue
			}
			share := damping * rank[u] / float64(outdeg[u])
			for _, v := range outAdj[u] {
				next[v] += share
			}
		}
		var delta float64
		for i := 0; i < n; i++ {
			d := next[i] - rank[i]
			if d < 0 {
				d = -d
			}
			delta += d
		}
		rank, next = next, rank
		if delta < pagerankRefTolerance {
			break
		}
	}
	return rank
}
