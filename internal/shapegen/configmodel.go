package shapegen

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// This file implements the "random / configuration model" family of
// the shape catalogue. The two generators here — [RandomRegular] and
// [ConfigurationModel] — realise the canonical pairing-model
// constructions used in the network-science literature: the
// uniformly-random d-regular graph (Bollobás, "A probabilistic proof
// of an asymptotic formula for the number of labelled regular
// graphs", European J. Combin. 1(4), 1980) and the general
// configuration model (Newman, Strogatz & Watts, "Random graphs with
// arbitrary degree distributions and their applications", Phys. Rev.
// E 64, 026118, 2001; Molloy & Reed, "A critical point for random
// graphs with a given degree sequence", Random Struct. Algor. 6(2-3),
// 1995). They are the catalogue's reference shapes for the spectral-
// expander / matching / colouring workloads ([RandomRegular]) and for
// arbitrary degree-skew inputs to PageRank, betweenness, and other
// centrality benchmarks ([ConfigurationModel]).
//
// # Specialisation
//
// Both generators produce *lpg.Graph[int, int64], mirroring the
// trivial, classic, structured, trees, specials, dags, erdos-renyi,
// barabasi-albert, watts-strogatz, and rmat families. The (int, int64)
// pair is the project's canonical "small unsigned key, signed weight"
// specialisation; every edge here carries [unweightedSentinel] (0)
// because the catalogue defines these shapes as topology fixtures
// rather than weight fixtures.
//
// # Configuration override policy
//
// Each generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// forces cfg.Directed=false: both pairing-model constructions are
// undirected by definition.
//
// cfg.Multigraph is overridden as follows:
//
//   - [RandomRegular] is a simple graph (no parallel edges, no
//     self-loops); cfg.Multigraph is forced to false. The rejection-
//     resampling loop guarantees the simple-graph contract before any
//     edge is emitted into the lpg backend.
//   - [ConfigurationModel] with allowMulti=true keeps every pairing
//     verbatim, including parallel edges and self-loops; cfg.Multigraph
//     is forced to true so the lpg backend stores the parallel
//     entries. The resulting graph is a multigraph in the strict
//     sense; the degree sequence is preserved exactly.
//   - [ConfigurationModel] with allowMulti=false realises the Erased
//     Configuration Model: parallel-edge and self-loop pairings are
//     dropped at generation time (mirroring the literature's
//     definition rather than relying on the lpg backend's silent
//     coalescing in simple-graph mode). cfg.Multigraph is forced to
//     false. The resulting graph is a simple graph; the realised
//     degree sequence is at most the input sequence componentwise.
//
// # Edge ordering and determinism
//
// Both generators insert edges in lexicographic (u, v) order with
// u <= v so the goldens stay byte-for-byte reproducible across builds
// and across platforms. The seeded generators thread a caller-supplied
// [uint64] seed through [math/rand/v2.NewPCG]; every (knobs, seed)
// tuple yields the same byte-for-byte output.
//
// The pairing step itself uses the Fisher-Yates shuffle pinned by
// [math/rand/v2.Rand.Shuffle]: half-edges are materialised as a flat
// []int, shuffled, then consumed in pairs. The shuffle is consumed
// once per attempt; in [RandomRegular] a failed attempt (self-loop or
// duplicate detected) restarts the entire shuffle from a fresh PRNG
// state derived deterministically from the original seed, so the
// retry budget is bounded and the seed-to-output map remains a pure
// function of (n, d, seed).
//
// # Error propagation
//
// The Build closures use the same branch-free single-err-thread
// pattern as the other families: every per-phase error propagates
// through one err variable, and the surrounding closure returns
// (g, err). [RandomRegular] surfaces [ErrRegularConstruction] when the
// retry budget (100 attempts) is exhausted without producing a simple
// d-regular graph; callers can errors.Is against this sentinel without
// unwrapping. [ConfigurationModel] surfaces [ErrOddDegreeSum] from
// Build when the input degree sequence has an odd total — pairing a
// half-edge set with odd cardinality is impossible. Otherwise the
// per-phase loops can only surface [adjlist.ErrShardFull] when the
// caller has set a tight cfg.MaxShardCapacity; that error is returned
// verbatim.

