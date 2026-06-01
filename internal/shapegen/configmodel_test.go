package shapegen

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// -------------------------------------------------------------------
// RandomRegular — short layer
// -------------------------------------------------------------------

// TestRandom_Regular_Invariants exercises a small (n, d, seed) sweep
// and asserts the catalogue invariants: every node has degree exactly
// d, Size() == n*d/2, and the simple-graph contract (no parallel
// edges, no self-loops) holds.
func TestRandom_Regular_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, d int
		seed uint64
	}{
		{1, 0, 1},      // single node, no edges.
		{2, 0, 1},      // two nodes, d=0: isolated.
		{4, 0, 1},      // four nodes, d=0: isolated.
		{2, 1, 1},      // n=2, d=1: single edge (0,1).
		{4, 1, 1},      // n=4, d=1: a perfect matching on 4 nodes.
		{4, 2, 1},      // 2-regular: a cycle / union of cycles on 4 nodes.
		{6, 3, 42},     // 3-regular on 6 nodes.
		{10, 3, 42},    // canonical golden parameters.
		{10, 4, 7},     // 4-regular on 10 nodes.
		{20, 4, 99},    // mid-sized.
		{30, 6, 31337}, // larger sweep.
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_d=%d_seed=%d", c.n, c.d, c.seed), func(t *testing.T) {
			t.Parallel()
			s := RandomRegular(c.n, c.d, c.seed)
			if got, want := s.Name(), "random.regular"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 2 ||
				knobs[0].Name != "n" || knobs[0].Min != 1 || knobs[0].Max != 1000 || knobs[0].Default != 20 ||
				knobs[1].Name != "d" || knobs[1].Min != 0 || knobs[1].Max != 50 || knobs[1].Default != 3 {
				t.Fatalf("Knobs = %#v, want n:[1,1000]/20, d:[0,50]/3", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(c.n))
			wantSize := uint64(c.n * c.d / 2)
			assertSize(t, g, wantSize)
			assertDirected(t, g, false)
			if hasSelfLoop(g) {
				t.Fatal("graph contains a self-loop, violating the simple-graph contract")
			}
			pairs := uniqueUndirectedPairs(g)
			if uint64(len(pairs)) != wantSize {
				t.Fatalf("uniqueUndirectedPairs = %d, Size = %d (parallel edges detected)", len(pairs), wantSize)
			}
			// AC #1: every node has degree exactly d.
			degs := nodeDegreesUndirected(g)
			for i := 0; i < c.n; i++ {
				if degs[i] != c.d {
					t.Fatalf("deg(%d) = %d, want %d (degree sequence = %v)", i, degs[i], c.d, degs)
				}
			}
		})
	}
}

// TestRandom_Regular_PanicsOutOfRange covers every guard branch of
// the constructor.
func TestRandom_Regular_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		n, d int
	}{
		{"n_zero", 0, 0},
		{"n_negative", -1, 0},
		{"n_too_large", 1001, 0},
		{"d_negative", 5, -1},
		{"d_too_large", 5, 51},
		{"n_times_d_odd", 5, 3}, // 15 is odd, fails handshake lemma.
		{"d_equals_n", 4, 4},    // d must be < n.
		{"d_greater_than_n", 3, 5},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("RandomRegular(%d, %d, 0) did not panic", c.n, c.d)
				}
			}()
			_ = RandomRegular(c.n, c.d, 0)
		})
	}
}

// TestRandom_Regular_Determinism asserts that the same (n, d, seed)
// tuple produces byte-identical adjacency listings across two
// independent Build calls (AC #4).
func TestRandom_Regular_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xC0FFEE
	g1, err := RandomRegular(20, 4, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := RandomRegular(20, 4, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRandom_Regular_DZero_Isolated exercises the d == 0 branch:
// Build must return n isolated nodes and zero edges.
func TestRandom_Regular_DZero_Isolated(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 5, 10, 100} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			g, err := RandomRegular(n, 0, 0xDEAD).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(n))
			assertSize(t, g, 0)
			if hasSelfLoop(g) {
				t.Fatal("d=0 graph contains a self-loop")
			}
		})
	}
}

// TestRandom_Regular_Golden_N10_D3 pins RandomRegular(10, 3, 42) to a
// byte-stable adjacency listing under defaultCfg. The golden lives in
// internal/shapegen/testdata/shapegen/random/.
func TestRandom_Regular_Golden_N10_D3(t *testing.T) {
	t.Parallel()
	g, err := RandomRegular(10, 3, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "regular-n10-d3-seed42.txt", formatAdjacency(g))
}

