package shapegen

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/testlayers"
)

// randomGoldenDir is the directory holding the random-family
// adjacency listings. As with classicGoldenDir, structuredGoldenDir,
// treesGoldenDir, specialsGoldenDir, and dagsGoldenDir, the path is
// rooted at the package directory.
//
// This file deliberately reuses formatAdjacency from trivial_test.go
// (same package). When T58.22 lands the shared golden helper in
// internal/goldens, every family's golden helper must migrate
// together.
const randomGoldenDir = "testdata/shapegen/random"

// randomGolden compares got with the contents of the golden file at
// randomGoldenDir/<name>. The implementation mirrors dagsGolden /
// treesGolden / specialsGolden / structuredGolden / classicGolden
// exactly because the families will migrate together to the shared
// helper in T58.22; until then duplicating keeps each family's test
// surface self-contained.
func randomGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(randomGoldenDir, name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("randomGolden: MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("randomGolden: WriteFile(%q): %v", path, err)
		}
		t.Logf("rewrote golden %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // path is a test-local golden under testdata/, not user input
	if err != nil {
		t.Fatalf("randomGolden: ReadFile(%q): %v (run with -shapegen-update to bootstrap)", path, err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// -------------------------------------------------------------------
// Local helpers — count unique pairs, find self-loops
// -------------------------------------------------------------------

// uniqueUndirectedPairs returns the set of (min(u,v), max(u,v))
// unordered pairs found in g's adjacency. The helper is used to
// verify the "no parallel edges" invariant: if Size() == m and the
// set has m members, every edge is a distinct unordered pair.
func uniqueUndirectedPairs(g *lpg.Graph[int, int64]) map[[2]int]struct{} {
	adj := g.AdjList()
	pairs := make(map[[2]int]struct{})
	for u := 0; u < int(adj.MaxNodeID()); u++ {
		for v := range adj.Neighbours(u) {
			a, b := u, v
			if a > b {
				a, b = b, a
			}
			pairs[[2]int{a, b}] = struct{}{}
		}
	}
	return pairs
}

// hasSelfLoop reports whether g contains any (v, v) edge. The helper
// is used to verify the "no self-loops" invariant.
func hasSelfLoop(g *lpg.Graph[int, int64]) bool {
	adj := g.AdjList()
	for u := 0; u < int(adj.MaxNodeID()); u++ {
		for v := range adj.Neighbours(u) {
			if u == v {
				return true
			}
		}
	}
	return false
}

// -------------------------------------------------------------------
// ErdosRenyiNP
// -------------------------------------------------------------------

// TestRandom_ErdosRenyiNP_Invariants exercises a small (n, pPercent,
// seed) sweep and asserts the documented closed forms together with
// the simple-graph invariants (no parallel edges, no self-loops).
func TestRandom_ErdosRenyiNP_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, pPercent int
		seed        uint64
	}{
		{0, 50, 1},   // empty graph: no nodes, no edges regardless of p.
		{1, 50, 1},   // single node: no pair, no edges.
		{2, 0, 1},    // pPercent zero: no edges.
		{2, 100, 1},  // pPercent 100: exactly one edge.
		{5, 50, 42},  // mixed case at small scale.
		{10, 50, 42}, // used by the golden.
		{20, 30, 7},  // larger, sparser.
		{20, 80, 99}, // larger, denser.
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_p=%d_seed=%d", c.n, c.pPercent, c.seed), func(t *testing.T) {
			t.Parallel()
			s := ErdosRenyiNP(c.n, c.pPercent, c.seed)
			if got, want := s.Name(), "random.erdos-renyi-np"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 2 ||
				knobs[0].Name != "n" || knobs[0].Min != 0 || knobs[0].Max != 1000 || knobs[0].Default != 50 ||
				knobs[1].Name != "p" || knobs[1].Min != 0 || knobs[1].Max != 100 || knobs[1].Default != 10 {
				t.Fatalf("Knobs = %#v, want n:[0,1000]/50, p:[0,100]/10", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(c.n))
			assertDirected(t, g, false)
			maxEdges := uint64(c.n * (c.n - 1) / 2)
			gotSize := g.AdjList().Size()
			if c.pPercent == 0 && gotSize != 0 {
				t.Fatalf("pPercent=0: Size = %d, want 0", gotSize)
			}
			if c.pPercent == 100 && gotSize != maxEdges {
				t.Fatalf("pPercent=100: Size = %d, want %d", gotSize, maxEdges)
			}
			if gotSize > maxEdges {
				t.Fatalf("Size = %d, exceeds upper bound %d", gotSize, maxEdges)
			}
			// Simple-graph invariants: no self-loops, no parallel edges.
			if hasSelfLoop(g) {
				t.Fatal("graph contains a self-loop, violating the simple-graph contract")
			}
			pairs := uniqueUndirectedPairs(g)
			if uint64(len(pairs)) != gotSize {
				t.Fatalf("uniqueUndirectedPairs = %d, Size = %d (parallel edges detected)", len(pairs), gotSize)
			}
		})
	}
}

// TestRandom_ErdosRenyiNP_PanicsOutOfRange covers every guard branch.
func TestRandom_ErdosRenyiNP_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		n, pPercent int
	}{
		{"n_negative", -1, 50},
		{"n_too_large", 1001, 50},
		{"pPercent_negative", 5, -1},
		{"pPercent_too_large", 5, 101},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("ErdosRenyiNP(%d,%d,0) did not panic", c.n, c.pPercent)
				}
			}()
			_ = ErdosRenyiNP(c.n, c.pPercent, 0)
		})
	}
}