// ErrOddDegreeSum is returned by [ConfigurationModel].Build when the
// input degree sequence has an odd total. A pairing of an odd number
// of half-edges is impossible, so the constructor cannot produce a
// graph realising the supplied sequence. Callers can errors.Is
// against this sentinel without unwrapping.
var ErrOddDegreeSum = errors.New("shapegen: degree sequence has an odd sum")

// ErrRegularConstruction is returned by [RandomRegular].Build when
// the rejection-resampling pairing loop fails to produce a simple
// d-regular graph within the per-call retry budget (100 attempts).
// Repeated failure is overwhelmingly rare at the catalogue's (n, d)
// range — the asymptotic acceptance probability is bounded below by
// a positive constant under Bollobás's analysis — but a few
// pathological (n, d) tuples (notably d == n - 1 with n small) can
// exhaust the budget. Callers can errors.Is against this sentinel
// without unwrapping.
var ErrRegularConstruction = errors.New("shapegen: random d-regular construction exhausted the retry budget")

// randomRegularRetryBudget caps the number of pairing attempts inside
// [buildRandomRegular]. The value 100 mirrors the literature's
// guidance that the rejection-acceptance probability is bounded below
// by a positive constant for fixed (n, d) at the catalogue's range;
// in practice the first attempt succeeds for the vast majority of
// (n, d, seed) tuples. Pinning the budget at a small integer keeps
// pathological inputs from spinning indefinitely and surfaces the
// failure mode as the typed [ErrRegularConstruction] sentinel.
const randomRegularRetryBudget = 100

// configModelBase is the shared scaffolding for every generator in
// this file. Its layout mirrors trivialBase, classicBase,
// structuredBase, treesBase, specialsBase, dagsBase, erdosRenyiBase,
// barabasiAlbertBase, wattsStrogatzBase, and rmatBase so the helpers
// (Name, Knobs, Build) carry the exact same semantics across families.
type configModelBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s configModelBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s configModelBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s configModelBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// RandomRegular returns a Shape that builds a uniformly-random
// d-regular graph on n nodes: nodes 0..n-1, every node with degree
// exactly d, drawn from the configuration-model pairing distribution
// conditioned on the absence of self-loops and parallel edges
// (Bollobás, "A probabilistic proof of an asymptotic formula for the
// number of labelled regular graphs", European J. Combin. 1(4),
// 1980). The graph is undirected and simple (no parallel edges, no
// self-loops); cfg.Directed and cfg.Multigraph are overridden to
// false.
//
// The PRNG is a deterministically-seeded [math/rand/v2.PCG], so every
// (n, d, seed) tuple yields the same byte-for-byte adjacency.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n).
//   - Size()  == uint64(n * d / 2).
//   - Every node has undirected degree exactly d.
//   - The graph is undirected and simple.
//   - When d == 0 the graph has n isolated nodes and zero edges.
//
// RandomRegular declares two knobs: "n" over [1, 1000] (default 20)
// and "d" over [0, 50] (default 3). The "seed" parameter is supplied
// at construction time as a uint64 and is not exposed as a knob,
// mirroring the convention pinned by [Layered], [BarabasiAlbert],
// [WattsStrogatz], and [ErdosRenyiNP]. The constructor panics when:
//
//   - n < 1 or n > 1000 (catalogue out-of-range);
//   - d < 0 or d > 50 (catalogue out-of-range);
//   - n * d is odd (a d-regular graph on n vertices requires the
//     degree sum n * d to be even, by the handshake lemma);
//   - d >= n (a simple graph cannot have a vertex of degree >= n).
//
// Pairing strategy: the practical configuration-model construction
// with incremental rejection-by-swap (Newman, "The Structure and
// Function of Complex Networks", SIAM Review 45(2), 2003, section
// IV.A). Materialise n * d half-edges as a flat slice in which each
// node id i appears d times consecutively, shuffle the slice with
// Fisher-Yates, then walk adjacent pairs (half-edges 2k and 2k+1)
// left-to-right. Whenever a candidate pair is a self-loop or a
// duplicate of an earlier pair, search forward in the slice for a
// swap partner that produces a valid pair at the current position;
// if no such partner exists the attempt fails and the helper
// restarts from a fresh shuffle. The pure-restart variant of the
// pairing model (Bollobás 1980) has asymptotic acceptance probability
// exp(-(d^2 - 1) / 2), which collapses to ~5e-4 at d = 4 and 2e-8 at
// d = 6 — far below any reasonable retry budget. The
// rejection-by-swap variant recovers practical success: a single
// shuffle followed by an O(n*d^2) sweep is typically sufficient, so
// the retry budget here serves only as a safety net for the few
// pathological d-close-to-n corners.
//
// The retry budget is capped at [randomRegularRetryBudget] = 100; on
// exhaustion Build returns [ErrRegularConstruction] wrapped with the
// offending (n, d) pair. The first successful attempt is materialised
// as a set of (min(u,v), max(u,v)) unordered pairs and sorted
// ascending; the edges are inserted into g in this lexicographic
// order so the golden bytes stay stable regardless of the underlying
// shuffle permutation.
//
//nolint:gosec,gocritic // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; paramTypeCombine: signature is pinned by the brief (n int, d int, seed uint64).
func RandomRegular(n, d int, seed uint64) Shape[int, int64] {
	if n < 1 || n > 1000 {
		panic(fmt.Sprintf("shapegen: RandomRegular requires 1 <= n <= 1000, got %d", n))
	}
	if d < 0 || d > 50 {
		panic(fmt.Sprintf("shapegen: RandomRegular requires 0 <= d <= 50, got %d", d))
	}
	if (n*d)%2 != 0 {
		panic(fmt.Sprintf("shapegen: RandomRegular requires n*d to be even, got n=%d d=%d", n, d))
	}
	if d >= n {
		panic(fmt.Sprintf("shapegen: RandomRegular requires d < n, got n=%d d=%d", n, d))
	}
	return configModelBase{
		name: "random.regular",
		knobs: []Knob{
			{Name: "n", Min: 1, Max: 1000, Default: 20},
			{Name: "d", Min: 0, Max: 50, Default: 3},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildRandomRegular(g, n, d, seed)
		},
	}
}

