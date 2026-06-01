package shapegen

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// This file implements the "random / Erdős-Rényi" family of the shape
// catalogue. The two generators here — [ErdosRenyiNP] and
// [ErdosRenyiNM] — realise the canonical Bernoulli-pair G(n, p) and
// fixed-edge G(n, m) random-graph models (Erdős & Rényi, "On the
// Evolution of Random Graphs", 1960; Gilbert, "Random Graphs", 1959).
// They are the catalogue's reference shapes for the giant-component
// phase transition that drives WCC-style benchmarks, and the dense-
// random baseline for Dijkstra, PageRank, and community-detection
// algorithms.
//
// # Specialisation
//
// Both generators produce *lpg.Graph[int, int64], mirroring the
// trivial, classic, structured, trees, specials, and dags families.
// The (int, int64) pair is the project's canonical "small unsigned
// key, signed weight" specialisation; every edge here carries
// [unweightedSentinel] (0) because the catalogue defines these shapes
// as topology fixtures rather than weight fixtures.
//
// # Configuration override policy
//
// Each generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// forces cfg.Directed=false and cfg.Multigraph=false: both Erdős-Rényi
// shapes are undirected simple graphs (no parallel edges, no self-
// loops) by definition.
//
// # Edge ordering and determinism
//
// Each constructor inserts edges in a deterministic order so the
// goldens stay byte-for-byte reproducible across builds and across
// platforms. The seeded generators thread a caller-supplied [uint64]
// seed through [math/rand/v2.NewPCG] so every (knobs, seed) tuple
// yields the same byte-for-byte output. The `pPercent` integer knob
// matches the percent-of-max convention pinned by [Layered] (T58.8):
// a single integer knob in [0, 100] interpreted as a probability.
//
// # Error propagation
//
// The Build closures use the same branch-free single-err-thread
// pattern as the other families: every per-phase error propagates
// through one err variable, and the surrounding closure returns
// (g, err). [ErdosRenyiNM] surfaces [ErrEdgeCountTooHigh] when the
// caller asks for more edges than C(n, 2) admits; callers can
// errors.Is against this sentinel without unwrapping. Otherwise the
// per-phase loops can only surface [adjlist.ErrShardFull] when the
// caller has set a tight cfg.MaxShardCapacity. That error is
// returned verbatim.

// ErrEdgeCountTooHigh is returned by [ErdosRenyiNM].Build when the
// requested edge count m exceeds C(n, 2), the number of unordered
// pairs available in a simple graph on n nodes. Callers can
// errors.Is against this sentinel without unwrapping.
var ErrEdgeCountTooHigh = errors.New("shapegen: edge count exceeds C(n, 2)")

// erdosRenyiBase is the shared scaffolding for every generator in
// this file. Its layout mirrors trivialBase, classicBase,
// structuredBase, treesBase, specialsBase, and dagsBase so the
// helpers (Name, Knobs, Build) carry the exact same semantics across
// families.
type erdosRenyiBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s erdosRenyiBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s erdosRenyiBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s erdosRenyiBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// ErdosRenyiNP returns a Shape that builds an Erdős-Rényi G(n, p)
// random graph: nodes 0..n-1 with every unordered pair {i, j}
// connected independently by an edge with probability p = pPercent/100.
// The graph is undirected and simple (no parallel edges, no
// self-loops); cfg.Directed and cfg.Multigraph are overridden to
// false.
//
// The pPercent knob follows the percent-of-max convention pinned by
// [Layered] (T58.8): an integer in [0, 100] that maps to the Bernoulli
// success probability via a single IntN(100) draw per pair. The
// PRNG is a deterministically-seeded [math/rand/v2.PCG], so every
// (n, pPercent, seed) tuple yields the same byte-for-byte adjacency.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n).
//   - 0 <= Size() <= uint64(n * (n - 1) / 2).
//   - The graph is undirected and simple.
//   - When pPercent == 0 the graph has no edges; when pPercent == 100
//     every unordered pair is connected and the graph coincides with
//     the undirected complete graph K_n.
//   - For large enough n, the expected edge count C(n, 2) * p falls
//     within the +/-3 sigma window of a Binomial(C(n, 2), p)
//     distribution — pinned by TestRandom_ErdosRenyiNP_ExpectedEdgeCount
//     at (n=200, pPercent=10, 100 seeds).
//
// ErdosRenyiNP declares two knobs: "n" over [0, 1000] (default 50)
// and "p" over [0, 100] (default 10). The "seed" parameter is supplied
// at construction time as a uint64 and is not exposed as a knob,
// mirroring the [Layered] convention. The constructor panics when n
// is negative or above 1000, or when pPercent is negative or above
// 100.
//
// Edges are inserted in lexicographic (i, j) order with i < j: for
// every i in [0, n), for every j in [i + 1, n), the candidate edge
// (i, j) is drawn from a single IntN(100); the edge is inserted iff
// the draw is strictly less than pPercent. The PRNG is consumed
// exactly once per unordered pair, regardless of the outcome, so the
// seed-to-output map is a pure function of the (n, pPercent, seed)
// tuple.
//
//nolint:gosec,gocritic // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; paramTypeCombine: signature is pinned by the brief (n int, pPercent int, seed uint64).
func ErdosRenyiNP(n int, pPercent int, seed uint64) Shape[int, int64] {
	if n < 0 || n > 1000 {
		panic(fmt.Sprintf("shapegen: ErdosRenyiNP requires 0 <= n <= 1000, got %d", n))
	}
	if pPercent < 0 || pPercent > 100 {
		panic(fmt.Sprintf("shapegen: ErdosRenyiNP requires 0 <= pPercent <= 100, got %d", pPercent))
	}
	return erdosRenyiBase{
		name: "random.erdos-renyi-np",
		knobs: []Knob{
			{Name: "n", Min: 0, Max: 1000, Default: 50},
			{Name: "p", Min: 0, Max: 100, Default: 10},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildErdosRenyiNP(g, n, pPercent, seed)
		},
	}
}

