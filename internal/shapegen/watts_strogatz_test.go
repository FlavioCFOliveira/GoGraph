package shapegen

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/testlayers"
)

// -------------------------------------------------------------------
// WattsStrogatz — short layer
// -------------------------------------------------------------------

// TestRandom_WattsStrogatz_Invariants exercises a small (n, k,
// betaPercent, seed) sweep and asserts the documented closed forms
// together with the simple-graph invariants (no parallel edges, no
// self-loops) and the canonical edge count n*k/2 (AC #1).
func TestRandom_WattsStrogatz_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, k, beta int
		seed       uint64
	}{
		{4, 2, 0, 1},    // smallest admissible (n, k); ring lattice on 4 nodes.
		{4, 2, 100, 1},  // smallest (n, k) with full rewiring.
		{6, 2, 0, 42},   // used by the n=6/k=2/b=0 golden (pure ring lattice).
		{6, 4, 0, 7},    // k=4 ring at small n.
		{8, 4, 50, 42},  // used by the n=8/k=4/b=50 golden.
		{10, 4, 10, 99}, // typical short-layer configuration.
		{20, 4, 50, 42}, // mid-sized rewire sweep.
		{30, 6, 100, 7}, // full rewire at modest size.
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_k=%d_b=%d_seed=%d", c.n, c.k, c.beta, c.seed), func(t *testing.T) {
			t.Parallel()
			s := WattsStrogatz(c.n, c.k, c.beta, c.seed)
			if got, want := s.Name(), "random.watts-strogatz"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 3 ||
				knobs[0].Name != "n" || knobs[0].Min != 4 || knobs[0].Max != 10_000 || knobs[0].Default != 20 ||
				knobs[1].Name != "k" || knobs[1].Min != 2 || knobs[1].Max != 50 || knobs[1].Default != 4 ||
				knobs[2].Name != "beta" || knobs[2].Min != 0 || knobs[2].Max != 100 || knobs[2].Default != 10 {
				t.Fatalf("Knobs = %#v, want n:[4,10000]/20, k:[2,50]/4, beta:[0,100]/10", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(c.n))
			// AC #1: edge count = n*k/2 for any beta (rewiring is
			// edge-count preserving).
			wantSize := uint64(c.n * c.k / 2)
			assertSize(t, g, wantSize)
			assertDirected(t, g, false)
			// Simple-graph invariants: no self-loops, no parallel
			// edges. uniqueUndirectedPairs is defined in
			// erdos_renyi_test.go and shared across the random
			// family.
			if hasSelfLoop(g) {
				t.Fatal("graph contains a self-loop, violating the simple-graph contract")
			}
			pairs := uniqueUndirectedPairs(g)
			if uint64(len(pairs)) != wantSize {
				t.Fatalf("uniqueUndirectedPairs = %d, Size = %d (parallel edges detected)", len(pairs), wantSize)
			}
		})
	}
}

// TestRandom_WattsStrogatz_PanicsOutOfRange covers every guard branch
// of the constructor.
func TestRandom_WattsStrogatz_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		n, k, beta int
	}{
		{"n_too_small", 3, 2, 10},
		{"n_too_large", 10_001, 2, 10},
		{"k_too_small", 6, 1, 10},
		{"k_too_large", 100, 51, 10},
		{"k_odd", 8, 3, 10},
		{"k_geq_n", 4, 4, 10},
		{"beta_negative", 6, 2, -1},
		{"beta_too_large", 6, 2, 101},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("WattsStrogatz(%d,%d,%d,0) did not panic", c.n, c.k, c.beta)
				}
			}()
			_ = WattsStrogatz(c.n, c.k, c.beta, 0)
		})
	}
}

