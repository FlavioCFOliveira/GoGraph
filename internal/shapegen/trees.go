package shapegen

import (
	"fmt"
	"math/rand/v2"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// This file implements the "trees" family of the shape catalogue.
// Trees are the canonical fixtures for stressing DFS recursion-depth
// handling (iterative DFS over a path-degenerate tree), BFS level
// boundaries (full binary and complete k-ary trees), tree algorithms
// such as LCA and centroid (caterpillars, spiders, lobsters), and
// the degenerate cases that masquerade as paths (Path delegation).
//
// # Specialisation
//
// Every generator in this file produces a *lpg.Graph[int, int64],
// mirroring the trivial, classic and structured families. The (int,
// int64) pair is the project's canonical "small unsigned key, signed
// weight" specialisation; all edges here carry [unweightedSentinel]
// (0) because the catalogue defines tree shapes as topology fixtures
// rather than weight fixtures.
//
// # Configuration override policy
//
// Each generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// overrides cfg.Directed=true and cfg.Multigraph=false: every tree
// shape defined here is a directed simple graph in which every
// non-root node has exactly one incoming edge (from its parent). The
// directed orientation matches the trivial-family convention and
// keeps the undirected-skeleton diameter helper in classic_test.go
// applicable when callers need it.
//
// # Edge ordering and determinism
//
// Each constructor inserts edges in a deterministic order so the
// goldens stay reproducible. The PruferTree generator threads a
// caller-supplied [uint64] seed through [math/rand/v2.NewPCG] so
// every (n, seed) pair produces the same byte-for-byte golden across
// platforms.
//
// # Error propagation
//
// The Build closures use the same branch-free single-err-thread
// pattern as the classic and structured families: every per-phase
// error propagates through one err variable, and the surrounding
// closure returns (g, err). Every tree constructor validates its
// parameters at construction time and panics on negative or
// out-of-bounds knob values; the per-phase loops can only surface
// [adjlist.ErrShardFull] when the caller has set a tight
// cfg.MaxShardCapacity. That error is returned verbatim.
//
// # Lobster definition
//
// The Lobster shape is parameterised by a slice depths where
// depths[i] is the height of the chain attached to spine node i.
// The spine has len(depths) nodes; spine node i carries a single
// chain of length depths[i] rooted at it. depths[i]=0 means a bare
// spine node (no attachment); depths[i]=1 means one leaf;
// depths[i]=2 means leaf + leaf-of-leaf; etc. Closed form:
// N = len(depths) + sum(depths), E = N - 1. Example
// Lobster([1,2,1]): spine of 3, node 0 has one leaf, node 1 has one
// leaf and one grandleaf, node 2 has one leaf, so
// N = 3 + 4 = 7, E = 6.

// treesBase is the shared scaffolding for every generator in this
// file. Its layout mirrors classicBase and structuredBase so the
// helpers (Name, Knobs, Build) carry the exact same semantics.
type treesBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s treesBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s treesBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s treesBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// BalancedBinary returns a Shape that builds the perfect (a.k.a.
// balanced) binary tree of depth d: the rooted tree in which every
// node at depth < d has exactly two children, and every leaf lives
// at depth d. Nodes are numbered in BFS order starting from the root
// (id 0); the children of node i are 2i+1 and 2i+2.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(1 << (d + 1) - 1) — i.e., 2^(d+1) - 1 nodes.
//   - Size()  == Order() - 1 (every tree has N-1 edges).
//   - The graph has zero cycles (verified in the test suite via a
//     local DFS, not via the search.* package).
//
// BalancedBinary declares a single knob "depth" over [0, 20]. The
// short layer caps depth at 20 (2^21 - 1 ≈ 2.1M nodes); soak/nightly
// callers may drive the constructor up to depth 22 (~ 8.4M nodes).
// The constructor panics when depth is negative or above 20: the
// catalogue does not define a balanced binary tree outside that
// range.
//
// Edges are inserted in ascending child id order: for every node i
// in [1, N), the parent edge ((i-1)/2, i) is published. This
// enumeration mirrors the BFS numbering and lets the goldens stay
// reproducible across runs.
func BalancedBinary(depth int) Shape[int, int64] {
	if depth < 0 || depth > 20 {
		panic(fmt.Sprintf("shapegen: BalancedBinary requires 0 <= depth <= 20, got %d", depth))
	}
	return treesBase{
		name:  "trees.balanced-binary",
		knobs: []Knob{{Name: "depth", Min: 0, Max: 20, Default: 3}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildBalancedBinary(g, depth)
		},
	}
}

// buildBalancedBinary interns nodes 0..2^(d+1)-2 in g and inserts the
// N-1 parent edges in ascending child order. The first AddEdge error
// short-circuits the loop.
func buildBalancedBinary(g *lpg.Graph[int, int64], depth int) error {
	n := (1 << (depth + 1)) - 1
	err := addNodesRange(g, n)
	for i := 1; i < n && err == nil; i++ {
		parent := (i - 1) / 2
		err = g.AddEdge(canonicalNode(parent), canonicalNode(i), unweightedSentinel)
	}
	return err
}

// CompleteKAry returns a Shape that builds the perfect complete
// k-ary tree of depth d: the rooted tree in which every node at
// depth < d has exactly k children and every leaf lives at depth d.
// Nodes are numbered in BFS order starting from the root (id 0);
// node i (i >= 1) has parent (i-1)/k for k >= 1.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 1 when k == 0 (a complete 0-ary tree is just the root).
//   - Order() == uint64(d + 1) when k == 1 (a chain of d+1 nodes).
//   - Order() == uint64((k^(d+1) - 1) / (k - 1)) when k >= 2.
//   - Size()  == Order() - 1 (every tree has N-1 edges).
//
// CompleteKAry declares two knobs "k" over [1, 10] and "depth" over
// [0, 12]. The lower bound on k is 1 because the knob represents a
// sweep parameter for property-based tests; the special case k == 0
// is admitted by the constructor (Order = 1, Size = 0) but is not
// part of the sweep. The upper bounds bound k^(d+1) at 10^13 in the
// worst case; property-based callers must cap their own draws below
// this to stay within the short layer's budget.
//
// The constructor panics when k < 0, k > 10, depth < 0, or depth > 12.
//
// Edges are inserted in ascending child id order: for every node i
// in [1, N), the parent edge ((i-1)/k, i) is published. This
// enumeration mirrors the BFS numbering and lets the goldens stay
// reproducible across runs.
func CompleteKAry(k, depth int) Shape[int, int64] {
	if k < 0 || k > 10 {
		panic(fmt.Sprintf("shapegen: CompleteKAry requires 0 <= k <= 10, got %d", k))
	}
	if depth < 0 || depth > 12 {
		panic(fmt.Sprintf("shapegen: CompleteKAry requires 0 <= depth <= 12, got %d", depth))
	}
	return treesBase{
		name: "trees.complete-kary",
		knobs: []Knob{
			{Name: "k", Min: 1, Max: 10, Default: 3},
			{Name: "depth", Min: 0, Max: 12, Default: 2},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildCompleteKAry(g, k, depth)
		},
	}
}

// buildCompleteKAry interns the closed-form node count of the
// k-ary tree of depth d and inserts the N-1 parent edges in
// ascending child order. The k == 0 case produces just the root.
// The k == 1 case produces a chain of d+1 nodes. The k >= 2 case
// uses the canonical (k^(d+1) - 1) / (k - 1) closed form. The first
// AddEdge error short-circuits the loop.
func buildCompleteKAry(g *lpg.Graph[int, int64], k, depth int) error {
	n := completeKAryOrder(k, depth)
	err := addNodesRange(g, n)
	if k == 0 {
		return err
	}
	for i := 1; i < n && err == nil; i++ {
		parent := (i - 1) / k
		err = g.AddEdge(canonicalNode(parent), canonicalNode(i), unweightedSentinel)
	}
	return err
}

// completeKAryOrder returns the number of nodes in the perfect
// complete k-ary tree of depth d. The closed form depends on k:
//
//   - k == 0 → N = 1 (root only).
//   - k == 1 → N = d + 1 (chain).
//   - k >= 2 → N = (k^(d+1) - 1) / (k - 1).
//
// The computation is performed in plain int because the constructor
// bounds k <= 10 and depth <= 12 keep k^(d+1) well below 2^53.
func completeKAryOrder(k, depth int) int {
	switch k {
	case 0:
		return 1
	case 1:
		return depth + 1
	default:
		power := 1
		for i := 0; i <= depth; i++ {
			power *= k
		}
		return (power - 1) / (k - 1)
	}
}

// PruferTree returns a Shape that builds a uniformly random labelled
// tree on n nodes, decoded via Cayley's formula from a Prüfer
// sequence drawn from a deterministically seeded [math/rand/v2.PCG]
// source.
//
// The Prüfer encoding (Heinz Prüfer, 1918) is a bijection between
// labelled trees on n nodes and integer sequences of length n-2 with
// entries in [0, n-1]. Decoding a uniform sequence therefore yields
// a uniform tree by Cayley's formula, and supports the AC#2
// distribution test that 10_000 seeds for n == 10 produce
// uniformly-distributed edge histograms.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n).
//   - Size()  == 0 when n <= 1; uint64(n - 1) otherwise.
//
// PruferTree declares a single knob "n" over [2, 5000]. Soak callers
// may drive the constructor up to n == 50_000 directly. The
// constructor panics when n is negative or above 5000: the catalogue
// does not define a Prüfer tree outside that range.
//
// Edges are emitted in the order produced by the standard Prüfer
// decoding algorithm: every iteration pairs the smallest-indexed
// node with degree 1 with the head of the remaining sequence, and
// the final pair joins the last two remaining nodes. This is
// independent of the AddEdge insertion order from the perspective of
// the resulting graph, but it pins the golden bytes because the
// formatter sorts the output lexicographically.
func PruferTree(n int, seed uint64) Shape[int, int64] {
	if n < 0 || n > 5000 {
		panic(fmt.Sprintf("shapegen: PruferTree requires 0 <= n <= 5000, got %d", n))
	}
	return treesBase{
		name:  "trees.prufer",
		knobs: []Knob{{Name: "n", Min: 2, Max: 5000, Default: 10}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildPruferTree(g, n, seed)
		},
	}
}

// buildPruferTree interns nodes 0..n-1 and inserts the n-1 edges of
// the Prüfer-decoded tree, in the order produced by the decoding
// algorithm. For n < 2 the result has zero edges; for n == 2 the
// canonical single edge (0, 1) is inserted. For n >= 3 the standard
// O(n^2) "smallest leaf wins" decoding is used: at each step the
// smallest-indexed degree-1 node is paired with the head of the
// remaining sequence. The first AddEdge error short-circuits every
// stage; the per-stage helpers keep the cyclomatic complexity of
// the top-level Build closure within the project lint budget.
func buildPruferTree(g *lpg.Graph[int, int64], n int, seed uint64) error {
	err := addNodesRange(g, n)
	switch {
	case err != nil, n < 2:
		return err
	case n == 2:
		return g.AddEdge(canonicalNode(0), canonicalNode(1), unweightedSentinel)
	}
	seq := drawPruferSequence(n, seed)
	degree := pruferDegrees(n, seq)
	if decodeErr := addPruferDecodedEdges(g, seq, degree); decodeErr != nil {
		return decodeErr
	}
	return addFinalPruferEdge(g, degree)
}

// drawPruferSequence returns a length-(n-2) Prüfer sequence with
// entries in [0, n-1] drawn from a deterministically-seeded
// [math/rand/v2.PCG] source. The seed is used for both PCG
// initialisation values so the resulting sequence is fully
// determined by the caller-supplied uint64.
//
// Use of math/rand/v2 here is deliberate: the brief pins it as the
// PRNG for Prüfer encoding, with the determinism being a hard
// catalogue contract. crypto/rand would defeat the goldens.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see PruferTree godoc.
func drawPruferSequence(n int, seed uint64) []int {
	r := rand.New(rand.NewPCG(seed, seed))
	seq := make([]int, n-2)
	for i := range seq {
		seq[i] = r.IntN(n)
	}
	return seq
}

// pruferDegrees returns the degree vector implied by a Prüfer
// sequence on n nodes: every node starts at degree 1 and each
// occurrence in seq adds one to that node's degree. The closed
// form is degree[v] = 1 + (count of v in seq); the total degree
// sums to 2*(n-1), matching the tree's edge count.
func pruferDegrees(n int, seq []int) []int {
	degree := make([]int, n)
	for v := range degree {
		degree[v] = 1
	}
	for _, v := range seq {
		degree[v]++
	}
	return degree
}

// smallestLeaf advances from the supplied index to the smallest v
// in [from, n) with degree[v] == 1. The caller is responsible for
// ensuring at least one leaf remains; the function returns n when
// no leaf is found, which is treated as an algorithmic invariant
// violation by the call sites in [buildPruferTree].
func smallestLeaf(degree []int, from int) int {
	v := from
	for v < len(degree) && degree[v] != 1 {
		v++
	}
	return v
}

// addPruferDecodedEdges runs the inner loop of the Prüfer decoder:
// for every entry in seq the smallest-indexed degree-1 node is
// paired with the entry, edges are emitted in that order, and the
// degree vector is decremented to reflect the removed leaf. The
// err short-circuits the loop on the first AddEdge failure; the
// caller is responsible for closing the final edge between the
// two remaining leaves via [addFinalPruferEdge].
func addPruferDecodedEdges(g *lpg.Graph[int, int64], seq, degree []int) error {
	var err error
	leaf := smallestLeaf(degree, 0)
	for i := 0; i < len(seq) && err == nil; i++ {
		v := seq[i]
		err = g.AddEdge(canonicalNode(leaf), canonicalNode(v), unweightedSentinel)
		degree[leaf]--
		degree[v]--
		if degree[v] == 1 && v < leaf {
			leaf = v
			continue
		}
		leaf = smallestLeaf(degree, leaf+1)
	}
	return err
}

// addFinalPruferEdge closes the Prüfer-decoded tree by inserting
// the single edge between the two remaining degree-1 nodes after
// the decoding loop has consumed the entire sequence. A linear
// scan of degree recovers their indices; the helper is
// allocation-free.
func addFinalPruferEdge(g *lpg.Graph[int, int64], degree []int) error {
	a, b := -1, -1
	for v := 0; v < len(degree); v++ {
		if degree[v] != 1 {
			continue
		}
		if a == -1 {
			a = v
			continue
		}
		b = v
		break
	}
	return g.AddEdge(canonicalNode(a), canonicalNode(b), unweightedSentinel)
}

// PathDegenerate returns a Shape that builds the path-degenerate
// tree on n nodes — the canonical worst case for tree algorithms
// that assume balanced height (LCA without preprocessing, naive
// centroid decomposition, recursive DFS). It delegates to
// [Path](n, false), the undirected path P_n. The resulting Shape's
// Name() therefore reports "classic.path"; tests that need a
// catalogue-stable name for the tree-family entry must inspect the
// declared knobs or the Order/Size of the built graph.
//
// Catalogue invariants on the returned graph match those of
// [Path](n, false):
//
//   - Order() == uint64(n)
//   - Size()  == 0 when n <= 1; n - 1 otherwise.
//   - The graph is undirected.
//
// PathDegenerate exposes the same single knob "n" as Path, over
// [0, 100_000]. The constructor panics on n < 0 via the delegated
// Path guard.
//
// PathDegenerate exists so that the trees-family catalogue surfaces
// the path-degenerate worst case as a first-class entry. The
// delegation keeps a single implementation of the path topology in
// the package; the equivalence is verified by
// TestTrees_PathDegenerate_DelegatesToClassic.
func PathDegenerate(n int) Shape[int, int64] {
	return Path(n, false)
}

// Caterpillar returns a Shape that builds the caterpillar tree
// C(spine, leafDist): a spine path of length spine joined to
// leafDist[i] additional leaves at each spine node i. The "spine"
// is the path 0 -- 1 -- ... -- (spine-1); leaf nodes are appended
// after the spine, with node spine+offset being the j-th leaf of
// spine node i for the appropriate offset computed by the build.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(spine + sum(leafDist)).
//   - Size()  == Order() - 1 (every tree has N-1 edges) provided
//     Order() >= 1; equals 0 when spine == 0 and len(leafDist) == 0.
//
// Caterpillar declares a single knob "spine" over [1, 1000]. The
// leafDist slice is variadic and not exposed as a numeric knob; the
// constructor panics when spine < 1, when len(leafDist) != spine,
// or when any leafDist[i] is negative or above 50.
//
// Edges are inserted in this deterministic order:
//
//  1. the spine-1 path edges (0, 1), (1, 2), ..., (spine-2, spine-1);
//  2. for each spine node i in ascending order, the leafDist[i]
//     leaf edges (i, leafStart), (i, leafStart+1), ..., where
//     leafStart counts contiguously from spine across all spine
//     nodes.
//
// The first AddEdge error short-circuits every loop.
func Caterpillar(spine int, leafDist []int) Shape[int, int64] {
	if spine < 1 || spine > 1000 {
		panic(fmt.Sprintf("shapegen: Caterpillar requires 1 <= spine <= 1000, got %d", spine))
	}
	if len(leafDist) != spine {
		panic(fmt.Sprintf("shapegen: Caterpillar requires len(leafDist) == spine (%d), got %d", spine, len(leafDist)))
	}
	for i, l := range leafDist {
		if l < 0 || l > 50 {
			panic(fmt.Sprintf("shapegen: Caterpillar leafDist[%d] = %d, must be in [0, 50]", i, l))
		}
	}
	// Copy leafDist so subsequent caller mutations cannot affect
	// Build. This matches the Multipartite pattern in classic.go.
	owned := make([]int, len(leafDist))
	copy(owned, leafDist)
	return treesBase{
		name:  "trees.caterpillar",
		knobs: []Knob{{Name: "spine", Min: 1, Max: 1000, Default: 3}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildCaterpillar(g, spine, owned)
		},
	}
}

// buildCaterpillar interns the spine + sum(leafDist) nodes of the
// caterpillar and inserts its N-1 edges in the order documented at
// Caterpillar. The first AddEdge error short-circuits every loop.
func buildCaterpillar(g *lpg.Graph[int, int64], spine int, leafDist []int) error {
	total := spine
	for _, l := range leafDist {
		total += l
	}
	err := addNodesRange(g, total)
	// Spine path edges in ascending source order.
	for i := 0; i < spine-1 && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode(i+1), unweightedSentinel)
	}
	// Leaf edges: leafStart counts contiguously from `spine` across
	// every spine node's leaf block.
	leafStart := spine
	for i := 0; i < spine && err == nil; i++ {
		for j := 0; j < leafDist[i] && err == nil; j++ {
			err = g.AddEdge(canonicalNode(i), canonicalNode(leafStart+j), unweightedSentinel)
		}
		leafStart += leafDist[i]
	}
	return err
}

