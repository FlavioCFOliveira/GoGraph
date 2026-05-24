package shapegen

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// This file implements the "random / Lancichinetti-Fortunato-Radicchi"
// generator of the shape catalogue. [LFR] realises a *simplified*
// variant of the LFR benchmark (Lancichinetti, Fortunato & Radicchi,
// "Benchmark graphs for testing community detection algorithms",
// Phys. Rev. E 78, 046110, 2008): a heterogeneous community graph
// whose node-degree distribution and community-size distribution both
// follow truncated power laws, with a tunable mixing parameter mu
// controlling the fraction of every node's edges that cross community
// boundaries. The LFR benchmark is the standard input for modern
// community-detection papers (Leiden, label propagation, Infomap,
// spectral clustering); the GoGraph catalogue ships [LFR] as the
// reference scale-free heterogeneous-community fixture and pins the
// distribution-fit ACs in the soak layer.
//
// # Simplification vs the reference algorithm
//
// The 2008 reference algorithm (the Python "lfr_benchmark_graphs"
// implementation that ships with NetworkX is the canonical port)
// performs an iterative repair pass that adjusts node-to-community
// assignment and per-node stub split until every realised node has
// exactly the requested intra/inter ratio. That repair loop has no
// closed-form termination bound and the reference Python code falls
// back to heuristic retries with hand-tuned thresholds that have
// drifted across releases.
//
// The simplified variant shipped here ships the four key statistical
// properties of the reference paper without the iterative repair:
//
//  1. Node-degree sequence drawn from a truncated power-law
//     p(k) ∝ k^(-gamma) on [minDeg, maxDeg], with minDeg = max(1,
//     avgDeg/2). The truncated inverse-CDF sampler reproduces the
//     reference paper's tail exponent gamma within +/- 5% at n = 5000.
//  2. Community-size sequence drawn from a truncated power-law
//     p(s) ∝ s^(-beta) on [minCom, maxCom] until sum >= n; the last
//     community is trimmed so the partition covers exactly n nodes.
//     The tail exponent beta is recovered within +/- 5% at n = 5000.
//  3. Node-to-community assignment is performed in degree-descending
//     order; a node is admitted into the first community whose
//     remaining capacity (in nodes) is positive and whose member
//     count is large enough to accommodate the node's intra-degree
//     ceiling. The greedy admission policy is a relaxation of the
//     reference Python code's degree-aware admission and matches the
//     same statistical envelope at n = 5000.
//  4. Edge realisation follows the Erased Configuration Model: per
//     node, the requested intra-stub count is the round of
//     (1 - mu/100) * deg, and the inter-stub count is the remainder.
//     Intra-stubs are paired within each community via Fisher-Yates
//     shuffle + adjacent-pair consumption, and inter-stubs are paired
//     globally with the same procedure but with a per-community
//     bucket filter that drops any pairing whose endpoints share a
//     community. Self-loops, parallel edges, and pairings that
//     violate the community filter are dropped at generation time;
//     the realised mixing parameter therefore converges to the
//     requested mu/100 from above as n grows.
//
// The simplified variant is **not pixel-perfect** to the reference
// Python lfr_benchmark_graphs; it does **match the key statistical
// properties** documented in the four bullets above, with the
// concentration windows the AC pins (+/- 5% on the tail exponents,
// +/- 5 percentage points on the realised mixing fraction).
//
// # Specialisation
//
// LFR produces *lpg.Graph[int, int64], mirroring every other generator
// in the random family. The (int, int64) pair is the project's
// canonical "small unsigned key, signed weight" choice; every edge
// here carries [unweightedSentinel] (0) because the catalogue defines
// LFR as a topology fixture and a community fixture — the community
// information lives in the per-node "community_id" property, not on
// the edges.
//
// # Configuration override policy
//
// The generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// forces cfg.Directed=false and cfg.Multigraph=false: the LFR
// benchmark is an undirected simple graph by definition.
//
// # Ground-truth community labels
//
// Every node carries its zero-based community index as an
// [lpg.Int64Value] property under the key "community_id". Consumers
// running community-detection algorithms read the labels back through
// [lpg.Graph.GetNodeProperty] to score recovery quality (NMI, ARI,
// accuracy) against the planted partition.
//
// # Error propagation
//
// The Build closure uses the same branch-free single-err-thread
// pattern as the other families: every per-phase error propagates
// through one err variable, and the surrounding closure returns
// (g, err). The per-phase loops can only surface
// [adjlist.ErrShardFull] when the caller has set a tight
// cfg.MaxShardCapacity; that error is returned verbatim. The
// node-to-community assignment phase surfaces
// [ErrLFRAssignmentFailed] when the greedy admission exhausts every
// community without placing a node; this is overwhelmingly rare at
// the catalogue's (n, avgDeg, maxDeg, minCom, maxCom) sweep but can
// be reached at degenerate corners (for example maxDeg >= maxCom and
// every community at exactly maxCom).