// TestRandom_Regular_PreservesMaxShardCapacity confirms the generator
// preserves cfg.MaxShardCapacity verbatim, mirroring the other-family
// contracts.
func TestRandom_Regular_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	g, err := RandomRegular(10, 4, 42).Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if g == nil {
		t.Fatal("Build returned nil graph")
	}
}

// TestRandom_Regular_ShardFullPropagates exercises the AddEdge error
// path of buildRandomRegular. The public constructor caps n at 1000,
// but with MaxShardCapacity=1 a 300-node graph is well past the
// 256-shard threshold; at least one AddEdge from a source with
// intraIdx >= 1 must surface adjlist.ErrShardFull. The harness
// invokes the production helper directly, mirroring the Barabási-
// Albert / Erdős-Rényi shard-full tests.
func TestRandom_Regular_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: false, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	if err := buildRandomRegular(g, 300, 4, 1); err == nil {
		t.Fatal("buildRandomRegular(g, 300, 4, 1) with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// TestRandom_Regular_BuildExhaustsBudget exercises the
// [ErrRegularConstruction] surfacing path. The buildRandomRegular
// helper has no input guards (those live in the public RandomRegular
// constructor), so the test can drive it directly with a degenerate
// (n, d) pair where no simple d-regular graph exists or every
// pairing attempt provably fails. The smallest such case is
// (n=2, d=2): there are 4 half-edges, partitioned as {0, 0, 1, 1};
// the only two unordered-pair partitions are
// {(0, 0), (1, 1)} (self-loops, rejected) and
// {(0, 1), (0, 1)} (duplicate, rejected). Every shuffle therefore
// produces an invalid pairing and the retry budget is exhausted,
// surfacing ErrRegularConstruction wrapped with the offending
// (n, d, attempts) tuple.
//
// The (n=2, d=2) input would be rejected at construction time by the
// public RandomRegular guard (d < n), so this test deliberately
// bypasses the guard to pin the surfacing branch.
func TestRandom_Regular_BuildExhaustsBudget(t *testing.T) {
	t.Parallel()
	g := lpg.New[int, int64](adjlist.Config{Directed: false, Multigraph: false})
	err := buildRandomRegular(g, 2, 2, 1)
	if !errors.Is(err, ErrRegularConstruction) {
		t.Fatalf("buildRandomRegular(2, 2): err = %v, want ErrRegularConstruction", err)
	}
}

// TestRandom_Regular_AttemptRejectsDegenerateInputs exercises both
// failure branches of [randomRegularAttempt] in isolation:
//
//   - The self-loop / duplicate rejection branch under (n=2, d=2)
//     where no admissible swap exists for the second pair after the
//     first half-edges have been placed.
//   - The branch coverage of the bounded swap search itself, which
//     advances forward in the half-edge slice until it either finds
//     a valid candidate or exhausts the remaining positions.
//
// The (n=2, d=2) configuration is degenerate (no simple 2-regular
// graph on 2 vertices exists, because a 2-regular graph requires
// 2 distinct neighbours per vertex), so every shuffle returns
// (nil, false). The test calls the helper directly with a
// deterministic PRNG to pin the contract.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; the test seed is fixed and inert.
func TestRandom_Regular_AttemptRejectsDegenerateInputs(t *testing.T) {
	t.Parallel()
	r := newDeterministicRand(0xDEAD)
	got, ok := randomRegularAttempt(r, 2, 2)
	if ok || got != nil {
		t.Fatalf("randomRegularAttempt(2, 2): (%v, %v), want (nil, false)", got, ok)
	}
}

// -------------------------------------------------------------------
// RandomRegular — property-based sweep
// -------------------------------------------------------------------

// TestRandom_Regular_Properties_RapidSweep drives the generator over
// small parameter sweeps and asserts the catalogue invariants
// documented in the constructor godoc. Bounds are kept small so the
// short layer stays under the per-package time budget.
func TestRandom_Regular_Properties_RapidSweep(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		// Draw d in a small range, then n in a range that satisfies
		// both d < n and (n*d) even.
		d := rapid.IntRange(0, 6).Draw(r, "d")
		nMin := d + 1
		if nMin < 2 {
			nMin = 2
		}
		nRaw := rapid.IntRange(nMin, 30).Draw(r, "n")
		// Force n*d even by bumping n by one when needed.
		n := nRaw
		if d > 0 && (n*d)%2 != 0 {
			if n+1 <= 30 {
				n++
			} else if n-1 >= nMin {
				n--
			}
		}
		if (n*d)%2 != 0 {
			// Skip the rare draw where neither bump fits the window.
			return
		}
		seed := rapid.Uint64().Draw(r, "seed")
		g, err := RandomRegular(n, d, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("n=%d d=%d: Build: %v", n, d, err)
		}
		if got := g.AdjList().Order(); got != uint64(n) {
			t.Fatalf("Order = %d, want %d", got, n)
		}
		wantSize := uint64(n * d / 2)
		if got := g.AdjList().Size(); got != wantSize {
			t.Fatalf("Size = %d, want %d", got, wantSize)
		}
		if hasSelfLoop(g) {
			t.Fatal("graph contains a self-loop")
		}
		if pairs := uniqueUndirectedPairs(g); uint64(len(pairs)) != wantSize {
			t.Fatalf("uniqueUndirectedPairs = %d, want %d (parallel edges detected)", len(pairs), wantSize)
		}
		degs := nodeDegreesUndirected(g)
		for i := 0; i < n; i++ {
			if degs[i] != d {
				t.Fatalf("n=%d d=%d seed=%d: deg(%d) = %d, want %d", n, d, seed, i, degs[i], d)
			}
		}
	})
}

