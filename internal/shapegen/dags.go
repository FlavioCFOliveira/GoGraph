package shapegen

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// This file implements the "DAGs" family of the shape catalogue plus
// one cyclic flowgraph fixture used for dominator-tree verification.
// The DAG generators exercise the algorithms whose correctness
// depends on a topological ordering — topological sort itself,
// longest-path, dominator analysis, Bellman-Ford with negative edges
// but no negative cycles, and Johnson reweighting. The single cyclic
// fixture [LengauerTarjanExample] sits in this file because its
// purpose — pinning the canonical worked example of the Lengauer-
// Tarjan dominator algorithm — belongs to the same algorithmic
// family even though the graph itself is intentionally cyclic.
//
// # Specialisation
//
// Every generator in this file produces a *lpg.Graph[int, int64],
// mirroring the trivial, classic, structured, trees, and specials
// families. The (int, int64) pair is the project's canonical "small
// unsigned key, signed weight" specialisation. Most generators carry
// [unweightedSentinel] (0) on their edges because the catalogue
// defines them as topology fixtures; [NegativeWeightAcyclic] is the
// only exception and emits signed weights drawn from a
// deterministically-seeded [math/rand/v2.PCG] source.
//
// # Configuration override policy
//
// Each generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// forces cfg.Directed=true and cfg.Multigraph=false. Every shape in
// this family is a directed simple graph by definition; orientation
// is the load-bearing property that makes the topological-sort and
// dominator invariants meaningful.
//
// # Edge ordering and determinism
//
// Each constructor inserts edges in a deterministic order so the
// goldens stay byte-for-byte reproducible across builds and across
// platforms. The seeded generators (Layered, BuildDepDAG,
// NegativeWeightAcyclic) thread a caller-supplied [uint64] seed
// through [math/rand/v2.NewPCG] so every (knobs, seed) tuple yields
// the same byte-for-byte output.
//
// # Acyclicity contract
//
// Every generator in this file except [LengauerTarjanExample]
// produces an acyclic graph; the test suite verifies this with a
// local Kahn topological-sort check and a local Tarjan-style SCC
// check (asserting N singleton SCCs). The cyclic exception is
// documented explicitly on [LengauerTarjanExample] and is the only
// generator whose name appears in the test's skip list.
//
// # Error propagation
//
// The Build closures use the same branch-free single-err-thread
// pattern as the other families: every per-phase error propagates
// through one err variable, and the surrounding closure returns
// (g, err). Constructors validate their numeric parameters at
// construction time and panic on out-of-range input; the per-phase
// loops can only surface [adjlist.ErrShardFull] when the caller has
// set a tight cfg.MaxShardCapacity. That error is returned verbatim.

// dagsBase is the shared scaffolding for every generator in this
// file. Its layout mirrors trivialBase, classicBase, structuredBase,
// treesBase, and specialsBase so the helpers (Name, Knobs, Build)
// carry the exact same semantics across families.
type dagsBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s dagsBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s dagsBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s dagsBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// TransitiveTournament returns a Shape that builds the transitive
// tournament on n nodes: the unique acyclic orientation of the
// complete graph K_n in which every pair (i, j) with i < j is
// joined by the single directed edge i -> j. Equivalently, it is
// the comparability graph of the total order 0 < 1 < ... < n-1.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n).
//   - Size()  == uint64(n * (n - 1) / 2).
//   - The graph is acyclic (verified in the test suite via a local
//     Kahn topological sort, not via the search.* package).
//   - The unique topological order is the identity permutation
//     0, 1, ..., n-1.
//
// TransitiveTournament declares a single knob "n" over [0, 200]. The
// short layer caps n at 200 because the edge count grows as
// O(n^2) — n=200 gives 19_900 edges, comfortably inside the short
// budget. The constructor panics when n is negative or above 200:
// the catalogue does not define a transitive tournament outside
// that range.
//
// Edges are inserted in lexicographic order: (0,1), (0,2), ...,
// (0,n-1), (1,2), ..., (n-2,n-1). This is also the order in which
// the goldens enumerate them.
func TransitiveTournament(n int) Shape[int, int64] {
	if n < 0 || n > 200 {
		panic(fmt.Sprintf("shapegen: TransitiveTournament requires 0 <= n <= 200, got %d", n))
	}
	return dagsBase{
		name:  "dags.transitive-tournament",
		knobs: []Knob{{Name: "n", Min: 0, Max: 200, Default: 5}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildTransitiveTournament(g, n)
		},
	}
}