// ErrLFRAssignmentFailed is returned by [LFR].Build when the greedy
// node-to-community assignment exhausts every community without
// placing a node — a condition reachable only when every realised
// community is at exact capacity and the unplaced node's intra-
// degree ceiling exceeds the per-community admission budget. Callers
// can errors.Is against this sentinel without unwrapping.
var ErrLFRAssignmentFailed = errors.New("shapegen: LFR node-to-community assignment exhausted every community without placement")

// lfrBase is the per-generator scaffolding for this file. Its layout
// mirrors sbmBase, configModelBase, rmatBase, etc. so the helpers
// (Name, Knobs, Build) carry the exact same semantics across families.
type lfrBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s lfrBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s lfrBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s lfrBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// LFR returns a Shape that builds a simplified LFR community
// benchmark on n nodes. Node degrees are drawn from a truncated
// power-law with exponent gamma := gammaPercent / 100 on
// [max(1, avgDeg/2), maxDeg]; community sizes are drawn from a
// truncated power-law with exponent beta := betaPercent / 100 on
// [minCom, maxCom] until the cumulative size reaches n. Every node is
// assigned to a community by a greedy degree-descending admission
// pass; the per-node stub split places round((1 - muPercent/100) *
// deg) stubs into the intra-community pool and the remainder into
// the global inter-community pool. Edges are realised by Fisher-Yates
// pairing of the stub pools under the Erased Configuration Model
// (self-loops and parallel edges are dropped at generation time).
//
// The PRNG is a deterministically-seeded [math/rand/v2.PCG], so every
// (n, gammaPercent, betaPercent, avgDeg, maxDeg, minCom, maxCom,
// muPercent, seed) tuple yields the same byte-for-byte adjacency.
//
// # Catalogue invariants on the returned graph
//
//   - Order() == uint64(n).
//   - The graph is undirected and simple (no parallel edges, no
//     self-loops).
//   - Every node carries a "community_id" [lpg.Int64Value] property
//     whose value is the zero-based index of the community that
//     produced it.
//   - At n = 5000 over 5 seeds the empirical degree distribution
//     fits a power-law tail with exponent gamma_hat within +/- 5% of
//     gammaPercent / 100, and the empirical community-size
//     distribution fits a power-law with exponent beta_hat within
//     +/- 5% of betaPercent / 100, both measured by CCDF log-log
//     least-squares regression on the [kMin, max] tail (k_min = 5
//     for degrees, s_min = minCom for sizes). The statistical test
//     lives in the soak layer; the regression method mirrors
//     [fitPowerLawExponentCCDF] from the Barabási-Albert family.
//   - The aggregate inter-edge fraction across communities at n =
//     5000 over 5 seeds lies within +/- 5 percentage points of
//     muPercent / 100. The statistical test lives in the soak layer.
//
// # Parameters and validation
//
// LFR declares eight knobs: "n" over [50, 50000] (default 1000),
// "gamma" over [200, 350] (default 300), "beta" over [100, 200]
// (default 150), "avgDeg" over [2, 50] (default 10), "maxDeg" over
// [5, 200] (default 50), "minCom" over [5, 100] (default 10),
// "maxCom" over [10, 1000] (default 50), and "mu" over [0, 100]
// (default 30). The "seed" parameter is supplied at construction
// time as a uint64 and is not exposed as a knob, mirroring the
// convention pinned by every other random-family generator. The
// constructor panics when:
//
//   - n < 50 or n > 50000;
//   - gammaPercent < 200 or gammaPercent > 350;
//   - betaPercent < 100 or betaPercent > 200;
//   - avgDeg < 2 or avgDeg > 50;
//   - maxDeg < 5 or maxDeg > 200;
//   - maxDeg < avgDeg (the truncated power-law tail must include the
//     mean);
//   - minCom < 5 or minCom > 100;
//   - maxCom < 10 or maxCom > 1000;
//   - maxCom < minCom;
//   - muPercent < 0 or muPercent > 100.
//
//nolint:gocritic // paramTypeCombine: signature is pinned by the brief (n, gammaPercent, betaPercent, avgDeg, maxDeg, minCom, maxCom, muPercent int, seed uint64).
func LFR(n, gammaPercent, betaPercent, avgDeg, maxDeg, minCom, maxCom, muPercent int, seed uint64) Shape[int, int64] {
	validateLFRParams(n, gammaPercent, betaPercent, avgDeg, maxDeg, minCom, maxCom, muPercent)
	return lfrBase{
		name: "random.lfr",
		knobs: []Knob{
			{Name: "n", Min: 50, Max: 50_000, Default: 1000},
			{Name: "gamma", Min: 200, Max: 350, Default: 300},
			{Name: "beta", Min: 100, Max: 200, Default: 150},
			{Name: "avgDeg", Min: 2, Max: 50, Default: 10},
			{Name: "maxDeg", Min: 5, Max: 200, Default: 50},
			{Name: "minCom", Min: 5, Max: 100, Default: 10},
			{Name: "maxCom", Min: 10, Max: 1000, Default: 50},
			{Name: "mu", Min: 0, Max: 100, Default: 30},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildLFR(g, n, gammaPercent, betaPercent, avgDeg, maxDeg, minCom, maxCom, muPercent, seed)
		},
	}
}