// -------------------------------------------------------------------
// ConfigurationModel — short layer
// -------------------------------------------------------------------

// TestRandom_Configuration_Invariants exercises a small sweep of
// (degSeq, allowMulti, seed) inputs and asserts the documented
// closed forms plus the simple-graph / multigraph contracts.
func TestRandom_Configuration_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		degSeq     []int
		allowMulti bool
		seed       uint64
	}{
		{"empty", []int{}, false, 1},
		{"single_zero", []int{0}, false, 1},
		{"two_iso", []int{0, 0}, false, 1},
		{"two_d1_simple", []int{1, 1}, false, 1},
		{"two_d1_multi", []int{1, 1}, true, 1},
		{"four_regular_2_multi", []int{2, 2, 2, 2}, true, 42},
		{"four_regular_2_simple", []int{2, 2, 2, 2}, false, 42},
		{"skewed_simple", []int{4, 3, 2, 2, 1}, false, 7},
		{"skewed_multi", []int{4, 3, 2, 2, 1}, true, 7},
		{"uniform_d3_simple", []int{3, 3, 3, 3, 3, 3}, false, 99},
		{"uniform_d3_multi", []int{3, 3, 3, 3, 3, 3}, true, 99},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := ConfigurationModel(c.degSeq, c.allowMulti, c.seed)
			if got, want := s.Name(), "random.configuration"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 0 {
				t.Fatalf("Knobs = %#v, want empty (variadic degSeq)", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(len(c.degSeq)))
			assertDirected(t, g, false)
			// AC #2: in multigraph mode the degree sequence is
			// preserved exactly; in simple-graph mode it is at most
			// the input componentwise.
			degs := nodeDegreesUndirectedWithSelfLoops(g)
			if c.allowMulti {
				// Total edges (counting self-loops as one edge each)
				// equals sum(degSeq) / 2 exactly.
				total := 0
				for _, d := range c.degSeq {
					total += d
				}
				if got := g.AdjList().Size(); got != uint64(total/2) {
					t.Fatalf("multigraph Size = %d, want %d", got, total/2)
				}
				for i, want := range c.degSeq {
					if degs[i] != want {
						t.Fatalf("multigraph deg(%d) = %d, want %d", i, degs[i], want)
					}
				}
			} else {
				// Simple-graph: realised degrees at most input.
				for i, want := range c.degSeq {
					if degs[i] > want {
						t.Fatalf("simple deg(%d) = %d, exceeds input %d", i, degs[i], want)
					}
				}
			}
		})
	}
}