// buildTransitiveTournament interns nodes 0..n-1 in g and inserts
// every edge (i, j) with i < j in ascending (i, j) lexicographic
// order. The first AddEdge error short-circuits the loop.
func buildTransitiveTournament(g *lpg.Graph[int, int64], n int) error {
	err := addNodesRange(g, n)
	for i := 0; i < n && err == nil; i++ {
		for j := i + 1; j < n && err == nil; j++ {
			err = g.AddEdge(canonicalNode(i), canonicalNode(j), unweightedSentinel)
		}
	}
	return err
}

// Diamond returns a Shape that builds the diamond DAG of parameter
// k: a source node, a sink node, and two parallel chains of length
// k connecting them. Concretely, the graph has 2k + 2 nodes when
// k >= 1 (a source at id 0, a sink at id 2k+1, and two internal
// chains occupying ids 1..k and k+1..2k respectively) and is the
// degenerate single-edge graph 0 -> 1 when k == 0.
//
// For k >= 1 the two chains start at the source and converge on the
// sink:
//
//	chain A: 0 -> 1 -> 2 -> ... -> k -> 2k+1
//	chain B: 0 -> k+1 -> k+2 -> ... -> 2k -> 2k+1
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 2 when k == 0; uint64(2*k + 2) when k >= 1.
//   - Size()  == 1 when k == 0; uint64(2 * (k + 1)) when k >= 1.
//   - The graph is acyclic.
//   - For k >= 1, there are exactly two directed paths of length
//     k + 1 (edge count) from source 0 to sink 2k+1: one along
//     chain A and one along chain B. AC #3 phrases this as "exactly
//     2 paths of length k" using the "k internal chain links" count;
//     either phrasing identifies the same pair of paths.
//
// The k == 0 special case is documented explicitly: Diamond(0)
// degenerates to the single-edge graph 0 -> 1, with no internal
// chains. AC #3 admits this case verbatim ("for k >= 1") so the
// path-count assertion is skipped at k == 0.
//
// Diamond declares a single knob "k" over [0, 1000]. The constructor
// panics when k is negative or above 1000.
//
// Edges are inserted in this deterministic order:
//
//  1. when k == 0: the single edge (0, 1).
//  2. when k >= 1:
//     a. the k chain-A internal edges (0, 1), (1, 2), ..., (k-1, k);
//     b. the chain-A sink edge (k, 2k+1);
//     c. the k chain-B internal edges (0, k+1), (k+1, k+2), ...,
//     (2k-1, 2k);
//     d. the chain-B sink edge (2k, 2k+1).
//
// The first AddEdge error short-circuits every loop.
func Diamond(k int) Shape[int, int64] {
	if k < 0 || k > 1000 {
		panic(fmt.Sprintf("shapegen: Diamond requires 0 <= k <= 1000, got %d", k))
	}
	return dagsBase{
		name:  "dags.diamond",
		knobs: []Knob{{Name: "k", Min: 0, Max: 1000, Default: 3}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildDiamond(g, k)
		},
	}
}