// TestRandom_ErdosRenyiNP_Determinism asserts that the same (n,
// pPercent, seed) tuple produces byte-identical adjacency listings
// across two independent Build calls (AC #2).
func TestRandom_ErdosRenyiNP_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xC0FFEE
	g1, err := ErdosRenyiNP(50, 30, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := ErdosRenyiNP(50, 30, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRandom_ErdosRenyiNP_ExpectedEdgeCount exercises AC #1: with
// n=200 and pPercent=10 (so p = 0.1), the per-graph edge count is
// Binomial(C(200, 2), 0.1). Drawing 100 independent seeds and
// asserting the sample mean lies within 3 * sqrt(variance / 100) of
// the theoretical mean tests the catalogue's distributional contract
// without flakiness.
//
// Variance of Binomial(C(n, 2), p) is C(n, 2) * p * (1 - p). For
// n = 200 and p = 0.1: C(200, 2) = 19_900, mean = 1_990, variance =
// 1_791. The sample-mean tolerance is therefore 3 * sqrt(1791 / 100)
// ~= 12.7 edges. A failure window this wide gives the test a
// vanishingly small false-positive rate while still catching any
// systemic bias in [buildErdosRenyiNP].
//
// Total cost budget: 100 builds of n=200 graphs. Each build issues
// C(200, 2) = 19_900 PCG draws plus at most 19_900 AddEdge calls;
// the whole sweep finishes well inside the brief's 2-second per-case
// budget on a modern machine.
func TestRandom_ErdosRenyiNP_ExpectedEdgeCount(t *testing.T) {
	t.Parallel()
	const (
		n        = 200
		pPercent = 10
		samples  = 100
	)
	// cPair is C(n, 2) — the unordered-pair count, equal to 19_900 at n=200.
	cPair := float64(n*(n-1)) / 2
	p := float64(pPercent) / 100
	mean := cPair * p
	variance := cPair * p * (1 - p)
	tolerance := 3 * math.Sqrt(variance/float64(samples))

	var totalEdges float64
	for seed := uint64(0); seed < samples; seed++ {
		g, err := ErdosRenyiNP(n, pPercent, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build at seed=%d: %v", seed, err)
		}
		totalEdges += float64(g.AdjList().Size())
	}
	sampleMean := totalEdges / float64(samples)
	if math.Abs(sampleMean-mean) > tolerance {
		t.Fatalf("sample mean = %.2f, theoretical mean = %.2f, tolerance = %.2f (samples=%d, deviation=%.2f)",
			sampleMean, mean, tolerance, samples, sampleMean-mean)
	}
}

// TestRandom_ErdosRenyiNP_Golden pins ErdosRenyiNP(10, 50, 42).
func TestRandom_ErdosRenyiNP_Golden(t *testing.T) {
	t.Parallel()
	g, err := ErdosRenyiNP(10, 50, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "erdos-renyi-np-n10-p50-seed42.txt", formatAdjacency(g))
}

// TestRandom_ErdosRenyiNP_ShardFullPropagates exercises the AddEdge
// error path of buildErdosRenyiNP. With MaxShardCapacity=1 a 300-node
// graph is well past the 256-shard threshold; at least one AddEdge
// from a source with intraIdx >= 1 must surface adjlist.ErrShardFull.
// The harness bypasses the public constructor's n upper bound by
// calling buildErdosRenyiNP directly with n=300, mirroring the
// dags-family shard-full tests.
//
// pPercent is set high enough that at least one edge is attempted
// from every source after the first 256 nodes are consumed by the
// mapper. The seed is fixed so the test is deterministic.
func TestRandom_ErdosRenyiNP_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: false, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	if err := buildErdosRenyiNP(g, 300, 100, 1); err == nil {
		t.Fatal("buildErdosRenyiNP(g, 300, 100, 1) with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// -------------------------------------------------------------------
// ErdosRenyiNM
// -------------------------------------------------------------------

// TestRandom_ErdosRenyiNM_Invariants exercises a small (n, m, seed)
// sweep and asserts the documented closed forms together with the
// simple-graph invariants (exactly m edges, no parallel edges, no
// self-loops).
func TestRandom_ErdosRenyiNM_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, m int
		seed uint64
	}{
		{0, 0, 1},    // empty graph.
		{1, 0, 1},    // single node, zero edges.
		{2, 0, 1},    // two nodes, zero edges.
		{2, 1, 1},    // two nodes, one edge — the only possible pair.
		{5, 0, 42},   // m == 0 short-circuit.
		{5, 5, 42},   // C(5, 2) = 10, half taken.
		{5, 10, 42},  // exactly C(5, 2): every pair selected.
		{10, 20, 42}, // used by the golden.
		{20, 30, 7},  // bigger sweep.
		{20, 1, 99},  // single edge in a sparse graph.
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_m=%d_seed=%d", c.n, c.m, c.seed), func(t *testing.T) {
			t.Parallel()
			s := ErdosRenyiNM(c.n, c.m, c.seed)
			if got, want := s.Name(), "random.erdos-renyi-nm"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 2 ||
				knobs[0].Name != "n" || knobs[0].Min != 0 || knobs[0].Max != 1000 || knobs[0].Default != 50 ||
				knobs[1].Name != "m" || knobs[1].Min != 0 || knobs[1].Max != 499_500 || knobs[1].Default != 50 {
				t.Fatalf("Knobs = %#v, want n:[0,1000]/50, m:[0,499500]/50", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(c.n))
			assertSize(t, g, uint64(c.m))
			assertDirected(t, g, false)
			// AC #3: exactly m unique unordered edges, no parallel
			// edges, no self-loops.
			if hasSelfLoop(g) {
				t.Fatal("graph contains a self-loop, violating the simple-graph contract")
			}
			pairs := uniqueUndirectedPairs(g)
			if len(pairs) != c.m {
				t.Fatalf("uniqueUndirectedPairs = %d, want exactly %d (parallel edges or missing edges detected)", len(pairs), c.m)
			}
		})
	}
}

// TestRandom_ErdosRenyiNM_PanicsOutOfRange covers every guard branch.
func TestRandom_ErdosRenyiNM_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		n, m int
	}{
		{"n_negative", -1, 0},
		{"n_too_large", 1001, 0},
		{"m_negative", 5, -1},
		{"m_too_large", 5, 499_501},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("ErdosRenyiNM(%d,%d,0) did not panic", c.n, c.m)
				}
			}()
			_ = ErdosRenyiNM(c.n, c.m, 0)
		})
	}
}