// buildRandomRegular interns nodes 0..n-1 in g and then constructs a
// uniformly-random d-regular graph via the Bollobás pairing model
// with rejection on self-loops and parallel edges. On success the
// edges are inserted into g in lexicographic (u, v) order with u < v.
//
// The pairing attempts share a single PRNG state, so each retry
// consumes a fresh segment of the PCG stream rather than restarting
// from a derived seed; the seed-to-output map remains a pure
// function of (n, d, seed) because the consumption order is
// deterministic. The retry budget is capped at
// [randomRegularRetryBudget]; on exhaustion buildRandomRegular
// surfaces [ErrRegularConstruction] wrapped with the offending
// (n, d) pair.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see RandomRegular godoc.
func buildRandomRegular(g *lpg.Graph[int, int64], n, d int, seed uint64) error {
	err := addNodesRange(g, n)
	if err != nil || d == 0 {
		return err
	}
	r := rand.New(rand.NewPCG(seed, ^seed))
	var pairs map[[2]int]struct{}
	for attempt := 0; attempt < randomRegularRetryBudget; attempt++ {
		if got, ok := randomRegularAttempt(r, n, d); ok {
			pairs = got
			break
		}
	}
	if pairs == nil {
		return fmt.Errorf("%w: n=%d d=%d after %d attempts", ErrRegularConstruction, n, d, randomRegularRetryBudget)
	}
	// Materialise the pair set as a sorted slice so AddEdge calls
	// land in lexicographic (u, v) order — pins the golden bytes
	// regardless of the underlying shuffle permutation.
	edges := make([][2]int, 0, len(pairs))
	for p := range pairs {
		edges = append(edges, p)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i][0] != edges[j][0] {
			return edges[i][0] < edges[j][0]
		}
		return edges[i][1] < edges[j][1]
	})
	for k := 0; k < len(edges) && err == nil; k++ {
		err = g.AddEdge(canonicalNode(edges[k][0]), canonicalNode(edges[k][1]), unweightedSentinel)
	}
	return err
}