// buildDiamond interns the diamond's nodes and inserts its edges in
// the order documented at [Diamond]. The function flattens the
// k==0 special case and the k>=1 chains into a single edge schedule,
// then iterates the schedule with the family's standard
// "err-threaded loop guard" idiom. Every AddEdge therefore runs
// inside one short-circuiting loop: the first AddEdge to surface
// adjlist.ErrShardFull stops the schedule and returns the error
// verbatim.
//
// The schedule is constructed deterministically so the goldens stay
// reproducible; see [Diamond] for the per-k edge insertion order.
func buildDiamond(g *lpg.Graph[int, int64], k int) error {
	if k == 0 {
		err := addNodesRange(g, 2)
		edges := [...][2]int{{0, 1}}
		for i := 0; i < len(edges) && err == nil; i++ {
			err = g.AddEdge(canonicalNode(edges[i][0]), canonicalNode(edges[i][1]), unweightedSentinel)
		}
		return err
	}
	total := 2*k + 2
	err := addNodesRange(g, total)
	sink := 2*k + 1
	edges := diamondEdgeSchedule(k, sink)
	for i := 0; i < len(edges) && err == nil; i++ {
		err = g.AddEdge(canonicalNode(edges[i][0]), canonicalNode(edges[i][1]), unweightedSentinel)
	}
	return err
}

// diamondEdgeSchedule returns the edge insertion schedule for
// Diamond(k) at k >= 1. The order is fixed by [Diamond]:
// chain A's internal edges + chain A's sink edge + chain B's first
// edge + chain B's internal edges + chain B's sink edge.
//
// The schedule is materialised into a single slice so [buildDiamond]
// can iterate it with the family's standard err-threaded loop, with
// no per-phase early-return branches left dead under the current
// adjlist contract.
func diamondEdgeSchedule(k, sink int) [][2]int {
	edges := make([][2]int, 0, 2*(k+1))
	// Chain A: 0 -> 1 -> 2 -> ... -> k -> sink.
	for i := 0; i < k; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	// Chain A's sink edge, then chain B's first edge — combined to
	// satisfy gocritic's appendCombine recommendation.
	edges = append(edges, [2]int{k, sink}, [2]int{0, k + 1})
	// Chain B: k+1 -> k+2 -> ... -> 2k -> sink.
	for i := k + 1; i < 2*k; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	edges = append(edges, [2]int{2 * k, sink})
	return edges
}

// Layered returns a Shape that builds a layered DAG of L layers,
// each holding w nodes, with edges from every node in layer i to
// every node in layer i+1 drawn independently from a Bernoulli
// distribution with success probability density/100. The PRNG is a
// deterministically-seeded [math/rand/v2.PCG] so every
// (L, w, density, seed) tuple produces the same byte-for-byte
// adjacency.
//
// Nodes are numbered in layer-major order: layer 0 holds ids
// 0..w-1, layer 1 holds ids w..2w-1, and so on. The pair (layer i,
// position j) maps to node id i*w + j.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(L * w).
//   - 0 <= Size() <= uint64((L - 1) * w * w).
//   - The graph is acyclic (edges only go forward through layers).
//   - When density == 0 the graph has no edges; when density == 100
//     every possible inter-layer edge is present and the graph has
//     exactly (L - 1) * w * w edges.
//
// Layered declares four knobs: "L" over [1, 100], "w" over [1, 100],
// and "density" over [0, 100] (interpreted as a percentage and
// divided by 100 inside Build). The "seed" parameter is supplied at
// construction time as a uint64 and is not exposed as a knob, mirroring
// the [PruferTree] convention.
//
// The constructor panics when L is below 1 or above 100, when w is
// below 1 or above 100, or when density is below 0 or above 100.
//
// Edges are inserted in this deterministic order: for every layer i
// from 0 to L-2, for every source position j from 0 to w-1, for
// every destination position k from 0 to w-1, the candidate edge
// (i*w + j, (i+1)*w + k) is drawn from the Bernoulli source. The
// PCG draw consumes one IntN(100) per candidate, regardless of the
// outcome, so the seed-to-output map is a pure function of the
// (L, w, density, seed) tuple.
//
// The L parameter is intentionally upper-case because the brief
// pins it as the catalogue identifier for the layer count and the
// godoc / golden filenames reuse it verbatim; a lint waiver covers
// this single deviation from the project's lowercase-parameter
// convention.
//
//nolint:gosec,gocritic // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; captLocal: L is pinned as a catalogue parameter name by the brief.
func Layered(L, w, density int, seed uint64) Shape[int, int64] {
	if L < 1 || L > 100 {
		panic(fmt.Sprintf("shapegen: Layered requires 1 <= L <= 100, got %d", L))
	}
	if w < 1 || w > 100 {
		panic(fmt.Sprintf("shapegen: Layered requires 1 <= w <= 100, got %d", w))
	}
	if density < 0 || density > 100 {
		panic(fmt.Sprintf("shapegen: Layered requires 0 <= density <= 100, got %d", density))
	}
	return dagsBase{
		name: "dags.layered",
		knobs: []Knob{
			{Name: "L", Min: 1, Max: 100, Default: 3},
			{Name: "w", Min: 1, Max: 100, Default: 3},
			{Name: "density", Min: 0, Max: 100, Default: 50},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildLayered(g, L, w, density, seed)
		},
	}
}

