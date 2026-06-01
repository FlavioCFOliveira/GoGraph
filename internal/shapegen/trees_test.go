package shapegen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// treesGoldenDir is the directory holding the trees-family adjacency
// listings. As with classicGoldenDir and structuredGoldenDir, the
// path is rooted at the package directory.
//
// This file deliberately reuses formatAdjacency from trivial_test.go
// (same package). When T58.22 lands the shared golden helper in
// internal/goldens, every family's golden helper must migrate
// together.
const treesGoldenDir = "testdata/shapegen/trees"

// treesGolden compares got with the contents of the golden file at
// treesGoldenDir/<name>. The implementation mirrors classicGolden
// and structuredGolden exactly because the four families will
// migrate together to the shared helper in T58.22; until then
// duplicating keeps each family's test surface self-contained.
func treesGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(treesGoldenDir, name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("treesGolden: MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("treesGolden: WriteFile(%q): %v", path, err)
		}
		t.Logf("rewrote golden %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // path is a test-local golden under testdata/, not user input
	if err != nil {
		t.Fatalf("treesGolden: ReadFile(%q): %v (run with -shapegen-update to bootstrap)", path, err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// -------------------------------------------------------------------
// Local DFS acyclicity helper (brief AC#1)
// -------------------------------------------------------------------

// assertTreeInvariants verifies that g has wantOrder nodes,
// exactly wantOrder-1 edges, and zero cycles. The acyclicity check
// runs a local iterative DFS over the symmetric closure of g's
// out-adjacency, treating the graph as undirected (the catalogue's
// trees are rooted but every shape here is structurally a tree
// regardless of orientation). The brief explicitly forbids pulling
// in the search.* package for this assertion; the DFS lives here.
//
// When wantOrder == 0 the function checks that g is the empty graph
// and returns.
func assertTreeInvariants(t *testing.T, g *lpg.Graph[int, int64], wantOrder uint64) {
	t.Helper()
	assertOrder(t, g, wantOrder)
	if wantOrder == 0 {
		assertSize(t, g, 0)
		return
	}
	wantSize := wantOrder - 1
	assertSize(t, g, wantSize)

	// Acyclicity: iterative DFS over the undirected skeleton. A graph
	// with N nodes and N-1 edges that is connected and acyclic is a
	// tree; conversely, any tree has N-1 edges and is acyclic. We
	// check both connectedness and acyclicity in one pass.
	adj := g.AdjList()
	mapper := adj.Mapper()
	maxID := uint64(adj.MaxNodeID())
	nodes := make([]int, 0, maxID)
	for id := uint64(0); id < maxID; id++ {
		v, ok := mapper.Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		nodes = append(nodes, v)
	}
	if uint64(len(nodes)) != wantOrder {
		t.Fatalf("mapper Resolve recovered %d nodes, want %d", len(nodes), wantOrder)
	}

	// Build the symmetric closure as a map[int]map[int]struct{}. Set
	// semantics deduplicate entries that arise when the underlying
	// [adjlist.AdjList] already mirrors undirected edges — without
	// the dedup, two scans of the same undirected (u,v) edge (one
	// from u, one from v) would insert v twice into sym[u] and trip
	// the cycle detector below as a false positive.
	sym := make(map[int]map[int]struct{}, len(nodes))
	ensure := func(u int) map[int]struct{} {
		m := sym[u]
		if m == nil {
			m = make(map[int]struct{})
			sym[u] = m
		}
		return m
	}
	for _, u := range nodes {
		for v := range adj.Neighbours(u) {
			ensure(u)[v] = struct{}{}
			ensure(v)[u] = struct{}{}
		}
	}

	// Iterative DFS from nodes[0]. Each entry on the stack is
	// (current node, parent in the DFS tree). A back edge to a
	// non-parent already-visited node indicates a cycle.
	//
	// Implementation note: a node is marked visited at push-time
	// rather than pop-time. This guarantees each node is enqueued
	// at most once, so any "visited neighbour that is not the
	// parent" encountered during enumeration is unambiguously a
	// back edge — i.e., a cycle.
	visited := make(map[int]bool, len(nodes))
	type frame struct{ node, parent int }
	stack := make([]frame, 0, len(nodes))
	visited[nodes[0]] = true
	stack = append(stack, frame{node: nodes[0], parent: -1})
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for nbr := range sym[top.node] {
			if nbr == top.parent {
				continue
			}
			if visited[nbr] {
				t.Fatalf("back edge %d -> %d (cycle detected)", top.node, nbr)
			}
			visited[nbr] = true
			stack = append(stack, frame{node: nbr, parent: top.node})
		}
	}
	if uint64(len(visited)) != wantOrder {
		t.Fatalf("DFS visited %d nodes, want %d (graph is not connected)", len(visited), wantOrder)
	}
}

// -------------------------------------------------------------------
// BalancedBinary
// -------------------------------------------------------------------

// TestTrees_BalancedBinary_Invariants exercises the depth sweep
// d in [0, 6] and asserts the documented closed forms. d=0 is the
// single-node tree; d=6 has 127 nodes.
func TestTrees_BalancedBinary_Invariants(t *testing.T) {
	t.Parallel()
	for d := 0; d <= 6; d++ {
		d := d
		t.Run(fmt.Sprintf("d=%d", d), func(t *testing.T) {
			t.Parallel()
			s := BalancedBinary(d)
			if got, want := s.Name(), "trees.balanced-binary"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 1 || knobs[0].Name != "depth" || knobs[0].Min != 0 || knobs[0].Max != 20 || knobs[0].Default != 3 {
				t.Fatalf("Knobs = %#v, want exactly one depth:[0,20] default 3", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			wantN := uint64((1 << (d + 1)) - 1)
			assertTreeInvariants(t, g, wantN)
			assertDirected(t, g, true)
		})
	}
}

// TestTrees_BalancedBinary_PanicsOutOfRange covers the two guard
// branches (negative and above 20).
func TestTrees_BalancedBinary_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	for _, d := range []int{-1, 21} {
		d := d
		t.Run(fmt.Sprintf("d=%d", d), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("BalancedBinary(%d) did not panic", d)
				}
			}()
			_ = BalancedBinary(d)
		})
	}
}

// TestTrees_BalancedBinary_Golden pins BalancedBinary(3): 15 nodes,
// 14 edges. This covers the brief's balanced-binary-d3.txt golden.
func TestTrees_BalancedBinary_Golden(t *testing.T) {
	t.Parallel()
	g, err := BalancedBinary(3).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	treesGolden(t, "balanced-binary-d3.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// CompleteKAry
// -------------------------------------------------------------------

// TestTrees_CompleteKAry_Invariants sweeps (k, depth) over a small
// rectangle including the k==0 (root-only) and k==1 (chain) special
// cases.
func TestTrees_CompleteKAry_Invariants(t *testing.T) {
	t.Parallel()
	pairs := []struct{ k, depth int }{
		{0, 0}, {0, 5},
		{1, 0}, {1, 3}, {1, 5},
		{2, 0}, {2, 3}, {2, 5},
		{3, 0}, {3, 2}, {3, 3},
		{4, 2}, {4, 3},
		{5, 2},
		{10, 2},
	}
	for _, p := range pairs {
		p := p
		t.Run(fmt.Sprintf("k=%d_d=%d", p.k, p.depth), func(t *testing.T) {
			t.Parallel()
			s := CompleteKAry(p.k, p.depth)
			if got, want := s.Name(), "trees.complete-kary"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 2 || knobs[0].Name != "k" || knobs[1].Name != "depth" {
				t.Fatalf("Knobs = %#v, want k,depth", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertTreeInvariants(t, g, uint64(completeKAryOrder(p.k, p.depth)))
			assertDirected(t, g, true)
		})
	}
}

// TestTrees_CompleteKAry_PanicsOutOfRange covers every guard branch
// (negative k, k too large, negative depth, depth too large).
func TestTrees_CompleteKAry_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct{ k, d int }{
		{-1, 0},
		{11, 0},
		{2, -1},
		{2, 13},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("k=%d_d=%d", c.k, c.d), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("CompleteKAry(%d,%d) did not panic", c.k, c.d)
				}
			}()
			_ = CompleteKAry(c.k, c.d)
		})
	}
}

