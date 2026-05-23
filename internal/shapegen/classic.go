package shapegen

import (
	"errors"
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// This file implements the "classic" family of the shape catalogue.
// These are the canonical skeletons every graph-library benchmark
// suite reaches for: paths P_n (worst case for BFS/DFS depth), cycles
// C_n, stars S_n and double-stars (hot-shard hubs), the complete
// graph K_n (quadratic dense workloads), the complete bipartite
// graph K_{m,n} (matching workloads), and the complete multipartite
// generalisation K_{n_1,...,n_k}.
//
// # Specialisation
//
// As with the trivial family, every constructor in this file produces
// a *lpg.Graph[int, int64]. The int user keys mirror node indices in
// the catalogue's invariants; int64 is the weight type used by every
// search algorithm in this module. All edges produced here carry
// [unweightedSentinel] (0) — these shapes are topology fixtures, not
// weight fixtures.
//
// # Configuration override policy
//
// Each generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, but
// overrides cfg.Directed and cfg.Multigraph whenever the catalogue
// definition fixes them. Multigraph is always set to false here:
// every classic skeleton is, by definition, a simple graph.
//
// # Golden format
//
// Goldens for this family live under
// internal/shapegen/testdata/shapegen/classic/ and use the same
// adjacency listing format as the trivial family (see
// formatAdjacency in trivial_test.go). For undirected shapes the
// listing therefore contains both (u,v) and (v,u) entries — the
// adjlist backend mirrors undirected edges internally and iterating
// Neighbours yields the mirror. The shape's Size() still counts each
// undirected edge once, matching the catalogue invariant.
//
// # Error propagation
//
// Cycle is the only constructor in this file that can refuse its
// input: an undirected cycle requires n >= 3 and a directed cycle
// requires n >= 1. Failure is surfaced from Build via the typed
// sentinel [ErrCycleTooSmall]; the constructor itself never panics
// on small n (in contrast with the trivial family's ParallelDigon),
// because the brief for this task fixes that contract explicitly.
// Other constructors that accept negative parameters panic at
// construction time, matching the trivial family's convention.
//
// # Edge ordering and determinism
//
// Each constructor inserts edges in a deterministic order so the
// goldens stay reproducible. The exact order is documented per
// generator; what matters is that the same (shape, knobs, cfg)
// triple always produces the same byte-for-byte golden.

// ErrCycleTooSmall is returned by Cycle.Build when n is below the
// minimum required by the catalogue: n >= 3 for undirected cycles and
// n >= 1 for directed cycles. Callers can errors.Is against this
// sentinel without unwrapping.
var ErrCycleTooSmall = errors.New("shapegen: cycle requires n >= 3 (undirected) or n >= 1 (directed)")

// classicBase is the shared scaffolding for every generator in this
// file. Its layout mirrors trivialBase so the helpers (Name, Knobs,
// Build) carry the exact same semantics.
type classicBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s classicBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s classicBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s classicBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// classicNodeKnob is the canonical knob for shapes parameterised by a
// single non-negative integer n. The upper bound matches the brief's
// "knobs sweep n up to 1e5 in short mode" clause; property-based
// tests are responsible for capping their own draws below that to
// keep the short layer fast.
func classicNodeKnob(defaultN int) Knob {
	return Knob{Name: "n", Min: 0, Max: 100_000, Default: defaultN}
}

// Path returns a Shape that builds the path graph P_n: nodes
// 0,1,...,n-1 with edges (i, i+1) for 0 <= i < n-1. When directed is
// true every edge points from i to i+1 only; when directed is false
// the underlying [adjlist.AdjList] mirrors each insertion.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n)
//   - Size()  == 0 when n <= 1; n-1 otherwise.
//   - Diameter is n-1 when n >= 1.
//   - Degree sequence (undirected, n >= 2): 1, 2, 2, ..., 2, 1.
//   - Out-degree sequence (directed): 1, 1, ..., 1, 0.
//
// Path declares a single knob "n" over [0, 100_000]; property-based
// tests should draw a smaller range to stay within the short layer
// budget. Multigraph is always set to false. Build returns
// adjlist.ErrShardFull verbatim when cfg.MaxShardCapacity refuses an
// AddEdge; in that case the partially built graph is discarded.
//
// The constructor panics when n < 0 because the catalogue does not
// define a path with a negative number of nodes.
func Path(n int, directed bool) Shape[int, int64] {
	if n < 0 {
		panic(fmt.Sprintf("shapegen: Path requires n >= 0, got %d", n))
	}
	return classicBase{
		name:  "classic.path",
		knobs: []Knob{classicNodeKnob(5)},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = directed
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildPath(g, n)
		},
	}
}