// buildLayered interns the L*w nodes of the layered DAG and inserts
// inter-layer edges drawn from a deterministically-seeded
// [math/rand/v2.PCG] source. Iteration order is layer-major, then
// source-position-major, then destination-position-major; the PRNG
// is consumed exactly once per (i, j, k) triple. The first AddEdge
// error short-circuits every loop.
//
//nolint:gosec,gocritic // G404: math/rand/v2 is the pinned PRNG; captLocal: L matches the public Layered API parameter name pinned by the brief.
func buildLayered(g *lpg.Graph[int, int64], L, w, density int, seed uint64) error {
	total := L * w
	err := addNodesRange(g, total)
	r := rand.New(rand.NewPCG(seed, seed))
	for i := 0; i < L-1 && err == nil; i++ {
		srcBase := i * w
		dstBase := (i + 1) * w
		for j := 0; j < w && err == nil; j++ {
			for k := 0; k < w && err == nil; k++ {
				// One PCG draw per candidate edge; comparison
				// against density realises the Bernoulli outcome.
				draw := r.IntN(100)
				if draw >= density {
					continue
				}
				err = g.AddEdge(canonicalNode(srcBase+j), canonicalNode(dstBase+k), unweightedSentinel)
			}
		}
	}
	return err
}

// lengauerTarjanEdges is the canonical edge list of the
// Lengauer-Tarjan worked example, in the exact insertion order
// pinned by the brief. Node user-keys are 1..13 with the literal
// labels R, A, B, C, D, E, F, G, H, I, J, K, L (in that order).
//
// The edge list is cyclic by construction (it features K -> R,
// H -> E inside the loop, J -> I and I -> K closing a cycle, etc.);
// this is the whole point of the fixture — to exercise dominator
// analysis on a non-trivial flowgraph rather than a DAG. The
// dominator-tree pinning lives in the test, computed from scratch
// against this fixture.
//
// Reference: Lengauer & Tarjan, "A Fast Algorithm for Finding
// Dominators in a Flowgraph", TOPLAS 1(1), 1979.
var lengauerTarjanEdges = [...][2]int{
	{1, 2},   // R -> A
	{1, 3},   // R -> B
	{1, 4},   // R -> C
	{2, 5},   // A -> D
	{3, 2},   // B -> A
	{3, 5},   // B -> D
	{3, 6},   // B -> E
	{4, 7},   // C -> F
	{4, 8},   // C -> G
	{5, 13},  // D -> L
	{6, 9},   // E -> H
	{7, 10},  // F -> I
	{8, 10},  // G -> I
	{8, 11},  // G -> J
	{9, 6},   // H -> E
	{9, 12},  // H -> K
	{10, 12}, // I -> K
	{11, 10}, // J -> I
	{12, 10}, // K -> I
	{12, 1},  // K -> R
	{13, 9},  // L -> H
}

// lengauerTarjanLabels carries the literature labels R, A..L for
// user-keys 1..13 in 1-based index order: lengauerTarjanLabels[0]
// is the label of user-key 1 (R), lengauerTarjanLabels[1] is the
// label of user-key 2 (A), and so on.
var lengauerTarjanLabels = [...]string{
	"R", "A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L",
}