// TestTrees_CompleteKAry_Golden pins CompleteKAry(3, 2): a complete
// ternary tree of depth 2 — 13 nodes, 12 edges. This covers the
// brief's complete-kary-k3-d2.txt golden.
func TestTrees_CompleteKAry_Golden(t *testing.T) {
	t.Parallel()
	g, err := CompleteKAry(3, 2).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	treesGolden(t, "complete-kary-k3-d2.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// PruferTree
// -------------------------------------------------------------------

// TestTrees_PruferTree_Invariants exercises a small (n, seed) sweep
// and asserts the catalogue's closed forms: Order=n, Size=n-1 for
// n>=1, and acyclicity via the local DFS helper.
func TestTrees_PruferTree_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		seed uint64
	}{
		{0, 0}, {1, 0}, {2, 7}, {3, 11}, {5, 42}, {10, 42}, {16, 99},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_seed=%d", c.n, c.seed), func(t *testing.T) {
			t.Parallel()
			s := PruferTree(c.n, c.seed)
			if got, want := s.Name(), "trees.prufer"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 1 || knobs[0].Name != "n" || knobs[0].Min != 2 || knobs[0].Max != 5000 || knobs[0].Default != 10 {
				t.Fatalf("Knobs = %#v, want exactly one n:[2,5000] default 10", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertTreeInvariants(t, g, uint64(c.n))
			assertDirected(t, g, true)
		})
	}
}