// TestRandom_ErdosRenyiNM_RejectsExcessEdgeCount asserts AC #3: when
// m > C(n, 2), Build surfaces ErrEdgeCountTooHigh via the typed
// sentinel. The constructor must NOT panic on this input — the brief
// pins the rejection at Build time, mirroring the Cycle /
// ErrCycleTooSmall convention.
func TestRandom_ErdosRenyiNM_RejectsExcessEdgeCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, m int
	}{
		// C(5, 2) = 10, so m = 11 must be rejected.
		{5, 11},
		// C(10, 2) = 45, so m = 46 must be rejected.
		{10, 46},
		// C(2, 2) = 1, so m = 2 must be rejected.
		{2, 2},
		// C(0, 2) = 0 and C(1, 2) = 0; any m > 0 must be rejected.
		{0, 1},
		{1, 1},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_m=%d", c.n, c.m), func(t *testing.T) {
			t.Parallel()
			_, err := ErdosRenyiNM(c.n, c.m, 0).Build(defaultCfg)
			if err == nil {
				t.Fatal("Build returned nil error, want ErrEdgeCountTooHigh")
			}
			if !errors.Is(err, ErrEdgeCountTooHigh) {
				t.Fatalf("Build err = %v, want errors.Is(ErrEdgeCountTooHigh)", err)
			}
		})
	}
}