// LengauerTarjanExample returns a Shape that builds the canonical
// flowgraph from Lengauer & Tarjan, "A Fast Algorithm for Finding
// Dominators in a Flowgraph", TOPLAS 1(1), 1979. The graph has 13
// nodes with user-keys 1..13 and literature labels R, A, B, C, D,
// E, F, G, H, I, J, K, L attached via SetNodeLabel. It carries 21
// directed edges in the insertion order pinned by
// [lengauerTarjanEdges].
//
// IMPORTANT: this generator is intentionally cyclic. It is included
// in the DAGs file because its purpose — dominator-tree verification
// — sits in the same algorithmic family, but it is the only entry
// in the family whose underlying graph is NOT a DAG. The
// dominator-tree pinning is encoded in the test
// TestDAGs_LengauerTarjanExample_DominatorTree, which computes
// immediate dominators from scratch via iterative dataflow
// (Cooper-Harvey-Kennedy) and compares against the table:
//
//	idom(A) = R, idom(B) = R, idom(C) = R, idom(D) = R,
//	idom(E) = R, idom(F) = C, idom(G) = C, idom(H) = R,
//	idom(I) = R, idom(J) = G, idom(K) = R, idom(L) = D.
//	(R has no idom.)
//
// LengauerTarjanExample declares no knobs (the graph is fully
// specified by [lengauerTarjanEdges]).
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 13.
//   - Size()  == 21.
//   - The graph is directed and cyclic (the user-amended AC #5
//     skip-list comment cites this).
func LengauerTarjanExample() Shape[int, int64] {
	return dagsBase{
		name: "dags.lengauer-tarjan",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildLengauerTarjan(g)
		},
	}
}

// buildLengauerTarjan interns nodes with user-keys 1..13, attaches
// the corresponding literature label via SetNodeLabel, and inserts
// every edge from [lengauerTarjanEdges] in declaration order. The
// first SetNodeLabel / AddEdge error short-circuits the loop.
func buildLengauerTarjan(g *lpg.Graph[int, int64]) error {
	var err error
	for i := 0; i < len(lengauerTarjanLabels) && err == nil; i++ {
		// User-key is 1-based: user-key (i+1) carries the label at
		// lengauerTarjanLabels[i].
		err = g.SetNodeLabel(i+1, lengauerTarjanLabels[i])
	}
	for i := 0; i < len(lengauerTarjanEdges) && err == nil; i++ {
		e := lengauerTarjanEdges[i]
		err = g.AddEdge(e[0], e[1], unweightedSentinel)
	}
	return err
}

// BuildDepDAG returns a Shape that builds a synthetic build-graph
// DAG of the kind that appears in dependency-graph analysis. The
// graph has a single root (id 0) at layer 0; each subsequent layer
// has up to fanOut * |previous layer| nodes; each child has up to
// fanIn parents drawn uniformly without replacement from the
// previous layer.
//
// The "up to" qualifiers reflect the deterministic randomised
// construction: at every step, the number of children spawned at
// layer i+1 is min(fanOut * |layer i|, maxLayerWidth), and every
// child draws min(fanIn, |layer i|) parents from layer i. The
// concrete layer widths therefore depend on (depth, fanIn, fanOut)
// rather than the seed, but the parent choices depend on the seed.
//
// Catalogue invariants on the returned graph:
//
//   - Order() >= 1 (always at least the root).
//   - The graph is acyclic — every edge points from a node at layer
//     i to a node at layer i+1.
//   - The graph is connected from the root (every node has at least
//     one parent in the previous layer, except the root).
//
// BuildDepDAG declares three knobs: "depth" over [0, 20], "fanIn"
// over [1, 10], and "fanOut" over [1, 10]. The "seed" parameter is
// supplied at construction time as a uint64 and is not exposed as a
// knob, mirroring the [PruferTree] convention. The constructor
// panics when depth is negative or above 20, when fanIn is below 1
// or above 10, or when fanOut is below 1 or above 10.
//
// The layer-width recurrence is bounded by buildDepDAGMaxWidth
// (1024) so a malicious (depth, fanOut) pair cannot cause an
// allocation explosion; once the recurrence hits the cap, every
// subsequent layer holds exactly buildDepDAGMaxWidth nodes.
//
// Edges are inserted in this deterministic order: for every layer
// i from 1 to depth, for every child position c in ascending order,
// for every drawn parent position p in ascending order, the edge
// (parent, child) is inserted. The PRNG is a deterministically-
// seeded [math/rand/v2.PCG] and is consumed once per parent draw.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see BuildDepDAG godoc.
func BuildDepDAG(depth, fanIn, fanOut int, seed uint64) Shape[int, int64] {
	if depth < 0 || depth > 20 {
		panic(fmt.Sprintf("shapegen: BuildDepDAG requires 0 <= depth <= 20, got %d", depth))
	}
	if fanIn < 1 || fanIn > 10 {
		panic(fmt.Sprintf("shapegen: BuildDepDAG requires 1 <= fanIn <= 10, got %d", fanIn))
	}
	if fanOut < 1 || fanOut > 10 {
		panic(fmt.Sprintf("shapegen: BuildDepDAG requires 1 <= fanOut <= 10, got %d", fanOut))
	}
	return dagsBase{
		name: "dags.build-dep",
		knobs: []Knob{
			{Name: "depth", Min: 0, Max: 20, Default: 3},
			{Name: "fanIn", Min: 1, Max: 10, Default: 2},
			{Name: "fanOut", Min: 1, Max: 10, Default: 2},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildDepDAG(g, depth, fanIn, fanOut, seed)
		},
	}
}