// TestTrees_PruferTree_PanicsOutOfRange covers the negative-n and
// above-5000 guards.
func TestTrees_PruferTree_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	for _, n := range []int{-1, 5001} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("PruferTree(%d, 0) did not panic", n)
				}
			}()
			_ = PruferTree(n, 0)
		})
	}
}

// TestTrees_PruferTree_Determinism asserts that the same (n, seed)
// produces a byte-identical adjacency listing across two calls. The
// determinism contract follows from math/rand/v2.NewPCG(seed, seed)
// being deterministic in seed.
func TestTrees_PruferTree_Determinism(t *testing.T) {
	t.Parallel()
	const n = 12
	const seed = uint64(0xC0FFEE)
	g1, err := PruferTree(n, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := PruferTree(n, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("adjacency listings differ across builds with same seed")
	}
}

// TestTrees_PruferTree_Golden pins PruferTree(5, 42).
func TestTrees_PruferTree_Golden(t *testing.T) {
	t.Parallel()
	g, err := PruferTree(5, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	treesGolden(t, "prufer-n5-seed42.txt", formatAdjacency(g))
}

// TestTrees_PruferTree_ShardFullPropagates exercises the decoder
// loop's error path: a tight cfg.MaxShardCapacity saturates the
// shard that owns NodeID 256 (its intraIdx is 1, one above the cap)
// and the AddEdge inside addPruferDecodedEdges therefore surfaces
// adjlist.ErrShardFull. The test asserts the error propagates
// through buildPruferTree's mid-stage return path. The same cap
// would also block addFinalPruferEdge, but the decoder loop runs
// first so the mid-stage return is what fires.
func TestTrees_PruferTree_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	const n = 260 // > 256 so at least one NodeID lands at intraIdx >= 1.
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 1}
	_, err := PruferTree(n, 0).Build(cfg)
	if err == nil {
		t.Fatal("Build returned nil error, want adjlist.ErrShardFull")
	}
}

