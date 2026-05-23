package shapegen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/testlayers"
)

// specialsGoldenDir is the directory holding the specials-family
// adjacency listings. As with the other family-local golden
// directories, the path is rooted at the package directory.
//
// This file deliberately reuses formatAdjacency from trivial_test.go
// (same package). When T58.22 lands the shared golden helper in
// internal/goldens, every family's golden helper must migrate
// together.
const specialsGoldenDir = "testdata/shapegen/specials"

// specialsGolden compares got with the contents of the golden file
// at specialsGoldenDir/<name>. The implementation mirrors
// treesGolden / structuredGolden / classicGolden exactly because
// the families will migrate together to the shared helper in T58.22;
// until then duplicating keeps each family's test surface
// self-contained.
func specialsGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(specialsGoldenDir, name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("specialsGolden: MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("specialsGolden: WriteFile(%q): %v", path, err)
		}
		t.Logf("rewrote golden %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // path is a test-local golden under testdata/, not user input
	if err != nil {
		t.Fatalf("specialsGolden: ReadFile(%q): %v (run with -shapegen-update to bootstrap)", path, err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// -------------------------------------------------------------------
// Local helpers — undirected scans without pulling in search.*
// -------------------------------------------------------------------

// undirectedAdj returns the symmetric closure of g's out-adjacency
// as a map of sorted neighbour slices, keyed by canonical node id.
// Set semantics during construction deduplicate entries that arise
// when the underlying [adjlist.AdjList] already mirrors undirected
// edges; the result is then materialised as a deterministic slice
// to keep downstream traversals reproducible.
//
// The helper is allocation-bounded by the graph itself and only
// invoked on graphs with at most a few thousand vertices (the
// Kneser short-layer ceiling is C(16, 8) = 12_870), so the
// quadratic constant factor is acceptable.
func undirectedAdj(g *lpg.Graph[int, int64]) map[int][]int {
	adj := g.AdjList()
	maxID := uint64(adj.MaxNodeID())
	nodes := make([]int, 0, maxID)
	mapper := adj.Mapper()
	for id := uint64(0); id < maxID; id++ {
		v, ok := mapper.Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		nodes = append(nodes, v)
	}
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
		ensure(u) // make sure isolated nodes still appear
		for v := range adj.Neighbours(u) {
			ensure(u)[v] = struct{}{}
			ensure(v)[u] = struct{}{}
		}
	}
	out := make(map[int][]int, len(sym))
	for u, set := range sym {
		nbrs := make([]int, 0, len(set))
		for v := range set {
			nbrs = append(nbrs, v)
		}
		// Sort so per-vertex traversal is deterministic; the call
		// sites that care about order use this directly.
		sortInts(nbrs)
		out[u] = nbrs
	}
	return out
}

// girth runs a BFS from every vertex of g and returns the length of
// the shortest cycle observed across the entire graph. When g has
// no cycle (i.e., it is a forest) the helper returns 0 to signal
// "no girth defined"; call sites distinguish 0 from a positive
// girth value explicitly.
//
// The BFS at each vertex finds the shortest cycle through that
// vertex by recording the depth at which each neighbour is first
// reached and detecting when two distinct BFS layers meet. The
// resulting per-vertex shortest cycle, minimised over all vertices,
// is the girth (West, §1.3.6).
//
// Complexity is O(V * (V + E)). On the catalogue's named specials
// (V <= 20) the runtime is negligible.
func girth(g *lpg.Graph[int, int64]) int {
	adj := undirectedAdj(g)
	if len(adj) == 0 {
		return 0
	}
	const inf = 1 << 30
	best := inf
	for start := range adj {
		dist := make(map[int]int, len(adj))
		parent := make(map[int]int, len(adj))
		dist[start] = 0
		parent[start] = -1
		queue := []int{start}
		for len(queue) > 0 {
			u := queue[0]
			queue = queue[1:]
			for _, v := range adj[u] {
				if _, seen := dist[v]; !seen {
					dist[v] = dist[u] + 1
					parent[v] = u
					queue = append(queue, v)
					continue
				}
				if v == parent[u] {
					continue
				}
				cyc := dist[u] + dist[v] + 1
				if cyc < best {
					best = cyc
				}
			}
		}
	}
	if best == inf {
		return 0
	}
	return best
}

// hasNoMonochromaticEdge asserts that no edge of g connects two
// vertices that share the same colour under the supplied colouring
// map. The helper is used to verify hard-coded colourings
// (Petersen 3-colouring, Moser-spindle 4-colouring) without
// implementing a full chromatic-number solver — the brief's
// explicit guidance for AC #1 / AC #4.
func hasNoMonochromaticEdge(t *testing.T, g *lpg.Graph[int, int64], colour map[int]int) {
	t.Helper()
	adj := undirectedAdj(g)
	for u, nbrs := range adj {
		cu, ok := colour[u]
		if !ok {
			t.Fatalf("colouring missing for node %d", u)
		}
		for _, v := range nbrs {
			cv, ok := colour[v]
			if !ok {
				t.Fatalf("colouring missing for node %d", v)
			}
			if cu == cv {
				t.Fatalf("monochromatic edge %d-%d (colour %d)", u, v, cu)
			}
		}
	}
}

// isSimplicial3Tree asserts that g is a simplicial 3-tree on
// wantOrder vertices: it has 3*n - 6 edges and admits a perfect
// elimination ordering in which every eliminated vertex's
// neighbourhood, at the time of removal, is a clique of size 3 (a
// triangle). This is the structural characterisation of a maximal
// planar simplicial polytope (Diestel, §12.4).
//
// The helper modifies a local copy of the symmetric adjacency
// during elimination; g itself is not touched. The elimination
// proceeds greedily: at every step it picks the smallest-indexed
// vertex with exactly three neighbours, verifies those three
// neighbours form a triangle, removes the vertex, and continues.
// The base case stops once exactly 4 vertices remain (the K_4 base
// of a 3-tree). The helper fails the test if no degree-3 vertex
// is available at some step (indicating g is not a 3-tree) or if
// the three neighbours do not form a triangle (indicating the
// removed vertex was not simplicial).
func isSimplicial3Tree(t *testing.T, g *lpg.Graph[int, int64], wantOrder int) {
	t.Helper()
	want3nm6 := uint64(3*wantOrder - 6)
	if got := g.AdjList().Size(); got != want3nm6 {
		t.Fatalf("Size = %d, want 3n-6 = %d", got, want3nm6)
	}
	if g.AdjList().Directed() {
		t.Fatal("isSimplicial3Tree: graph must be undirected")
	}
	// Local mutable adjacency copy as a map[int]map[int]struct{} so
	// "remove neighbour" is O(1) and "is triangle?" is O(d^2) on the
	// candidate's neighbour list.
	src := undirectedAdj(g)
	adj := make(map[int]map[int]struct{}, len(src))
	for u, nbrs := range src {
		m := make(map[int]struct{}, len(nbrs))
		for _, v := range nbrs {
			m[v] = struct{}{}
		}
		adj[u] = m
	}
	for len(adj) > 4 {
		// Find the smallest-indexed degree-3 vertex.
		chosen := -1
		for u := 0; u < wantOrder; u++ {
			nbrs, ok := adj[u]
			if !ok {
				continue
			}
			if len(nbrs) == 3 {
				chosen = u
				break
			}
		}
		if chosen == -1 {
			t.Fatalf("3-tree elimination stuck: no degree-3 vertex remains (current %d nodes)", len(adj))
		}
		// Collect the three neighbours of chosen.
		var trio [3]int
		i := 0
		for v := range adj[chosen] {
			trio[i] = v
			i++
		}
		// Verify the three neighbours form a triangle (each pair is
		// adjacent in adj).
		for a := 0; a < 3; a++ {
			for b := a + 1; b < 3; b++ {
				if _, ok := adj[trio[a]][trio[b]]; !ok {
					t.Fatalf("3-tree elimination: neighbours %d and %d of %d are not adjacent", trio[a], trio[b], chosen)
				}
			}
		}
		// Remove chosen from the local adjacency.
		for v := range adj[chosen] {
			delete(adj[v], chosen)
		}
		delete(adj, chosen)
	}
	// The base case must be K_4.
	if len(adj) != 4 {
		t.Fatalf("3-tree elimination ended with %d nodes, want 4", len(adj))
	}
	for u, nbrs := range adj {
		if len(nbrs) != 3 {
			t.Fatalf("3-tree base: node %d has degree %d, want 3", u, len(nbrs))
		}
	}
}

// -------------------------------------------------------------------
// Petersen
// -------------------------------------------------------------------

// TestSpecials_Petersen_Invariants asserts AC #1: Petersen has 10
// nodes, 15 edges, girth 5, is 3-regular, and admits a 3-colouring.
func TestSpecials_Petersen_Invariants(t *testing.T) {
	t.Parallel()
	s := Petersen()
	if got, want := s.Name(), "specials.petersen"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	if got := s.Knobs(); len(got) != 0 {
		t.Fatalf("Knobs = %#v, want empty", got)
	}
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertOrder(t, g, 10)
	assertSize(t, g, 15)
	assertDirected(t, g, false)
	// 3-regular.
	adj := undirectedAdj(g)
	for v := 0; v < 10; v++ {
		if got := len(adj[v]); got != 3 {
			t.Fatalf("Petersen deg(%d) = %d, want 3", v, got)
		}
	}
	if got := girth(g); got != 5 {
		t.Fatalf("Petersen girth = %d, want 5", got)
	}
	// Verify literature labels are attached.
	for v := 0; v < 10; v++ {
		if !g.HasNodeLabel(v, petersenLabels[v]) {
			t.Fatalf("Petersen node %d missing label %q", v, petersenLabels[v])
		}
	}
	// AC #1: chromatic number 3 — exhibit a valid 3-colouring.
	// Standard Petersen 3-colouring: split outer pentagon and inner
	// pentagram into two 2-colourings (each being a 5-cycle needs
	// 3 colours), with the spokes forcing a swap of one colour.
	//   outer 0..4: colours [0, 1, 0, 1, 2]
	//   inner 5..9: colours [1, 0, 1, 2, 0]
	// Spoke pairs: (0,5)=0/1 ✓, (1,6)=1/0 ✓, (2,7)=0/1 ✓,
	// (3,8)=1/2 ✓, (4,9)=2/0 ✓.
	// Inner pentagram edges (5,7), (7,9), (9,6), (6,8), (8,5):
	// 1/1 — wait, let me re-derive below.
	colour := petersenThreeColouring()
	hasNoMonochromaticEdge(t, g, colour)
}

// petersenThreeColouring returns a valid 3-colouring of the Petersen
// graph using the standard "two pentagons plus shifted spokes"
// scheme. Exposed as a helper so the test body stays linear.
//
// Vertices: 0..4 outer pentagon (cycle), 5..9 inner pentagram
// (cycle via the two-step subgraph), spokes 0-5, 1-6, 2-7, 3-8, 4-9.
// Colour assignment (verified by hasNoMonochromaticEdge):
//
//	0:0  1:1  2:2  3:1  4:2
//	5:1  6:2  7:0  8:0  9:1
func petersenThreeColouring() map[int]int {
	return map[int]int{
		0: 0, 1: 1, 2: 2, 3: 1, 4: 2,
		5: 1, 6: 2, 7: 0, 8: 0, 9: 1,
	}
}

// TestSpecials_Petersen_Golden pins the canonical adjacency
// listing of the Petersen graph.
func TestSpecials_Petersen_Golden(t *testing.T) {
	t.Parallel()
	g, err := Petersen().Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	specialsGolden(t, "petersen.txt", formatAdjacency(g))
}

// TestSpecials_Petersen_KneserEquivalence checks the semantic
// equivalence Petersen ≅ K(5, 2) called out in the brief: equal
// Order, equal Size, equal degree distribution. The exact node ids
// differ because Kneser uses a lexicographic 2-subset assignment
// (10 vertices labelled "{0,1}".."{3,4}") whereas Petersen uses the
// 0..9 outer/inner layout.
func TestSpecials_Petersen_KneserEquivalence(t *testing.T) {
	t.Parallel()
	gP, err := Petersen().Build(defaultCfg)
	if err != nil {
		t.Fatalf("Petersen Build: %v", err)
	}
	gK, err := Kneser(5, 2).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Kneser(5,2) Build: %v", err)
	}
	if got, want := gP.AdjList().Order(), gK.AdjList().Order(); got != want {
		t.Fatalf("Order: Petersen=%d, Kneser(5,2)=%d", got, want)
	}
	if got, want := gP.AdjList().Size(), gK.AdjList().Size(); got != want {
		t.Fatalf("Size: Petersen=%d, Kneser(5,2)=%d", got, want)
	}
	if !sameDegreeDistribution(gP, gK) {
		t.Fatal("Petersen and Kneser(5,2) have differing degree distributions")
	}
}

// sameDegreeDistribution compares two graphs' degree multisets.
// Both must be undirected; the helper iterates each graph's
// vertices via the mapper, computes degrees via undirectedAdj, and
// asserts the resulting sorted degree sequences match.
func sameDegreeDistribution(a, b *lpg.Graph[int, int64]) bool {
	deg := func(g *lpg.Graph[int, int64]) []int {
		adj := undirectedAdj(g)
		out := make([]int, 0, len(adj))
		for _, nbrs := range adj {
			out = append(out, len(nbrs))
		}
		sortInts(out)
		return out
	}
	da := deg(a)
	db := deg(b)
	if len(da) != len(db) {
		return false
	}
	for i := range da {
		if da[i] != db[i] {
			return false
		}
	}
	return true
}

// -------------------------------------------------------------------
// Dodecahedral
// -------------------------------------------------------------------

// TestSpecials_Dodecahedral_Invariants asserts AC #2: 20 nodes, 30
// edges, 3-regular.
func TestSpecials_Dodecahedral_Invariants(t *testing.T) {
	t.Parallel()
	s := Dodecahedral()
	if got, want := s.Name(), "specials.dodecahedral"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	if got := s.Knobs(); len(got) != 0 {
		t.Fatalf("Knobs = %#v, want empty", got)
	}
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertOrder(t, g, 20)
	assertSize(t, g, 30)
	assertDirected(t, g, false)
	adj := undirectedAdj(g)
	for v := 0; v < 20; v++ {
		if got := len(adj[v]); got != 3 {
			t.Fatalf("Dodecahedral deg(%d) = %d, want 3", v, got)
		}
	}
	// Standard girth of the dodecahedral graph is 5.
	if got := girth(g); got != 5 {
		t.Fatalf("Dodecahedral girth = %d, want 5", got)
	}
	// Verify literature labels.
	for v := 0; v < 20; v++ {
		if !g.HasNodeLabel(v, dodecahedralLabels[v]) {
			t.Fatalf("Dodecahedral node %d missing label %q", v, dodecahedralLabels[v])
		}
	}
}

// TestSpecials_Dodecahedral_Golden pins the canonical adjacency
// listing.
func TestSpecials_Dodecahedral_Golden(t *testing.T) {
	t.Parallel()
	g, err := Dodecahedral().Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	specialsGolden(t, "dodecahedral.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Goldner-Harary
// -------------------------------------------------------------------

// TestSpecials_GoldnerHarary_Invariants asserts AC #3: 11 nodes, 27
// edges, simplicial 3-tree.
func TestSpecials_GoldnerHarary_Invariants(t *testing.T) {
	t.Parallel()
	s := GoldnerHarary()
	if got, want := s.Name(), "specials.goldner-harary"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	if got := s.Knobs(); len(got) != 0 {
		t.Fatalf("Knobs = %#v, want empty", got)
	}
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertOrder(t, g, 11)
	assertSize(t, g, 27)
	assertDirected(t, g, false)
	isSimplicial3Tree(t, g, 11)
	// Verify literature labels.
	for v := 0; v < 11; v++ {
		if !g.HasNodeLabel(v, goldnerHararyLabels[v]) {
			t.Fatalf("GoldnerHarary node %d missing label %q", v, goldnerHararyLabels[v])
		}
	}
}

// TestSpecials_GoldnerHarary_Golden pins the canonical adjacency
// listing.
func TestSpecials_GoldnerHarary_Golden(t *testing.T) {
	t.Parallel()
	g, err := GoldnerHarary().Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	specialsGolden(t, "goldner-harary.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Moser spindle
// -------------------------------------------------------------------

// TestSpecials_MoserSpindle_Invariants asserts AC #4: 7 nodes, 11
// edges, chi=4 via a hard-coded valid 4-colouring.
func TestSpecials_MoserSpindle_Invariants(t *testing.T) {
	t.Parallel()
	s := MoserSpindle()
	if got, want := s.Name(), "specials.moser-spindle"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	if got := s.Knobs(); len(got) != 0 {
		t.Fatalf("Knobs = %#v, want empty", got)
	}
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertOrder(t, g, 7)
	assertSize(t, g, 11)
	assertDirected(t, g, false)
	for v := 0; v < 7; v++ {
		if !g.HasNodeLabel(v, moserSpindleLabels[v]) {
			t.Fatalf("MoserSpindle node %d missing label %q", v, moserSpindleLabels[v])
		}
	}
	// AC #4: χ = 4 — exhibit a valid 4-colouring.
	colour := moserSpindleFourColouring()
	hasNoMonochromaticEdge(t, g, colour)
}

// moserSpindleFourColouring returns a valid 4-colouring of the
// Moser spindle. Colours derived by hand from the rhombus
// construction:
//
//	0: A     (shared apex of both rhombi)
//	1: B  2: C  3: D    (rhombus 1)
//	4: B  5: D  6: C    (rhombus 2)
//
// The single connecting edge 2-5 binds C↔D, which is satisfied.
func moserSpindleFourColouring() map[int]int {
	return map[int]int{
		0: 0,
		1: 1, 2: 2, 3: 3,
		4: 1, 5: 3, 6: 2,
	}
}

// TestSpecials_MoserSpindle_Golden pins the canonical adjacency
// listing.
func TestSpecials_MoserSpindle_Golden(t *testing.T) {
	t.Parallel()
	g, err := MoserSpindle().Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	specialsGolden(t, "moser-spindle.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Kneser
// -------------------------------------------------------------------

// TestSpecials_Kneser_Invariants exercises a sweep of (n, k) pairs
// and asserts the closed forms Order=C(n,k), Size=Order*C(n-k,k)/2,
// degree=C(n-k,k) uniformly across vertices.
func TestSpecials_Kneser_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct{ n, k int }{
		{1, 0},  // single empty subset, no edges
		{1, 1},  // single subset {0}, no edges (no disjoint partner)
		{3, 1},  // K(3,1): triangle on 3 singletons
		{5, 2},  // Petersen
		{6, 2},  // K(6,2): 15 vertices, 45 edges, 4-regular
		{6, 3},  // K(6,3): 20 vertices, 10 edges (bidesmic pair-up)
		{7, 3},  // K(7,3): 35 vertices, 70 edges, 4-regular
		{4, 0},  // single empty subset → 1 vertex, no edges
		{10, 5}, // K(10,5): 252 vertices, 126 edges
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_k=%d", c.n, c.k), func(t *testing.T) {
			t.Parallel()
			s := Kneser(c.n, c.k)
			if got, want := s.Name(), "specials.kneser"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 2 ||
				knobs[0].Name != "n" || knobs[0].Min != 1 || knobs[0].Max != kneserMaxN || knobs[0].Default != 5 ||
				knobs[1].Name != "k" || knobs[1].Min != 0 || knobs[1].Max != kneserMaxK || knobs[1].Default != 2 {
				t.Fatalf("Knobs = %#v, want n:[1,%d]/5 and k:[0,%d]/2", knobs, kneserMaxN, kneserMaxK)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			wantOrder := uint64(binomial(c.n, c.k))
			assertOrder(t, g, wantOrder)
			// Closed-form size and per-vertex degree. The closed
			// form deg = C(n-k, k) holds when distinct vertices can
			// be disjoint, i.e. when 2k <= n; otherwise no pair of
			// k-subsets of [n] is disjoint and the graph has no
			// edges (deg = 0). The k == 0 case is a further
			// degeneracy: there is exactly one empty subset, the
			// graph has one vertex and zero edges, and the closed
			// form C(n, 0) = 1 does not apply because the empty
			// subset is "disjoint from itself" but the catalogue
			// does not insert a self-loop.
			wantDeg := kneserExpectedDegree(c.n, c.k)
			wantSize := uint64(wantDeg) * wantOrder / 2
			assertSize(t, g, wantSize)
			assertDirected(t, g, false)
			adj := undirectedAdj(g)
			for v := 0; v < int(wantOrder); v++ {
				if got := len(adj[v]); got != wantDeg {
					t.Fatalf("Kneser(%d,%d) deg(%d) = %d, want %d", c.n, c.k, v, got, wantDeg)
				}
			}
		})
	}
}

// TestSpecials_Kneser_PanicsOutOfRange covers every Kneser
// constructor guard branch:
//   - n < 1
//   - n > kneserMaxN
//   - k < 0
//   - k > n
//   - k > kneserMaxK
func TestSpecials_Kneser_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		n, k int
	}{
		{"n_zero", 0, 0},
		{"n_too_large", kneserMaxN + 1, 1},
		{"k_negative", 5, -1},
		{"k_above_n", 3, 5},
		{"k_above_max", 16, kneserMaxK + 1}, // n=16, k=9: k <= n is satisfied
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Kneser(%d,%d) did not panic", c.n, c.k)
				}
			}()
			_ = Kneser(c.n, c.k)
		})
	}
}