// buildDepDAGMaxWidth caps the per-layer width of a BuildDepDAG so
// the fanOut^depth recurrence cannot allocate without bound. At
// fanOut = 10 and depth = 20 the unbounded recurrence is 10^20
// nodes — far beyond any practical use. The cap ensures the
// per-layer width stabilises at 1024 nodes regardless of the
// (depth, fanOut) draw.
const buildDepDAGMaxWidth = 1024

// buildDepDAG interns the BuildDepDAG nodes and inserts edges in the
// order documented at [BuildDepDAG]. The PRNG is a deterministically-
// seeded [math/rand/v2.PCG] consumed once per parent draw.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see BuildDepDAG godoc.
func buildDepDAG(g *lpg.Graph[int, int64], depth, fanIn, fanOut int, seed uint64) error {
	// Layer 0 always has the single root. AddNode is infallible under
	// the current adjlist contract; the err thread carried below
	// nevertheless short-circuits every subsequent AddNode and
	// AddEdge so any future cap-checking addition surfaces verbatim.
	err := g.AddNode(canonicalNode(0))
	if depth == 0 || err != nil {
		return err
	}
	r := rand.New(rand.NewPCG(seed, seed))
	prevLayer := []int{0}
	nextID := 1
	for i := 1; i <= depth && err == nil; i++ {
		nextWidth := fanOut * len(prevLayer)
		if nextWidth > buildDepDAGMaxWidth {
			nextWidth = buildDepDAGMaxWidth
		}
		layer := make([]int, 0, nextWidth)
		// Materialise every child node first so the AddEdge calls
		// in the parent loop need only the integer key.
		for c := 0; c < nextWidth && err == nil; c++ {
			err = g.AddNode(canonicalNode(nextID))
			layer = append(layer, nextID)
			nextID++
		}
		// Draw min(fanIn, |prevLayer|) parents per child uniformly
		// without replacement from prevLayer. Edges are inserted
		// (parent, child) with parents in ascending position order
		// after the draw.
		parentsPerChild := fanIn
		if parentsPerChild > len(prevLayer) {
			parentsPerChild = len(prevLayer)
		}
		buf := make([]int, len(prevLayer))
		for c := 0; c < nextWidth && err == nil; c++ {
			copy(buf, prevLayer)
			// Fisher-Yates partial shuffle: first parentsPerChild
			// entries of buf become the chosen parents.
			for k := 0; k < parentsPerChild; k++ {
				j := k + r.IntN(len(buf)-k)
				buf[k], buf[j] = buf[j], buf[k]
			}
			chosen := make([]int, parentsPerChild)
			copy(chosen, buf[:parentsPerChild])
			// Insert in ascending parent order so the goldens
			// stay reproducible regardless of the draw permutation.
			sort.Ints(chosen)
			for k := 0; k < len(chosen) && err == nil; k++ {
				err = g.AddEdge(canonicalNode(chosen[k]), canonicalNode(layer[c]), unweightedSentinel)
			}
		}
		prevLayer = layer
	}
	return err
}

