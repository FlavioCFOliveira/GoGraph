package sim

import (
	"context"
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
	return append(vs, pagerankerStatefulViolations(tick)...)
}

// pagerankerStatefulSalt keeps the stateful arm's draw stream disjoint from the
// one-shot fixtures above, so adding the arm changed no existing fixture.
const pagerankerStatefulSalt uint64 = 0x2495_7A7E_F011_0000

// pagerankerStatefulOpts returns the stateful arm's two option sets. The
// dampings differ so the two results differ, which is what arms the aliasing
// pin; both converge well inside the iteration budget on these small fixtures.
// It is a function rather than a package-level array because a package-level
// mutable variable is the shape CLAUDE.md forbids and global_state_guard_test.go
// polices.
func pagerankerStatefulOpts() [2]centrality.PageRankOptions {
	return [2]centrality.PageRankOptions{
		{Damping: 0.85, MaxIterations: 60, Tolerance: 1e-9},
		{Damping: 0.60, MaxIterations: 60, Tolerance: 1e-9},
	}
}

// pagerankerStatefulViolations checks the two promises the STATEFUL
// [centrality.PageRanker] publishes, on the per-tick fixture and therefore after
// every crash/recovery the surrounding scenario injects:
//
//   - each Run is bit-for-bit identical to the equivalent one-shot
//     [centrality.PageRankCtx], compared on the BIT PATTERN rather than within
//     pagerankEpsilon, which is the right tolerance for the reference comparison
//     above and would make an identity claim unfalsifiable;
//   - the returned slice aliases an internal buffer and is invalidated by the
//     next Run: after the second Run the first Run's SLICE must read the new
//     values while a COPY of it must not, and only two backing arrays may ever be
//     returned.
//
// The fixtures here are a handful of nodes, so both Runs take the SERIAL push
// path — that is what makes this arm cheap enough to run on the search battery's
// cadence. The regime-interleaved half of the same claims, over a fixture above
// the parallel threshold, is the `pagerank-ranker` scenario
// (internal/sim/pagerank_ranker.go).
func pagerankerStatefulViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ pagerankerStatefulSalt)
	n, edges := pagerankGenGraph(seed)
	c := pagerankBuildCSR(n, edges)
	pr := centrality.NewPageRanker(c)

	var vs []Violation
	var buffers []*float64
	var firstSlice, firstCopy []float64
	var firstHash uint64
	for i, opts := range pagerankerStatefulOpts() {
		got, iters, err := pr.Run(context.Background(), opts)
		if err != nil {
			return append(vs, searchDeviation(tick, "PageRanker.Run", err))
		}
		want, wantIters, err := centrality.PageRankCtx(context.Background(), c, opts)
		if err != nil {
			return append(vs, searchDeviation(tick, "PageRankCtx", err))
		}
		if bitDiff, firstDiff, maxULP := prCompareBits(got, want); bitDiff != 0 {
			vs = append(vs, Violation{
				Kind: ViolationSearchDivergence, Tick: tick, Op: "search:PageRanker.Run",
				Message: fmt.Sprintf("run %d (damping %.2f): %d of %d rank bit patterns differ from the "+
					"one-shot PageRankCtx, first at index %d, max ULP distance %d (n=%d)",
					i, opts.Damping, bitDiff, n, firstDiff, maxULP, n),
			})
		}
		if iters != wantIters {
			vs = append(vs, Violation{
				Kind: ViolationSearchDivergence, Tick: tick, Op: "search:PageRanker.Run",
				Message: fmt.Sprintf("run %d (damping %.2f): Run took %d iterations, the one-shot %d (n=%d)",
					i, opts.Damping, iters, wantIters, n),
			})
		}
		if idx := prBufferIndex(&buffers, got); idx > 1 {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: "search:PageRanker.Run",
				Message: fmt.Sprintf("run %d returned backing array #%d; a PageRanker owns two rank "+
					"vectors and Run's result must alias one of them", i, idx),
			})
		}
		if i == 0 {
			firstSlice = got
			firstCopy = append([]float64(nil), got...)
			firstHash = prHashFloats(firstCopy)
			continue
		}
		vs = append(vs, pagerankerAliasViolations(tick, n, got, firstSlice, firstCopy, firstHash)...)
	}
	return vs
}

// pagerankerAliasViolations adjudicates the aliasing pin after the second Run:
// the copy must be intact, and the first Run's slice must have been overwritten —
// gated on the two results actually differing, since an overwrite that wrote the
// same values would be unobservable.
//
// # The copy-intact clause is a THEOREM on this path, not a detector
//
// Called from [pagerankerStatefulViolations], the first clause below CANNOT
// fire, and saying so is better than leaving it looking like a check.
// `firstCopy` there is `append([]float64(nil), got...)` — a freshly allocated
// array — `firstHash` is taken from it immediately, and nothing between the two
// Runs writes to it. So `prHashFloats(firstCopy) != firstHash` is false by
// construction on that path.
//
// It is kept for one reason, and it is not the appearance of rigour: it is a
// tripwire against a future refactor of THIS harness that made the "copy" alias
// the returned slice — the exact caller mistake the aliasing contract warns
// about. Under that refactor the clause fires immediately and loudly instead of
// the sibling clause silently comparing an array with itself. The clause's LOGIC
// is proved fireable by [TestPageRankerStatefulArm_AliasClausesFire], which
// supplies an aliasing copy directly, and the scenario's
// [prPerturbAliasCopyAliases] fires the same clause through a real
// configuration path.
//
// The second clause below is a genuine detector on this path: it fires whenever
// a Run stops invalidating the buffer, which is a property of the library and
// not of this harness.
func pagerankerAliasViolations(tick int64, n int, second, firstSlice, firstCopy []float64, firstHash uint64) []Violation {
	var vs []Violation
	if prHashFloats(firstCopy) != firstHash {
		vs = append(vs, Violation{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "search:PageRanker.Run",
			Message: fmt.Sprintf("the copy of the first Run's result changed after the second Run, so "+
				"the aliasing control is unsound (n=%d)", n),
		})
	}
	differed, _, _ := prCompareBits(second, firstCopy)
	if differed == 0 {
		// Not armed: the two runs produced the same vector, so the overwrite is
		// unobservable on this fixture.
		return vs
	}
	changed, _, _ := prCompareBits(firstSlice, firstCopy)
	if changed == 0 {
		vs = append(vs, Violation{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "search:PageRanker.Run",
			Message: fmt.Sprintf("not one of the %d elements of the first Run's returned slice changed "+
				"after the second Run, although the two results differ in %d elements; Run's godoc says "+
				"the returned slice is invalidated by the next Run", n, differed),
		})
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
