package shapegen

import (
	"fmt"
	"sort"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// This file implements the "named sparse specials" family of the
// shape catalogue. These are the canonical small graphs from the
// graph-theory literature whose answers across MST, matching,
// chromatic number, girth, planarity, and treewidth are
// hand-verifiable and serve as cross-references for every algorithm
// in search/. The constants embedded below come directly from the
// standard references (Diestel, "Graph Theory", 5th ed.; Bondy &
// Murty, "Graph Theory"; West, "Introduction to Graph Theory").
//
// # Specialisation
//
// Every generator in this file produces a *lpg.Graph[int, int64],
// mirroring the trivial, classic, structured, and trees families.
// The (int, int64) pair is the project's canonical "small unsigned
// key, signed weight" specialisation; all edges here carry
// [unweightedSentinel] (0) because the catalogue defines these
// shapes as topology fixtures rather than weight fixtures.
//
// Each Build pass also attaches a single node label per vertex,
// carrying the canonical name from the underlying graph's standard
// literature (e.g. "u0".."u4"/"v0".."v4" for Petersen's outer and
// inner pentagons, "A".."T" for the Dodecahedral vertices). The
// labels are not load-bearing for any catalogue invariant — Order,
// Size, degree distribution, and 4-colourability are all asserted
// without consulting the label map — but they document the
// identification used in the cited references so that hand
// verification against textbooks stays mechanical.
//
// # Configuration override policy
//
// Each generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// overrides cfg.Directed=false and cfg.Multigraph=false: every
// special graph defined here is an undirected simple graph.
//
// # Edge ordering and determinism
//
// Each constructor inserts edges in a deterministic order — the
// order documented per generator — so the goldens stay byte-for-byte
// reproducible across builds. Kneser additionally inserts vertices
// (k-subsets of [n]) in lexicographic order so its node IDs are
// stable across runs.
//
// # Error propagation
//
// The Build closures use the same branch-free single-err-thread
// pattern as the classic, structured, and trees families: every
// per-phase error propagates through one err variable, and the
// surrounding closure returns (g, err). The four constant-edge
// generators are pre-validated at construction time (no parameter
// can put them out of range), so the per-phase loops can only
// surface [adjlist.ErrShardFull] when the caller has set a tight
// cfg.MaxShardCapacity. Kneser validates its (n, k) parameters at
// construction time and panics on out-of-range input, matching the
// rest of the catalogue.

// specialsBase is the shared scaffolding for every generator in
// this file. Its layout mirrors trivialBase, classicBase,
// structuredBase, and treesBase so the helpers (Name, Knobs, Build)
// carry the exact same semantics.
type specialsBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s specialsBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s specialsBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s specialsBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// petersenEdges is the canonical edge list of the Petersen graph in
// the "outer pentagon + inner pentagram + spokes" layout (Diestel,
// 5th ed., §1.7). Vertices 0..4 form the outer pentagon, vertices
// 5..9 form the inner pentagram, and spokes connect 0↔5, 1↔6, 2↔7,
// 3↔8, 4↔9.
var petersenEdges = [...][2]int{
	// Outer pentagon (5 edges).
	{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 0},
	// Inner pentagram (5 edges) — the two-step cycle on {5..9}.
	{5, 7}, {7, 9}, {9, 6}, {6, 8}, {8, 5},
	// Spokes (5 edges).
	{0, 5}, {1, 6}, {2, 7}, {3, 8}, {4, 9},
}

// petersenLabels maps each vertex id to its canonical literature
// label: u0..u4 for the outer pentagon, v0..v4 for the inner
// pentagram. The labels follow Diestel's notation.
var petersenLabels = [...]string{
	"u0", "u1", "u2", "u3", "u4",
	"v0", "v1", "v2", "v3", "v4",
}

// Petersen returns a Shape that builds the Petersen graph: the
// canonical 10-vertex, 15-edge, 3-regular graph with girth 5,
// chromatic number 3, and crossing number 2 (hence non-planar). It
// is the smallest 3-regular graph without a Hamiltonian path (only
// without a Hamiltonian cycle in the standard sense; it does have a
// Hamiltonian path), the smallest hypohamiltonian graph, and the
// unique 5-cage (Diestel, 5th ed., §1.8).
//
// The Petersen graph is isomorphic to the Kneser graph K(5, 2):
// every vertex is a 2-subset of {0,1,2,3,4} and two vertices are
// adjacent iff their subsets are disjoint. The semantic equivalence
// with [Kneser](5, 2) is asserted by
// TestSpecials_Petersen_KneserEquivalence (matching Order, Size,
// and the degree distribution rather than the byte-for-byte
// adjacency, because Kneser uses a lexicographic node-id
// assignment over k-subsets whereas Petersen uses the 0..9 layout
// pinned by [petersenEdges]).
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 10.
//   - Size()  == 15.
//   - Every vertex has degree exactly 3 (3-regular).
//   - Girth == 5.
//   - Chromatic number == 3 (a valid 3-colouring is asserted in the
//     test; the lower bound χ ≥ 3 follows from the fact that
//     Petersen contains odd cycles and is documented in the godoc,
//     not asserted at runtime per the brief).
//   - Non-planar: contains a K_{3,3} minor (Diestel, §4.4). The
//     non-planarity is documented in the godoc and AC, not
//     asserted at runtime — the brief explicitly forbids
//     implementing a planarity check.
//
// Petersen declares no knobs (the graph is fully specified by
// [petersenEdges]).
func Petersen() Shape[int, int64] {
	return specialsBase{
		name: "specials.petersen",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildLabelledEdgeList(g, petersenLabels[:], petersenEdges[:])
		},
	}
}