// validateLFRParams enforces the catalogue's out-of-range guards.
// Each branch panics with a fmt.Sprintf message that identifies the
// offending knob and value so callers see actionable diagnostics
// without unwrapping. The validation order mirrors the knob
// declaration order, so the first failure points at the first
// out-of-range knob.
//
//nolint:gocyclo,gocritic // gocyclo: 12 disjoint range checks form one validation pass; splitting would obscure intent. paramTypeCombine: signature mirrors LFR.
func validateLFRParams(n, gammaPercent, betaPercent, avgDeg, maxDeg, minCom, maxCom, muPercent int) {
	if n < 50 || n > 50_000 {
		panic(fmt.Sprintf("shapegen: LFR requires 50 <= n <= 50000, got %d", n))
	}
	if gammaPercent < 200 || gammaPercent > 350 {
		panic(fmt.Sprintf("shapegen: LFR requires 200 <= gammaPercent <= 350, got %d", gammaPercent))
	}
	if betaPercent < 100 || betaPercent > 200 {
		panic(fmt.Sprintf("shapegen: LFR requires 100 <= betaPercent <= 200, got %d", betaPercent))
	}
	if avgDeg < 2 || avgDeg > 50 {
		panic(fmt.Sprintf("shapegen: LFR requires 2 <= avgDeg <= 50, got %d", avgDeg))
	}
	if maxDeg < 5 || maxDeg > 200 {
		panic(fmt.Sprintf("shapegen: LFR requires 5 <= maxDeg <= 200, got %d", maxDeg))
	}
	if maxDeg < avgDeg {
		panic(fmt.Sprintf("shapegen: LFR requires maxDeg >= avgDeg, got maxDeg=%d avgDeg=%d", maxDeg, avgDeg))
	}
	if minCom < 5 || minCom > 100 {
		panic(fmt.Sprintf("shapegen: LFR requires 5 <= minCom <= 100, got %d", minCom))
	}
	if maxCom < 10 || maxCom > 1000 {
		panic(fmt.Sprintf("shapegen: LFR requires 10 <= maxCom <= 1000, got %d", maxCom))
	}
	if maxCom < minCom {
		panic(fmt.Sprintf("shapegen: LFR requires maxCom >= minCom, got maxCom=%d minCom=%d", maxCom, minCom))
	}
	if muPercent < 0 || muPercent > 100 {
		panic(fmt.Sprintf("shapegen: LFR requires 0 <= muPercent <= 100, got %d", muPercent))
	}
}