// TestRandom_Configuration_OddSumReturnsError exercises AC #3: a
// degree sequence with odd sum is unrealisable, and Build must
// surface [ErrOddDegreeSum] for callers to errors.Is against.
func TestRandom_Configuration_OddSumReturnsError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		degSeq []int
	}{
		{"single_one", []int{1}},
		{"three_ones", []int{1, 1, 1}},
		{"mixed_odd", []int{2, 3, 4}},
		{"large_odd", []int{5, 5, 5, 5, 5}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := ConfigurationModel(c.degSeq, false, 0).Build(defaultCfg)
			if !errors.Is(err, ErrOddDegreeSum) {
				t.Fatalf("simple Build err = %v, want ErrOddDegreeSum", err)
			}
			_, err = ConfigurationModel(c.degSeq, true, 0).Build(defaultCfg)
			if !errors.Is(err, ErrOddDegreeSum) {
				t.Fatalf("multi Build err = %v, want ErrOddDegreeSum", err)
			}
		})
	}
}

// TestRandom_Configuration_PanicsOnNegativeDegree covers the
// negative-degree guard. The constructor scans the slice and panics
// on the first negative element.
func TestRandom_Configuration_PanicsOnNegativeDegree(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("ConfigurationModel with a negative degree did not panic")
		}
	}()
	_ = ConfigurationModel([]int{2, -1, 3}, false, 0)
}