// dodecahedralEdges is the canonical edge list of the dodecahedron
// graph (Schlegel diagram) with vertices 0..19 arranged in four
// concentric layers: L0 = {0..4} outer pentagon, L1 = {5..9} ring
// of "outer-middle" vertices, L2 = {10..14} ring of "inner-middle"
// vertices, L3 = {15..19} inner pentagon. Each vertex has degree 3.
// (Bondy & Murty, "Graph Theory", Appendix A.4.)
var dodecahedralEdges = [...][2]int{
	// Outer pentagon (L0), 5 edges.
	{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 0},
	// L0 → L1 connectors, 5 edges.
	{0, 5}, {1, 6}, {2, 7}, {3, 8}, {4, 9},
	// L1 ↔ L2 connectors (the "twist", 10 edges).
	{5, 10}, {5, 14},
	{6, 11}, {6, 10},
	{7, 12}, {7, 11},
	{8, 13}, {8, 12},
	{9, 14}, {9, 13},
	// L2 → L3 connectors, 5 edges.
	{10, 15}, {11, 16}, {12, 17}, {13, 18}, {14, 19},
	// Inner pentagon (L3), 5 edges.
	{15, 16}, {16, 17}, {17, 18}, {18, 19}, {19, 15},
}

// dodecahedralLabels carries the standard A..T literature labels for
// the 20 vertices of the dodecahedron (West, "Introduction to Graph
// Theory", §1.1).
var dodecahedralLabels = [...]string{
	"A", "B", "C", "D", "E", // outer pentagon
	"F", "G", "H", "I", "J", // outer-middle ring
	"K", "L", "M", "N", "O", // inner-middle ring
	"P", "Q", "R", "S", "T", // inner pentagon
}