// buildLFR is the top-level orchestrator for the LFR generator. It
// interns nodes 0..n-1 in g, draws the node-degree sequence and the
// community-size sequence from their respective truncated power-law
// distributions, performs the greedy degree-descending node-to-
// community assignment, attaches the "community_id" property to
// every node, splits each node's degree into intra- and inter-
// community stubs, and realises edges via the Erased Configuration
// Model on the per-community intra-stub pools and the global inter-
// stub pool. The first error short-circuits every subsequent phase,
// matching the branch-free err-thread convention shared across the
// random family.
//
//nolint:gosec,gocyclo,gocritic // G404: math/rand/v2 is the pinned PRNG for catalogue determinism. gocyclo: phase orchestration with err-gated sequencing is a single intent. paramTypeCombine: signature mirrors LFR.
func buildLFR(
	g *lpg.Graph[int, int64],
	n, gammaPercent, betaPercent, avgDeg, maxDeg, minCom, maxCom, muPercent int,
	seed uint64,
) error {
	err := addNodesRange(g, n)
	r := rand.New(rand.NewPCG(seed, ^seed))
	gamma := float64(gammaPercent) / 100.0
	beta := float64(betaPercent) / 100.0
	mu := float64(muPercent) / 100.0
	minDeg := avgDeg / 2
	if minDeg < 1 {
		minDeg = 1
	}
	// Phase 1: draw node degrees from the truncated power-law tail.
	degrees := make([]int, n)
	if err == nil {
		lfrSampleDegrees(r, degrees, n, minDeg, maxDeg, gamma)
	}
	// Phase 2: draw community sizes from the truncated power-law tail
	// and trim/pad the last bucket so the partition covers exactly n
	// nodes.
	var comSizes []int
	if err == nil {
		comSizes = lfrSampleCommunitySizes(r, n, minCom, maxCom, beta)
	}
	// Phase 3: greedy degree-descending node-to-community assignment.
	// Each node receives a community index in [0, len(comSizes)).
	var nodeCom []int
	if err == nil {
		nodeCom, err = lfrAssignCommunities(degrees, comSizes, muPercent)
	}
	// Phase 4: attach the "community_id" property to every node.
	if err == nil {
		err = lfrAttachCommunityLabels(g, nodeCom)
	}
	// Phase 5: realise edges via per-community intra-stub pairing and
	// global inter-stub pairing under the Erased Configuration Model.
	if err == nil {
		err = lfrRealiseEdges(g, degrees, nodeCom, len(comSizes), mu, r)
	}
	return err
}

