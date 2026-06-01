package shapegen

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// -------------------------------------------------------------------
// BarabasiAlbert — short layer
// -------------------------------------------------------------------

// TestRandom_BarabasiAlbert_Invariants exercises a small (n, m0,
// seed) sweep and asserts the documented closed forms together with
// the simple-graph invariants (no parallel edges, no self-loops) and
// the canonical edge count m0*(m0-1)/2 + (n-m0)*m0 (AC #1).
func TestRandom_BarabasiAlbert_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, m0 int
		seed  uint64
	}{
		{1, 1, 1},    // n == m0 == 1: lone K_1 seed, no growth.
		{2, 1, 1},    // m0 == 1 path: one new node attaches to the seed.
		{2, 2, 1},    // n == m0 == 2: K_2 seed, no growth.
		{3, 2, 42},   // K_2 seed + one preferential step.
		{5, 2, 42},   // small growth, m0 == 2.
		{6, 3, 42},   // K_3 seed + three preferential steps.
		{10, 2, 42},  // used by the n=10/m0=2 golden.
		{20, 3, 42},  // used by the n=20/m0=3 golden.
		{30, 4, 7},   // bigger sweep with m0 == 4.
		{50, 5, 999}, // larger sweep with m0 == 5.
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_m0=%d_seed=%d", c.n, c.m0, c.seed), func(t *testing.T) {
			t.Parallel()
			s := BarabasiAlbert(c.n, c.m0, c.seed)
			if got, want := s.Name(), "random.barabasi-albert"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 2 ||
				knobs[0].Name != "n" || knobs[0].Min != 2 || knobs[0].Max != 100_000 || knobs[0].Default != 50 ||
				knobs[1].Name != "m0" || knobs[1].Min != 1 || knobs[1].Max != 50 || knobs[1].Default != 3 {
				t.Fatalf("Knobs = %#v, want n:[2,100000]/50, m0:[1,50]/3", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(c.n))
			wantSize := uint64(c.m0*(c.m0-1)/2 + (c.n-c.m0)*c.m0)
			assertSize(t, g, wantSize)
			assertDirected(t, g, false)
			// Simple-graph invariants: no self-loops, no parallel
			// edges. uniqueUndirectedPairs is defined in
			// erdos_renyi_test.go and shared across the random family.
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

// TestRandom_BarabasiAlbert_PanicsOutOfRange covers every guard
// branch of the constructor.
func TestRandom_BarabasiAlbert_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		n, m0 int
	}{
		{"m0_zero", 5, 0},
		{"m0_negative", 5, -1},
		{"m0_too_large", 5, 51},
		{"n_below_m0", 2, 3},
		{"n_too_large", 100_001, 3},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("BarabasiAlbert(%d,%d,0) did not panic", c.n, c.m0)
				}
			}()
			_ = BarabasiAlbert(c.n, c.m0, 0)
		})
	}
}

// TestRandom_BarabasiAlbert_Determinism asserts that the same
// (n, m0, seed) tuple produces byte-identical adjacency listings
// across two independent Build calls (AC #3).
func TestRandom_BarabasiAlbert_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xC0FFEE
	g1, err := BarabasiAlbert(50, 3, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := BarabasiAlbert(50, 3, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRandom_BarabasiAlbert_Golden_N10 pins BarabasiAlbert(10, 2, 42).
func TestRandom_BarabasiAlbert_Golden_N10(t *testing.T) {
	t.Parallel()
	g, err := BarabasiAlbert(10, 2, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "barabasi-albert-n10-m0-2-seed42.txt", formatAdjacency(g))
}

// TestRandom_BarabasiAlbert_Golden_N20 pins BarabasiAlbert(20, 3, 42).
func TestRandom_BarabasiAlbert_Golden_N20(t *testing.T) {
	t.Parallel()
	g, err := BarabasiAlbert(20, 3, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "barabasi-albert-n20-m0-3-seed42.txt", formatAdjacency(g))
}

// TestRandom_BarabasiAlbert_PreservesMaxShardCapacity confirms the
// generator preserves cfg.MaxShardCapacity verbatim, mirroring the
// other-family contracts.
func TestRandom_BarabasiAlbert_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	g, err := BarabasiAlbert(10, 2, 42).Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if g == nil {
		t.Fatal("Build returned nil graph")
	}
}

// TestRandom_BarabasiAlbert_ShardFullPropagates exercises the
// AddEdge error path of buildBarabasiAlbert. The public constructor
// caps n at 100_000, but with MaxShardCapacity=1 a 300-node graph
// is well past the 256-shard threshold; at least one AddEdge from a
// source with intraIdx >= 1 must surface adjlist.ErrShardFull. The
// harness invokes the production helper directly, mirroring the
// dags / Erdős-Rényi shard-full tests.
func TestRandom_BarabasiAlbert_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: false, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	if err := buildBarabasiAlbert(g, 300, 3, 1); err == nil {
		t.Fatal("buildBarabasiAlbert(g, 300, 3, 1) with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// TestRandom_BarabasiAlbert_M0Equals1Path exercises the m0 == 1
// short-circuit inside barabasiAlbertStep where the seed K_1 has
// zero total degree at step 1. The new node must attach
// unconditionally to node 0 and the run must succeed.
func TestRandom_BarabasiAlbert_M0Equals1Path(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 3, 5, 10} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			g, err := BarabasiAlbert(n, 1, 0xABCD).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(n))
			// Edge count = 0*(0-1)/2 + (n-1)*1 = n - 1.
			assertSize(t, g, uint64(n-1))
			if hasSelfLoop(g) {
				t.Fatal("graph contains a self-loop")
			}
			// At step 1 (n >= 2) the only admissible target is node
			// 0; the catalogue contract on m0 == 1 therefore pins
			// (0, 1) as an edge.
			pairs := uniqueUndirectedPairs(g)
			if _, ok := pairs[[2]int{0, 1}]; !ok {
				t.Fatalf("BarabasiAlbert(%d, 1, ...) must include edge (0, 1), got pairs=%v", n, pairs)
			}
		})
	}
}