// Spider returns a Shape that builds the spider graph S(legs, legLen):
// a central node (id 0) joined to legs distinct paths each of
// length legLen. The j-th leg occupies node ids
// 1 + j*legLen .. (j+1)*legLen, with an edge from the centre to the
// first node of every leg and chain edges along each leg.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(1 + legs * legLen).
//   - Size()  == Order() - 1 (every tree has N-1 edges).
//   - The centre has degree legs; each leg-end leaf has degree 1;
//     every interior leg node has degree 2.
//
// Spider declares two knobs "legs" over [1, 100] and "legLen" over
// [1, 100]. The constructor panics when either is below 1 or above
// 100; the catalogue does not define a spider with zero or
// excessively long legs in the short layer.
//
// Edges are inserted in this deterministic order: for each leg j in
// ascending order, first the centre-to-first-node edge
// (0, 1 + j*legLen), then the chain edges
// (1 + j*legLen, 2 + j*legLen), ..., ((j+1)*legLen - 1, (j+1)*legLen).
// The first AddEdge error short-circuits every loop.
func Spider(legs, legLen int) Shape[int, int64] {
	if legs < 1 || legs > 100 {
		panic(fmt.Sprintf("shapegen: Spider requires 1 <= legs <= 100, got %d", legs))
	}
	if legLen < 1 || legLen > 100 {
		panic(fmt.Sprintf("shapegen: Spider requires 1 <= legLen <= 100, got %d", legLen))
	}
	return treesBase{
		name: "trees.spider",
		knobs: []Knob{
			{Name: "legs", Min: 1, Max: 100, Default: 3},
			{Name: "legLen", Min: 1, Max: 100, Default: 2},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildSpider(g, legs, legLen)
		},
	}
}