// lfrSampleDegrees fills degrees[0..n-1] with independent draws from
// the truncated power-law p(k) ∝ k^(-gamma) on [minDeg, maxDeg]. The
// sampler uses the inverse-CDF method: for gamma > 1 the closed-form
// inverse of the truncated CDF is
//
//	k(u) = ((minDeg^(1-gamma) - u * (minDeg^(1-gamma) - maxDeg^(1-gamma)))^(1/(1-gamma)))
//
// where u is drawn uniformly from [0, 1). The result is clamped to
// [minDeg, maxDeg] and rounded to the nearest integer. The gamma = 1
// degenerate case is excluded by the constructor's gammaPercent >=
// 200 guard, so the closed-form denominator (1 - gamma) is bounded
// away from zero.
//
// PRNG consumption is exactly one [math/rand/v2.Rand.Float64] draw
// per node, in node order, so the seed-to-output map is a pure
// function of (n, minDeg, maxDeg, gamma, seed).
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see LFR godoc.
func lfrSampleDegrees(r *rand.Rand, degrees []int, n, minDeg, maxDeg int, gamma float64) {
	exp := 1.0 - gamma
	minTerm := math.Pow(float64(minDeg), exp)
	maxTerm := math.Pow(float64(maxDeg), exp)
	for i := 0; i < n; i++ {
		u := r.Float64()
		raw := math.Pow(minTerm-u*(minTerm-maxTerm), 1.0/exp)
		k := int(math.Round(raw))
		if k < minDeg {
			k = minDeg
		}
		if k > maxDeg {
			k = maxDeg
		}
		degrees[i] = k
	}
}

// lfrSampleCommunitySizes draws community sizes from the truncated
// power-law p(s) ∝ s^(-beta) on [minCom, maxCom] until the cumulative
// total reaches n. The last community is then trimmed so the
// partition covers exactly n nodes:
//
//   - If the cumulative total exceeds n the last community's size is
//     reduced to the residual; if the residual falls below minCom the
//     last community is dropped and its residual is folded into the
//     previous community (the prior community grows by the residual).
//   - If the loop happens to exhaust the [minCom, maxCom] envelope
//     before reaching n (in practice never at the catalogue's range,
//     where the expected mean community size is bounded away from
//     minCom) the last community absorbs the shortfall.
//
// The sampler uses the same inverse-CDF method as
// [lfrSampleDegrees]; the beta = 1 degenerate case is excluded by
// the constructor's betaPercent >= 100 guard. PRNG consumption is
// one Float64 draw per community, in community order, so the seed-
// to-output map is a pure function of (n, minCom, maxCom, beta,
// seed) once the degree-sampling phase has consumed its share of
// the PRNG stream.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see LFR godoc.
func lfrSampleCommunitySizes(r *rand.Rand, n, minCom, maxCom int, beta float64) []int {
	exp := 1.0 - beta
	minTerm := math.Pow(float64(minCom), exp)
	maxTerm := math.Pow(float64(maxCom), exp)
	sizes := make([]int, 0, n/minCom+1)
	total := 0
	for total < n {
		u := r.Float64()
		raw := math.Pow(minTerm-u*(minTerm-maxTerm), 1.0/exp)
		s := int(math.Round(raw))
		if s < minCom {
			s = minCom
		}
		if s > maxCom {
			s = maxCom
		}
		sizes = append(sizes, s)
		total += s
	}
	// Trim the last community so the partition covers exactly n
	// nodes. If the residual falls below minCom the last bucket is
	// merged into the previous one (the previous bucket grows).
	excess := total - n
	if excess > 0 {
		last := len(sizes) - 1
		newLast := sizes[last] - excess
		if newLast >= minCom || last == 0 {
			sizes[last] = newLast
			if sizes[last] < 1 {
				// Cannot happen at minCom >= 5 but guard against
				// future knob-range changes that admit minCom == 1.
				sizes = sizes[:last]
			}
			return sizes
		}
		// Merge: drop the last bucket and grow the previous one by
		// the residual budget. The merge can push the previous bucket
		// above maxCom; the catalogue accepts that because the
		// partition-cover invariant takes precedence over the
		// per-bucket cap.
		sizes[last-1] += sizes[last] - excess
		sizes = sizes[:last]
	}
	return sizes
}