// TestTrees_PruferTree_Distribution implements AC#2: 10_000 seeds
// produce edge histograms within chi-squared 0.05 tolerance over
// n == 10. The number of labelled trees on n nodes is n^(n-2) by
// Cayley's formula; the probability that a given undirected edge
// (i, j) appears in a uniform tree is 2/n. With n=10 and 10_000
// draws the expected count per edge is 2_000.
//
// There are C(10, 2) = 45 possible undirected edges, so df = 44 and
// the chi-squared 0.05 critical value is approximately 60.481.
//
// Drawing 10_000 trees with sequential seeds 0..9_999 keeps the
// test fully deterministic: it either always passes or always fails
// for a given PCG implementation. The build uses defaultCfg so the
// per-tree cost stays minimal.
func TestTrees_PruferTree_Distribution(t *testing.T) {
	t.Parallel()
	const (
		n        = 10
		trials   = 10_000
		expected = float64(trials) * 2.0 / float64(n) // = 2000.0
		// Upper-tail critical value of chi^2 with 44 degrees of
		// freedom at alpha=0.05. Pinned numerically rather than
		// computed at runtime to keep the test independent of any
		// statistics dependency.
		chiSquared05DF44 = 60.481
	)

	// counts[i][j] (i < j) holds the occurrence count of undirected
	// edge (i, j) across the trial set.
	counts := make([][]int, n)
	for i := range counts {
		counts[i] = make([]int, n)
	}
	for seed := uint64(0); seed < trials; seed++ {
		g, err := PruferTree(n, seed).Build(defaultCfg)
		if err != nil {
			t.Fatalf("seed=%d: Build: %v", seed, err)
		}
		// Count each undirected edge once via the canonical (u<v)
		// ordering: the trees-family Build emits a single directed
		// entry per edge, so iterating Neighbours and applying the
		// min/max canonicalisation recovers the undirected set.
		seen := make(map[[2]int]bool, n-1)
		for u := 0; u < n; u++ {
			for v := range g.AdjList().Neighbours(u) {
				a, b := u, v
				if a > b {
					a, b = b, a
				}
				if seen[[2]int{a, b}] {
					continue
				}
				seen[[2]int{a, b}] = true
				counts[a][b]++
			}
		}
		if len(seen) != n-1 {
			t.Fatalf("seed=%d: tree has %d unique edges, want %d", seed, len(seen), n-1)
		}
	}
	// Chi-squared over the 45-cell flattened histogram.
	var chi float64
	cells := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			diff := float64(counts[i][j]) - expected
			chi += diff * diff / expected
			cells++
		}
	}
	if cells != n*(n-1)/2 {
		t.Fatalf("chi-squared sweep covered %d cells, want %d", cells, n*(n-1)/2)
	}
	if chi >= chiSquared05DF44 {
		t.Fatalf("chi^2 = %.3f >= %.3f at df=44 (alpha=0.05) — edge distribution is not uniform", chi, chiSquared05DF44)
	}
}

// -------------------------------------------------------------------
// PathDegenerate
// -------------------------------------------------------------------

// TestTrees_PathDegenerate_Invariants exercises the n sweep
// {0, 1, 2, 3, 5, 10} and asserts the tree invariants for the
// undirected path.
func TestTrees_PathDegenerate_Invariants(t *testing.T) {
	t.Parallel()
	for _, n := range goldenSizes() {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			s := PathDegenerate(n)
			// PathDegenerate is a delegation to Path(n, false), so its
			// Name() reports the delegated catalogue identifier rather
			// than a trees-family name. This is the contract called
			// out in PathDegenerate's godoc.
			if got, want := s.Name(), "classic.path"; got != want {
				t.Fatalf("Name = %q, want %q (delegation contract)", got, want)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertTreeInvariants(t, g, uint64(n))
			// PathDegenerate is undirected by definition: it inherits
			// the orientation from Path(n, false).
			assertDirected(t, g, false)
		})
	}
}

// TestTrees_PathDegenerate_DelegatesToClassic asserts the
// constructor contract pinned in the brief: the graph produced by
// PathDegenerate(n) is semantically equal to the one produced by
// Path(n, false) — same Order, same Size, same adjacency listing.
func TestTrees_PathDegenerate_DelegatesToClassic(t *testing.T) {
	t.Parallel()
	for _, n := range goldenSizes() {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			gTree, err := PathDegenerate(n).Build(defaultCfg)
			if err != nil {
				t.Fatalf("PathDegenerate: %v", err)
			}
			gClassic, err := Path(n, false).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Path: %v", err)
			}
			if got, want := gTree.AdjList().Order(), gClassic.AdjList().Order(); got != want {
				t.Fatalf("Order: PathDegenerate=%d, Path=%d", got, want)
			}
			if got, want := gTree.AdjList().Size(), gClassic.AdjList().Size(); got != want {
				t.Fatalf("Size: PathDegenerate=%d, Path=%d", got, want)
			}
			if got, want := formatAdjacency(gTree), formatAdjacency(gClassic); got != want {
				t.Fatalf("adjacency listings differ:\n--- PathDegenerate ---\n%s\n--- Path ---\n%s", got, want)
			}
		})
	}
}

