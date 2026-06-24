package sim

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// matchingSeedMix is XORed with the tick to seed this check's [Seed], keeping
// its draw stream disjoint from every other per-tick check in the package (each
// uses its own distinct mix constant, e.g. crashSeedMix, swarmSeedMix). The
// value is the MurmurHash3 / splitmix64 finaliser constant, chosen only because
// it is well-mixed and not used elsewhere in this package.
const matchingSeedMix uint64 = 0xff51afd7ed558ccd

// matchingLeftMax / matchingRightMax bound the bipartite instance the check
// builds. They are kept small so both the Hopcroft-Karp run and the independent
// Kuhn's-algorithm reference stay O(V*E) cheap on every tick, while still large
// enough that the maximum matching is non-trivial (some left vertices compete
// for the same right vertex, so a greedy match would be sub-maximal).
const (
	matchingLeftMax  = 6
	matchingRightMax = 6
)

// matchingAssignMax bounds the SQUARE assignment instance. n is kept at or below
// 6 so the brute-force reference — which enumerates all n! permutations — stays
// feasible (6! = 720). Costs are small non-negative integers so the comparison
// against the brute-force optimum is exact.
const (
	matchingAssignMax  = 6
	matchingAssignMinN = 4
	matchingCostMax    = 20 // cost entries drawn from [0, matchingCostMax)
)

// matchingViolations runs the MATCHING family (maximum-cardinality bipartite
// matching + minimum-cost square assignment) on a deterministic, seed-derived
// instance and cross-checks each library algorithm against an independent
// reference it implements here.
//
//   - search.HopcroftKarp is compared on matching CARDINALITY (the matching
//     itself is non-unique) against a from-scratch Kuhn's-algorithm augmenting-
//     path maximum bipartite matching over the same adjacency, and the returned
//     Matching is validated for internal consistency (a true matching).
//   - search.Hungarian is compared on TOTAL COST (exactly, the costs being
//     integer-valued) against a brute-force minimum over all n! assignments, and
//     its RowToCol is validated to be a permutation.
//
// The instance is a pure function of tick (via [NewSeed]), so any divergence is
// reproducible from the tick alone. A clean check returns nil; each divergence
// appends a [ViolationSearchDivergence] tagged with the algorithm's Op.
func matchingViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ matchingSeedMix)
	vs := matchingBipartiteViolations(tick, seed)
	vs = append(vs, matchingAssignViolations(tick, seed)...)
	return vs
}

// ---------------------------------------------------------------------------
// Bipartite maximum-cardinality matching.
// ---------------------------------------------------------------------------

// matchingBipartiteViolations builds a deterministic bipartite graph, runs
// Hopcroft-Karp on its CSR, and cross-checks the result's cardinality against a
// from-scratch Kuhn reference plus a structural validity check.
func matchingBipartiteViolations(tick int64, seed *Seed) []Violation {
	// At least one vertex on each side so the instance is non-degenerate; the
	// counts are seed-derived within the bound.
	nLeft := 1 + seed.IntN(matchingLeftMax)
	nRight := 1 + seed.IntN(matchingRightMax)

	// adj[u] is the sorted, de-duplicated set of right-vertex offsets (in
	// [0,nRight)) adjacent to left vertex u. Each (left,right) pair is included
	// with probability ~1/2, drawn from the seed so the instance is reproducible
	// and the iteration order below is index order, never map order.
	adj := make([][]int, nLeft)
	var totalEdges int
	for u := 0; u < nLeft; u++ {
		for r := 0; r < nRight; r++ {
			if seed.IntN(2) == 0 {
				adj[u] = append(adj[u], r)
				totalEdges++
			}
		}
	}

	c := matchingBuildBipartiteCSR(nLeft, nRight, adj, totalEdges)
	got := search.HopcroftKarp(c, nLeft)

	var vs []Violation
	// Cardinality: the maximum matching size is unique even though the matching
	// is not, so it is the sound invariant to compare.
	if want := matchingKuhnMaxMatching(nLeft, nRight, adj); got.Size != want {
		vs = append(vs, matchingDiverge(tick, "HopcroftKarp",
			fmt.Sprintf("matching cardinality %d disagrees with the Kuhn reference %d (nLeft=%d nRight=%d edges=%d)",
				got.Size, want, nLeft, nRight, totalEdges))...)
	}
	// Structural validity: the returned Matching must be a genuine matching that
	// honours the input adjacency, independent of its size.
	vs = append(vs, matchingValidate(tick, nLeft, nRight, adj, got)...)
	return vs
}