// NegativeWeightAcyclic returns a Shape that builds an acyclic
// directed graph carrying signed edge weights — specifically, the
// transitive-tournament skeleton on n nodes with a fraction signMix
// of edges given negative weights. The graph is acyclic by
// construction (edges only go forward in the topological order
// 0 < 1 < ... < n-1), so Bellman-Ford and Johnson reweighting run
// safely despite the negative weights.
//
// Edge weights are drawn uniformly from [-1000, 1000] excluding 0,
// with the sign drawn from a Bernoulli source with probability
// signMix/100 of being negative. The PRNG is a deterministically-
// seeded [math/rand/v2.PCG] so every (n, signMix, seed) tuple
// produces the same byte-for-byte output.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n).
//   - Size()  == uint64(n * (n - 1) / 2).
//   - The graph is acyclic.
//   - Every edge weight is in [-1000, -1] or [1, 1000].
//   - When signMix == 0 every weight is positive; when signMix ==
//     100 every weight is negative.
//
// NegativeWeightAcyclic declares two knobs: "n" over [0, 100] and
// "signMix" over [0, 100] (interpreted as a percentage and divided
// by 100 inside Build). The "seed" parameter is supplied at
// construction time as a uint64 and is not exposed as a knob,
// mirroring the [PruferTree] convention. The constructor panics
// when n is negative or above 100, or when signMix is below 0 or
// above 100.
//
// Edges are inserted in lexicographic (i, j) order with i < j; the
// PRNG is consumed twice per edge — once for the magnitude in
// [1, 1000], once for the sign Bernoulli draw — so the seed-to-
// output map is a pure function of the (n, signMix, seed) tuple.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see NegativeWeightAcyclic godoc.
func NegativeWeightAcyclic(n, signMix int, seed uint64) Shape[int, int64] {
	if n < 0 || n > 100 {
		panic(fmt.Sprintf("shapegen: NegativeWeightAcyclic requires 0 <= n <= 100, got %d", n))
	}
	if signMix < 0 || signMix > 100 {
		panic(fmt.Sprintf("shapegen: NegativeWeightAcyclic requires 0 <= signMix <= 100, got %d", signMix))
	}
	return dagsBase{
		name: "dags.negative-weight-acyclic",
		knobs: []Knob{
			{Name: "n", Min: 0, Max: 100, Default: 5},
			{Name: "signMix", Min: 0, Max: 100, Default: 50},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildNegativeWeightAcyclic(g, n, signMix, seed)
		},
	}
}

// buildNegativeWeightAcyclic interns nodes 0..n-1 in g and inserts
// the n*(n-1)/2 edges of the transitive-tournament skeleton with
// signed weights drawn from a deterministically-seeded
// [math/rand/v2.PCG] source. The first AddEdge error short-circuits
// the loop.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see NegativeWeightAcyclic godoc.
func buildNegativeWeightAcyclic(g *lpg.Graph[int, int64], n, signMix int, seed uint64) error {
	err := addNodesRange(g, n)
	r := rand.New(rand.NewPCG(seed, seed))
	for i := 0; i < n && err == nil; i++ {
		for j := i + 1; j < n && err == nil; j++ {
			// Magnitude in [1, 1000]; sign Bernoulli(signMix/100).
			mag := int64(r.IntN(1000) + 1)
			signDraw := r.IntN(100)
			w := mag
			if signDraw < signMix {
				w = -mag
			}
			err = g.AddEdge(canonicalNode(i), canonicalNode(j), w)
		}
	}
	return err
}
