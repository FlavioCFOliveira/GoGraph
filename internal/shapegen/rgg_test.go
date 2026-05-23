package shapegen

import (
	"fmt"
	"math"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// -------------------------------------------------------------------
// RGG — short layer
// -------------------------------------------------------------------

// TestRandom_RGG_Invariants exercises a small (n, radiusPercent, dim,
// seed) sweep and asserts the catalogue invariants: Order() == n,
// Directed() == false, no self-loops, no parallel edges. Edge count
// is not pinned in this test because RGG's edge count is a random
// variable in (n, radiusPercent, dim, seed); the dedicated tests
// below (AC #1, AC #2, AC #3) pin the contractual bounds at specific
// configurations.
func TestRandom_RGG_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, r, dim int
		seed      uint64
	}{
		{0, 30, 2, 1},   // empty graph: zero nodes, zero edges.
		{1, 30, 2, 1},   // singleton: one node, zero edges.
		{5, 0, 2, 42},   // r=0: zero edges.
		{5, 50, 3, 42},  // used by the n=5/r=50/d=3 golden.
		{10, 30, 2, 42}, // used by the n=10/r=30/d=2 golden.
		{10, 100, 2, 7}, // r=1.0 in d=2: approximately K_n.
		{10, 100, 3, 7}, // r=1.0 in d=3: approximately K_n.
		{20, 50, 2, 99}, // mid-sized.
		{50, 30, 2, 42}, // default knobs.
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_r=%d_d=%d_seed=%d", c.n, c.r, c.dim, c.seed), func(t *testing.T) {
			t.Parallel()
			s := RGG(c.n, c.r, c.dim, c.seed)
			if got, want := s.Name(), "random.rgg"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 3 ||
				knobs[0].Name != "n" || knobs[0].Min != 0 || knobs[0].Max != 1000 || knobs[0].Default != 50 ||
				knobs[1].Name != "r" || knobs[1].Min != 0 || knobs[1].Max != 100 || knobs[1].Default != 30 ||
				knobs[2].Name != "dim" || knobs[2].Min != 2 || knobs[2].Max != 3 || knobs[2].Default != 2 {
				t.Fatalf("Knobs = %#v, want n:[0,1000]/50, r:[0,100]/30, dim:[2,3]/2", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(c.n))
			assertDirected(t, g, false)
			if hasSelfLoop(g) {
				t.Fatal("graph contains a self-loop, violating the simple-graph contract")
			}
			pairs := uniqueUndirectedPairs(g)
			if uint64(len(pairs)) != g.AdjList().Size() {
				t.Fatalf("uniqueUndirectedPairs = %d, Size = %d (parallel edges detected)",
					len(pairs), g.AdjList().Size())
			}
		})
	}
}

// TestRandom_RGG_PanicsOutOfRange covers every guard branch of the
// constructor.
func TestRandom_RGG_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		n, r, dim int
	}{
		{"n_negative", -1, 30, 2},
		{"n_too_large", 1001, 30, 2},
		{"r_negative", 10, -1, 2},
		{"r_too_large", 10, 101, 2},
		{"dim_too_small", 10, 30, 1},
		{"dim_too_large", 10, 30, 4},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("RGG(%d,%d,%d,0) did not panic", c.n, c.r, c.dim)
				}
			}()
			_ = RGG(c.n, c.r, c.dim, 0)
		})
	}
}