// matchingBuildBipartiteCSR assembles the directed left->right CSR the search
// package expects: node ids [0,nLeft) are the left partition, [nLeft,
// nLeft+nRight) are the right partition, and every edge points from a left id to
// a right id (offset by nLeft). The offsets/edges arrays are built by hand so
// the check does not depend on adjlist; right vertices carry no out-edges.
func matchingBuildBipartiteCSR(nLeft, nRight int, adj [][]int, totalEdges int) *csr.CSR[float64] {
	order := uint64(nLeft + nRight)
	// vertices has length order+1; vertices[id] is the start of id's out-edges.
	vertices := make([]uint64, order+1)
	edges := make([]graph.NodeID, 0, totalEdges)
	var pos uint64
	for u := 0; u < nLeft; u++ {
		vertices[u] = pos
		for _, r := range adj[u] {
			edges = append(edges, graph.NodeID(nLeft+r))
			pos++
		}
	}
	// Right vertices (and the terminal slot) have no out-edges: their offsets all
	// equal the current cursor.
	for id := nLeft; id <= nLeft+nRight; id++ {
		vertices[id] = pos
	}
	// weights nil: cardinality matching ignores weights.
	return csr.FromArrays[float64](vertices, edges, nil, order, pos)
}

// matchingKuhnMaxMatching computes the maximum-cardinality bipartite matching
// size by Kuhn's algorithm (repeated DFS for augmenting paths). This is a
// deliberately separate, textbook implementation from the library's
// Hopcroft-Karp, so agreement is independent evidence rather than a tautology.
//
// matchR[r] is the left vertex currently matched to right vertex r, or -1. For
// each left vertex it runs one augmenting-path DFS; the number of successful
// augmentations is the matching size.
func matchingKuhnMaxMatching(nLeft, nRight int, adj [][]int) int {
	matchR := make([]int, nRight)
	for r := range matchR {
		matchR[r] = -1
	}
	size := 0
	for u := 0; u < nLeft; u++ {
		seen := make([]bool, nRight)
		if matchingKuhnAugment(u, adj, matchR, seen) {
			size++
		}
	}
	return size
}

// matchingKuhnAugment tries to find an augmenting path from left vertex u,
// flipping matches along it on success. seen guards against revisiting a right
// vertex within this DFS.
func matchingKuhnAugment(u int, adj [][]int, matchR []int, seen []bool) bool {
	for _, r := range adj[u] {
		if seen[r] {
			continue
		}
		seen[r] = true
		// r is free, or its current left partner can be re-routed elsewhere.
		if matchR[r] == -1 || matchingKuhnAugment(matchR[r], adj, matchR, seen) {
			matchR[r] = u
			return true
		}
	}
	return false
}

// matchingValidate asserts the library's Matching is a structurally valid
// matching for the given adjacency: MatchL/MatchR are mutually consistent, every
// asserted match corresponds to a real input edge, no right vertex is claimed
// twice, and Size equals the number of matched left vertices. The unmatched
// sentinel is ^graph.NodeID(0) (see search.HopcroftKarp).
func matchingValidate(tick int64, nLeft, nRight int, adj [][]int, m search.Matching) []Violation {
	const unmatched = ^graph.NodeID(0)
	var vs []Violation

	if len(m.MatchL) != nLeft {
		return matchingDiverge(tick, "HopcroftKarp",
			fmt.Sprintf("MatchL length %d != nLeft %d", len(m.MatchL), nLeft))
	}
	if len(m.MatchR) < nLeft+nRight {
		// HopcroftKarp sizes MatchR to MaxNodeID() = nLeft+nRight here.
		return matchingDiverge(tick, "HopcroftKarp",
			fmt.Sprintf("MatchR length %d < nLeft+nRight %d", len(m.MatchR), nLeft+nRight))
	}

	// Build the adjacency membership set for an O(1) edge-existence test.
	adjSet := make([]map[int]struct{}, nLeft)
	for u := 0; u < nLeft; u++ {
		s := make(map[int]struct{}, len(adj[u]))
		for _, r := range adj[u] {
			s[r] = struct{}{}
		}
		adjSet[u] = s
	}

	matched := 0
	for u := 0; u < nLeft; u++ {
		v := m.MatchL[u]
		if v == unmatched {
			continue
		}
		matched++
		ri := int(v) - nLeft
		if ri < 0 || ri >= nRight {
			vs = append(vs, matchingDiverge(tick, "HopcroftKarp",
				fmt.Sprintf("left %d matched to %d which is not a right vertex [%d,%d)", u, v, nLeft, nLeft+nRight))...)
			continue
		}
		if _, ok := adjSet[u][ri]; !ok {
			vs = append(vs, matchingDiverge(tick, "HopcroftKarp",
				fmt.Sprintf("left %d matched to right offset %d but no such edge exists", u, ri))...)
		}
		if back := m.MatchR[int(v)]; back != graph.NodeID(u) {
			vs = append(vs, matchingDiverge(tick, "HopcroftKarp",
				fmt.Sprintf("MatchL[%d]=%d but MatchR[%d]=%d (not symmetric)", u, v, int(v), int64(back)))...)
		}
	}

	// No right vertex may be claimed by two distinct left vertices.
	claimedBy := make([]int, nRight)
	for r := range claimedBy {
		claimedBy[r] = -1
	}
	for u := 0; u < nLeft; u++ {
		v := m.MatchL[u]
		if v == unmatched {
			continue
		}
		ri := int(v) - nLeft
		if ri < 0 || ri >= nRight {
			continue // already reported above
		}
		if prev := claimedBy[ri]; prev != -1 {
			vs = append(vs, matchingDiverge(tick, "HopcroftKarp",
				fmt.Sprintf("right offset %d claimed by both left %d and left %d", ri, prev, u))...)
		}
		claimedBy[ri] = u
	}

	if m.Size != matched {
		vs = append(vs, matchingDiverge(tick, "HopcroftKarp",
			fmt.Sprintf("Size %d != number of matched left vertices %d", m.Size, matched))...)
	}
	return vs
}