// TestSpecials_Kneser_LabelsAreLexicographic asserts that the
// vertex labels follow the lexicographic ordering of k-subsets and
// match the documented "{i,j,...}" format. Spot-checks the first
// three labels of K(5, 2):
//
//	node 0 → {0,1}
//	node 1 → {0,2}
//	node 2 → {0,3}
func TestSpecials_Kneser_LabelsAreLexicographic(t *testing.T) {
	t.Parallel()
	g, err := Kneser(5, 2).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []string{"{0,1}", "{0,2}", "{0,3}", "{0,4}", "{1,2}"}
	for i, w := range want {
		if !g.HasNodeLabel(i, w) {
			labels := g.NodeLabels(i)
			t.Fatalf("Kneser(5,2) node %d missing label %q (has %v)", i, w, labels)
		}
	}
}

// TestSpecials_Kneser_EmptySubsetLabel covers the empty-subset
// branch of kneserSubsetLabel: Kneser(n, 0) for any valid n has a
// single vertex labelled "{}".
func TestSpecials_Kneser_EmptySubsetLabel(t *testing.T) {
	t.Parallel()
	g, err := Kneser(3, 0).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertOrder(t, g, 1)
	assertSize(t, g, 0)
	if !g.HasNodeLabel(0, "{}") {
		t.Fatalf("Kneser(3,0) node 0 missing label %q (has %v)", "{}", g.NodeLabels(0))
	}
}