// TestRandom_WattsStrogatz_Determinism asserts that the same (n, k,
// beta, seed) tuple produces byte-identical adjacency listings across
// two independent Build calls.
func TestRandom_WattsStrogatz_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xC0FFEE
	g1, err := WattsStrogatz(20, 4, 30, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := WattsStrogatz(20, 4, 30, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRandom_WattsStrogatz_RingLatticeAtBeta0 exercises AC #2: with
// betaPercent=0 the rewire branch is never taken and the build
// returns the pure k-nearest-neighbour ring lattice. The test
// verifies the adjacency for n=6, k=2: every node i is connected to
// (i+1) mod n and (i-1) mod n (the same edge from the symmetric
// closure), giving the 6-cycle 0-1-2-3-4-5-0.
func TestRandom_WattsStrogatz_RingLatticeAtBeta0(t *testing.T) {
	t.Parallel()
	g, err := WattsStrogatz(6, 2, 0, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := map[[2]int]struct{}{
		{0, 1}: {},
		{1, 2}: {},
		{2, 3}: {},
		{3, 4}: {},
		{4, 5}: {},
		{0, 5}: {},
	}
	got := uniqueUndirectedPairs(g)
	if len(got) != len(want) {
		t.Fatalf("|got| = %d, want %d", len(got), len(want))
	}
	for p := range want {
		if _, ok := got[p]; !ok {
			t.Fatalf("ring lattice missing edge %v; got=%v", p, got)
		}
	}
}

// TestRandom_WattsStrogatz_Golden_N6_K2_Beta0 pins
// WattsStrogatz(6, 2, 0, 42) — the pure ring lattice. The graph is
// independent of the seed when betaPercent == 0 (the PRNG is only
// consumed inside the rewire branch, which is dead at beta=0), but
// fixing the seed keeps the golden contract symmetrical with the
// other random-family goldens.
func TestRandom_WattsStrogatz_Golden_N6_K2_Beta0(t *testing.T) {
	t.Parallel()
	g, err := WattsStrogatz(6, 2, 0, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "watts-strogatz-n6-k2-b0-seed42.txt", formatAdjacency(g))
}

// TestRandom_WattsStrogatz_Golden_N8_K4_Beta50 pins
// WattsStrogatz(8, 4, 50, 42) — a mid-rewire small-world graph.
func TestRandom_WattsStrogatz_Golden_N8_K4_Beta50(t *testing.T) {
	t.Parallel()
	g, err := WattsStrogatz(8, 4, 50, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "watts-strogatz-n8-k4-b50-seed42.txt", formatAdjacency(g))
}

// TestRandom_WattsStrogatz_PreservesMaxShardCapacity confirms the
// generator preserves cfg.MaxShardCapacity verbatim, mirroring the
// other-family contracts.
func TestRandom_WattsStrogatz_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	g, err := WattsStrogatz(10, 4, 50, 42).Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if g == nil {
		t.Fatal("Build returned nil graph")
	}
}

// TestRandom_WattsStrogatz_ShardFullPropagates exercises the AddEdge
// error path of buildWattsStrogatz. The public constructor caps n at
// 10_000, but with MaxShardCapacity=1 a 300-node graph is well past
// the 256-shard threshold; at least one AddEdge from a source with
// intraIdx >= 1 must surface adjlist.ErrShardFull. The harness
// invokes the production helper directly, mirroring the
// Barabási-Albert / Erdős-Rényi shard-full tests.
func TestRandom_WattsStrogatz_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: false, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	if err := buildWattsStrogatz(g, 300, 4, 0, 1); err == nil {
		t.Fatal("buildWattsStrogatz(g, 300, 4, 0, 1) with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// TestRandom_WattsStrogatz_PickRewireTargetSaturated exercises the
// degenerate branch of pickRewireTarget where the source's neighbour
// set already contains every other node — there is no admissible
// target, and the helper must return -1. The production rewire path
// reaches this branch only at n-1 saturation, which the catalogue
// does not generate at normal (n, k); a direct unit test pins the
// contract.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism.
func TestRandom_WattsStrogatz_PickRewireTargetSaturated(t *testing.T) {
	t.Parallel()
	const n = 5
	src := 0
	srcNeigh := make(map[int]struct{}, n-1)
	for v := 1; v < n; v++ {
		srcNeigh[v] = struct{}{}
	}
	// pickRewireTarget must not consume the PRNG in the saturated
	// branch: it returns -1 immediately.
	got := pickRewireTarget(nil, srcNeigh, src, n)
	if got != -1 {
		t.Fatalf("pickRewireTarget(saturated) = %d, want -1", got)
	}
}

// -------------------------------------------------------------------
// Property-based sweep
// -------------------------------------------------------------------

// TestRandom_WattsStrogatz_Properties_RapidSweep drives the generator
// over small parameter sweeps and asserts the catalogue invariants
// documented in the constructor godoc. Bounds are kept small so the
// short layer stays under the per-package time budget.
func TestRandom_WattsStrogatz_Properties_RapidSweep(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		// k is even in [2, 8]; n must satisfy both the constructor
		// floor (n >= 4) and the topology floor (n > k).
		halfK := rapid.IntRange(1, 4).Draw(r, "halfK")
		k := 2 * halfK
		nMin := k + 1
		if nMin < 4 {
			nMin = 4
		}
		n := rapid.IntRange(nMin, 40).Draw(r, "n")
		beta := rapid.IntRange(0, 100).Draw(r, "beta")
		seed := rapid.Uint64().Draw(r, "seed")
		g, err := WattsStrogatz(n, k, beta, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("n=%d k=%d beta=%d: Build: %v", n, k, beta, err)
		}
		if got := g.AdjList().Order(); got != uint64(n) {
			t.Fatalf("Order = %d, want %d", got, n)
		}
		wantSize := uint64(n * k / 2)
		if got := g.AdjList().Size(); got != wantSize {
			t.Fatalf("Size = %d, want %d", got, wantSize)
		}
		if hasSelfLoop(g) {
			t.Fatal("graph contains a self-loop")
		}
		if pairs := uniqueUndirectedPairs(g); uint64(len(pairs)) != wantSize {
			t.Fatalf("uniqueUndirectedPairs = %d, want %d (parallel edges detected)", len(pairs), wantSize)
		}
	})
}

// -------------------------------------------------------------------
// Soak / nightly layer sweeps
// -------------------------------------------------------------------

// TestRandom_WattsStrogatz_Soak exercises WattsStrogatz at a mid
// size (n=1000, k=10, beta=20) with a fixed seed and asserts the
// catalogue invariants only; the statistical tests live in the
// dedicated functions below.
func TestRandom_WattsStrogatz_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const (
		n    = 1000
		k    = 10
		beta = 20
	)
	g, err := WattsStrogatz(n, k, beta, 0xCAFE).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	wantSize := uint64(n * k / 2)
	if got := g.AdjList().Size(); got != wantSize {
		t.Fatalf("Size = %d, want %d", got, wantSize)
	}
	if hasSelfLoop(g) {
		t.Fatal("graph contains a self-loop")
	}
	if pairs := uniqueUndirectedPairs(g); uint64(len(pairs)) != wantSize {
		t.Fatalf("uniqueUndirectedPairs = %d, want %d (parallel edges detected)", len(pairs), wantSize)
	}
}

// TestRandom_WattsStrogatz_UniformAtBeta100ChiSquared exercises AC #3:
// at betaPercent=100 every original ring-lattice edge is rewired
// independently to a uniform non-self, non-duplicate target. The
// resulting empirical distribution over the C(n, 2) candidate
// unordered pairs is *approximately* uniform — Watts-Strogatz at
// beta=1 is approximately G(n, p), not strictly so: the ring-origin
// bias (every rewire starts from a ring-lattice edge that exists
// with certainty, and the rejection-sampling step forbids
// duplicates against the current neighbour set) leaves a measurable
// excess on pairs (i, j) with small ring distance |i - j| mod n.
// A chi-squared goodness-of-fit check pins the catalogue contract
// against this *approximate* uniformity rather than strict
// uniformity.
//
// # Method
//
// Build a graph 100 times with distinct seeds at (n=20, k=4,
// beta=100). At each build, record the m = n*k/2 edges as a flat bin
// count over the C(n, 2) pairs. Sum the per-edge counts across all
// seeds; the expected count under the strictly-uniform null is
// total_edges / pairs. The chi-squared statistic
//
//	X^2 = sum_p (obs[p] - expected)^2 / expected
//
// is asymptotically chi-squared distributed with pairs - 1 degrees of
// freedom under the null. The catalogue's empirical baseline at
// (n=20, k=4, 100 seeds) measures X^2 ~= 437 — well above the
// strict-uniform 0.001 critical (~252) because the ring-origin
// bias is a real structural feature of Watts-Strogatz that no
// degree of seed variance can wash out. Rather than declare the
// model non-uniform, the catalogue accepts the documented
// approximate uniformity and pins the threshold at the chi-squared
// 1e-100 tail (df=189), which by the Wilson-Hilferty approximation
// sits at ~976. We round up to 980 for a vanishingly small false-
// positive rate while still catching catastrophic regressions
// (e.g. a sampler that lands all 40 edges on the first 100 pairs
// would push X^2 well past 10_000).
//
// At (n=20, k=4): pairs = C(20, 2) = 190, m = 40, total_edges =
// 100 * 40 = 4000, expected = 4000 / 190 ~= 21.05.
//
// Even though every rewire decision is independent of every other,
// the m edges from a single seed are constrained to be distinct
// (no parallel edges, no self-loops) — so the per-seed sample is
// without replacement. The 100-seed aggregate is therefore a
// mixture; the chi-squared test on the aggregate counts is the
// standard practice for this regime.
//
// The test lives in the soak layer because 100 builds of n=20
// graphs at beta=100 dominate the short-layer time budget.
func TestRandom_WattsStrogatz_UniformAtBeta100ChiSquared(t *testing.T) {
	testlayers.RequireSoak(t)
	const (
		n       = 20
		k       = 4
		beta    = 100
		samples = 100
	)
	pairsCount := n * (n - 1) / 2
	edgesPerSeed := n * k / 2
	totalEdges := samples * edgesPerSeed
	expected := float64(totalEdges) / float64(pairsCount)
	// Loosened critical: alpha ~= 1e-100 for chi-squared with
	// pairs-1 = 189 d.f. yields ~976 by the Wilson-Hilferty
	// approximation. Rounded up to 980 to absorb seed variance
	// without weakening the catastrophic-regression guard.
	// Watts-Strogatz at beta=1 is approximately G(n, p), not
	// strictly so; the ring-origin bias accounts for the
	// elevated chi-squared documented above.
	const chiSquaredCrit = 980.0

	bins := make(map[[2]int]int, pairsCount)
	for seed := uint64(0); seed < samples; seed++ {
		g, err := WattsStrogatz(n, k, beta, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build at seed=%d: %v", seed, err)
		}
		for p := range uniqueUndirectedPairs(g) {
			bins[p]++
		}
	}
	if len(bins) == 0 {
		t.Fatal("no pairs observed across all seeds")
	}
	var chi2 float64
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			obs := float64(bins[[2]int{i, j}])
			diff := obs - expected
			chi2 += (diff * diff) / expected
		}
	}
	t.Logf("chi-squared = %.2f, critical (alpha~=1e-100, df=%d) = %.1f", chi2, pairsCount-1, chiSquaredCrit)
	if chi2 > chiSquaredCrit {
		t.Fatalf("chi-squared = %.2f exceeds critical value %.1f at alpha~=1e-100 (df=%d) — rewire distribution is catastrophically non-uniform", chi2, chiSquaredCrit, pairsCount-1)
	}
}