// ---------------------------------------------------------------------------
// Square minimum-cost assignment.
// ---------------------------------------------------------------------------

// matchingAssignViolations builds a deterministic SQUARE integer cost matrix,
// runs Hungarian, and cross-checks the total cost against a brute-force optimum
// plus a permutation check on RowToCol.
func matchingAssignViolations(tick int64, seed *Seed) []Violation {
	// n in [matchingAssignMinN, matchingAssignMax].
	n := matchingAssignMinN + seed.IntN(matchingAssignMax-matchingAssignMinN+1)

	// Integer costs in [0, matchingCostMax) kept as float64 (Hungarian is
	// float64-only) but exactly representable, so the comparison is exact.
	costInt := make([]int, n*n)
	cost := make([]float64, n*n)
	for i := range costInt {
		v := seed.IntN(matchingCostMax)
		costInt[i] = v
		cost[i] = float64(v)
	}

	got, err := search.Hungarian(cost, n, n)
	if err != nil {
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "search:Hungarian",
			Message: fmt.Sprintf("Hungarian returned an unexpected error on a %dx%d instance: %v", n, n, err),
		}}
	}

	var vs []Violation
	// Total cost: compared exactly against the brute-force optimum (integers).
	want := matchingBruteForceAssign(costInt, n)
	if int(got.TotalCost) != want || got.TotalCost != float64(want) {
		vs = append(vs, matchingDiverge(tick, "Hungarian",
			fmt.Sprintf("total cost %v disagrees with the brute-force optimum %d (n=%d)", got.TotalCost, want, n))...)
	}
	// RowToCol must be a permutation of [0,n) for a square instance, and its
	// induced cost must equal TotalCost.
	vs = append(vs, matchingValidateAssignment(tick, costInt, n, got)...)
	return vs
}

// matchingBruteForceAssign returns the minimum total cost over all n!
// row->column bijections, by exhaustive permutation. n is bounded by
// matchingAssignMax (<= 6), so n! is small. This is the independent reference
// for Hungarian's optimum.
func matchingBruteForceAssign(cost []int, n int) int {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	best := -1
	matchingPermute(perm, 0, func(p []int) {
		sum := 0
		for row, col := range p {
			sum += cost[row*n+col]
		}
		if best == -1 || sum < best {
			best = sum
		}
	})
	return best
}

// matchingPermute enumerates every permutation of perm[k:] in place (Heap-style
// prefix recursion), invoking visit on each complete permutation. The order of
// enumeration is fixed and independent of any map, preserving determinism.
func matchingPermute(perm []int, k int, visit func([]int)) {
	if k == len(perm) {
		visit(perm)
		return
	}
	for i := k; i < len(perm); i++ {
		perm[k], perm[i] = perm[i], perm[k]
		matchingPermute(perm, k+1, visit)
		perm[k], perm[i] = perm[i], perm[k]
	}
}

// matchingValidateAssignment asserts RowToCol is a permutation of [0,n) and that
// summing the chosen cells reproduces TotalCost exactly.
func matchingValidateAssignment(tick int64, cost []int, n int, a search.Assignment) []Violation {
	if len(a.RowToCol) != n {
		return matchingDiverge(tick, "Hungarian",
			fmt.Sprintf("RowToCol length %d != n %d", len(a.RowToCol), n))
	}
	seen := make([]bool, n)
	sum := 0
	for row, col := range a.RowToCol {
		if col < 0 || col >= n {
			return matchingDiverge(tick, "Hungarian",
				fmt.Sprintf("RowToCol[%d]=%d out of range [0,%d)", row, col, n))
		}
		if seen[col] {
			return matchingDiverge(tick, "Hungarian",
				fmt.Sprintf("RowToCol assigns column %d twice (not a permutation)", col))
		}
		seen[col] = true
		sum += cost[row*n+col]
	}
	if float64(sum) != a.TotalCost {
		return matchingDiverge(tick, "Hungarian",
			fmt.Sprintf("RowToCol induces cost %d but TotalCost reports %v", sum, a.TotalCost))
	}
	return nil
}

// matchingDiverge builds a single matching-family divergence violation.
func matchingDiverge(tick int64, algo, msg string) []Violation {
	return []Violation{{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo, Message: msg}}
}