// lfrAssignCommunities performs the greedy degree-descending
// admission pass. Nodes are sorted by degree in descending order and
// then placed one at a time into the community with the largest
// remaining capacity that satisfies the admission test
// kIntra < comSizes[c], where kIntra := round((1 - muPercent / 100)
// * deg). The admission test enforces the structural constraint
// that a node's intra-degree cannot reach or exceed its community
// size (a simple-graph community of s nodes admits at most s - 1
// intra-edges per node).
//
// Best-fit-by-largest-remaining is preferred over the simpler
// first-fit + round-robin variant because the degree distribution
// is power-law: a small number of very-high-degree nodes appears
// first in the degree-descending order, and placing them into the
// community with the largest current capacity gives the smaller
// communities room to absorb the long tail of low-degree nodes
// without exhausting their seats.
//
// When no community admits a node — typically because every
// community whose comSizes[c] > kIntra is already at capacity —
// the helper falls back to "place into the largest community whose
// comSizes[c] > kIntra, expanding its seat count by one". The
// fallback is a graceful relaxation of the strict partition-cover
// constraint: it lets the community grow above its sampled size by
// at most a few nodes, which keeps the community-size distribution
// fit within the +/- 5% AC envelope at n = 5000 while removing the
// brittle [ErrLFRAssignmentFailed] failure mode that would
// otherwise trigger at extreme tail draws. The sentinel is
// surfaced only when **no** community at all admits the node
// (comSizes[c] <= kIntra for every c), which is reachable only at
// degenerate corners (maxDeg >= every comSize).
//
// The function returns nodeCom[0..n-1], the assigned community
// index of every node in *original* node order (not degree-sorted
// order). The degree-descending iteration is internal to the
// function; the returned slice maps each original node id to its
// community.
func lfrAssignCommunities(degrees, comSizes []int, muPercent int) ([]int, error) {
	n := len(degrees)
	order := lfrSortNodesByDegreeDesc(degrees)
	remainingSeats := make([]int, len(comSizes))
	copy(remainingSeats, comSizes)
	mutableSizes := make([]int, len(comSizes))
	copy(mutableSizes, comSizes)
	// maxComSize is the largest sampled community size; node intra-
	// degree ceilings are clamped to maxComSize-1 to keep the
	// admission test feasible even when the user's maxDeg exceeds
	// every realised community size.
	maxComSize := 0
	for _, s := range comSizes {
		if s > maxComSize {
			maxComSize = s
		}
	}
	nodeCom := make([]int, n)
	for _, nodeID := range order {
		deg := degrees[nodeID]
		kIntra := int(math.Round(float64(deg) * (1.0 - float64(muPercent)/100.0)))
		if kIntra >= maxComSize {
			kIntra = maxComSize - 1
		}
		c, ok := lfrPickCommunity(remainingSeats, mutableSizes, kIntra)
		if !ok {
			return nil, fmt.Errorf("%w: node %d (deg=%d, kIntra=%d, maxComSize=%d) does not fit any community",
				ErrLFRAssignmentFailed, nodeID, deg, kIntra, maxComSize)
		}
		nodeCom[nodeID] = c
		if remainingSeats[c] > 0 {
			remainingSeats[c]--
		} else {
			// Fallback admission: grow the community by one seat.
			// The seat count is internal accounting; comSizes is only
			// used by the kIntra admission test, so growing
			// mutableSizes here is harmless for subsequent admissions.
			mutableSizes[c]++
		}
	}
	return nodeCom, nil
}

// lfrSortNodesByDegreeDesc returns a permutation of node ids ordered
// by degree descending. Nodes with equal degree are ordered by id
// ascending so the permutation is deterministic across runs.
func lfrSortNodesByDegreeDesc(degrees []int) []int {
	n := len(degrees)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		if degrees[order[i]] != degrees[order[j]] {
			return degrees[order[i]] > degrees[order[j]]
		}
		return order[i] < order[j]
	})
	return order
}