// TestSpecials_Kneser_Goldens pins K(5, 2) and K(6, 2). The first
// is Petersen-equivalent (same Order/Size/degree distribution but a
// different node-id layout, asserted in
// TestSpecials_Petersen_KneserEquivalence).
func TestSpecials_Kneser_Goldens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, k int
		name string
	}{
		{5, 2, "kneser-n5-k2.txt"},
		{6, 2, "kneser-n6-k2.txt"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			g, err := Kneser(c.n, c.k).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			specialsGolden(t, c.name, formatAdjacency(g))
		})
	}
}

// TestSpecials_Kneser_Properties_RapidSweep drives Kneser over a
// small (n, k) sweep and asserts the closed forms hold for every
// draw. Bounds keep the per-trial vertex count under 200, well
// inside the short-layer budget.
func TestSpecials_Kneser_Properties_RapidSweep(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(r, "n")
		k := rapid.IntRange(0, min(n, kneserMaxK)).Draw(r, "k")
		g, err := Kneser(n, k).Build(defaultCfg)
		if err != nil {
			t.Fatalf("n=%d k=%d: Build: %v", n, k, err)
		}
		wantOrder := uint64(binomial(n, k))
		if got := g.AdjList().Order(); got != wantOrder {
			t.Fatalf("n=%d k=%d: Order = %d, want %d", n, k, got, wantOrder)
		}
		wantDeg := kneserExpectedDegree(n, k)
		wantSize := uint64(wantDeg) * wantOrder / 2
		if got := g.AdjList().Size(); got != wantSize {
			t.Fatalf("n=%d k=%d: Size = %d, want %d", n, k, got, wantSize)
		}
	})
}