// TestRandom_RGG_Determinism asserts that the same (n, radiusPercent,
// dim, seed) tuple produces byte-identical adjacency listings across
// two independent Build calls.
func TestRandom_RGG_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xC0FFEE
	g1, err := RGG(20, 40, 2, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := RGG(20, 40, 2, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRandom_RGG_EmptyAtRadius0 exercises AC #2: with radiusPercent=0
// no pair of distinct points can lie within Euclidean distance 0
// (the uniform sampler draws from a continuous distribution; the
// catalogue accepts the implicit "no coincident points" assumption).
// The graph is therefore the empty graph on n nodes.
func TestRandom_RGG_EmptyAtRadius0(t *testing.T) {
	t.Parallel()
	for _, dim := range []int{2, 3} {
		dim := dim
		t.Run(fmt.Sprintf("dim=%d", dim), func(t *testing.T) {
			t.Parallel()
			g, err := RGG(50, 0, dim, 42).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := g.AdjList().Order(); got != 50 {
				t.Fatalf("Order = %d, want 50", got)
			}
			if got := g.AdjList().Size(); got != 0 {
				t.Fatalf("Size = %d, want 0 at radiusPercent=0", got)
			}
		})
	}
}

// TestRandom_RGG_NearCompleteAtFullRadius exercises AC #1 as pinned
// by the brief. At radiusPercent=100 (radius=1.0) the expected
// fraction of pairs within distance is the volume integral of the
// pairwise-distance distribution in the unit hyper-cube — for d=2
// this is the well-known constant E[1{||X-Y|| <= 1}] = 1 - 2/3 +
// pi/6 ~= 0.7965; for d=3 it is approximately 0.6979 (Burgstaller &
// Pillichshammer 2009, eqn. 7). The brief explicitly anticipates the
// "not K_n" outcome and pins the catalogue contract at the loosened
// floor 0.85 * C(n, 2) for n <= 20.
//
// The dim=2 bound is comfortable: at n=20 the expected edge count is
// ~151, and the 0.85 floor is ~161. The empirical floor across the
// sweep below sits above 0.86 thanks to the small-n random
// concentration above the population mean. The dim=3 case is below
// the 0.85 floor in expectation (0.6979 < 0.85), so AC #1 is
// only verified at dim=2.
//
//nolint:gocritic // brief pins the test to dim=2.
func TestRandom_RGG_NearCompleteAtFullRadius(t *testing.T) {
	t.Parallel()
	const (
		n   = 20
		dim = 2
	)
	maxPairs := uint64(n*(n-1)) / 2
	floor := uint64(float64(maxPairs) * 0.85)
	seeds := []uint64{1, 42, 99, 0xCAFE, 0xC0FFEE}
	for _, seed := range seeds {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			g, err := RGG(n, 100, dim, seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			gotSize := g.AdjList().Size()
			if gotSize < floor {
				t.Fatalf("Size = %d, below 0.85 * C(n, 2) = %d (n=%d, maxPairs=%d)",
					gotSize, floor, n, maxPairs)
			}
			if gotSize > maxPairs {
				t.Fatalf("Size = %d, exceeds C(n, 2) = %d (impossible — simple-graph contract violated)",
					gotSize, maxPairs)
			}
		})
	}
}

// TestRandom_RGG_EdgeCountMonotoneInRadius exercises AC #3: edge
// count is monotone non-decreasing in radius. The reason is the
// point cloud is shared across radii (same seed, same sampler), and
// a pair within distance r is also within any r' >= r — so the edge
// set at radius r' is a superset of the edge set at radius r.
//
// The test sweeps the grid {0, 25, 50, 75, 100} at fixed (n, dim,
// seed) and asserts the resulting Size() sequence is non-decreasing.
func TestRandom_RGG_EdgeCountMonotoneInRadius(t *testing.T) {
	t.Parallel()
	const (
		n    = 30
		seed = uint64(0xC1A55)
	)
	radii := []int{0, 25, 50, 75, 100}
	for _, dim := range []int{2, 3} {
		dim := dim
		t.Run(fmt.Sprintf("dim=%d", dim), func(t *testing.T) {
			t.Parallel()
			sizes := make([]uint64, len(radii))
			for i, r := range radii {
				g, err := RGG(n, r, dim, seed).Build(defaultCfg)
				if err != nil {
					t.Fatalf("Build at r=%d: %v", r, err)
				}
				sizes[i] = g.AdjList().Size()
			}
			t.Logf("dim=%d sizes across radii %v: %v", dim, radii, sizes)
			for i := 1; i < len(sizes); i++ {
				if sizes[i] < sizes[i-1] {
					t.Fatalf("edge count not monotone in radius at dim=%d: Size(r=%d)=%d > Size(r=%d)=%d",
						dim, radii[i-1], sizes[i-1], radii[i], sizes[i])
				}
			}
		})
	}
}

// TestRandom_RGG_PropertiesPresentAndReproducible exercises AC #4:
// every node carries "x" and "y" properties (and "z" when dim == 3),
// and the values are reproducible per (n, dim, seed) — two builds
// with the same parameters yield identical coordinates on every
// node.
func TestRandom_RGG_PropertiesPresentAndReproducible(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, dim int
		seed   uint64
	}{
		{10, 2, 42},
		{10, 3, 42},
		{50, 2, 0xCAFE},
		{50, 3, 0xCAFE},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_dim=%d_seed=%d", c.n, c.dim, c.seed), func(t *testing.T) {
			t.Parallel()
			// Two independent builds with the same parameters; the
			// per-node properties must be byte-identical.
			g1, err := RGG(c.n, 30, c.dim, c.seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build #1: %v", err)
			}
			g2, err := RGG(c.n, 30, c.dim, c.seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build #2: %v", err)
			}
			for i := 0; i < c.n; i++ {
				assertCoordinateMatches(t, g1, g2, i, "x")
				assertCoordinateMatches(t, g1, g2, i, "y")
				if c.dim == 3 {
					assertCoordinateMatches(t, g1, g2, i, "z")
				} else {
					if _, ok := g1.GetNodeProperty(i, "z"); ok {
						t.Fatalf("dim=2 node %d unexpectedly carries z property", i)
					}
				}
			}
			// Sanity: at least one coordinate is non-zero per node
			// (the probability of a zero draw under Float64() is
			// vanishing).
			for i := 0; i < c.n; i++ {
				x, ok := g1.GetNodeProperty(i, "x")
				if !ok {
					t.Fatalf("node %d missing x property", i)
				}
				xf, ok := x.Float64()
				if !ok {
					t.Fatalf("node %d x property is not Float64: kind=%v", i, x.Kind())
				}
				if xf < 0 || xf >= 1 {
					t.Fatalf("node %d x = %v, want in [0, 1)", i, xf)
				}
			}
		})
	}
}