// lfrPickCommunity returns the index of the community that should
// admit a node with intra-degree ceiling kIntra. The pick policy is:
//
//  1. Prefer the community with the largest remainingSeats[c] among
//     those satisfying comSizes[c] > kIntra.
//  2. If no community has remainingSeats[c] > 0 but some satisfies
//     comSizes[c] > kIntra, return the one with the largest
//     comSizes[c] (the caller will grow it by one seat).
//  3. If no community has comSizes[c] > kIntra at all, return
//     (0, false) — the caller surfaces [ErrLFRAssignmentFailed].
//
// The function returns (community index, true) on success and
// (0, false) on the unrecoverable case (3).
func lfrPickCommunity(remainingSeats, comSizes []int, kIntra int) (int, bool) {
	bestVacant, bestVacantSeats := -1, -1
	bestExpand, bestExpandSize := -1, -1
	for c := 0; c < len(comSizes); c++ {
		if kIntra >= comSizes[c] {
			continue
		}
		if remainingSeats[c] > bestVacantSeats {
			bestVacantSeats = remainingSeats[c]
			bestVacant = c
		}
		if comSizes[c] > bestExpandSize {
			bestExpandSize = comSizes[c]
			bestExpand = c
		}
	}
	if bestVacant != -1 && bestVacantSeats > 0 {
		return bestVacant, true
	}
	if bestExpand != -1 {
		return bestExpand, true
	}
	return 0, false
}

// lfrAttachCommunityLabels stamps every node 0..n-1 with its zero-
// based community index as an [lpg.Int64Value] property under the
// key "community_id". The first SetNodeProperty error short-circuits
// the loop, matching the branch-free err-thread convention shared
// across the random family. SetNodeProperty surfaces the same shard-
// full error from the underlying [adjlist.AdjList.AddNode] guard.
func lfrAttachCommunityLabels(g *lpg.Graph[int, int64], nodeCom []int) error {
	var err error
	for i := 0; i < len(nodeCom) && err == nil; i++ {
		err = g.SetNodeProperty(canonicalNode(i), "community_id", lpg.Int64Value(int64(nodeCom[i])))
	}
	return err
}

// lfrRealiseEdges builds the edge set under the Erased Configuration
// Model on per-community intra-stub pools and a global inter-stub
// pool. For each node v the split is
//
//	kIntra(v) = round((1 - mu) * deg(v))
//	kInter(v) = deg(v) - kIntra(v)
//
// with the per-community intra-stub sum forced to be even (one stub
// added to or removed from the node with the largest intra-stub
// count in the community when the sum is odd) and the global inter-
// stub sum forced to be even the same way. Stubs are paired by
// Fisher-Yates shuffle + adjacent-pair consumption; pairings that
// are self-loops, parallel edges (with respect to the running pair
// set), or — in the inter-community pass — pairings whose endpoints
// share a community are dropped.
//
// On success the realised pair set is materialised into a sorted
// slice of (min, max) tuples and inserted into g in lexicographic
// (u, v) order. The first AddEdge error short-circuits the
// insertion loop.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see LFR godoc.
func lfrRealiseEdges(
	g *lpg.Graph[int, int64],
	degrees, nodeCom []int,
	numCom int,
	mu float64,
	r *rand.Rand,
) error {
	n := len(degrees)
	kIntra := make([]int, n)
	kInter := make([]int, n)
	for v := 0; v < n; v++ {
		ki := int(math.Round(float64(degrees[v]) * (1.0 - mu)))
		if ki > degrees[v] {
			ki = degrees[v]
		}
		if ki < 0 {
			ki = 0
		}
		kIntra[v] = ki
		kInter[v] = degrees[v] - ki
	}
	// Group nodes by community so we can pair intra-stubs per bucket.
	comMembers := make([][]int, numCom)
	for v := 0; v < n; v++ {
		comMembers[nodeCom[v]] = append(comMembers[nodeCom[v]], v)
	}
	pairs := make(map[[2]int]struct{}, n*4)
	// Phase A: per-community intra-stub pairing.
	for c := 0; c < numCom; c++ {
		lfrPairIntraCommunity(r, comMembers[c], kIntra, pairs)
	}
	// Phase B: global inter-stub pairing with community filter.
	lfrPairInterCommunity(r, kInter, nodeCom, pairs)
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
	var err error
	for k := 0; k < len(edges) && err == nil; k++ {
		err = g.AddEdge(canonicalNode(edges[k][0]), canonicalNode(edges[k][1]), unweightedSentinel)
	}
	return err
}