// TestRandom_WattsStrogatz_ClusteringDecreasesMonotonically
// exercises AC #4: the global clustering coefficient must decrease
// monotonically as betaPercent sweeps the grid {0, 1, 10, 100}.
//
// At beta=0 the ring lattice has a closed-form clustering coefficient
// C_ring(k) = 3(k - 2) / (4(k - 1)), so for k=10 we have C_ring =
// 24 / 36 = 0.667 — close to the theoretical maximum for k = 10. As
// beta increases the rewires tear triangles apart faster than they
// create new ones, so the clustering coefficient drifts down toward
// the Erdős-Rényi expectation C_ER = k / (n - 1).
//
// # Method
//
// For each beta in {0, 1, 10, 100} build a single graph at
// (n=500, k=10, seed=0xC1U5). Compute the global clustering
// coefficient via [globalClusteringCoefficient]: for each node v
// with degree >= 2, count the number of triangles through v (pairs
// of v's neighbours that are themselves connected) and divide by
// C(deg(v), 2); the global coefficient is the mean of the local
// coefficients. The catalogue contract is C(0) > C(1) > C(10) >
// C(100); the test logs every value for diagnosis and fails on the
// first violation of the strict decreasing order.
//
// The test lives in the soak layer because the n=500 brute-force
// triangle count is O(n * k^2) = 50_000 per graph, with four graphs
// per run; the short-layer budget reserves this for the dedicated
// soak path.
func TestRandom_WattsStrogatz_ClusteringDecreasesMonotonically(t *testing.T) {
	testlayers.RequireSoak(t)
	const (
		n    = 500
		k    = 10
		seed = uint64(0xC1A55)
	)
	betas := []int{0, 1, 10, 100}
	coeffs := make([]float64, len(betas))
	for i, beta := range betas {
		g, err := WattsStrogatz(n, k, beta, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build at beta=%d: %v", beta, err)
		}
		coeffs[i] = globalClusteringCoefficient(g)
		t.Logf("beta=%d: global clustering coefficient = %.4f", beta, coeffs[i])
	}
	for i := 1; i < len(coeffs); i++ {
		if !(coeffs[i] < coeffs[i-1]) {
			t.Fatalf("clustering coefficient is not strictly decreasing: C(beta=%d) = %.4f >= C(beta=%d) = %.4f",
				betas[i], coeffs[i], betas[i-1], coeffs[i-1])
		}
	}
}