// buildSpider interns the 1 + legs*legLen nodes of S(legs, legLen)
// and inserts its N-1 edges in the order documented at Spider. The
// first AddEdge error short-circuits every loop.
func buildSpider(g *lpg.Graph[int, int64], legs, legLen int) error {
	err := addNodesRange(g, 1+legs*legLen)
	for j := 0; j < legs && err == nil; j++ {
		base := 1 + j*legLen
		err = g.AddEdge(canonicalNode(0), canonicalNode(base), unweightedSentinel)
		for i := 0; i < legLen-1 && err == nil; i++ {
			err = g.AddEdge(canonicalNode(base+i), canonicalNode(base+i+1), unweightedSentinel)
		}
	}
	return err
}

// Lobster returns a Shape that builds the lobster tree L(depths) per
// the disambiguated definition pinned in the package-level docstring:
// depths[i] is the height of the chain attached to spine node i. The
// spine has len(depths) nodes (ids 0..S-1); spine node i carries a
// single chain of length depths[i] rooted at it. depths[i]=0 means a
// bare spine node (no attachment); depths[i]=1 means one leaf;
// depths[i]=2 means leaf + leaf-of-leaf; etc.
//
// Closed form: N = len(depths) + sum(depths), E = N - 1.
//
// Example: Lobster([1,2,1]) builds a spine of 3, node 0 has one
// leaf, node 1 has one leaf and one grandleaf, node 2 has one leaf
// — N = 3 + 4 = 7, E = 6.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(len(depths) + sum(depths)).
//   - Size()  == Order() - 1 when Order() >= 1; 0 otherwise.
//
// Lobster declares no knobs: the depths slice is variadic and the
// length-of-slice / per-entry bounds are checked at construction
// time. The constructor panics when len(depths) is below 1 or above
// 50, or when any depths[i] is negative or above 5.
//
// Edges are inserted in this deterministic order:
//
//  1. the len(depths)-1 spine path edges (0, 1), (1, 2),
//     ..., (S-2, S-1);
//  2. for each spine node i in ascending order, the depths[i] chain
//     edges (i, chainStart), (chainStart, chainStart+1), ...,
//     (chainStart+depths[i]-2, chainStart+depths[i]-1), where
//     chainStart counts contiguously from len(depths) across all
//     spine nodes.
//
// The first AddEdge error short-circuits every loop.
func Lobster(depths []int) Shape[int, int64] {
	if len(depths) < 1 || len(depths) > 50 {
		panic(fmt.Sprintf("shapegen: Lobster requires 1 <= len(depths) <= 50, got %d", len(depths)))
	}
	for i, d := range depths {
		if d < 0 || d > 5 {
			panic(fmt.Sprintf("shapegen: Lobster depths[%d] = %d, must be in [0, 5]", i, d))
		}
	}
	// Copy depths so subsequent caller mutations cannot affect
	// Build. This matches the Multipartite pattern in classic.go.
	owned := make([]int, len(depths))
	copy(owned, depths)
	return treesBase{
		name: "trees.lobster",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildLobster(g, owned)
		},
	}
}

// buildLobster interns the len(depths) + sum(depths) nodes of the
// lobster and inserts its N-1 edges in the order documented at
// Lobster. The first AddEdge error short-circuits every loop.
func buildLobster(g *lpg.Graph[int, int64], depths []int) error {
	spine := len(depths)
	total := spine
	for _, d := range depths {
		total += d
	}
	err := addNodesRange(g, total)
	// Spine edges first, in ascending source order.
	for i := 0; i < spine-1 && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode(i+1), unweightedSentinel)
	}
	// Chain edges: for each spine node i, walk its single chain of
	// length depths[i] rooted at i. chainStart counts contiguously
	// from `spine` across all spine nodes.
	chainStart := spine
	for i := 0; i < spine && err == nil; i++ {
		d := depths[i]
		if d == 0 {
			continue
		}
		// The first chain link is (i, chainStart).
		err = g.AddEdge(canonicalNode(i), canonicalNode(chainStart), unweightedSentinel)
		// Subsequent chain links walk in ascending id order.
		for k := 0; k < d-1 && err == nil; k++ {
			err = g.AddEdge(canonicalNode(chainStart+k), canonicalNode(chainStart+k+1), unweightedSentinel)
		}
		chainStart += d
	}
	return err
}