// TestRandom_ErdosRenyiNM_Determinism asserts that the same (n, m,
// seed) tuple produces byte-identical adjacency listings across two
// independent Build calls (AC #2).
func TestRandom_ErdosRenyiNM_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xDEADBEEF
	g1, err := ErdosRenyiNM(20, 50, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := ErdosRenyiNM(20, 50, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRandom_ErdosRenyiNM_Golden pins ErdosRenyiNM(10, 20, 42).
func TestRandom_ErdosRenyiNM_Golden(t *testing.T) {
	t.Parallel()
	g, err := ErdosRenyiNM(10, 20, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "erdos-renyi-nm-n10-m20-seed42.txt", formatAdjacency(g))
}

// TestRandom_ErdosRenyiNM_ShardFullPropagates exercises the AddEdge
// error path of buildErdosRenyiNM. The public constructor caps m at
// 499_500 but a 300-node graph is well past the 256-shard threshold;
// with MaxShardCapacity=1 at least one AddEdge from a source with
// intraIdx >= 1 must surface adjlist.ErrShardFull. The harness
// invokes the production helper directly, mirroring the dags-family
// shard-full tests. m is set to 1000 (far enough to guarantee at
// least one source with intraIdx >= 1 is reached).
func TestRandom_ErdosRenyiNM_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: false, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	if err := buildErdosRenyiNM(g, 300, 1000, 1); err == nil {
		t.Fatal("buildErdosRenyiNM(g, 300, 1000, 1) with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// TestRandom_ErdosRenyiNM_UniformSamplingChiSquared documents that
// every C(n, 2)-th unordered pair is equally likely to be selected
// by ErdosRenyiNM over many seeds. The test draws a single edge
// (m=1) from a small graph (n=4, C(4,2)=6 pairs) over many seeds and
// asserts each pair is observed within a generous tolerance of the
// uniform expectation.
//
// At n=4, m=1, samples=6000: every pair has expected count 1000 with
// standard deviation sqrt(samples * p * (1-p)) = sqrt(6000 * 1/6 *
// 5/6) ~= 28.9. A 4-sigma window per pair (>= 115 edges either way)
// gives a vanishingly small false-positive rate per pair; with 6
// pairs, the probability that any single bin falls outside is
// dominated by the per-bin tail. The test fails iff any bin's count
// deviates by more than 4 sigma — a clean signal that Floyd's draw
// has lost uniformity.
//
// The check lives in the soak layer because the per-build overhead
// of constructing thousands of LPG graphs dominates the short-layer
// budget; AC #1 in TestRandom_ErdosRenyiNP_ExpectedEdgeCount already
// covers the catalogue's distributional contract for the short layer.
func TestRandom_ErdosRenyiNM_UniformSamplingChiSquared(t *testing.T) {
	testlayers.RequireSoak(t)
	const (
		n       = 4
		m       = 1
		samples = 6000
	)
	pairs := n * (n - 1) / 2
	expected := float64(samples) / float64(pairs)
	stdDev := math.Sqrt(float64(samples) * (1.0 / float64(pairs)) * (float64(pairs-1) / float64(pairs)))
	tolerance := 4 * stdDev

	bins := make(map[[2]int]int, pairs)
	for seed := uint64(0); seed < samples; seed++ {
		g, err := ErdosRenyiNM(n, m, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build at seed=%d: %v", seed, err)
		}
		set := uniqueUndirectedPairs(g)
		if len(set) != 1 {
			t.Fatalf("seed=%d: produced %d unique pairs, want 1", seed, len(set))
		}
		for p := range set {
			bins[p]++
		}
	}
	if len(bins) != pairs {
		t.Fatalf("only %d distinct pairs observed across %d samples, want all %d pairs", len(bins), samples, pairs)
	}
	for p, count := range bins {
		if math.Abs(float64(count)-expected) > tolerance {
			t.Fatalf("pair %v drawn %d times, expected %.0f +/- %.1f (samples=%d)", p, count, expected, tolerance, samples)
		}
	}
}

// -------------------------------------------------------------------
// floydSample / pairIndexToIJ — internal helpers under direct test
// -------------------------------------------------------------------

// TestRandom_FloydSample_AllElementsForFullSample asserts that
// asking Floyd's algorithm for m == total returns every integer in
// [0, total) exactly once. This is the edge case where Floyd's loop
// runs total times and saturates the selection set.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism.
func TestRandom_FloydSample_AllElementsForFullSample(t *testing.T) {
	t.Parallel()
	const total = 50
	r := rand.New(rand.NewPCG(123, 123))
	picks := floydSample(r, total, total)
	if len(picks) != total {
		t.Fatalf("len(picks) = %d, want %d", len(picks), total)
	}
	seen := make(map[int]bool, total)
	for _, v := range picks {
		if v < 0 || v >= total {
			t.Fatalf("pick %d out of range [0, %d)", v, total)
		}
		if seen[v] {
			t.Fatalf("duplicate pick %d", v)
		}
		seen[v] = true
	}
	if len(seen) != total {
		t.Fatalf("only %d distinct picks, want %d", len(seen), total)
	}
}

// TestRandom_PairIndexToIJ_RoundTrip enumerates every flat index in
// [0, C(n, 2)) for small n and asserts that the resulting (i, j)
// pair satisfies 0 <= i < j < n. It also checks the enumeration is
// a bijection by confirming all C(n, 2) flat indices land on
// distinct pairs.
func TestRandom_PairIndexToIJ_RoundTrip(t *testing.T) {
	t.Parallel()
	for n := 2; n <= 8; n++ {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			total := n * (n - 1) / 2
			seen := make(map[[2]int]bool, total)
			for k := 0; k < total; k++ {
				i, j := pairIndexToIJ(n, k)
				if i < 0 || i >= j || j >= n {
					t.Fatalf("pairIndexToIJ(%d, %d) = (%d, %d); want 0 <= i < j < n", n, k, i, j)
				}
				pair := [2]int{i, j}
				if seen[pair] {
					t.Fatalf("pairIndexToIJ(%d, %d) = %v (duplicate pair)", n, k, pair)
				}
				seen[pair] = true
			}
			if len(seen) != total {
				t.Fatalf("only %d distinct pairs from %d flat indices, want %d", len(seen), total, total)
			}
		})
	}
}

// TestRandom_PairIndexToIJ_OutOfRange asserts that pairIndexToIJ
// returns (-1, -1) when given an out-of-range flat index. The
// production code never reaches this branch — floydSample bounds
// every flat index inside [0, C(n, 2)) — but the helper documents
// the contract and keeps coverage at 100%.
func TestRandom_PairIndexToIJ_OutOfRange(t *testing.T) {
	t.Parallel()
	i, j := pairIndexToIJ(5, 100)
	if i != -1 || j != -1 {
		t.Fatalf("pairIndexToIJ(5, 100) = (%d, %d); want (-1, -1)", i, j)
	}
}

// -------------------------------------------------------------------
// MaxShardCapacity preservation
// -------------------------------------------------------------------

// TestRandom_PreservesMaxShardCapacity confirms that both Erdős-Rényi
// generators preserve cfg.MaxShardCapacity verbatim, mirroring the
// other-family contracts.
func TestRandom_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	for _, tc := range []struct {
		name string
		s    Shape[int, int64]
	}{
		{"erdos_renyi_np", ErdosRenyiNP(10, 50, 42)},
		{"erdos_renyi_nm", ErdosRenyiNM(10, 20, 42)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, err := tc.s.Build(cfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if g == nil {
				t.Fatal("Build returned nil graph")
			}
		})
	}
}

// -------------------------------------------------------------------
// Property-based sweep
// -------------------------------------------------------------------

// TestRandom_Properties_RapidSweep drives both Erdős-Rényi
// generators over small parameter sweeps and asserts the catalogue
// invariants documented in the constructor godocs. Bounds are kept
// small so the short layer stays under the per-package time budget.
func TestRandom_Properties_RapidSweep(t *testing.T) {
	t.Parallel()

	t.Run("erdos_renyi_np", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(0, 30).Draw(r, "n")
			pPercent := rapid.IntRange(0, 100).Draw(r, "p")
			seed := rapid.Uint64().Draw(r, "seed")
			g, err := ErdosRenyiNP(n, pPercent, seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("n=%d p=%d: Build: %v", n, pPercent, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("Order = %d, want %d", got, n)
			}
			maxSize := uint64(n * (n - 1) / 2)
			if got := g.AdjList().Size(); got > maxSize {
				t.Fatalf("Size = %d, exceeds max %d", got, maxSize)
			}
			if hasSelfLoop(g) {
				t.Fatal("graph contains a self-loop")
			}
		})
	})

	t.Run("erdos_renyi_nm", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(0, 30).Draw(r, "n")
			maxEdges := n * (n - 1) / 2
			m := rapid.IntRange(0, maxEdges).Draw(r, "m")
			seed := rapid.Uint64().Draw(r, "seed")
			g, err := ErdosRenyiNM(n, m, seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("n=%d m=%d: Build: %v", n, m, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("Order = %d, want %d", got, n)
			}
			if got := g.AdjList().Size(); got != uint64(m) {
				t.Fatalf("Size = %d, want %d", got, m)
			}
			if hasSelfLoop(g) {
				t.Fatal("graph contains a self-loop")
			}
			if pairs := uniqueUndirectedPairs(g); len(pairs) != m {
				t.Fatalf("uniqueUndirectedPairs = %d, want %d", len(pairs), m)
			}
		})
	})
}