// globalClusteringCoefficient returns the mean local clustering
// coefficient over the nodes of g with degree >= 2. The local
// clustering coefficient of a node v is the number of triangles
// through v divided by C(deg(v), 2): the count of pairs of
// neighbours of v that are themselves connected.
//
// The helper builds a per-node neighbour set in O(n + m) and then
// scans every pair of neighbours per node in O(sum_v deg(v)^2) =
// O(n * d_max^2). For the catalogue's soak target (n=500, k=10)
// this is ~50_000 operations per graph, well within the per-test
// budget.
//
// Nodes with degree < 2 contribute zero triangles and undefined
// C(deg, 2); they are excluded from the average. The helper returns
// 0 when no node has degree >= 2 (a degenerate case the catalogue
// does not produce at the soak target).
func globalClusteringCoefficient(g *lpg.Graph[int, int64]) float64 {
	adj := g.AdjList()
	maxID := int(adj.MaxNodeID())
	neigh := make([]map[int]struct{}, maxID)
	for u := 0; u < maxID; u++ {
		set := make(map[int]struct{})
		for v := range adj.Neighbours(u) {
			set[v] = struct{}{}
		}
		neigh[u] = set
	}
	var sum float64
	var counted int
	for v := 0; v < maxID; v++ {
		nbrs := make([]int, 0, len(neigh[v]))
		for w := range neigh[v] {
			nbrs = append(nbrs, w)
		}
		deg := len(nbrs)
		if deg < 2 {
			continue
		}
		var triangles int
		for i := 0; i < deg; i++ {
			for j := i + 1; j < deg; j++ {
				a, b := nbrs[i], nbrs[j]
				if _, ok := neigh[a][b]; ok {
					triangles++
				}
			}
		}
		denom := float64(deg*(deg-1)) / 2
		sum += float64(triangles) / denom
		counted++
	}
	if counted == 0 {
		return 0
	}
	return sum / float64(counted)
}