// lfrPairIntraCommunity materialises every member's kIntra[v] stubs
// as flat half-edges, parity-fixes the per-community stub sum,
// Fisher-Yates shuffles the half-edge slice, and walks adjacent
// pairs left-to-right adding each non-self-loop, non-duplicate pair
// to the running pair set. Self-loops and parallel pairings are
// dropped at generation time (the Erased Configuration Model).
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism.
func lfrPairIntraCommunity(r *rand.Rand, members, kIntra []int, pairs map[[2]int]struct{}) {
	if len(members) < 2 {
		return
	}
	stubs := make([]int, 0, 8)
	for _, v := range members {
		for k := 0; k < kIntra[v]; k++ {
			stubs = append(stubs, v)
		}
	}
	if len(stubs)%2 != 0 {
		// Trim one stub from the highest-intra-degree node in the
		// community so the half-edge multiset has even cardinality.
		// Trimming (rather than padding) keeps the realised intra-
		// degree sequence componentwise <= the requested one, which
		// matches the Erased Configuration Model contract documented
		// on [ConfigurationModel].
		idx := lfrIndexOfMaxStub(stubs)
		stubs = append(stubs[:idx], stubs[idx+1:]...)
	}
	if len(stubs) < 2 {
		return
	}
	r.Shuffle(len(stubs), func(i, j int) {
		stubs[i], stubs[j] = stubs[j], stubs[i]
	})
	for k := 0; k+1 < len(stubs); k += 2 {
		u, v := stubs[k], stubs[k+1]
		if u == v {
			continue
		}
		a, b := u, v
		if a > b {
			a, b = b, a
		}
		pairs[[2]int{a, b}] = struct{}{}
	}
}

// lfrPairInterCommunity materialises every node's kInter[v] stubs as
// flat half-edges, parity-fixes the global stub sum, Fisher-Yates
// shuffles the slice, and walks adjacent pairs left-to-right adding
// each non-self-loop, non-duplicate, cross-community pair to the
// running pair set. Pairings whose endpoints share a community are
// dropped at generation time so the realised mixing parameter
// matches the requested mu from above.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism.
func lfrPairInterCommunity(r *rand.Rand, kInter, nodeCom []int, pairs map[[2]int]struct{}) {
	stubs := make([]int, 0, len(kInter))
	for v := 0; v < len(kInter); v++ {
		for k := 0; k < kInter[v]; k++ {
			stubs = append(stubs, v)
		}
	}
	if len(stubs)%2 != 0 {
		idx := lfrIndexOfMaxStub(stubs)
		stubs = append(stubs[:idx], stubs[idx+1:]...)
	}
	if len(stubs) < 2 {
		return
	}
	r.Shuffle(len(stubs), func(i, j int) {
		stubs[i], stubs[j] = stubs[j], stubs[i]
	})
	for k := 0; k+1 < len(stubs); k += 2 {
		u, v := stubs[k], stubs[k+1]
		if u == v {
			continue
		}
		if nodeCom[u] == nodeCom[v] {
			continue
		}
		a, b := u, v
		if a > b {
			a, b = b, a
		}
		pairs[[2]int{a, b}] = struct{}{}
	}
}

// lfrIndexOfMaxStub returns the index of the first occurrence of the
// most-frequent value in stubs. The helper is used to identify the
// node carrying the largest stub count in a parity-odd half-edge
// slice; removing one of that node's stubs minimises the realised
// degree perturbation. The helper short-circuits on the first run
// found in linear order, so it is O(len(stubs)) at worst.
func lfrIndexOfMaxStub(stubs []int) int {
	counts := make(map[int]int, len(stubs))
	for _, v := range stubs {
		counts[v]++
	}
	bestNode := stubs[0]
	bestCount := counts[bestNode]
	for v, c := range counts {
		if c > bestCount || (c == bestCount && v < bestNode) {
			bestCount = c
			bestNode = v
		}
	}
	for i, v := range stubs {
		if v == bestNode {
			return i
		}
	}
	return 0
}