// Dodecahedral returns a Shape that builds the dodecahedron graph:
// the canonical 20-vertex, 30-edge, 3-regular planar graph that is
// the 1-skeleton of the regular dodecahedron polytope. It has girth
// 5 and chromatic number 3 (Bondy & Murty, Appendix A.4).
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 20.
//   - Size()  == 30.
//   - Every vertex has degree exactly 3 (3-regular).
//   - Planar (1-skeleton of a polytope): documented in the godoc
//     and AC, not asserted at runtime — the brief explicitly
//     forbids implementing a planarity check.
//
// Dodecahedral declares no knobs.
func Dodecahedral() Shape[int, int64] {
	return specialsBase{
		name: "specials.dodecahedral",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildLabelledEdgeList(g, dodecahedralLabels[:], dodecahedralEdges[:])
		},
	}
}

// goldnerHararyEdges is the canonical edge list of the Goldner-
// Harary graph (Goldner & Harary, 1975), constructed deterministically
// as a maximal planar simplicial 3-tree on 11 vertices. The
// construction stellates K_4 four times then stellates three of the
// resulting faces incident with vertex 4:
//
//  1. K_4 base on {0, 1, 2, 3} contributes 6 edges.
//  2. Stellate the four faces of K_4 — adding apexes 4 (for face
//     (1,2,3)), 5 (for (0,2,3)), 6 (for (0,1,3)), 7 (for (0,1,2)) —
//     each contributing 3 edges (12 edges total). At this point
//     vertex 4 is adjacent to 1, 2, and 3, so triangles (4,1,2),
//     (4,1,3), and (4,2,3) all exist in the triangulation.
//  3. Stellate the three new faces incident with vertex 4: apex 8
//     on face (4,1,2), 9 on face (4,1,3), 10 on face (4,2,3). Each
//     contributes 3 edges (9 edges total).
//
// Total: 6 + 12 + 9 = 27 edges on 11 vertices. The resulting graph
// is a simplicial 3-tree (every triangle is the boundary of a face
// in the constructed triangulation) and therefore maximal planar
// with the closed-form edge count 3n - 6 = 27.
//
// The construction is fully deterministic; the exact embedding may
// differ from other "Goldner-Harary" depictions in the literature
// but the structural invariants pinned in AC #3 (11 nodes, 27 edges,
// simplicial 3-tree) hold by construction.
var goldnerHararyEdges = [...][2]int{
	// Phase 1: K_4 on {0, 1, 2, 3}, 6 edges.
	{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3},
	// Phase 2a: stellate face (1, 2, 3) → apex 4.
	{4, 1}, {4, 2}, {4, 3},
	// Phase 2b: stellate face (0, 2, 3) → apex 5.
	{5, 0}, {5, 2}, {5, 3},
	// Phase 2c: stellate face (0, 1, 3) → apex 6.
	{6, 0}, {6, 1}, {6, 3},
	// Phase 2d: stellate face (0, 1, 2) → apex 7.
	{7, 0}, {7, 1}, {7, 2},
	// Phase 3a: stellate face (4, 1, 2) → apex 8. The triangle
	// (4, 1, 2) exists because phase 2a connected 4 to 1 and to 2,
	// and (1, 2) is a K_4 edge from phase 1.
	{8, 4}, {8, 1}, {8, 2},
	// Phase 3b: stellate face (4, 1, 3) → apex 9.
	{9, 4}, {9, 1}, {9, 3},
	// Phase 3c: stellate face (4, 2, 3) → apex 10.
	{10, 4}, {10, 2}, {10, 3},
}

// goldnerHararyLabels uses the literature convention of naming the
// K_4 base "a0..a3" and stellation apexes "b1..b7" in insertion
// order.
var goldnerHararyLabels = [...]string{
	"a0", "a1", "a2", "a3",
	"b1", "b2", "b3", "b4",
	"b5", "b6", "b7",
}