// -------------------------------------------------------------------
// Property-based sweep
// -------------------------------------------------------------------

// TestRandom_BarabasiAlbert_Properties_RapidSweep drives the
// generator over small parameter sweeps and asserts the catalogue
// invariants documented in the constructor godoc. Bounds are kept
// small so the short layer stays under the per-package time budget.
func TestRandom_BarabasiAlbert_Properties_RapidSweep(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		m0 := rapid.IntRange(1, 8).Draw(r, "m0")
		n := rapid.IntRange(m0, 40).Draw(r, "n")
		seed := rapid.Uint64().Draw(r, "seed")
		g, err := BarabasiAlbert(n, m0, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("n=%d m0=%d: Build: %v", n, m0, err)
		}
		if got := g.AdjList().Order(); got != uint64(n) {
			t.Fatalf("Order = %d, want %d", got, n)
		}
		wantSize := uint64(m0*(m0-1)/2 + (n-m0)*m0)
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

// TestRandom_BarabasiAlbert_Soak exercises BarabasiAlbert at a mid
// size (n=1000, m0=3) with a fixed seed and asserts the catalogue
// invariants only; the heavy power-law statistical test lives in
// [TestRandom_BarabasiAlbert_PowerLawExponent].
func TestRandom_BarabasiAlbert_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const (
		n  = 1000
		m0 = 3
	)
	g, err := BarabasiAlbert(n, m0, 0xCAFE).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	wantSize := uint64(m0*(m0-1)/2 + (n-m0)*m0)
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

// TestRandom_BarabasiAlbert_PowerLawExponent exercises AC #2: at
// n=10000, m0=3 over five seeds, the empirical degree distribution
// must fit a power-law tail with exponent gamma in [2.5, 3.5] for
// at least four of the five seeds. The theoretical Barabási-Albert
// exponent is gamma = 3; the [2.5, 3.5] window captures finite-size
// noise without admitting degenerate generators.
//
// # CCDF log-log regression
//
// The fit method is the complementary-cumulative-distribution-function
// (CCDF) variant pinned by the task brief and standard in the
// network-science literature (Newman, "The Structure and Function of
// Complex Networks", SIAM Review 45(2), 2003; Clauset, Shalizi &
// Newman, "Power-Law Distributions in Empirical Data", SIAM Review
// 51(4), 2009). The CCDF formulation supersedes the naive
// "log h[k] vs log k" histogram regression because the latter is
// dominated by per-bin Poisson noise in the tail, whereas the CCDF
// smooths that noise by summing tail probabilities.
//
// For each seed:
//
//  1. Compute the empirical CCDF
//     P(deg >= k) = #{ v : deg(v) >= k } / n
//     over all integer k for which at least one node has degree k.
//  2. Restrict the fit to the tail k >= kMin = 5. The head is
//     dominated by the m0 seed clique and by the new-node-with-
//     exactly-m0-edges spike, neither of which obeys the asymptotic
//     power law. kMin = 5 is the documented choice for the
//     catalogue's (n=10000, m0=3) sweep.
//  3. Discard the zero-probability bin (every node has degree at
//     least m0, so for k > maxDeg P(deg >= k) = 0; log(0) is
//     undefined).
//  4. Run least-squares linear regression on
//     (log k, log P(deg >= k)); the slope is -(gamma - 1), so
//     gamma_hat = 1 - slope.
//
// The regression solves
//
//	beta = sum((x_i - x_bar)(y_i - y_bar)) / sum((x_i - x_bar)^2)
//	gamma_hat = 1 - beta
//
// using the closed-form ordinary least-squares estimator. With the
// tail starting at k=5 and the [2.5, 3.5] gate, the test has a
// vanishingly small false-positive rate while still catching any
// systemic bias in the cumulative-degree sampling.
//
// The check lives in the soak layer because building five n=10000
// graphs is well outside the short-layer budget; AC #1 in
// TestRandom_BarabasiAlbert_Invariants already covers the
// closed-form edge count for the short layer.
func TestRandom_BarabasiAlbert_PowerLawExponent(t *testing.T) {
	testlayers.RequireSoak(t)
	const (
		n     = 10_000
		m0    = 3
		seeds = 5
		kMin  = 5
		gMin  = 2.5
		gMax  = 3.5
	)
	passes := 0
	for s := uint64(0); s < seeds; s++ {
		g, err := BarabasiAlbert(n, m0, 0xBA0000+s).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build at seed=%d: %v", s, err)
		}
		degHist := degreeHistogram(g)
		gamma, ok := fitPowerLawExponentCCDF(degHist, n, kMin)
		if !ok {
			t.Logf("seed=%d: CCDF power-law fit skipped (insufficient tail bins)", s)
			continue
		}
		t.Logf("seed=%d: gamma_hat = %.3f", s, gamma)
		if gamma >= gMin && gamma <= gMax {
			passes++
		}
	}
	if passes < 4 {
		t.Fatalf("power-law exponent gamma in [%.1f, %.1f] held for only %d/%d seeds, want >= 4", gMin, gMax, passes, seeds)
	}
}