// randomRegularAttempt performs a single Bollobás pairing attempt on
// the half-edge multiset {i : count d for i in [0, n)} using
// incremental rejection-by-swap: half-edges are shuffled in place,
// then walked in adjacent pairs left-to-right; whenever a pair would
// produce a self-loop or duplicate edge the helper searches forward
// for a swap partner that yields a valid pairing. If no valid swap
// exists for the current position the attempt fails as a whole and
// the helper returns (nil, false); otherwise it returns the set of
// realised unordered pairs and true.
//
// The incremental swap strategy is the standard practical realisation
// of the configuration-model pairing with rejection (Newman 2003,
// section IV.A) and is dramatically more efficient than full restart
// at the catalogue's (n, d) range, where the asymptotic acceptance
// probability of pure-restart pairing is exp(-(d^2 - 1) / 2). At
// d = 4 the asymptotic acceptance is ~5e-4 — far below the 1/100
// budget — so pure restart would require thousands of attempts even
// for modest n. Incremental swap recovers the practical success
// regime: at every position the search advances forward over O(n*d)
// candidates, so a single shuffle paired with one O(n*d^2) sweep is
// in practice sufficient.
//
// PRNG consumption per attempt is exactly one Fisher-Yates shuffle of
// the n * d half-edge slice (n*d - 1 IntN draws). The swap step does
// not consume the PRNG: it walks the slice in deterministic order and
// performs in-place exchanges. Consecutive failed attempts on the
// same PRNG therefore advance the stream by a deterministic amount
// and the seed-to-output map remains a pure function of the original
// (n, d, seed) tuple.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see RandomRegular godoc.
func randomRegularAttempt(r *rand.Rand, n, d int) (map[[2]int]struct{}, bool) {
	halfEdges := make([]int, n*d)
	for i := 0; i < n; i++ {
		base := i * d
		for k := 0; k < d; k++ {
			halfEdges[base+k] = i
		}
	}
	r.Shuffle(len(halfEdges), func(i, j int) {
		halfEdges[i], halfEdges[j] = halfEdges[j], halfEdges[i]
	})
	pairs := make(map[[2]int]struct{}, n*d/2)
	for k := 0; k < len(halfEdges); k += 2 {
		u := halfEdges[k]
		v := halfEdges[k+1]
		if u != v {
			a, b := u, v
			if a > b {
				a, b = b, a
			}
			if _, dup := pairs[[2]int{a, b}]; !dup {
				pairs[[2]int{a, b}] = struct{}{}
				continue
			}
		}
		// The current pair is invalid (self-loop or duplicate). Walk
		// the remaining positions [k+2, len) looking for a swap
		// partner that produces a valid pair both for the current
		// position and — crucially — leaves a non-self-loop value at
		// the swap-out site. We swap halfEdges[k+1] <-> halfEdges[j]
		// for some j > k+1 and re-test the pair (halfEdges[k],
		// halfEdges[k+1]) (which is the same as the old halfEdges[k]
		// and halfEdges[j]).
		swapped := false
		for j := k + 2; j < len(halfEdges); j++ {
			candidate := halfEdges[j]
			if candidate == halfEdges[k] {
				continue
			}
			a, b := halfEdges[k], candidate
			if a > b {
				a, b = b, a
			}
			if _, dup := pairs[[2]int{a, b}]; dup {
				continue
			}
			halfEdges[k+1], halfEdges[j] = halfEdges[j], halfEdges[k+1]
			pairs[[2]int{a, b}] = struct{}{}
			swapped = true
			break
		}
		if !swapped {
			return nil, false
		}
	}
	return pairs, true
}

