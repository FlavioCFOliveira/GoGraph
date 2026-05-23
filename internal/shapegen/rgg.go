package shapegen

import (
	"fmt"
	"math/rand/v2"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// This file implements the "random / random geometric graph (RGG)"
// generator of the shape catalogue. [RGG] realises the canonical
// Penrose / Gilbert geometric-random-graph model (Gilbert, "Random
// plane networks", J. SIAM 9(4), 1961; Penrose, "Random Geometric
// Graphs", Oxford University Press, 2003): n points are placed
// uniformly at random in the unit hyper-cube [0, 1]^dim, and an
// undirected edge is added between every pair of points within
// Euclidean distance r. The resulting graph is the catalogue's
// reference planar-like / road-network proxy and percolation
// testbed: it exercises A* with the Euclidean heuristic
// (admissible because the per-node coordinates are stored as
// properties on the graph), nearest-neighbour search, and any
// algorithm whose performance is sensitive to spatial locality.
//
// # Specialisation
//
// RGG produces *lpg.Graph[int, int64], mirroring every other
// generator in the random/, trivial/, classic/, structured/, trees/,
// specials/, and dags/ families. The (int, int64) pair is the
// project's canonical "small unsigned key, signed weight" choice;
// every edge here carries [unweightedSentinel] (0) because the
// catalogue treats RGG as a topology fixture rather than a weight
// fixture — the metric information lives in the per-node
// coordinates, not on the edges.
//
// # Configuration override policy
//
// The generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// forces cfg.Directed=false and cfg.Multigraph=false: the geometric
// model is an undirected simple graph (no parallel edges, no self-
// loops) by definition. The "no self-loop" invariant holds because
// the pair iteration ranges over i < j only; the "no parallel edge"
// invariant holds because each unordered pair is considered exactly
// once.
//
// # Edge ordering and determinism
//
// Edges are inserted in lexicographic (u, v) order with u < v so the
// goldens stay byte-for-byte reproducible across builds and across
// platforms. The seeded generator threads a caller-supplied [uint64]
// seed through [math/rand/v2.NewPCG]: every (n, radiusPercent, dim,
// seed) tuple yields the same byte-for-byte adjacency.
//
// The PRNG is consumed exactly dim times per node during the point-
// sampling phase (one Float64 draw per coordinate). The edge phase
// is a deterministic O(n^2) sweep that consults the point cloud only;
// no PRNG draws happen there. The seed-to-output map is therefore a
// pure function of the (n, radiusPercent, dim, seed) tuple.
//
// # Node properties
//
// Every node carries its sampled coordinates as [lpg.Float64Value]
// properties under the keys "x", "y", and (when dim == 3) "z". The
// properties are written after the node is interned and before the
// edge phase, so isolated nodes — produced when the radius is too
// small to reach any neighbour — still carry their coordinates.
// Consumers needing the Euclidean heuristic for A* read the
// coordinates back through [lpg.Graph.GetNodeProperty].
//
// # Error propagation
//
// The Build closure uses the same branch-free single-err-thread
// pattern as the other families: every per-phase error propagates
// through one err variable, and the surrounding closure returns
// (g, err). The per-phase loops can surface [adjlist.ErrShardFull]
// when the caller has set a tight cfg.MaxShardCapacity; that error
// is returned verbatim. Property writes go through
// [lpg.Graph.SetNodeProperty], which itself may surface the same
// shard-full error from the underlying [adjlist.AdjList.AddNode]
// guard; the helper threads that error through the same err
// variable.

// rggBase is the per-generator scaffolding for this file. Its layout
// mirrors barabasiAlbertBase / erdosRenyiBase / wattsStrogatzBase /
// rmatBase / configModelBase so the helpers (Name, Knobs, Build)
// carry the exact same semantics across families.
type rggBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s rggBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s rggBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s rggBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// RGG returns a Shape that builds a random geometric graph on n
// nodes in the unit hyper-cube [0, 1]^dim. Each node i is assigned a
// uniformly-distributed point p_i in [0, 1]^dim; an undirected edge
// (i, j) with i < j is added whenever the Euclidean distance
// ||p_i - p_j||_2 is at most radius := radiusPercent / 100.0
// (Gilbert, "Random plane networks", J. SIAM 9(4), 1961; Penrose,
// "Random Geometric Graphs", Oxford University Press, 2003). The
// graph is undirected and simple (no parallel edges, no self-loops);
// cfg.Directed and cfg.Multigraph are overridden to false.
//
// # Coordinate properties
//
// Every node carries its sampled coordinates as [lpg.Float64Value]
// properties: "x" and "y" for every dim, plus "z" when dim == 3.
// These properties are the source of truth for the Euclidean
// heuristic used by A* over RGG fixtures; they are also reproducible
// from (n, radiusPercent, dim, seed) — the point-sampling phase
// consumes exactly dim PRNG Float64 draws per node, in node order.
//
// # Parameters and validation
//
// The PRNG is a deterministically-seeded [math/rand/v2.PCG], so every
// (n, radiusPercent, dim, seed) tuple yields the same byte-for-byte
// adjacency. The constructor panics when:
//
//   - n < 0 or n > 1000 (catalogue out-of-range);
//   - radiusPercent < 0 or radiusPercent > 100 (catalogue out-of-
//     range);
//   - dim < 2 or dim > 3 (catalogue out-of-range).
//
// # Catalogue invariants on the returned graph
//
//   - Order() == uint64(n).
//   - The graph is undirected and simple.
//   - When radiusPercent == 0 the graph has zero edges: no pair of
//     distinct points lies within distance 0 (the sampler draws from
//     a continuous distribution; coincident points form a measure-
//     zero event and the catalogue accepts the implicit assumption).
//   - When radiusPercent reaches the unit-cube diameter
//     (sqrt(dim) * 100), every pair is within radius and the graph
//     is K_n. The knob ceiling is pinned at 100 by the brief, so the
//     d=2 case at radius=1.0 is *approximately* K_n: the expected
//     edge fraction is the probability that a uniform pair in the
//     unit square lies within distance 1, which integrates to
//     pi/3 - sqrt(3)/4 + 1 ~= 0.7965 of C(n, 2). The catalogue's
//     AC #1 therefore pins the contract at the empirically-bounded
//     floor 0.85 * C(n, 2) for n <= 20; see the dedicated test
//     [TestRandom_RGG_NearComplete_AtFullRadius] for the rationale
//     and the per-seed slack.
//   - Edge count is monotone non-decreasing in radiusPercent: a pair
//     within distance r is also within any r' >= r. The catalogue
//     test [TestRandom_RGG_EdgeCountMonotoneInRadius] pins the
//     contract over the grid {0, 25, 50, 75, 100} at fixed seed.
//   - Every node carries an "x" and "y" property; nodes with dim == 3
//     additionally carry a "z" property. All three values are
//     reproducible per (n, dim, seed) — the per-coordinate Float64
//     draw order is fixed at "x first for node 0, then y, then z,
//     then x for node 1, ...".
//
// RGG declares three knobs: "n" over [0, 1000] (default 50),
// "r" over [0, 100] (default 30), and "dim" over [2, 3] (default 2).
// The "seed" parameter is supplied at construction time as a uint64
// and is not exposed as a knob, mirroring the convention pinned by
// [Layered], [BarabasiAlbert], [WattsStrogatz], [ErdosRenyiNP],
// [RMAT], and [RandomRegular].
//
//nolint:gosec,gocritic // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; paramTypeCombine: signature is pinned by the brief (n int, radiusPercent int, dim int, seed uint64).
func RGG(n, radiusPercent, dim int, seed uint64) Shape[int, int64] {
	if n < 0 || n > 1000 {
		panic(fmt.Sprintf("shapegen: RGG requires 0 <= n <= 1000, got %d", n))
	}
	if radiusPercent < 0 || radiusPercent > 100 {
		panic(fmt.Sprintf("shapegen: RGG requires 0 <= radiusPercent <= 100, got %d", radiusPercent))
	}
	if dim < 2 || dim > 3 {
		panic(fmt.Sprintf("shapegen: RGG requires 2 <= dim <= 3, got %d", dim))
	}
	return rggBase{
		name: "random.rgg",
		knobs: []Knob{
			{Name: "n", Min: 0, Max: 1000, Default: 50},
			{Name: "r", Min: 0, Max: 100, Default: 30},
			{Name: "dim", Min: 2, Max: 3, Default: 2},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildRGG(g, n, radiusPercent, dim, seed)
		},
	}
}

// buildRGG interns nodes 0..n-1 in g, samples each node's
// coordinates as dim independent Uniform[0, 1) draws (in node order:
// x_0, y_0, (z_0,) x_1, y_1, ...), attaches them as [lpg.Float64Value]
// properties under the keys "x", "y", and (when dim == 3) "z", and
// then performs an O(n^2) pairwise distance sweep to insert every
// edge whose Euclidean distance is at most radius :=
// radiusPercent / 100.0. Edges are inserted in lexicographic (u, v)
// order with u < v so the goldens stay byte-for-byte reproducible.
// The first AddEdge or SetNodeProperty error short-circuits the loop,
// matching the branch-free err-thread convention shared with the
// other generators in this package.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see RGG godoc.
func buildRGG(g *lpg.Graph[int, int64], n, radiusPercent, dim int, seed uint64) error {
	err := addNodesRange(g, n)
	// coords[i] holds the dim-dimensional coordinate of node i, laid
	// out as a flat slice [x_0, y_0, (z_0,) x_1, y_1, ...]. The
	// allocation is performed before the err==nil guard so the
	// layout mirrors [buildWattsStrogatz] / [buildBarabasiAlbert]:
	// the per-phase loops below are gated on err == nil, so a
	// failure from addNodesRange short-circuits the work without an
	// explicit early return. The flat layout avoids one pointer
	// dereference per coordinate access in the inner distance loop.
	coords := make([]float64, n*dim)
	r := rand.New(rand.NewPCG(seed, ^seed))
	if err == nil {
		err = rggSamplePoints(g, coords, r, n, dim)
	}
	if err == nil {
		err = rggEmitEdges(g, coords, n, radiusPercent, dim)
	}
	return err
}

// rggSamplePoints draws dim Uniform[0, 1) coordinates for every node
// 0..n-1, in the canonical order (x_0, y_0, (z_0,) x_1, ...), and
// writes them both into the flat coords buffer and as
// [lpg.Float64Value] node properties under the keys "x", "y", and
// (when dim == 3) "z". The first SetNodeProperty error short-circuits
// the loop, matching the branch-free err-thread convention shared
// across the random family.
//
// PRNG consumption is exactly dim Float64 draws per node, in node
// order, so the seed-to-output map is a pure function of
// (n, dim, seed). [lpg.Graph.SetNodeProperty] surfaces only the
// error from the underlying [adjlist.AdjList.AddNode] guard, which
// in the current implementation never returns an error (intern is
// total); the err-thread is kept so the helper stays uniform with
// [buildBarabasiAlbert] / [buildWattsStrogatz] and survives a future
// SetNodeProperty failure mode without restructuring.
//
// Coordinate keys: the brief pins "x", "y", and (if dim == 3) "z".
// The keys are fixed strings, not parameterised, so the property
// names form a stable contract with downstream A* heuristics.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see RGG godoc.
func rggSamplePoints(g *lpg.Graph[int, int64], coords []float64, r *rand.Rand, n, dim int) error {
	var err error
	for i := 0; i < n && err == nil; i++ {
		base := i * dim
		for k := 0; k < dim; k++ {
			coords[base+k] = r.Float64()
		}
		err = g.SetNodeProperty(canonicalNode(i), "x", lpg.Float64Value(coords[base]))
		if err == nil {
			err = g.SetNodeProperty(canonicalNode(i), "y", lpg.Float64Value(coords[base+1]))
		}
		if err == nil && dim == 3 {
			err = g.SetNodeProperty(canonicalNode(i), "z", lpg.Float64Value(coords[base+2]))
		}
	}
	return err
}

// rggEmitEdges performs the O(n^2) pairwise distance sweep over the
// point cloud held in coords and inserts an undirected edge (i, j)
// with i < j into g whenever the Euclidean distance
// ||coords[i] - coords[j]||_2 is at most radius :=
// radiusPercent / 100.0. The first AddEdge error short-circuits the
// loop, matching the branch-free err-thread convention.
//
// Edges are inserted in lexicographic (u, v) order with u < v: the
// outer loop iterates i in ascending order, the inner loop iterates
// j = i+1..n-1 in ascending order. This pins the golden bytes
// regardless of any reordering inside the lpg backend.
//
// Distance test: the inner predicate compares the squared Euclidean
// distance against radius^2 to avoid a per-pair sqrt — semantically
// equivalent at the IEEE-754 precision used by the catalogue and
// roughly 2x faster than the sqrt variant on amd64. Floating-point
// rounding at the boundary distance == radius is implementation-
// defined; the catalogue accepts the rounding because the brief's
// AC #1 test budgets >= 0.85 * C(n, 2) at radius=1.0 (well below the
// IEEE-754 floor of measurable disagreement).
func rggEmitEdges(g *lpg.Graph[int, int64], coords []float64, n, radiusPercent, dim int) error {
	radius := float64(radiusPercent) / 100.0
	radiusSq := radius * radius
	var err error
	for i := 0; i < n && err == nil; i++ {
		ibase := i * dim
		for j := i + 1; j < n && err == nil; j++ {
			jbase := j * dim
			var distSq float64
			for k := 0; k < dim; k++ {
				diff := coords[ibase+k] - coords[jbase+k]
				distSq += diff * diff
			}
			if distSq <= radiusSq {
				err = g.AddEdge(canonicalNode(i), canonicalNode(j), unweightedSentinel)
			}
		}
	}
	return err
}