// buildPath interns nodes 0..n-1 in g and then inserts the n-1
// edges of P_n in ascending source order. The combined helper exists
// so the surrounding Build closure stays branch-free: the per-phase
// errors propagate through a single err variable rather than via an
// explicit if/return after each phase, which would leave dead
// branches uncovered while the [adjlist.AdjList] contract makes
// AddNode infallible.
func buildPath(g *lpg.Graph[int, int64], n int) error {
	err := addNodesRange(g, n)
	for i := 0; i < n-1 && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode(i+1), unweightedSentinel)
	}
	return err
}

// Cycle returns a Shape that builds the cycle graph C_n: nodes
// 0,1,...,n-1 with edges (i, (i+1) mod n) for 0 <= i < n. When
// directed is true every edge points from i to (i+1) mod n only;
// when directed is false the underlying [adjlist.AdjList] mirrors
// each insertion.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n)
//   - Size()  == uint64(n) when n is large enough to be valid.
//   - Diameter is floor(n / 2).
//   - Undirected: 2-regular for n >= 3.
//   - Directed: every node has in-degree 1 and out-degree 1.
//
// Cycle requires n >= 3 in the undirected case and n >= 1 in the
// directed case. When n is below the threshold Build returns
// [ErrCycleTooSmall] wrapped with the offending parameter so callers
// get a descriptive error while still being able to errors.Is the
// sentinel. The constructor itself never panics on small n; this
// matches the brief's explicit contract for Cycle.
//
// Special directed cases preserved by this implementation:
//
//   - Cycle(1, true) builds the single self-loop graph (one edge
//     0 -> 0). Multigraph is set to false because the catalogue
//     treats this as a simple graph with a single self-loop.
//   - Cycle(2, true) builds the directed digon (two anti-parallel
//     edges 0 -> 1 and 1 -> 0). Multigraph stays false because
//     these two edges are not parallel — they have distinct sources.
//
// The constructor panics when n < 0 because the catalogue does not
// define a cycle with a negative number of nodes.
func Cycle(n int, directed bool) Shape[int, int64] {
	if n < 0 {
		panic(fmt.Sprintf("shapegen: Cycle requires n >= 0, got %d", n))
	}
	return classicBase{
		name:  "classic.cycle",
		knobs: []Knob{classicNodeKnob(5)},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			if directed && n < 1 {
				return nil, fmt.Errorf("%w: directed cycle, got n=%d", ErrCycleTooSmall, n)
			}
			if !directed && n < 3 {
				return nil, fmt.Errorf("%w: undirected cycle, got n=%d", ErrCycleTooSmall, n)
			}
			cfg.Directed = directed
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, addCycleEdges(g, n)
		},
	}
}

// addCycleEdges inserts the n edges of the cycle C_n into g, in
// ascending source order: (0,1), (1,2), ..., (n-2, n-1), (n-1, 0).
// For n == 1 the loop body runs once and inserts (0, 0). Returning
// early on the first error keeps the build branch-free.
func addCycleEdges(g *lpg.Graph[int, int64], n int) error {
	var err error
	for i := 0; i < n && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode((i+1)%n), unweightedSentinel)
	}
	return err
}

// Star returns a Shape that builds the star graph S_n: a centre node
// (id 0) and n-1 leaves (ids 1..n-1). When outgoing is true every
// edge is 0 -> v; when outgoing is false every edge is v -> 0.
//
// The Star catalogue entry is always directed: the orientation of
// edges is the whole point of the shape, and the "outgoing" flag
// pins it. Multigraph is always set to false. cfg.Directed and
// cfg.Multigraph are overridden accordingly.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n)
//   - Size()  == 0 when n <= 1; n-1 otherwise.
//   - When outgoing == true: out-degree(0)=n-1, out-degree(v)=0 for v != 0.
//   - When outgoing == false: in-degree(0)=n-1, in-degree(v)=0 for v != 0.
//   - Diameter: 0 when n <= 1, 1 when n == 2, 2 when n >= 3 (treating
//     the graph as undirected for diameter purposes — the standard
//     catalogue definition).
//
// Star declares a single knob "n" over [0, 100_000]. The constructor
// panics when n < 0.
func Star(n int, outgoing bool) Shape[int, int64] {
	if n < 0 {
		panic(fmt.Sprintf("shapegen: Star requires n >= 0, got %d", n))
	}
	return classicBase{
		name:  "classic.star",
		knobs: []Knob{classicNodeKnob(5)},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildStar(g, n, outgoing)
		},
	}
}

// buildStar interns nodes 0..n-1 in g and inserts the n-1 spokes of
// the star S_n in ascending leaf order. Direction follows the
// outgoing flag. See buildPath for the rationale behind the combined
// helper.
func buildStar(g *lpg.Graph[int, int64], n int, outgoing bool) error {
	err := addNodesRange(g, n)
	for v := 1; v < n && err == nil; v++ {
		if outgoing {
			err = g.AddEdge(canonicalNode(0), canonicalNode(v), unweightedSentinel)
		} else {
			err = g.AddEdge(canonicalNode(v), canonicalNode(0), unweightedSentinel)
		}
	}
	return err
}