// GoldnerHarary returns a Shape that builds the Goldner-Harary
// graph: the canonical 11-vertex, 27-edge simplicial 3-tree built by
// stellating K_4 four times then stellating three of the resulting
// faces. By construction it is maximal planar (3n - 6 = 27 edges on
// n = 11 vertices) and a 3-tree (every triangle bounds a face of
// the triangulation).
//
// The Goldner-Harary graph is historically significant as a small
// non-Hamiltonian simplicial polytope (Goldner & Harary, 1975).
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 11.
//   - Size()  == 27.
//   - The graph is a simplicial 3-tree: there exists an elimination
//     ordering where each removed vertex's neighbourhood at the
//     time of removal is a clique of size 3 (verified in the test
//     via a deterministic elimination scan).
//   - Maximal planar (3n - 6 = 27): documented in the godoc and
//     AC, not asserted at runtime per the brief.
//
// GoldnerHarary declares no knobs.
func GoldnerHarary() Shape[int, int64] {
	return specialsBase{
		name: "specials.goldner-harary",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildLabelledEdgeList(g, goldnerHararyLabels[:], goldnerHararyEdges[:])
		},
	}
}

// moserSpindleEdges is the canonical edge list of the Moser spindle
// (Moser & Moser, 1961): two unit-distance rhombi joined at a vertex
// plus one connecting edge, yielding 7 vertices and 11 edges with
// chromatic number 4.
//
// Construction with vertex labels A..G ↔ 0..6:
//   - Rhombus 1: A=0, B=1, C=2, D=3, with edges 0-1, 1-2, 2-3, 3-0
//     and the short diagonal 1-3 (K_4 minus the long diagonal 0-2).
//   - Rhombus 2: A=0, E=4, F=5, G=6, with edges 0-4, 4-5, 5-6, 6-0
//     and the short diagonal 4-6 (K_4 minus the long diagonal 0-5).
//   - Connecting edge 2-5 (between the two "far" tips of the rhombi).
//
// Total: 5 + 5 + 1 = 11 edges.
var moserSpindleEdges = [...][2]int{
	// Rhombus 1 (K_4 on {0,1,2,3} minus edge 0-2), 5 edges.
	{0, 1}, {1, 2}, {2, 3}, {3, 0}, {1, 3},
	// Rhombus 2 (K_4 on {0,4,5,6} minus edge 0-5), 5 edges.
	{0, 4}, {4, 5}, {5, 6}, {6, 0}, {4, 6},
	// Connecting edge between the two rhombi tips.
	{2, 5},
}

// moserSpindleLabels carries the standard A..G literature labels.
var moserSpindleLabels = [...]string{
	"A", "B", "C", "D", "E", "F", "G",
}

// MoserSpindle returns a Shape that builds the Moser spindle: the
// canonical 7-vertex, 11-edge unit-distance graph with chromatic
// number 4 (Moser & Moser, 1961). It is the smallest 4-chromatic
// unit-distance graph in the plane and was used in the original
// proof that the chromatic number of the plane is at least 4.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 7.
//   - Size()  == 11.
//   - Chromatic number == 4. The test asserts both halves:
//   - χ ≤ 4 by exhibiting a hard-coded valid 4-colouring and
//     checking no edge is monochromatic.
//   - χ ≥ 4 is documented in the godoc and AC, not asserted at
//     runtime per the brief (the test does not implement a
//     3-colouring solver).
//
// MoserSpindle declares no knobs.
func MoserSpindle() Shape[int, int64] {
	return specialsBase{
		name: "specials.moser-spindle",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildLabelledEdgeList(g, moserSpindleLabels[:], moserSpindleEdges[:])
		},
	}
}

// buildLabelledEdgeList interns nodes 0..len(labels)-1 in g, attaches
// the corresponding label from labels to each, and inserts every
// edge from edges in declaration order. The first SetNodeLabel /
// AddEdge error short-circuits the loop. The combined helper keeps
// the surrounding Build closures branch-free and lets every constant-
// edge generator delegate to a single function.
//
// Pre-condition: labels and edges are package-local constants
// (declared above), so their lengths and contents are checked at
// compile time. The function does not re-validate them at runtime
// for the same reason addNodesRange in trivial.go does not.
func buildLabelledEdgeList(g *lpg.Graph[int, int64], labels []string, edges [][2]int) error {
	var err error
	for i := 0; i < len(labels) && err == nil; i++ {
		err = g.SetNodeLabel(canonicalNode(i), labels[i])
	}
	for i := 0; i < len(edges) && err == nil; i++ {
		e := edges[i]
		err = g.AddEdge(canonicalNode(e[0]), canonicalNode(e[1]), unweightedSentinel)
	}
	return err
}