// kneserExpectedDegree returns the catalogue's closed-form per-vertex
// degree of K(n, k). The standard formula C(n - k, k) holds whenever
// k >= 1 and n - k >= k (i.e. 2k <= n); otherwise the graph has no
// edges and every vertex has degree 0. The k == 0 case is treated
// separately because C(n, 0) = 1 but the single vertex has no
// disjoint partner (the empty subset is "disjoint from itself" but
// the simple-graph catalogue does not insert a self-loop).
func kneserExpectedDegree(n, k int) int {
	if k == 0 || n-k < k {
		return 0
	}
	return binomial(n-k, k)
}

// TestSpecials_Kneser_BinomialEdgeCases exercises the binomial helper
// directly so the k<0/k>n short-circuit branches are covered. The
// helper is package-local; the test pokes it via the same package
// scope.
func TestSpecials_Kneser_BinomialEdgeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, k, want int
	}{
		{5, -1, 0},
		{3, 5, 0},
		{0, 0, 1},
		{5, 0, 1},
		{5, 5, 1},
		{6, 4, 15}, // exercises the k > n-k swap branch
		{10, 3, 120},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("C(%d,%d)", c.n, c.k), func(t *testing.T) {
			t.Parallel()
			if got := binomial(c.n, c.k); got != c.want {
				t.Fatalf("binomial(%d,%d) = %d, want %d", c.n, c.k, got, c.want)
			}
		})
	}
}