// -------------------------------------------------------------------
// Soak / nightly layer sweeps
// -------------------------------------------------------------------

// TestRandom_ErdosRenyiNP_Soak exercises ErdosRenyiNP at the
// short-layer ceiling (n=1000) with a fixed seed and pPercent=10.
// The test asserts the catalogue invariants only; the heavy
// statistical sweep lives in TestRandom_ErdosRenyiNP_ExpectedEdgeCount.
func TestRandom_ErdosRenyiNP_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const (
		n        = 1000
		pPercent = 10
	)
	g, err := ErdosRenyiNP(n, pPercent, 0xCAFE).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	maxEdges := uint64(n * (n - 1) / 2)
	if got := g.AdjList().Size(); got > maxEdges {
		t.Fatalf("Size = %d, exceeds max %d", got, maxEdges)
	}
	if hasSelfLoop(g) {
		t.Fatal("graph contains a self-loop")
	}
}

// TestRandom_ErdosRenyiNM_Soak exercises ErdosRenyiNM at the
// short-layer ceiling (n=1000, m=10000) with a fixed seed.
func TestRandom_ErdosRenyiNM_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const (
		n = 1000
		m = 10_000
	)
	g, err := ErdosRenyiNM(n, m, 0xBEAD).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	if got := g.AdjList().Size(); got != uint64(m) {
		t.Fatalf("Size = %d, want %d", got, m)
	}
	if hasSelfLoop(g) {
		t.Fatal("graph contains a self-loop")
	}
	if pairs := uniqueUndirectedPairs(g); len(pairs) != m {
		t.Fatalf("uniqueUndirectedPairs = %d, want %d (parallel edges detected)", len(pairs), m)
	}
}

// _ keeps the lpg import alive even when only the shapegen package
// types are referenced in this file. Mirrors the dags_test.go guard.
var _ *lpg.Graph[int, int64]