// buildErdosRenyiNP interns nodes 0..n-1 in g and inserts each
// unordered pair (i, j) with i < j as an edge iff a Bernoulli(p)
// draw from a deterministically-seeded [math/rand/v2.PCG] succeeds.
// The PRNG is consumed exactly once per (i, j) pair in lexicographic
// order, so the seed-to-output map is a pure function of the
// (n, pPercent, seed) tuple. The first AddEdge error short-circuits
// every loop.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see ErdosRenyiNP godoc.
func buildErdosRenyiNP(g *lpg.Graph[int, int64], n, pPercent int, seed uint64) error {
	err := addNodesRange(g, n)
	r := rand.New(rand.NewPCG(seed, seed))
	for i := 0; i < n && err == nil; i++ {
		for j := i + 1; j < n && err == nil; j++ {
			// One PCG draw per unordered pair; comparison against
			// pPercent realises the Bernoulli outcome.
			draw := r.IntN(100)
			if draw >= pPercent {
				continue
			}
			err = g.AddEdge(canonicalNode(i), canonicalNode(j), unweightedSentinel)
		}
	}
	return err
}

// ErdosRenyiNM returns a Shape that builds an Erdős-Rényi G(n, m)
// random graph: nodes 0..n-1 with exactly m edges drawn uniformly
// without replacement from the C(n, 2) unordered pairs. The graph is
// undirected and simple (no parallel edges, no self-loops);
// cfg.Directed and cfg.Multigraph are overridden to false.
//
// The PRNG is a deterministically-seeded [math/rand/v2.PCG], so every
// (n, m, seed) tuple yields the same byte-for-byte adjacency.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n).
//   - Size()  == uint64(m) when m <= C(n, 2).
//   - The graph is undirected and simple — every emitted edge is a
//     distinct unordered pair (i, j) with i < j.
//
// When m > C(n, 2) the Build closure returns [ErrEdgeCountTooHigh]
// alongside a nil graph; callers can errors.Is against the sentinel
// without unwrapping. The constructor itself never panics on a high
// m, matching the [Cycle] / [ErrCycleTooSmall] convention for
// runtime-surfaced parameter errors.
//
// ErdosRenyiNM declares two knobs: "n" over [0, 1000] (default 50)
// and "m" over [0, 499_500] (default 50). The upper bound 499_500 is
// C(1000, 2), the maximum number of edges admissible by the n knob's
// upper bound. The "seed" parameter is supplied at construction time
// as a uint64 and is not exposed as a knob, mirroring the [Layered]
// convention. The constructor panics when n is negative or above
// 1000, or when m is negative or above 499_500.
//
// Sampling strategy: Floyd's combination sampling algorithm
// (Bentley & Floyd, "A Sample of Brilliance", CACM 30(9), 1987),
// which draws m unique integers from [0, C(n, 2)) in O(m) time and
// O(m) space using a single PCG draw per accepted element. The
// resulting integers are interpreted as flat indices into the
// lexicographically-ordered list of unordered pairs (i, j) with
// i < j; the inverse mapping recovers (i, j) from the flat index in
// O(1) via the closed-form pairIndexToIJ helper. After sampling, the
// flat indices are sorted ascending so edges are inserted in
// lexicographic (i, j) order, pinning the golden bytes regardless of
// the draw permutation.
//
// Floyd's algorithm is chosen over reservoir sampling because the
// catalogue's typical use is m << C(n, 2); Floyd's O(m) cost beats
// reservoir's O(C(n, 2)) by orders of magnitude in that regime,
// without sacrificing uniformity (every m-subset is equally likely
// by induction on m).
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see ErdosRenyiNM godoc.
func ErdosRenyiNM(n, m int, seed uint64) Shape[int, int64] {
	if n < 0 || n > 1000 {
		panic(fmt.Sprintf("shapegen: ErdosRenyiNM requires 0 <= n <= 1000, got %d", n))
	}
	if m < 0 || m > 499_500 {
		panic(fmt.Sprintf("shapegen: ErdosRenyiNM requires 0 <= m <= 499500, got %d", m))
	}
	return erdosRenyiBase{
		name: "random.erdos-renyi-nm",
		knobs: []Knob{
			{Name: "n", Min: 0, Max: 1000, Default: 50},
			{Name: "m", Min: 0, Max: 499_500, Default: 50},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			maxEdges := n * (n - 1) / 2
			if m > maxEdges {
				return nil, fmt.Errorf("%w: requested m=%d, C(n=%d, 2)=%d", ErrEdgeCountTooHigh, m, n, maxEdges)
			}
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildErdosRenyiNM(g, n, m, seed)
		},
	}
}