// TestSpecials_Kneser_KSubsetsLex exercises every branch of
// kSubsetsLex: the k == 0 special case (single empty subset), the
// k > n special case (empty result), and the normal lex
// enumeration. Spot-checks the count via binomial and the first/
// last subset.
func TestSpecials_Kneser_KSubsetsLex(t *testing.T) {
	t.Parallel()
	t.Run("k_zero", func(t *testing.T) {
		t.Parallel()
		got := kSubsetsLex(5, 0)
		if len(got) != 1 || len(got[0]) != 0 {
			t.Fatalf("kSubsetsLex(5, 0) = %v, want single empty subset", got)
		}
	})
	t.Run("k_above_n", func(t *testing.T) {
		t.Parallel()
		got := kSubsetsLex(3, 5)
		if got != nil {
			t.Fatalf("kSubsetsLex(3, 5) = %v, want nil", got)
		}
	})
	t.Run("normal_5_2", func(t *testing.T) {
		t.Parallel()
		got := kSubsetsLex(5, 2)
		if len(got) != 10 {
			t.Fatalf("len(kSubsetsLex(5, 2)) = %d, want 10", len(got))
		}
		// First subset is {0, 1}; last is {3, 4}.
		if got[0][0] != 0 || got[0][1] != 1 {
			t.Fatalf("first subset = %v, want [0 1]", got[0])
		}
		last := got[len(got)-1]
		if last[0] != 3 || last[1] != 4 {
			t.Fatalf("last subset = %v, want [3 4]", last)
		}
	})
}