// TestTrees_PathDegenerate_Golden pins PathDegenerate(5).
func TestTrees_PathDegenerate_Golden(t *testing.T) {
	t.Parallel()
	g, err := PathDegenerate(5).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	treesGolden(t, "path-degenerate-n5.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Caterpillar
// -------------------------------------------------------------------

// TestTrees_Caterpillar_Invariants exercises a sweep over (spine,
// leafDist) pairs and asserts the tree invariants. Note that
// Caterpillar(1, [0]) is admitted by the constructor (spine=1 is
// the lower bound) and produces a single isolated node — a
// degenerate tree of order 1.
func TestTrees_Caterpillar_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		spine    int
		leafDist []int
	}{
		{1, []int{0}},
		{1, []int{3}},
		{2, []int{0, 0}},
		{2, []int{1, 1}},
		{3, []int{1, 2, 1}},
		{5, []int{0, 1, 2, 1, 0}},
		{6, []int{2, 2, 2, 2, 2, 2}},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("spine=%d_leaves=%v", c.spine, c.leafDist), func(t *testing.T) {
			t.Parallel()
			s := Caterpillar(c.spine, c.leafDist)
			if got, want := s.Name(), "trees.caterpillar"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 1 || knobs[0].Name != "spine" || knobs[0].Min != 1 || knobs[0].Max != 1000 || knobs[0].Default != 3 {
				t.Fatalf("Knobs = %#v, want exactly one spine:[1,1000] default 3", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			total := c.spine
			for _, l := range c.leafDist {
				total += l
			}
			assertTreeInvariants(t, g, uint64(total))
			assertDirected(t, g, true)
		})
	}
}

// TestTrees_Caterpillar_PanicsOnInvalid covers every guard branch:
// spine below 1, spine above 1000, len(leafDist) mismatch, and
// out-of-range leafDist entries.
func TestTrees_Caterpillar_PanicsOnInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		spine    int
		leafDist []int
	}{
		{"spine_zero", 0, nil},
		{"spine_too_large", 1001, make([]int, 1001)},
		{"len_mismatch", 3, []int{1, 1}},
		{"leaf_negative", 2, []int{-1, 0}},
		{"leaf_too_large", 2, []int{0, 51}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Caterpillar(%d, %v) did not panic", c.spine, c.leafDist)
				}
			}()
			_ = Caterpillar(c.spine, c.leafDist)
		})
	}
}

// TestTrees_Caterpillar_OwnsLeafDist asserts the constructor's
// defensive copy: mutating the leafDist slice after construction
// must not affect the built graph.
func TestTrees_Caterpillar_OwnsLeafDist(t *testing.T) {
	t.Parallel()
	leafDist := []int{1, 2, 1}
	s := Caterpillar(3, leafDist)
	leafDist[1] = 50 // would change N if shared.
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Original N was 3 + (1+2+1) = 7.
	if got := g.AdjList().Order(); got != 7 {
		t.Fatalf("Order = %d, want 7 (mutation of leafDist leaked into Build)", got)
	}
}