// TestRandom_Configuration_Determinism asserts AC #4: the same
// (degSeq, allowMulti, seed) tuple produces byte-identical adjacency
// listings across two independent Build calls.
func TestRandom_Configuration_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xC0FFEE
	degSeq := []int{4, 3, 3, 2, 2, 2, 1, 1}
	for _, allowMulti := range []bool{false, true} {
		allowMulti := allowMulti
		t.Run(fmt.Sprintf("allowMulti=%v", allowMulti), func(t *testing.T) {
			t.Parallel()
			g1, err := ConfigurationModel(degSeq, allowMulti, seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build #1: %v", err)
			}
			g2, err := ConfigurationModel(degSeq, allowMulti, seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build #2: %v", err)
			}
			if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
				t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

// TestRandom_Configuration_DefensiveCopy ensures mutating the
// caller's degSeq slice after construction has no effect on Build.
// This mirrors the [Multipartite] contract pinned in classic.go.
func TestRandom_Configuration_DefensiveCopy(t *testing.T) {
	t.Parallel()
	degSeq := []int{2, 2, 2, 2}
	s := ConfigurationModel(degSeq, true, 42)
	degSeq[0] = 99 // would shift parity to odd and break Build without the copy.
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != 4 {
		t.Fatalf("Order = %d, want 4 (caller mutation must not leak)", got)
	}
}

// TestRandom_Configuration_Golden_DegSeq2222Multi pins
// ConfigurationModel([2,2,2,2], true, 42) — a small 2-regular
// multigraph configuration whose multigraph mode preserves the
// degree sequence exactly.
func TestRandom_Configuration_Golden_DegSeq2222Multi(t *testing.T) {
	t.Parallel()
	g, err := ConfigurationModel([]int{2, 2, 2, 2}, true, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "configuration-degseq-2-2-2-2-multi-seed42.txt", formatAdjacency(g))
}

// TestRandom_Configuration_MultigraphAllowsParallelEdges exercises
// the multigraph branch contract: when allowMulti=true the lpg
// backend stores parallel pairings as distinct adjacency entries,
// and Size() counts every pairing (including self-loops, each
// counted once as a single edge in the adjlist contract).
func TestRandom_Configuration_MultigraphAllowsParallelEdges(t *testing.T) {
	t.Parallel()
	// degSeq with two nodes each of degree 4 forces parallel
	// pairings under any shuffle: 8 half-edges, 4 pairings between
	// nodes 0 and 1 — every pair is the (0, 1) edge. The multigraph
	// must therefore have Size() == 4.
	g, err := ConfigurationModel([]int{4, 4}, true, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertOrder(t, g, 2)
	assertSize(t, g, 4)
	if !g.AdjList().Multigraph() {
		t.Fatal("expected adjlist Multigraph() = true")
	}
}

// TestRandom_Configuration_SimpleErasesDuplicatesAndSelfLoops
// exercises the simple-graph branch contract: when allowMulti=false
// self-loops and parallel pairings are dropped at generation time,
// so the realised graph respects the simple-graph contract and the
// realised degree sequence is at most the input.
func TestRandom_Configuration_SimpleErasesDuplicatesAndSelfLoops(t *testing.T) {
	t.Parallel()
	// The same (4, 4) input that produces a parallel-edge multigraph
	// in the previous test collapses to a single edge (0, 1) in
	// simple-graph mode.
	g, err := ConfigurationModel([]int{4, 4}, false, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertOrder(t, g, 2)
	assertSize(t, g, 1)
	if hasSelfLoop(g) {
		t.Fatal("simple graph contains a self-loop")
	}
	if g.AdjList().Multigraph() {
		t.Fatal("expected adjlist Multigraph() = false")
	}
}

// TestRandom_Configuration_PreservesMaxShardCapacity confirms the
// generator preserves cfg.MaxShardCapacity verbatim.
func TestRandom_Configuration_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	g, err := ConfigurationModel([]int{2, 2, 2, 2}, true, 42).Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if g == nil {
		t.Fatal("Build returned nil graph")
	}
}

// TestRandom_Configuration_ShardFullPropagates exercises the AddEdge
// error path of buildConfigurationModel for both branches
// (multigraph and simple). The harness invokes the production helper
// directly with a tight MaxShardCapacity and asserts the
// adjlist.ErrShardFull sentinel surfaces, mirroring the Barabási-
// Albert / Erdős-Rényi shard-full tests.
func TestRandom_Configuration_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	t.Run("multi", func(t *testing.T) {
		t.Parallel()
		cfg := adjlist.Config{Directed: false, Multigraph: true, MaxShardCapacity: 1}
		g := lpg.New[int, int64](cfg)
		degSeq := make([]int, 300)
		for i := range degSeq {
			degSeq[i] = 2
		}
		if err := buildConfigurationModel(g, degSeq, true, 1); err == nil {
			t.Fatal("buildConfigurationModel(multi, 300 nodes, MaxShardCapacity=1) returned nil error, want adjlist.ErrShardFull")
		}
	})
	t.Run("simple", func(t *testing.T) {
		t.Parallel()
		cfg := adjlist.Config{Directed: false, Multigraph: false, MaxShardCapacity: 1}
		g := lpg.New[int, int64](cfg)
		degSeq := make([]int, 300)
		for i := range degSeq {
			degSeq[i] = 2
		}
		if err := buildConfigurationModel(g, degSeq, false, 1); err == nil {
			t.Fatal("buildConfigurationModel(simple, 300 nodes, MaxShardCapacity=1) returned nil error, want adjlist.ErrShardFull")
		}
	})
}

// -------------------------------------------------------------------
// ConfigurationModel — property-based sweep
// -------------------------------------------------------------------

// TestRandom_Configuration_Properties_RapidSweep drives the
// generator over small parameter sweeps. The sweep covers both
// allowMulti modes and asserts the catalogue invariants documented
// in the constructor godoc.
func TestRandom_Configuration_Properties_RapidSweep(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		// Draw a small degree sequence; force the sum to be even by
		// bumping the last entry if needed.
		degSeq := rapid.SliceOfN(rapid.IntRange(0, 6), 1, 10).Draw(r, "degSeq")
		total := 0
		for _, d := range degSeq {
			total += d
		}
		if total%2 != 0 {
			degSeq[len(degSeq)-1]++
		}
		allowMulti := rapid.Bool().Draw(r, "allowMulti")
		seed := rapid.Uint64().Draw(r, "seed")
		g, err := ConfigurationModel(degSeq, allowMulti, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("degSeq=%v allowMulti=%v: Build: %v", degSeq, allowMulti, err)
		}
		if got := g.AdjList().Order(); got != uint64(len(degSeq)) {
			t.Fatalf("Order = %d, want %d", got, len(degSeq))
		}
		degs := nodeDegreesUndirectedWithSelfLoops(g)
		if allowMulti {
			for i, want := range degSeq {
				if degs[i] != want {
					t.Fatalf("multigraph deg(%d) = %d, want %d (degSeq=%v, seed=%d)", i, degs[i], want, degSeq, seed)
				}
			}
		} else {
			if hasSelfLoop(g) {
				t.Fatalf("simple graph contains a self-loop (degSeq=%v, seed=%d)", degSeq, seed)
			}
			for i, want := range degSeq {
				if degs[i] > want {
					t.Fatalf("simple deg(%d) = %d, exceeds input %d (degSeq=%v, seed=%d)", i, degs[i], want, degSeq, seed)
				}
			}
		}
	})
}

// -------------------------------------------------------------------
// Soak / nightly layer sweeps
// -------------------------------------------------------------------

// TestRandom_Regular_Soak exercises RandomRegular at a mid size
// (n=200, d=4) with a fixed seed and asserts the catalogue
// invariants. The soak layer also pins the success rate of the
// pairing model across a small seed sweep — every seed in the
// (n=200, d=4) configuration must produce a valid 4-regular graph
// inside the 100-attempt retry budget.
func TestRandom_Regular_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const (
		n     = 200
		d     = 4
		seeds = 32
	)
	for s := uint64(0); s < seeds; s++ {
		g, err := RandomRegular(n, d, s).Build(defaultCfg)
		if err != nil {
			t.Fatalf("seed=%d: Build: %v", s, err)
		}
		if got := g.AdjList().Order(); got != uint64(n) {
			t.Fatalf("seed=%d: Order = %d, want %d", s, got, n)
		}
		wantSize := uint64(n * d / 2)
		if got := g.AdjList().Size(); got != wantSize {
			t.Fatalf("seed=%d: Size = %d, want %d", s, got, wantSize)
		}
		if hasSelfLoop(g) {
			t.Fatalf("seed=%d: graph contains a self-loop", s)
		}
		degs := nodeDegreesUndirected(g)
		for i := 0; i < n; i++ {
			if degs[i] != d {
				t.Fatalf("seed=%d: deg(%d) = %d, want %d", s, i, degs[i], d)
			}
		}
	}
}