// buildErdosRenyiNM interns nodes 0..n-1 in g and inserts exactly m
// unordered edges drawn uniformly without replacement from the
// C(n, 2) pairs. The sampling is performed by [floydSample] using a
// deterministically-seeded [math/rand/v2.PCG] source; the resulting
// flat indices are mapped back to (i, j) pairs via [pairIndexToIJ]
// and sorted ascending so edges are inserted in lexicographic
// (i, j) order. The first AddEdge error short-circuits the loop.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see ErdosRenyiNM godoc.
func buildErdosRenyiNM(g *lpg.Graph[int, int64], n, m int, seed uint64) error {
	err := addNodesRange(g, n)
	if err != nil || m == 0 {
		return err
	}
	maxEdges := n * (n - 1) / 2
	r := rand.New(rand.NewPCG(seed, seed))
	picks := floydSample(r, maxEdges, m)
	sort.Ints(picks)
	for k := 0; k < len(picks) && err == nil; k++ {
		i, j := pairIndexToIJ(n, picks[k])
		err = g.AddEdge(canonicalNode(i), canonicalNode(j), unweightedSentinel)
	}
	return err
}

// floydSample returns m distinct integers from [0, total) drawn
// uniformly without replacement, using Floyd's combination sampling
// algorithm (Bentley & Floyd, "A Sample of Brilliance", CACM 30(9),
// 1987). The algorithm runs in O(m) time and uses O(m) auxiliary
// space; every m-subset of [0, total) is equally likely.
//
// Pre-condition: 0 <= m <= total. The caller is responsible for
// rejecting m > total before invoking this helper.
//
// Implementation notes:
//
//   - The classical Floyd loop runs for j in [total - m, total). At
//     each step it draws t = r.IntN(j + 1); if t is already in the
//     selection set it adds j instead, otherwise it adds t. This
//     preserves uniformity inductively.
//   - The selection set is materialised as a map[int]struct{} so
//     membership lookups stay O(1) without sacrificing the O(m)
//     bound. The returned slice carries the elements in insertion
//     order; callers that need a sorted order must sort explicitly.
func floydSample(r *rand.Rand, total, m int) []int {
	picks := make([]int, 0, m)
	seen := make(map[int]struct{}, m)
	for j := total - m; j < total; j++ {
		t := r.IntN(j + 1)
		if _, ok := seen[t]; ok {
			seen[j] = struct{}{}
			picks = append(picks, j)
			continue
		}
		seen[t] = struct{}{}
		picks = append(picks, t)
	}
	return picks
}

// pairIndexToIJ maps a flat index k in [0, C(n, 2)) to the unordered
// pair (i, j) with i < j it represents under lexicographic ordering:
// (0,1), (0,2), ..., (0, n-1), (1, 2), ..., (n-2, n-1). The
// closed-form inverse is computed in O(1) without floating point:
// row i contains (n - 1 - i) pairs starting from column i + 1, so a
// linear scan over rows recovers i, then j follows as
// i + 1 + (k - rowStart).
//
// The linear scan is bounded by n - 1 <= 999 under the constructor's
// n upper bound, well inside the per-edge cost budget for ErdosRenyiNM
// at the catalogue's short-layer ceiling. A constant-time variant
// using the integer-square-root closed form is available but adds
// complexity without observable benefit at this scale.
//
// Pre-condition: 0 <= k < n * (n - 1) / 2. The function returns
// (-1, -1) when k is out of range; under the catalogue contract the
// caller never reaches this branch because floydSample bounds k
// inside [0, C(n, 2)).
func pairIndexToIJ(n, k int) (i, j int) {
	rowStart := 0
	for i = 0; i < n-1; i++ {
		rowLen := n - 1 - i
		if k < rowStart+rowLen {
			return i, i + 1 + (k - rowStart)
		}
		rowStart += rowLen
	}
	return -1, -1
}