// kneserMaxN is the upper bound on the Kneser knob "n". C(16, 8) =
// 12_870 vertices is the heaviest the short layer admits; soak/
// nightly callers may drive the underlying constructor up to higher
// n directly.
const kneserMaxN = 16

// kneserMaxK is the upper bound on the Kneser knob "k". For
// k > n/2 the Kneser graph K(n, k) is empty (no two k-subsets of
// [n] are disjoint), so larger k is not useful in the catalogue.
const kneserMaxK = 8

// Kneser returns a Shape that builds the Kneser graph K(n, k): the
// graph whose vertices are the C(n, k) k-subsets of {0, 1, ..., n-1}
// and whose edges connect every pair of disjoint k-subsets.
//
// Node ids are assigned in lexicographic order of subsets: subset
// {0,1,...,k-1} is node 0, the next subset in lexicographic order is
// node 1, and so on through the C(n, k)-th subset. Each vertex
// carries a node label of the form "{i,j,...}" (comma-separated
// elements in ascending order, wrapped in braces) for hand
// inspection against the literature (West, §1.1.7).
//
// Catalogue invariants on the returned graph:
//
//   - Order() == C(n, k).
//   - Every vertex has degree C(n - k, k).
//   - Size()  == Order() * C(n - k, k) / 2.
//   - Kneser's theorem (Lovász, 1978) gives χ(K(n, k)) == n - 2k + 2
//     when n ≥ 2k; for n < 2k the graph has no edges and χ = 1
//     (single colour suffices). The chromatic number is documented
//     in the godoc; the test does not exercise a solver per the
//     brief.
//   - K(5, 2) is the Petersen graph (semantic equivalence asserted
//     in TestSpecials_Petersen_KneserEquivalence).
//
// Kneser declares two knobs: "n" over [1, kneserMaxN] (default 5)
// and "k" over [0, kneserMaxK] (default 2). The constructor panics
// when n is outside [1, kneserMaxN], when k is outside [0, n], or
// when k is above kneserMaxK. The Name() reports
// "specials.kneser" (parameter-free, per the brief: the knobs carry
// n and k).
//
// Edges are inserted in ascending (u, v) lexicographic order of the
// (subset, subset) pair; combined with the lexicographic node-id
// assignment this fully pins the golden bytes.
func Kneser(n, k int) Shape[int, int64] {
	if n < 1 || n > kneserMaxN {
		panic(fmt.Sprintf("shapegen: Kneser requires 1 <= n <= %d, got n=%d", kneserMaxN, n))
	}
	if k < 0 || k > n {
		panic(fmt.Sprintf("shapegen: Kneser requires 0 <= k <= n, got n=%d k=%d", n, k))
	}
	if k > kneserMaxK {
		panic(fmt.Sprintf("shapegen: Kneser requires k <= %d, got k=%d", kneserMaxK, k))
	}
	return specialsBase{
		name: "specials.kneser",
		knobs: []Knob{
			{Name: "n", Min: 1, Max: kneserMaxN, Default: 5},
			{Name: "k", Min: 0, Max: kneserMaxK, Default: 2},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildKneser(g, n, k)
		},
	}
}