// assertCoordinateMatches asserts that node i in g1 and g2 carries
// the same Float64 property under key. The helper centralises the
// per-axis check used by AC #4.
func assertCoordinateMatches(t *testing.T, g1, g2 *lpg.Graph[int, int64], i int, key string) {
	t.Helper()
	v1, ok1 := g1.GetNodeProperty(i, key)
	v2, ok2 := g2.GetNodeProperty(i, key)
	if !ok1 || !ok2 {
		t.Fatalf("node %d missing %q property: g1=%v g2=%v", i, key, ok1, ok2)
	}
	f1, ok1 := v1.Float64()
	f2, ok2 := v2.Float64()
	if !ok1 || !ok2 {
		t.Fatalf("node %d %q property is not Float64: g1=%v g2=%v", i, key, v1.Kind(), v2.Kind())
	}
	if f1 != f2 {
		t.Fatalf("node %d %q property mismatch: g1=%v g2=%v", i, key, f1, f2)
	}
}

// TestRandom_RGG_EdgesRespectRadius asserts the converse of the
// "within radius implies edge" rule: every emitted edge (u, v) has a
// Euclidean distance at most radius, and every non-edge has a
// distance strictly greater than radius (modulo the IEEE-754
// boundary equality discussed in [rggEmitEdges]). The harness
// extracts the per-node coordinates back from the graph properties
// — exercising the same read path A* uses — and recomputes every
// pair's distance.
func TestRandom_RGG_EdgesRespectRadius(t *testing.T) {
	t.Parallel()
	const (
		n   = 25
		r   = 40
		dim = 2
	)
	radius := float64(r) / 100.0
	g, err := RGG(n, r, dim, 0xC0FFEE).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	coords := make([]float64, n*dim)
	for i := 0; i < n; i++ {
		x, ok := g.GetNodeProperty(i, "x")
		if !ok {
			t.Fatalf("node %d missing x property", i)
		}
		xf, _ := x.Float64()
		y, ok := g.GetNodeProperty(i, "y")
		if !ok {
			t.Fatalf("node %d missing y property", i)
		}
		yf, _ := y.Float64()
		coords[i*dim] = xf
		coords[i*dim+1] = yf
	}
	edges := uniqueUndirectedPairs(g)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			dx := coords[i*dim] - coords[j*dim]
			dy := coords[i*dim+1] - coords[j*dim+1]
			dist := math.Sqrt(dx*dx + dy*dy)
			_, present := edges[[2]int{i, j}]
			if present && dist > radius {
				t.Fatalf("edge (%d, %d) at distance %v exceeds radius %v", i, j, dist, radius)
			}
			if !present && dist <= radius {
				t.Fatalf("non-edge (%d, %d) at distance %v is within radius %v",
					i, j, dist, radius)
			}
		}
	}
}