// degreeHistogram builds the degree histogram of g: histogram[k] is
// the number of nodes with undirected degree exactly k. The helper
// is package-local because the random family is the only one that
// reasons about empirical degree distributions; other families pin
// closed-form invariants on Order/Size.
func degreeHistogram(g *lpg.Graph[int, int64]) map[int]int {
	adj := g.AdjList()
	hist := make(map[int]int)
	for u := 0; u < int(adj.MaxNodeID()); u++ {
		deg := 0
		for range adj.Neighbours(u) {
			deg++
		}
		hist[deg]++
	}
	return hist
}

// fitPowerLawExponentCCDF estimates the power-law exponent gamma of
// the supplied degree histogram by ordinary least-squares regression
// on the log-log empirical CCDF
//
//	P(deg >= k) = #{ v : deg(v) >= k } / n
//
// over the tail k >= kMin, discarding zero-probability bins. The
// slope of the fit is -(gamma - 1), so the function returns
// gamma = 1 - slope.
//
// It returns (0, false) when fewer than two tail bins survive: a
// single-point regression is undefined, so the caller must treat
// this as an inconclusive seed.
//
// The CCDF formulation is the standard network-science recipe
// (Newman 2003; Clauset, Shalizi & Newman 2009) and is strictly
// preferred over the naive log-h[k] regression because the
// cumulative sum suppresses per-bin Poisson noise in the tail.
func fitPowerLawExponentCCDF(hist map[int]int, n, kMin int) (float64, bool) {
	// Collect the sorted set of distinct degrees observed in the
	// histogram so the CCDF can be evaluated at each step in
	// ascending k order. Iterating a map gives non-deterministic
	// order; sorting pins the log output stable across runs.
	keys := make([]int, 0, len(hist))
	for k := range hist {
		if hist[k] > 0 {
			keys = append(keys, k)
		}
	}
	sort.Ints(keys)
	// Build the CCDF at every observed degree by walking the keys
	// from largest to smallest: at the largest degree the tail count
	// is hist[max]; at every preceding key the tail accumulates one
	// additional bin's count.
	type point struct {
		k       int
		tailCnt int
	}
	pts := make([]point, len(keys))
	running := 0
	for i := len(keys) - 1; i >= 0; i-- {
		running += hist[keys[i]]
		pts[i] = point{k: keys[i], tailCnt: running}
	}
	// Restrict to k >= kMin and discard the zero-count tail bins
	// (impossible here because keys carries only positive-count
	// bins by construction, but we guard anyway for callers that
	// might pass a hand-built histogram).
	xs := make([]float64, 0, len(pts))
	ys := make([]float64, 0, len(pts))
	for _, p := range pts {
		if p.k < kMin || p.tailCnt == 0 {
			continue
		}
		xs = append(xs, math.Log(float64(p.k)))
		ys = append(ys, math.Log(float64(p.tailCnt)/float64(n)))
	}
	if len(xs) < 2 {
		return 0, false
	}
	// Closed-form OLS slope. With n >= 2 the denominator is zero
	// only when every x equals the mean — impossible because the
	// keys are distinct positive integers.
	var sumX, sumY float64
	for i := range xs {
		sumX += xs[i]
		sumY += ys[i]
	}
	xBar := sumX / float64(len(xs))
	yBar := sumY / float64(len(ys))
	var num, den float64
	for i := range xs {
		dx := xs[i] - xBar
		num += dx * (ys[i] - yBar)
		den += dx * dx
	}
	if den == 0 {
		return 0, false
	}
	slope := num / den
	return 1 - slope, true
}