// DoubleStar returns a Shape that builds the double-star graph
// D_{k1,k2}: two centre nodes (ids 0 and 1) joined by a single edge,
// with k1 leaves attached to centre 0 (ids 2..k1+1) and k2 leaves
// attached to centre 1 (ids k1+2..k1+k2+1).
//
// DoubleStar is undirected: this is the conventional catalogue
// definition and yields the cleanest degree-sequence invariant. The
// build overrides cfg.Directed=false and cfg.Multigraph=false.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(2 + k1 + k2)
//   - Size()  == uint64(k1 + k2 + 1)
//   - Degree of centre 0 == k1 + 1.
//   - Degree of centre 1 == k2 + 1.
//   - Degree of every leaf == 1.
//
// DoubleStar declares two knobs "k1" and "k2" over [0, 50_000];
// property-based tests should cap their draws below this to stay
// within the short layer's time budget. The constructor panics when
// either k1 or k2 is negative.
func DoubleStar(k1, k2 int) Shape[int, int64] {
	if k1 < 0 || k2 < 0 {
		panic(fmt.Sprintf("shapegen: DoubleStar requires k1, k2 >= 0, got k1=%d k2=%d", k1, k2))
	}
	return classicBase{
		name: "classic.double-star",
		knobs: []Knob{
			{Name: "k1", Min: 0, Max: 50_000, Default: 3},
			{Name: "k2", Min: 0, Max: 50_000, Default: 3},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildDoubleStar(g, k1, k2)
		},
	}
}

// buildDoubleStar interns the 2+k1+k2 nodes of the double-star graph
// and then inserts its k1+k2+1 edges in this deterministic order:
//
//  1. the centre-to-centre edge (0, 1);
//  2. the k1 leaf edges (0, 2), (0, 3), ..., (0, k1+1);
//  3. the k2 leaf edges (1, k1+2), (1, k1+3), ..., (1, k1+k2+1).
//
// All three phases share a single err variable so the surrounding
// Build closure stays branch-free; see buildPath for the rationale.
func buildDoubleStar(g *lpg.Graph[int, int64], k1, k2 int) error {
	err := addNodesRange(g, 2+k1+k2)
	if err == nil {
		err = g.AddEdge(canonicalNode(0), canonicalNode(1), unweightedSentinel)
	}
	for i := 0; i < k1 && err == nil; i++ {
		err = g.AddEdge(canonicalNode(0), canonicalNode(2+i), unweightedSentinel)
	}
	for i := 0; i < k2 && err == nil; i++ {
		err = g.AddEdge(canonicalNode(1), canonicalNode(2+k1+i), unweightedSentinel)
	}
	return err
}

// Complete returns a Shape that builds the complete graph K_n on
// nodes 0..n-1. When directed is true every ordered pair (i, j) with
// i != j is inserted (the canonical "tournament on K_n"); when
// directed is false every unordered pair {i, j} with i < j is
// inserted exactly once and the [adjlist.AdjList] mirrors the entry.
// Self-loops are never inserted: the catalogue defines K_n as a
// simple graph.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n)
//   - Size()  == uint64(n) * uint64(n-1) when directed; n*(n-1)/2 otherwise.
//   - Diameter is 0 when n <= 1, 1 when n >= 2.
//   - Triangle count (undirected, n >= 3): n*(n-1)*(n-2)/6 == C(n, 3).
//
// Complete declares a single knob "n" over [0, 100_000]. Sweep
// callers must keep in mind that Size grows as n^2 and that
// n=100_000 produces ~10^10 edges; the property-based test caps n
// below this. The constructor panics when n < 0.
func Complete(n int, directed bool) Shape[int, int64] {
	if n < 0 {
		panic(fmt.Sprintf("shapegen: Complete requires n >= 0, got %d", n))
	}
	return classicBase{
		name:  "classic.complete",
		knobs: []Knob{classicNodeKnob(5)},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = directed
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildComplete(g, n, directed)
		},
	}
}

// buildComplete interns nodes 0..n-1 in g and then inserts the
// edges of K_n. The directed variant iterates every ordered pair
// (i, j) with i != j in row-major order; the undirected variant
// iterates every unordered pair {i, j} with i < j. The first error
// short-circuits both phases.
func buildComplete(g *lpg.Graph[int, int64], n int, directed bool) error {
	err := addNodesRange(g, n)
	if directed {
		for i := 0; i < n && err == nil; i++ {
			for j := 0; j < n && err == nil; j++ {
				if i == j {
					continue
				}
				err = g.AddEdge(canonicalNode(i), canonicalNode(j), unweightedSentinel)
			}
		}
		return err
	}
	for i := 0; i < n && err == nil; i++ {
		for j := i + 1; j < n && err == nil; j++ {
			err = g.AddEdge(canonicalNode(i), canonicalNode(j), unweightedSentinel)
		}
	}
	return err
}