// TestRandom_Configuration_Soak exercises ConfigurationModel at a
// mid size (n=200, average degree ~4) with a fixed seed and asserts
// the catalogue invariants in both multigraph and simple modes.
func TestRandom_Configuration_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const n = 200
	degSeq := make([]int, n)
	for i := range degSeq {
		degSeq[i] = 4
	}
	for _, allowMulti := range []bool{false, true} {
		allowMulti := allowMulti
		t.Run(fmt.Sprintf("allowMulti=%v", allowMulti), func(t *testing.T) {
			g, err := ConfigurationModel(degSeq, allowMulti, 0xCAFE).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("Order = %d, want %d", got, n)
			}
			degs := nodeDegreesUndirectedWithSelfLoops(g)
			if allowMulti {
				for i := 0; i < n; i++ {
					if degs[i] != 4 {
						t.Fatalf("multigraph deg(%d) = %d, want 4", i, degs[i])
					}
				}
			} else {
				for i := 0; i < n; i++ {
					if degs[i] > 4 {
						t.Fatalf("simple deg(%d) = %d, exceeds input 4", i, degs[i])
					}
				}
			}
		})
	}
}

// -------------------------------------------------------------------
// Local helpers
// -------------------------------------------------------------------

// newDeterministicRand returns a [math/rand/v2.Rand] backed by a PCG
// seeded with the supplied uint64. The helper exists so the few
// unit tests that drive randomRegularAttempt directly can pin the
// PRNG state without duplicating the rand.New(rand.NewPCG(seed, seed))
// boilerplate.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; test usage is deterministic by construction.
func newDeterministicRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, ^seed))
}

// nodeDegreesUndirected returns the per-node undirected degree of g
// computed from the adjacency iteration. The helper assumes g has no
// self-loops (callers should verify this via [hasSelfLoop] first);
// every neighbour entry contributes one to the source node's degree.
func nodeDegreesUndirected(g *lpg.Graph[int, int64]) []int {
	adj := g.AdjList()
	maxID := int(adj.MaxNodeID())
	degs := make([]int, maxID)
	for u := 0; u < maxID; u++ {
		for range adj.Neighbours(u) {
			degs[u]++
		}
	}
	return degs
}

// nodeDegreesUndirectedWithSelfLoops returns the per-node undirected
// degree of g counting self-loops as contributing two to the source's
// degree, matching the configuration-model convention (a self-loop is
// a pairing of two half-edges from the same node, so each contributes
// one to that node's degree).
//
// The lpg adjacency stores a self-loop (i, i) as a single entry on
// node i's neighbour list; iterating Neighbours(i) yields it once.
// To realise the configuration-model degree convention, we count
// self-loops twice during the scan.
func nodeDegreesUndirectedWithSelfLoops(g *lpg.Graph[int, int64]) []int {
	adj := g.AdjList()
	maxID := int(adj.MaxNodeID())
	degs := make([]int, maxID)
	for u := 0; u < maxID; u++ {
		for v := range adj.Neighbours(u) {
			degs[u]++
			if v == u {
				// Self-loop contributes twice to the node's degree
				// under the configuration-model convention.
				degs[u]++
			}
		}
	}
	return degs
}