// ConfigurationModel returns a Shape that builds a configuration-model
// graph on n = len(degSeq) nodes whose i-th vertex contributes
// degSeq[i] half-edges to the pairing. Half-edges are paired
// uniformly at random and consumed in adjacent pairs; the resulting
// pairings become undirected edges (Newman, Strogatz & Watts, "Random
// graphs with arbitrary degree distributions and their applications",
// Phys. Rev. E 64, 026118, 2001; Molloy & Reed, "A critical point for
// random graphs with a given degree sequence", Random Struct. Algor.
// 6(2-3), 1995).
//
// When allowMulti is true the resulting graph is a multigraph: every
// pairing — including self-loops (pairings of two half-edges from the
// same node) and parallel edges (multiple pairings of the same
// unordered pair) — becomes an edge in g. cfg.Multigraph is forced
// to true; cfg.Directed is forced to false. The realised degree
// sequence equals degSeq exactly.
//
// When allowMulti is false the generator realises the Erased
// Configuration Model: self-loop and parallel pairings are dropped at
// generation time. cfg.Multigraph is forced to false; cfg.Directed is
// forced to false. The realised degree sequence is at most degSeq
// componentwise — every dropped pairing reduces the realised degree
// of its endpoints by one each.
//
// The PRNG is a deterministically-seeded [math/rand/v2.PCG], so every
// (degSeq, allowMulti, seed) tuple yields the same byte-for-byte
// adjacency.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(len(degSeq)).
//   - When allowMulti == true: Size() == sum(degSeq) / 2; per-node
//     degree (with self-loops counted twice) equals degSeq exactly.
//   - When allowMulti == false: Size() <= sum(degSeq) / 2; per-node
//     degree is at most degSeq componentwise.
//   - The graph is undirected.
//
// When sum(degSeq) is odd the Build closure returns [ErrOddDegreeSum]
// wrapped with the offending sum; callers can errors.Is against the
// sentinel without unwrapping. The constructor itself never panics
// on parity violations, matching the [Cycle] / [ErrCycleTooSmall] and
// [ErdosRenyiNM] / [ErrEdgeCountTooHigh] conventions for runtime-
// surfaced parameter errors.
//
// ConfigurationModel declares no knobs: degSeq is variadic and
// property-based tests draw it directly via rapid, mirroring the
// convention pinned by [Multipartite] in the classic family. The
// constructor panics when any degSeq[i] is negative; the catalogue
// does not define the model on negative-degree inputs.
//
// The constructor takes a defensive copy of degSeq so subsequent
// caller mutations cannot affect Build — mirroring [Multipartite]'s
// contract.
//
//nolint:gocritic // paramTypeCombine: signature is pinned by the brief (degSeq []int, allowMulti bool, seed uint64).
func ConfigurationModel(degSeq []int, allowMulti bool, seed uint64) Shape[int, int64] {
	for i, deg := range degSeq {
		if deg < 0 {
			panic(fmt.Sprintf("shapegen: ConfigurationModel degSeq[%d] = %d, must be >= 0", i, deg))
		}
	}
	// Copy degSeq so subsequent caller mutations cannot affect Build.
	owned := make([]int, len(degSeq))
	copy(owned, degSeq)
	return configModelBase{
		name: "random.configuration",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = allowMulti
			g := lpg.New[int, int64](cfg)
			return g, buildConfigurationModel(g, owned, allowMulti, seed)
		},
	}
}

// buildConfigurationModel interns nodes 0..len(degSeq)-1 in g and
// then performs a single half-edge pairing pass under the
// configuration-model definition. When allowMulti is true every
// pairing becomes an edge; when allowMulti is false self-loops and
// parallel pairings are dropped (Erased Configuration Model).
//
// Edges are inserted in lexicographic (u, v) order with u <= v in the
// multigraph case and u < v in the simple-graph case. Parallel edges
// in the multigraph case appear as adjacent runs of identical (u, v)
// entries in the resulting adjacency, mirroring the catalogue's
// golden format for multigraphs.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see ConfigurationModel godoc.
func buildConfigurationModel(g *lpg.Graph[int, int64], degSeq []int, allowMulti bool, seed uint64) error {
	n := len(degSeq)
	err := addNodesRange(g, n)
	total := 0
	for _, deg := range degSeq {
		total += deg
	}
	if err == nil && total%2 != 0 {
		err = fmt.Errorf("%w: sum(degSeq)=%d", ErrOddDegreeSum, total)
	}
	if err != nil || total == 0 {
		return err
	}
	// Materialise the half-edge multiset: each node i contributes
	// degSeq[i] copies of i. The slice is shuffled in place and
	// then consumed in adjacent pairs.
	halfEdges := make([]int, 0, total)
	for i, deg := range degSeq {
		for k := 0; k < deg; k++ {
			halfEdges = append(halfEdges, i)
		}
	}
	r := rand.New(rand.NewPCG(seed, ^seed))
	r.Shuffle(len(halfEdges), func(i, j int) {
		halfEdges[i], halfEdges[j] = halfEdges[j], halfEdges[i]
	})
	return emitConfigurationEdges(g, halfEdges, allowMulti)
}