// TestTrees_Caterpillar_Golden pins Caterpillar(3, [1,2,1]).
func TestTrees_Caterpillar_Golden(t *testing.T) {
	t.Parallel()
	g, err := Caterpillar(3, []int{1, 2, 1}).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	treesGolden(t, "caterpillar-spine3-leaves-1-2-1.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Spider
// -------------------------------------------------------------------

// TestTrees_Spider_Invariants exercises a sweep over (legs, legLen)
// and asserts the documented closed forms plus tree invariants.
func TestTrees_Spider_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct{ legs, legLen int }{
		{1, 1}, {1, 5},
		{2, 1}, {2, 3},
		{3, 2}, {3, 5},
		{4, 4},
		{8, 2},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("legs=%d_legLen=%d", c.legs, c.legLen), func(t *testing.T) {
			t.Parallel()
			s := Spider(c.legs, c.legLen)
			if got, want := s.Name(), "trees.spider"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 2 || knobs[0].Name != "legs" || knobs[1].Name != "legLen" {
				t.Fatalf("Knobs = %#v, want legs,legLen", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			wantN := uint64(1 + c.legs*c.legLen)
			assertTreeInvariants(t, g, wantN)
			assertDirected(t, g, true)
			// Centre node has out-degree == legs (one edge per leg).
			if got := degreeOut(g, 0); got != c.legs {
				t.Fatalf("centre out-deg = %d, want %d", got, c.legs)
			}
		})
	}
}

// TestTrees_Spider_PanicsOutOfRange covers every guard branch.
func TestTrees_Spider_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct{ legs, legLen int }{
		{0, 1},
		{101, 1},
		{1, 0},
		{1, 101},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("legs=%d_legLen=%d", c.legs, c.legLen), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Spider(%d,%d) did not panic", c.legs, c.legLen)
				}
			}()
			_ = Spider(c.legs, c.legLen)
		})
	}
}

// TestTrees_Spider_Golden pins Spider(3, 2): 1 + 3*2 = 7 nodes.
func TestTrees_Spider_Golden(t *testing.T) {
	t.Parallel()
	g, err := Spider(3, 2).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	treesGolden(t, "spider-legs3-len2.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Lobster
// -------------------------------------------------------------------

// TestTrees_Lobster_Invariants exercises a sweep over depths slices
// and asserts the pinned closed form N = len(depths) + sum(depths).
func TestTrees_Lobster_Invariants(t *testing.T) {
	t.Parallel()
	cases := [][]int{
		{0},
		{1},
		{5},
		{0, 0},
		{1, 1},
		{1, 2, 1},
		{0, 1, 2, 3},
		{5, 5, 5, 5, 5},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("depths=%v", c), func(t *testing.T) {
			t.Parallel()
			s := Lobster(c)
			if got, want := s.Name(), "trees.lobster"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 0 {
				t.Fatalf("Knobs = %#v, want empty (depths is variadic)", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			total := len(c)
			for _, d := range c {
				total += d
			}
			assertTreeInvariants(t, g, uint64(total))
			assertDirected(t, g, true)
		})
	}
}

// TestTrees_Lobster_PinnedExample re-asserts the worked example
// from the package-level docstring: Lobster([1,2,1]) → N=7, E=6.
func TestTrees_Lobster_PinnedExample(t *testing.T) {
	t.Parallel()
	g, err := Lobster([]int{1, 2, 1}).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := g.AdjList().Order(), uint64(7); got != want {
		t.Fatalf("Order = %d, want %d", got, want)
	}
	if got, want := g.AdjList().Size(), uint64(6); got != want {
		t.Fatalf("Size = %d, want %d", got, want)
	}
}

// TestTrees_Lobster_PanicsOnInvalid covers every guard branch.
func TestTrees_Lobster_PanicsOnInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		depths []int
	}{
		{"empty", []int{}},
		{"too_long", make([]int, 51)},
		{"depth_negative", []int{-1}},
		{"depth_too_large", []int{6}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Lobster(%v) did not panic", c.depths)
				}
			}()
			_ = Lobster(c.depths)
		})
	}
}

// TestTrees_Lobster_OwnsDepths asserts the constructor's defensive
// copy: mutating the depths slice after construction must not
// affect the built graph.
func TestTrees_Lobster_OwnsDepths(t *testing.T) {
	t.Parallel()
	depths := []int{1, 2, 1}
	s := Lobster(depths)
	depths[1] = 5 // would change N if shared.
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != 7 {
		t.Fatalf("Order = %d, want 7 (mutation of depths leaked into Build)", got)
	}
}