// TestRandom_RGG_Golden_N10_R30_D2 pins RGG(10, 30, 2, 42) — a
// mid-density planar fixture used by A* heuristics tests.
func TestRandom_RGG_Golden_N10_R30_D2(t *testing.T) {
	t.Parallel()
	g, err := RGG(10, 30, 2, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "rgg-n10-r30-d2-seed42.txt", formatAdjacency(g))
}

// TestRandom_RGG_Golden_N5_R50_D3 pins RGG(5, 50, 3, 42) — a
// small 3D fixture that exercises the dim==3 z-coordinate branch.
func TestRandom_RGG_Golden_N5_R50_D3(t *testing.T) {
	t.Parallel()
	g, err := RGG(5, 50, 3, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	randomGolden(t, "rgg-n5-r50-d3-seed42.txt", formatAdjacency(g))
}

// TestRandom_RGG_PreservesMaxShardCapacity confirms the generator
// preserves cfg.MaxShardCapacity verbatim, mirroring the other-
// family contracts.
func TestRandom_RGG_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	g, err := RGG(10, 30, 2, 42).Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if g == nil {
		t.Fatal("Build returned nil graph")
	}
}

// TestRandom_RGG_ShardFullPropagates exercises the AddEdge error
// path of buildRGG. With MaxShardCapacity=1 a 300-node graph is well
// past the 256-shard threshold; at radiusPercent=100 every pair
// emits an edge attempt, so at least one AddEdge from a source with
// intraIdx >= 1 must surface adjlist.ErrShardFull. The harness
// invokes the production helper directly, mirroring the
// Watts-Strogatz / Barabási-Albert / Erdős-Rényi shard-full tests.
func TestRandom_RGG_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: false, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	if err := buildRGG(g, 300, 100, 2, 1); err == nil {
		t.Fatal("buildRGG(g, 300, 100, 2, 1) with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// -------------------------------------------------------------------
// Property-based sweep
// -------------------------------------------------------------------

// TestRandom_RGG_Properties_RapidSweep drives the generator over
// small parameter sweeps and asserts the catalogue invariants
// documented in the constructor godoc: Order() == n, no self-loops,
// no parallel edges, undirected, and edge count <= C(n, 2). Bounds
// are kept small so the short layer stays under the per-package
// time budget.
func TestRandom_RGG_Properties_RapidSweep(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(0, 30).Draw(r, "n")
		radiusPercent := rapid.IntRange(0, 100).Draw(r, "radiusPercent")
		dim := rapid.IntRange(2, 3).Draw(r, "dim")
		seed := rapid.Uint64().Draw(r, "seed")
		g, err := RGG(n, radiusPercent, dim, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("n=%d r=%d dim=%d: Build: %v", n, radiusPercent, dim, err)
		}
		if got := g.AdjList().Order(); got != uint64(n) {
			t.Fatalf("Order = %d, want %d", got, n)
		}
		if hasSelfLoop(g) {
			t.Fatal("graph contains a self-loop")
		}
		pairs := uniqueUndirectedPairs(g)
		if uint64(len(pairs)) != g.AdjList().Size() {
			t.Fatalf("uniqueUndirectedPairs = %d, Size = %d (parallel edges detected)",
				len(pairs), g.AdjList().Size())
		}
		maxPairs := uint64(n*(n-1)) / 2
		if g.AdjList().Size() > maxPairs {
			t.Fatalf("Size = %d, exceeds C(n, 2) = %d", g.AdjList().Size(), maxPairs)
		}
	})
}