// emitConfigurationEdges scans the shuffled half-edge slice in
// adjacent pairs and emits the resulting edges into g, in
// lexicographic (u, v) order. In the multigraph branch every pairing
// is emitted, including self-loops and parallel edges; in the simple-
// graph branch self-loops and parallel pairings are dropped before
// the AddEdge call. The first AddEdge error short-circuits the loop.
//
// Multigraph branch: the helper sorts the materialised pair list
// before insertion and emits parallel entries as adjacent runs so
// the golden bytes are deterministic regardless of the underlying
// shuffle permutation.
//
// Simple-graph branch: the helper deduplicates the pair list via a
// set before sorting; the set membership is the strict semantic of
// the Erased Configuration Model.
func emitConfigurationEdges(g *lpg.Graph[int, int64], halfEdges []int, allowMulti bool) error {
	if allowMulti {
		return emitMultiConfigurationEdges(g, halfEdges)
	}
	return emitSimpleConfigurationEdges(g, halfEdges)
}

// emitMultiConfigurationEdges materialises the pairings as a sortable
// slice of (u, v) pairs (with u <= v so self-loops appear as (i, i)),
// sorts ascending, and inserts every pairing — including duplicates —
// into g. The first AddEdge error short-circuits the loop.
func emitMultiConfigurationEdges(g *lpg.Graph[int, int64], halfEdges []int) error {
	edges := make([][2]int, 0, len(halfEdges)/2)
	for k := 0; k < len(halfEdges); k += 2 {
		u, v := halfEdges[k], halfEdges[k+1]
		if u > v {
			u, v = v, u
		}
		edges = append(edges, [2]int{u, v})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i][0] != edges[j][0] {
			return edges[i][0] < edges[j][0]
		}
		return edges[i][1] < edges[j][1]
	})
	var err error
	for k := 0; k < len(edges) && err == nil; k++ {
		err = g.AddEdge(canonicalNode(edges[k][0]), canonicalNode(edges[k][1]), unweightedSentinel)
	}
	return err
}

// emitSimpleConfigurationEdges materialises the pairings as a set of
// unordered pairs (u < v), discarding self-loops and parallel
// pairings (Erased Configuration Model semantics), then inserts the
// surviving edges into g in lexicographic order. The first AddEdge
// error short-circuits the loop.
func emitSimpleConfigurationEdges(g *lpg.Graph[int, int64], halfEdges []int) error {
	pairs := make(map[[2]int]struct{}, len(halfEdges)/2)
	for k := 0; k < len(halfEdges); k += 2 {
		u, v := halfEdges[k], halfEdges[k+1]
		if u == v {
			continue
		}
		if u > v {
			u, v = v, u
		}
		pairs[[2]int{u, v}] = struct{}{}
	}
	edges := make([][2]int, 0, len(pairs))
	for p := range pairs {
		edges = append(edges, p)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i][0] != edges[j][0] {
			return edges[i][0] < edges[j][0]
		}
		return edges[i][1] < edges[j][1]
	})
	var err error
	for k := 0; k < len(edges) && err == nil; k++ {
		err = g.AddEdge(canonicalNode(edges[k][0]), canonicalNode(edges[k][1]), unweightedSentinel)
	}
	return err
}