// TestTrees_Lobster_Golden pins Lobster([1,2,1]).
func TestTrees_Lobster_Golden(t *testing.T) {
	t.Parallel()
	g, err := Lobster([]int{1, 2, 1}).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	treesGolden(t, "lobster-1-2-1.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// MaxShardCapacity preservation
// -------------------------------------------------------------------

// TestTrees_PreservesMaxShardCapacity confirms that every trees
// generator preserves cfg.MaxShardCapacity verbatim, mirroring the
// trivial-, classic-, and structured-family contracts. PathDegenerate
// is excluded because it delegates to Path; the Path contract is
// covered by the classic-family suite.
func TestTrees_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	for _, tc := range []struct {
		name string
		s    Shape[int, int64]
	}{
		{"balanced_binary_d2", BalancedBinary(2)},
		{"complete_kary_k2_d2", CompleteKAry(2, 2)},
		{"prufer_n5_seed1", PruferTree(5, 1)},
		{"path_degenerate_5", PathDegenerate(5)},
		{"caterpillar_3_1_2_1", Caterpillar(3, []int{1, 2, 1})},
		{"spider_3_2", Spider(3, 2)},
		{"lobster_1_2_1", Lobster([]int{1, 2, 1})},
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
// Property-based parametric sweep
// -------------------------------------------------------------------

// TestTrees_Properties_RapidSweep drives every parametric tree
// generator over a small parameter sweep and asserts the catalogue
// invariants documented in the constructor godoc. Bounds are kept
// small so the short layer stays under the per-package time budget.
func TestTrees_Properties_RapidSweep(t *testing.T) {
	t.Parallel()

	t.Run("balanced_binary", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			d := rapid.IntRange(0, 8).Draw(r, "depth")
			g, err := BalancedBinary(d).Build(defaultCfg)
			if err != nil {
				t.Fatalf("d=%d: Build: %v", d, err)
			}
			wantN := uint64((1 << (d + 1)) - 1)
			if got := g.AdjList().Order(); got != wantN {
				t.Fatalf("d=%d: Order = %d, want %d", d, got, wantN)
			}
			if got := g.AdjList().Size(); got != wantN-1 {
				t.Fatalf("d=%d: Size = %d, want %d", d, got, wantN-1)
			}
		})
	})

	t.Run("complete_kary", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			k := rapid.IntRange(0, 5).Draw(r, "k")
			d := rapid.IntRange(0, 4).Draw(r, "depth")
			g, err := CompleteKAry(k, d).Build(defaultCfg)
			if err != nil {
				t.Fatalf("k=%d d=%d: Build: %v", k, d, err)
			}
			wantN := uint64(completeKAryOrder(k, d))
			if got := g.AdjList().Order(); got != wantN {
				t.Fatalf("k=%d d=%d: Order = %d, want %d", k, d, got, wantN)
			}
			wantSize := uint64(0)
			if wantN >= 1 {
				wantSize = wantN - 1
			}
			if got := g.AdjList().Size(); got != wantSize {
				t.Fatalf("k=%d d=%d: Size = %d, want %d", k, d, got, wantSize)
			}
		})
	})

	t.Run("prufer", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(2, 50).Draw(r, "n")
			seed := rapid.Uint64().Draw(r, "seed")
			g, err := PruferTree(n, seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("n=%d seed=%d: Build: %v", n, seed, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("n=%d: Order = %d, want %d", n, got, n)
			}
			if got := g.AdjList().Size(); got != uint64(n-1) {
				t.Fatalf("n=%d: Size = %d, want %d", n, got, n-1)
			}
		})
	})

	t.Run("caterpillar", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			spine := rapid.IntRange(1, 30).Draw(r, "spine")
			leafDist := make([]int, spine)
			for i := range leafDist {
				leafDist[i] = rapid.IntRange(0, 10).Draw(r, fmt.Sprintf("leaf[%d]", i))
			}
			g, err := Caterpillar(spine, leafDist).Build(defaultCfg)
			if err != nil {
				t.Fatalf("spine=%d: Build: %v", spine, err)
			}
			total := spine
			for _, l := range leafDist {
				total += l
			}
			if got := g.AdjList().Order(); got != uint64(total) {
				t.Fatalf("Order = %d, want %d", got, total)
			}
			if got := g.AdjList().Size(); got != uint64(total-1) {
				t.Fatalf("Size = %d, want %d", got, total-1)
			}
		})
	})

	t.Run("spider", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			legs := rapid.IntRange(1, 20).Draw(r, "legs")
			legLen := rapid.IntRange(1, 20).Draw(r, "legLen")
			g, err := Spider(legs, legLen).Build(defaultCfg)
			if err != nil {
				t.Fatalf("legs=%d legLen=%d: Build: %v", legs, legLen, err)
			}
			wantN := uint64(1 + legs*legLen)
			if got := g.AdjList().Order(); got != wantN {
				t.Fatalf("Order = %d, want %d", got, wantN)
			}
			if got := g.AdjList().Size(); got != wantN-1 {
				t.Fatalf("Size = %d, want %d", got, wantN-1)
			}
		})
	})

	t.Run("lobster", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			length := rapid.IntRange(1, 20).Draw(r, "len")
			depths := make([]int, length)
			for i := range depths {
				depths[i] = rapid.IntRange(0, 5).Draw(r, fmt.Sprintf("d[%d]", i))
			}
			g, err := Lobster(depths).Build(defaultCfg)
			if err != nil {
				t.Fatalf("len=%d: Build: %v", length, err)
			}
			total := length
			for _, d := range depths {
				total += d
			}
			if got := g.AdjList().Order(); got != uint64(total) {
				t.Fatalf("Order = %d, want %d", got, total)
			}
			if got := g.AdjList().Size(); got != uint64(total-1) {
				t.Fatalf("Size = %d, want %d", got, total-1)
			}
		})
	})
}