// CompleteBipartite returns a Shape that builds the complete
// bipartite graph K_{m,n}: m left nodes (ids 0..m-1) and n right
// nodes (ids m..m+n-1) with every left node connected to every right
// node. The result is undirected by definition; cfg.Directed is
// overridden to false and cfg.Multigraph to false.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(m + n)
//   - Size()  == uint64(m) * uint64(n)
//   - Degree of every left node == n.
//   - Degree of every right node == m.
//   - Diameter is 0 when m+n <= 1, 1 when min(m,n) == 0 and m+n >= 2,
//     2 otherwise — note however that the brief does not pin
//     diameter for this shape, so the unit tests focus on Order,
//     Size, and the bipartite degree sequence.
//
// CompleteBipartite declares two knobs "m" and "n" over [0, 1000].
// The bound is chosen so the worst-case edge count m*n stays below
// 1_000_000 in the short layer; soak/nightly callers can drive the
// underlying constructor directly past this cap. The constructor
// panics when m < 0 or n < 0.
func CompleteBipartite(m, n int) Shape[int, int64] {
	if m < 0 || n < 0 {
		panic(fmt.Sprintf("shapegen: CompleteBipartite requires m, n >= 0, got m=%d n=%d", m, n))
	}
	return classicBase{
		name: "classic.complete-bipartite",
		knobs: []Knob{
			{Name: "m", Min: 0, Max: 1000, Default: 5},
			{Name: "n", Min: 0, Max: 1000, Default: 5},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildBipartite(g, m, n)
		},
	}
}

// buildBipartite interns the m+n nodes of K_{m,n} and then inserts
// every cross-side edge in row-major order: for each left index i
// in [0, m), for each right index j in [0, n), AddEdge(i, m+j). The
// first error short-circuits both phases.
func buildBipartite(g *lpg.Graph[int, int64], m, n int) error {
	err := addNodesRange(g, m+n)
	for i := 0; i < m && err == nil; i++ {
		for j := 0; j < n && err == nil; j++ {
			err = g.AddEdge(canonicalNode(i), canonicalNode(m+j), unweightedSentinel)
		}
	}
	return err
}

// Multipartite returns a Shape that builds the complete multipartite
// graph K_{parts[0], parts[1], ..., parts[k-1]}: a graph in which the
// node set is partitioned into k groups of sizes parts[0], parts[1],
// ..., parts[k-1] and every pair of nodes in distinct groups is
// joined by an edge (no intra-group edges).
//
// The graph is undirected; cfg.Directed and cfg.Multigraph are
// overridden to false. Nodes are assigned ids in contiguous blocks:
// group 0 takes ids 0..parts[0]-1, group 1 takes ids
// parts[0]..parts[0]+parts[1]-1, and so on. Empty groups (parts[i] ==
// 0) contribute zero nodes and zero edges; they are silently
// ignored. A nil or empty parts slice yields the empty graph.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(sum(parts))
//   - Size()  == sum_{i<j} parts[i] * parts[j].
//
// Multipartite declares no knobs: the parts slice is variadic and
// property-based tests draw it directly via rapid. The constructor
// panics when any parts[i] is negative.
func Multipartite(parts []int) Shape[int, int64] {
	for i, p := range parts {
		if p < 0 {
			panic(fmt.Sprintf("shapegen: Multipartite parts[%d] = %d, must be >= 0", i, p))
		}
	}
	// Copy parts so subsequent caller mutations cannot affect Build.
	owned := make([]int, len(parts))
	copy(owned, parts)
	return classicBase{
		name: "classic.multipartite",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildMultipartite(g, owned)
		},
	}
}

// buildMultipartite interns every node referenced by parts in
// ascending id order and then inserts every cross-group edge in a
// deterministic row-major order: the outer loops walk pairs of
// groups (i, j) with i < j; the inner loops walk node pairs within
// those groups in ascending id order. Returning early on the first
// error keeps the build branch-free.
func buildMultipartite(g *lpg.Graph[int, int64], parts []int) error {
	offsets := make([]int, len(parts))
	total := 0
	for i, p := range parts {
		offsets[i] = total
		total += p
	}
	err := addNodesRange(g, total)
	for i := 0; i < len(parts) && err == nil; i++ {
		for j := i + 1; j < len(parts) && err == nil; j++ {
			for a := 0; a < parts[i] && err == nil; a++ {
				for b := 0; b < parts[j] && err == nil; b++ {
					err = g.AddEdge(canonicalNode(offsets[i]+a), canonicalNode(offsets[j]+b), unweightedSentinel)
				}
			}
		}
	}
	return err
}