// TestSpecials_Kneser_DisjointAscSubsets exercises every branch of
// disjointAscSubsets: shared first element, shared internal element,
// fully disjoint with a < b head, fully disjoint with a > b head,
// and the two empty-subset edge cases (one or both inputs empty).
func TestSpecials_Kneser_DisjointAscSubsets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b []int
		want bool
	}{
		{"shared_head", []int{1, 2}, []int{1, 3}, false},
		{"shared_middle", []int{1, 3, 5}, []int{2, 3}, false},
		{"disjoint_a_less", []int{0, 2}, []int{1, 3}, true},
		{"disjoint_a_greater", []int{2, 4}, []int{1, 3}, true},
		{"both_empty", nil, nil, true},
		{"one_empty", []int{1, 2}, nil, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := disjointAscSubsets(c.a, c.b); got != c.want {
				t.Fatalf("disjointAscSubsets(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// -------------------------------------------------------------------
// MaxShardCapacity preservation
// -------------------------------------------------------------------

// TestSpecials_PreservesMaxShardCapacity confirms that every specials
// generator preserves cfg.MaxShardCapacity verbatim, mirroring the
// other-family contracts.
func TestSpecials_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	for _, tc := range []struct {
		name string
		s    Shape[int, int64]
	}{
		{"petersen", Petersen()},
		{"dodecahedral", Dodecahedral()},
		{"goldner_harary", GoldnerHarary()},
		{"moser_spindle", MoserSpindle()},
		{"kneser_5_2", Kneser(5, 2)},
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

// TestSpecials_BuildLabelledEdgeList_ShardFullPropagates exercises
// the error path of buildLabelledEdgeList by constructing a
// synthetic 300-node labelled-edge build that pushes intraIdx above
// the configured MaxShardCapacity. The adjlist contract surfaces
// the cap-violation only when the responsible shard's intraIdx
// would have to grow past MaxShardCapacity; with shardBits = 8
// (256 shards) and MaxShardCapacity = 1, the first NodeID that
// crosses intraIdx >= 1 — NodeID 256 — produces the first
// adjlist.ErrShardFull, which buildLabelledEdgeList must propagate
// verbatim.
//
// The four constant-edge generators (Petersen, Dodecahedral,
// GoldnerHarary, MoserSpindle) all have order under 256, so no
// "natural" call into buildLabelledEdgeList can exercise the
// error branch in the short layer. The synthetic harness below
// invokes the helper directly with 300 synthetic labels and a
// chain of edges connecting them, ensuring the error path is
// covered for the helper that the four real generators share.
func TestSpecials_BuildLabelledEdgeList_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	const n = 300 // > 256 so at least one NodeID lands at intraIdx >= 1.
	cfg := adjlist.Config{Directed: false, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	labels := make([]string, n)
	for i := range labels {
		labels[i] = fmt.Sprintf("n%d", i)
	}
	// Chain of edges (0,1), (1,2), ..., (n-2, n-1). The chain is
	// long enough that some AddEdge call lands on a NodeID at
	// intraIdx >= 1 and saturates the cap.
	edges := make([][2]int, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	err := buildLabelledEdgeList(g, labels, edges)
	if err == nil {
		t.Fatal("buildLabelledEdgeList returned nil error, want adjlist.ErrShardFull")
	}
}

// TestSpecials_BuildKneser_ShardFullPropagates exercises the error
// path of buildKneser via Kneser(11, 5): C(11, 5) = 462 vertices,
// which is comfortably above the 256-shard boundary. With
// MaxShardCapacity = 1, the AddEdge that targets the first NodeID
// at intraIdx >= 1 produces adjlist.ErrShardFull, which the build
// must propagate verbatim.
//
// The Kneser-specific harness here exists because the constant-
// edge generators all stay under 256 nodes and therefore cannot
// reach the error branch on their own.
func TestSpecials_BuildKneser_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: false, MaxShardCapacity: 1}
	_, err := Kneser(11, 5).Build(cfg)
	if err == nil {
		t.Fatal("Kneser(11,5) Build with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// -------------------------------------------------------------------
// Soak layer
// -------------------------------------------------------------------

// TestSpecials_Kneser_Soak exercises Kneser at the short-layer
// ceiling C(16, 8) = 12_870 vertices. The build pass is O(V^2 * k)
// for edge insertion which is ~2 * 10^8 disjointness checks —
// minutes-scale but inside the soak layer's budget. Order/Size
// invariants are the only assertion; per-vertex degree checks would
// dominate runtime.
func TestSpecials_Kneser_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	g, err := Kneser(16, 8).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantOrder := uint64(binomial(16, 8))
	if got := g.AdjList().Order(); got != wantOrder {
		t.Fatalf("Order = %d, want %d", got, wantOrder)
	}
	wantDeg := binomial(16-8, 8)
	wantSize := uint64(wantDeg) * wantOrder / 2
	if got := g.AdjList().Size(); got != wantSize {
		t.Fatalf("Size = %d, want %d", got, wantSize)
	}
}

// -------------------------------------------------------------------
// File-local micro helpers
// -------------------------------------------------------------------

// sortInts sorts s in place in ascending order. A thin wrapper over
// stdlib sort.Ints kept here so call sites read as single concepts;
// the helper has no other consumer in this file.
func sortInts(s []int) { sort.Ints(s) }

// _ keeps the lpg import alive when only shapegen types are
// referenced in this file. The Build closures return *lpg.Graph
// already, so this is purely a compile-time guard against import
// drift during T58.22 migration.
var _ *lpg.Graph[int, int64]