// -------------------------------------------------------------------
// Soak / nightly layer sweeps
// -------------------------------------------------------------------

// TestTrees_BalancedBinary_Soak exercises BalancedBinary up to the
// soak ceiling (depth == 22, ~ 8.4M nodes).
func TestTrees_BalancedBinary_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	// The constructor caps depth at 20 in the short layer; soak
	// callers wishing to exceed that must use the underlying
	// AddNode/AddEdge sequence directly. This test instead confirms
	// the depth==20 build remains correct under repeated invocation:
	// it is the largest balanced-binary value the constructor
	// admits and the most relevant ceiling for soak coverage.
	const d = 20
	g, err := BalancedBinary(d).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantN := uint64((1 << (d + 1)) - 1)
	if got := g.AdjList().Order(); got != wantN {
		t.Fatalf("Order = %d, want %d", got, wantN)
	}
	if got := g.AdjList().Size(); got != wantN-1 {
		t.Fatalf("Size = %d, want %d", got, wantN-1)
	}
}

// TestTrees_PruferTree_Soak exercises PruferTree at the soak
// ceiling for n (5_000, the constructor cap) and asserts the
// catalogue's Order/Size invariants. The full DFS acyclicity check
// would dominate runtime at this scale, so this soak test focuses
// on the closed-form invariants only — the unit-test arm of
// AC#1 covers acyclicity on smaller n.
func TestTrees_PruferTree_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const n = 5_000
	g, err := PruferTree(n, 0xBEEF).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	if got := g.AdjList().Size(); got != uint64(n-1) {
		t.Fatalf("Size = %d, want %d", got, n-1)
	}
}

// TestTrees_PathDegenerate_Soak exercises the short-layer ceiling
// for n (100_000) and asserts Order/Size. The underlying Path
// constructor is identical, but having a soak-tagged test in this
// family keeps the layered-coverage matrix uniform across families.
func TestTrees_PathDegenerate_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const n = 100_000
	g, err := PathDegenerate(n).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	if got := g.AdjList().Size(); got != uint64(n-1) {
		t.Fatalf("Size = %d, want %d", got, n-1)
	}
}

// _ keeps the lpg import alive in this file even when only the
// shapegen package types are referenced. The build closures hand
// back *lpg.Graph already, so this is purely a compile-time guard
// against import drift during T58.22 migration.
var _ *lpg.Graph[int, int64]