// buildKneser interns the C(n, k) k-subsets of [n] in lexicographic
// order, attaches their canonical "{i,j,...}" label to each, and
// inserts an edge between every disjoint pair. The first
// SetNodeLabel / AddEdge error short-circuits every loop.
//
// The combinatorial enumeration goes via [kSubsetsLex], which yields
// every k-subset in ascending lexicographic order with O(k) memory.
// Edge insertion is O(C(n, k)^2 * k) — quadratic in the vertex
// count with a small linear factor for the disjointness check —
// which is acceptable at the short-layer cap C(16, 8) = 12_870.
func buildKneser(g *lpg.Graph[int, int64], n, k int) error {
	subsets := kSubsetsLex(n, k)
	var err error
	// Phase 1: register every vertex with its canonical label.
	for i := 0; i < len(subsets) && err == nil; i++ {
		err = g.SetNodeLabel(canonicalNode(i), kneserSubsetLabel(subsets[i]))
	}
	// Phase 2: insert one edge per disjoint pair in ascending
	// (i, j) order with i < j.
	for i := 0; i < len(subsets) && err == nil; i++ {
		for j := i + 1; j < len(subsets) && err == nil; j++ {
			if disjointAscSubsets(subsets[i], subsets[j]) {
				err = g.AddEdge(canonicalNode(i), canonicalNode(j), unweightedSentinel)
			}
		}
	}
	return err
}

// kSubsetsLex returns every k-subset of {0, 1, ..., n-1} in
// ascending lexicographic order. Each subset is itself sorted
// ascending. For k == 0 the result is a single empty subset (the
// unique 0-subset); for k > n the result is empty.
//
// The enumeration uses the standard "co-lex / revolving door"
// iteration in plain form: start at {0, 1, ..., k-1} and repeatedly
// advance to the lexicographic successor by finding the rightmost
// position that can be incremented. The total cost is C(n, k)
// allocations of length-k slices, matching what the Build pass
// needs anyway for the disjointness scan.
func kSubsetsLex(n, k int) [][]int {
	if k == 0 {
		return [][]int{{}}
	}
	if k > n {
		return nil
	}
	cur := make([]int, k)
	for i := range cur {
		cur[i] = i
	}
	out := make([][]int, 0, binomial(n, k))
	for {
		snap := make([]int, k)
		copy(snap, cur)
		out = append(out, snap)
		// Find the rightmost position that can be incremented. The
		// position p can advance iff cur[p] < n - (k - p).
		p := k - 1
		for p >= 0 && cur[p] == n-(k-p) {
			p--
		}
		if p < 0 {
			break
		}
		cur[p]++
		for q := p + 1; q < k; q++ {
			cur[q] = cur[q-1] + 1
		}
	}
	return out
}

// binomial returns C(n, k) as a plain int. The Kneser bounds
// (n <= 16, k <= 8) keep the result well below 2^53.
func binomial(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	if k == 0 || k == n {
		return 1
	}
	if k > n-k {
		k = n - k
	}
	c := 1
	for i := 0; i < k; i++ {
		c = c * (n - i) / (i + 1)
	}
	return c
}

// disjointAscSubsets reports whether two ascending integer subsets
// share no element. The merge-style scan is O(len(a) + len(b)) and
// allocation-free.
func disjointAscSubsets(a, b []int) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			return false
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return true
}

// kneserSubsetLabel formats a single subset as "{i,j,...}" with the
// elements in ascending order. The empty subset is rendered as
// "{}". The output is stable across runs because [kSubsetsLex]
// keeps each subset sorted ascending.
func kneserSubsetLabel(subset []int) string {
	// Defensive copy + sort is unnecessary because kSubsetsLex
	// already yields ascending subsets; the sort guard exists only
	// to make the helper safe under any caller invariant. The
	// short-layer subsets have at most 8 elements so the overhead
	// is negligible.
	cp := make([]int, len(subset))
	copy(cp, subset)
	sort.Ints(cp)
	var b []byte
	b = append(b, '{')
	for i, v := range cp {
		if i > 0 {
			b = append(b, ',')
		}
		b = fmt.Appendf(b, "%d", v)
	}
	b = append(b, '}')
	return string(b)
}
