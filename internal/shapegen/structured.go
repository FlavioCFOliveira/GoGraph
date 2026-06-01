package shapegen

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// This file implements the "structured 2D/3D" family of the shape
// catalogue. These are highly regular topologies whose order, size,
// diameter, and degree sequence admit a closed-form expression and so
// are the gold standard for verifying BFS expansion rate, A*
// heuristic correctness, Dijkstra grid-routing benchmarks, and the
// planar/non-planar split.
//
// # Specialisation
//
// Every generator in this file produces a *lpg.Graph[int, int64],
// mirroring the trivial and classic families. The (int, int64) pair
// is the project's canonical "small unsigned key, signed weight"
// specialisation; all edges here carry [unweightedSentinel] (0)
// because the catalogue defines these shapes as topology fixtures
// rather than weight fixtures.
//
// # Configuration override policy
//
// Each generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// overrides cfg.Directed=false and cfg.Multigraph=false: every
// structured shape defined here is an undirected simple graph.
//
// # Edge ordering and determinism
//
// Each constructor inserts edges in a deterministic order so the
// goldens stay reproducible. The exact order is documented per
// generator; what matters is that the same (shape, knobs, cfg)
// triple always produces the same byte-for-byte golden.
//
// # Error propagation
//
// The Build closures use the same branch-free single-err-thread
// pattern as the classic family: every per-phase error propagates
// through one err variable, and the surrounding closure returns
// (g, err). Because every structured shape is fully validated at
// construction time (negative or out-of-range knobs panic in the
// constructor), the per-phase loops never produce a "logical" error;
// they can only surface [adjlist.ErrShardFull] when the caller has
// set a tight cfg.MaxShardCapacity. That error is returned verbatim.

// structuredBase is the shared scaffolding for every generator in
// this file. Its layout mirrors classicBase so the helpers (Name,
// Knobs, Build) carry the exact same semantics.
type structuredBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s structuredBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s structuredBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s structuredBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// Hypercube returns a Shape that builds the d-dimensional hypercube
// graph Q_d: nodes are the integers 0..2^d-1 interpreted as bit
// strings, with an undirected edge between any two nodes whose
// labels differ in exactly one bit. The graph is undirected and
// simple; cfg.Directed and cfg.Multigraph are overridden to false.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(1 << d).
//   - Size()  == uint64(d) * uint64(1<<(d-1)) for d >= 1; 0 for d == 0.
//   - Every node has degree exactly d (d-regular).
//   - Diameter == d for d >= 1; 0 for d == 0.
//
// Hypercube declares a single knob "d" over [0, 24]. The upper bound
// reflects the brief's worst-case knob: d=24 yields 2^24 = 16_777_216
// nodes and 24 * 2^23 ≈ 2 * 10^8 edges, which is the nightly-layer
// ceiling. The short layer is responsible for capping its own draws
// (d ≤ 12 by convention; see the property-based test below). The
// constructor panics when d is negative or above 24 because the
// catalogue does not define Q_d outside that range.
//
// Edges are inserted in ascending (u, v) order: for every node u in
// [0, 2^d), and for every bit position i in [0, d), if u has bit i
// unset, the edge (u, u | (1<<i)) is inserted. This enumerates each
// undirected edge exactly once.
func Hypercube(d int) Shape[int, int64] {
	if d < 0 || d > 24 {
		panic(fmt.Sprintf("shapegen: Hypercube requires 0 <= d <= 24, got %d", d))
	}
	return structuredBase{
		name:  "structured.hypercube",
		knobs: []Knob{{Name: "d", Min: 0, Max: 24, Default: 3}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildHypercube(g, d)
		},
	}
}

// buildHypercube interns nodes 0..2^d-1 in g and inserts the
// d * 2^(d-1) edges of Q_d in ascending (u, v) order. The first
// AddEdge error short-circuits the loop.
func buildHypercube(g *lpg.Graph[int, int64], d int) error {
	n := 1 << d
	err := addNodesRange(g, n)
	for u := 0; u < n && err == nil; u++ {
		for i := 0; i < d && err == nil; i++ {
			mask := 1 << i
			if u&mask == 0 {
				err = g.AddEdge(canonicalNode(u), canonicalNode(u|mask), unweightedSentinel)
			}
		}
	}
	return err
}

// Grid returns a Shape that builds the m-by-n grid graph L_{m,n}.
// Nodes are arranged in row-major order: cell (r, c) at row r in
// [0, m) and column c in [0, n) has id r*n + c. When eightNeighbour
// is false the graph is the von Neumann (4-neighbour) lattice: each
// cell is connected only to its orthogonally adjacent cells. When
// eightNeighbour is true the graph is the Moore (8-neighbour)
// lattice: each cell is additionally connected to its four diagonal
// neighbours.
//
// The graph is undirected and simple; cfg.Directed and cfg.Multigraph
// are overridden to false. Empty rows/columns are allowed: when
// m == 0 or n == 0 the returned graph has zero nodes and zero edges.
// A 1x1 grid has one node and zero edges in either neighbourhood
// scheme.
//
// Catalogue invariants for the 4-neighbour variant:
//
//   - Order() == uint64(m * n)
//   - Size()  == uint64((m-1)*n + m*(n-1)) for m, n >= 1; 0 otherwise.
//   - Diameter == (m-1) + (n-1) for m, n >= 1.
//
// Catalogue invariants for the 8-neighbour variant (m, n >= 1):
//
//   - Order() == uint64(m * n)
//   - Size()  == uint64(4*m*n - 3*(m+n) + 2) for m, n >= 2; the
//     4-neighbour size for a degenerate 1xk or kx1 strip (no
//     diagonals possible).
//
// Grid declares two knobs "m" and "n" over [0, 1000]. The upper
// bound is chosen so the worst-case node count m*n stays at 10^6 in
// the short layer; soak/nightly callers can drive the constructor
// directly. The constructor panics when m < 0 or n < 0.
//
// Edges are inserted in ascending source order. Per cell (r, c) the
// 4-neighbour variant emits at most the right neighbour (r, c+1)
// and the down neighbour (r+1, c); the 8-neighbour variant
// additionally emits the down-right (r+1, c+1) and down-left
// (r+1, c-1) diagonals. This enumeration covers each undirected
// edge exactly once.
func Grid(m, n int, eightNeighbour bool) Shape[int, int64] {
	if m < 0 || n < 0 {
		panic(fmt.Sprintf("shapegen: Grid requires m, n >= 0, got m=%d n=%d", m, n))
	}
	return structuredBase{
		name: "structured.grid",
		knobs: []Knob{
			{Name: "m", Min: 0, Max: 1000, Default: 3},
			{Name: "n", Min: 0, Max: 1000, Default: 3},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildGrid(g, m, n, eightNeighbour)
		},
	}
}

// buildGrid interns the m*n nodes of L_{m,n} in row-major order and
// then inserts the orthogonal (and, when requested, diagonal) edges
// in ascending source order. The first AddEdge error short-circuits
// every loop.
func buildGrid(g *lpg.Graph[int, int64], m, n int, eightNeighbour bool) error {
	err := addNodesRange(g, m*n)
	for r := 0; r < m && err == nil; r++ {
		for c := 0; c < n && err == nil; c++ {
			u := r*n + c
			if c+1 < n {
				err = g.AddEdge(canonicalNode(u), canonicalNode(u+1), unweightedSentinel)
			}
			if err == nil && r+1 < m {
				err = g.AddEdge(canonicalNode(u), canonicalNode(u+n), unweightedSentinel)
			}
			if !eightNeighbour {
				continue
			}
			if err == nil && r+1 < m && c+1 < n {
				err = g.AddEdge(canonicalNode(u), canonicalNode(u+n+1), unweightedSentinel)
			}
			if err == nil && r+1 < m && c-1 >= 0 {
				err = g.AddEdge(canonicalNode(u), canonicalNode(u+n-1), unweightedSentinel)
			}
		}
	}
	return err
}

// Torus returns a Shape that builds the m-by-n torus graph T_{m,n}.
// It is the 4-neighbour grid with wrap-around: every cell (r, c) is
// joined to (r, (c+1) mod n), ((r+1) mod m, c), and so on. The graph
// is undirected and simple; cfg.Directed and cfg.Multigraph are
// overridden to false.
//
// Catalogue invariants on the returned graph (m, n >= 1):
//
//   - Order() == uint64(m * n).
//   - Size()  == 2 * uint64(m * n) when m, n >= 3 (the canonical
//     case: every node has degree exactly 4).
//   - For m == 2 (or n == 2) the row wrap-around coincides with the
//     non-wrap row neighbour, so Size collapses to m*n + the column
//     contribution. The unit tests pin the exact value via a
//     closed-form helper; property-based assertions cap m, n at 3
//     to keep the closed form clean.
//   - Diameter == floor(m/2) + floor(n/2) for m, n >= 1.
//
// Torus declares two knobs "m" and "n" over [1, 1000] (1 is the
// smallest valid dimension; m=1, n=1 collapses to a single node with
// no edges in the simple-graph regime because every neighbour
// coincides with the cell itself). The constructor panics when
// m < 1 or n < 1.
//
// Edges are inserted in ascending source order. Per cell (r, c) the
// constructor emits the right neighbour (r, (c+1) mod n) only when
// that target is not the cell itself (n >= 2); analogously for the
// down neighbour ((r+1) mod m, c). When m == 2 or n == 2 the wrap
// neighbour coincides with the non-wrap neighbour, so the simple
// graph collapses the duplicate to a single edge: HasEdge is
// idempotent and AddEdge on the second call is a no-op in the
// non-multigraph regime.
func Torus(m, n int) Shape[int, int64] {
	if m < 1 || n < 1 {
		panic(fmt.Sprintf("shapegen: Torus requires m, n >= 1, got m=%d n=%d", m, n))
	}
	return structuredBase{
		name: "structured.torus",
		knobs: []Knob{
			{Name: "m", Min: 1, Max: 1000, Default: 3},
			{Name: "n", Min: 1, Max: 1000, Default: 3},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildTorus(g, m, n)
		},
	}
}

// buildTorus interns the m*n nodes of T_{m,n} in row-major order
// and inserts the wrap-around edges in ascending source order.
// Self-edges (when m == 1 or n == 1) are filtered out so the result
// remains a simple graph. The first AddEdge error short-circuits
// every loop.
func buildTorus(g *lpg.Graph[int, int64], m, n int) error {
	err := addNodesRange(g, m*n)
	for r := 0; r < m && err == nil; r++ {
		for c := 0; c < n && err == nil; c++ {
			u := r*n + c
			right := r*n + (c+1)%n
			if u != right {
				err = g.AddEdge(canonicalNode(u), canonicalNode(right), unweightedSentinel)
			}
			if err == nil {
				down := ((r+1)%m)*n + c
				if u != down {
					err = g.AddEdge(canonicalNode(u), canonicalNode(down), unweightedSentinel)
				}
			}
		}
	}
	return err
}

// Rook returns a Shape that builds the n-by-n rook graph R_n: the
// n^2 squares of an n-by-n chessboard, with two squares connected
// when they share a row or a column (the set of squares a rook can
// reach in one move). Equivalently, R_n is the line graph of the
// complete bipartite graph K_{n,n}, or the Cartesian product
// K_n □ K_n.
//
// Nodes are arranged in row-major order: square (r, c) at row r in
// [0, n) and column c in [0, n) has id r*n + c. The graph is
// undirected and simple; cfg.Directed and cfg.Multigraph are
// overridden to false.
//
// Catalogue invariants on the returned graph (n >= 1):
//
//   - Order() == uint64(n^2).
//   - Size()  == uint64(n^2 * (n-1)) (each node has degree 2(n-1);
//     the n^2 nodes each contribute n-1 row neighbours and n-1
//     column neighbours, and we count each undirected edge once).
//   - Diameter == 2 for n >= 2; 0 for n == 1.
//
// Rook declares a single knob "n" over [1, 1000]. The constructor
// panics when n < 1: the catalogue does not define a zero-rook
// graph (it would coincide with the empty graph).
//
// Edges are inserted in ascending source order. Per square (r, c)
// the constructor emits the row neighbours (r, c') for c' > c and
// the column neighbours (r', c) for r' > r. This enumerates each
// undirected edge exactly once.
func Rook(n int) Shape[int, int64] {
	if n < 1 {
		panic(fmt.Sprintf("shapegen: Rook requires n >= 1, got %d", n))
	}
	return structuredBase{
		name:  "structured.rook",
		knobs: []Knob{{Name: "n", Min: 1, Max: 1000, Default: 3}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildRook(g, n)
		},
	}
}

// buildRook interns the n^2 nodes of R_n in row-major order and
// inserts the row and column edges in ascending source order. The
// first AddEdge error short-circuits every loop.
func buildRook(g *lpg.Graph[int, int64], n int) error {
	err := addNodesRange(g, n*n)
	for r := 0; r < n && err == nil; r++ {
		for c := 0; c < n && err == nil; c++ {
			u := r*n + c
			// Row edges to the right.
			for c2 := c + 1; c2 < n && err == nil; c2++ {
				err = g.AddEdge(canonicalNode(u), canonicalNode(r*n+c2), unweightedSentinel)
			}
			// Column edges going down.
			for r2 := r + 1; r2 < n && err == nil; r2++ {
				err = g.AddEdge(canonicalNode(u), canonicalNode(r2*n+c), unweightedSentinel)
			}
		}
	}
	return err
}

// Mobius returns a Shape that builds the Möbius ladder M_n: the
// cycle C_{2n} with n additional "rungs" connecting every pair of
// antipodal nodes (i, i+n) for i in [0, n). The graph is
// undirected and simple; cfg.Directed and cfg.Multigraph are
// overridden to false.
//
// Nodes are 0..2n-1. The cycle edges run (i, (i+1) mod 2n) for
// i in [0, 2n); the rung edges run (i, i+n) for i in [0, n).
//
// Catalogue invariants on the returned graph (n >= 2):
//
//   - Order() == uint64(2 * n).
//   - Size()  == uint64(3 * n) (2n cycle edges plus n rungs;
//     3-regular).
//   - M_n is non-planar for n >= 3 (M_3 is K_{3,3}).
//
// Mobius declares a single knob "n" over [2, 1000]. The lower bound
// is 2 because M_n requires at least 2 antipodal pairs to be
// distinct from a smaller cycle; the constructor panics when n < 2.
//
// Edges are inserted in ascending source order: first the 2n cycle
// edges (0,1), (1,2), ..., (2n-2, 2n-1), (2n-1, 0); then the n
// rung edges (0, n), (1, n+1), ..., (n-1, 2n-1).
func Mobius(n int) Shape[int, int64] {
	if n < 2 {
		panic(fmt.Sprintf("shapegen: Mobius requires n >= 2, got %d", n))
	}
	return structuredBase{
		name:  "structured.mobius",
		knobs: []Knob{{Name: "n", Min: 2, Max: 1000, Default: 3}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildMobius(g, n)
		},
	}
}

// buildMobius interns the 2n nodes of M_n and inserts its 3n edges:
// the 2n cycle edges first, then the n antipodal rungs. The first
// AddEdge error short-circuits every loop.
func buildMobius(g *lpg.Graph[int, int64], n int) error {
	total := 2 * n
	err := addNodesRange(g, total)
	for i := 0; i < total && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode((i+1)%total), unweightedSentinel)
	}
	for i := 0; i < n && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode(i+n), unweightedSentinel)
	}
	return err
}

// Ladder returns a Shape that builds the ladder graph L_n: two
// paths P_n joined rung-by-rung. The "left rail" is the path
// 0 -- 1 -- ... -- (n-1); the "right rail" is the path
// n -- (n+1) -- ... -- (2n-1); the rungs connect node i with node
// n+i for i in [0, n).
//
// The graph is undirected and simple; cfg.Directed and
// cfg.Multigraph are overridden to false.
//
// Catalogue invariants on the returned graph (n >= 1):
//
//   - Order() == uint64(2 * n).
//   - Size()  == uint64(3*n - 2) for n >= 1 (2*(n-1) rail edges
//     plus n rungs; collapses to 0 for n == 1 because there are
//     no rail edges but there is one rung).
//
// Note: for n == 1 the "two paths" each degenerate to a single
// node and the ladder coincides with K_2 (Order=2, Size=1).
//
// Ladder declares a single knob "n" over [1, 1000]. The
// constructor panics when n < 1.
//
// Edges are inserted in ascending source order: first the n-1
// left-rail edges (0,1), (1,2), ..., (n-2, n-1); then the n-1
// right-rail edges (n, n+1), ..., (2n-2, 2n-1); then the n rungs
// (0, n), (1, n+1), ..., (n-1, 2n-1).
func Ladder(n int) Shape[int, int64] {
	if n < 1 {
		panic(fmt.Sprintf("shapegen: Ladder requires n >= 1, got %d", n))
	}
	return structuredBase{
		name:  "structured.ladder",
		knobs: []Knob{{Name: "n", Min: 1, Max: 1000, Default: 3}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildLadder(g, n)
		},
	}
}

// buildLadder interns the 2n nodes of L_n and inserts its 3n-2
// edges in the order documented at Ladder. The first AddEdge error
// short-circuits every loop.
func buildLadder(g *lpg.Graph[int, int64], n int) error {
	err := addNodesRange(g, 2*n)
	for i := 0; i < n-1 && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode(i+1), unweightedSentinel)
	}
	for i := 0; i < n-1 && err == nil; i++ {
		err = g.AddEdge(canonicalNode(n+i), canonicalNode(n+i+1), unweightedSentinel)
	}
	for i := 0; i < n && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode(n+i), unweightedSentinel)
	}
	return err
}

// Prism returns a Shape that builds the prism graph Y_n: two
// cycles C_n joined rung-by-rung. The "inner" cycle is the cycle
// 0 -- 1 -- ... -- (n-1) -- 0; the "outer" cycle is the cycle
// n -- (n+1) -- ... -- (2n-1) -- n; the rungs connect node i with
// node n+i for i in [0, n).
//
// The graph is undirected and simple; cfg.Directed and
// cfg.Multigraph are overridden to false.
//
// Catalogue invariants on the returned graph (n >= 3):
//
//   - Order() == uint64(2 * n).
//   - Size()  == uint64(3 * n) (2n cycle edges plus n rungs;
//     3-regular).
//
// Prism declares a single knob "n" over [3, 1000]. The lower bound
// is 3 because C_n is only defined for n >= 3; the constructor
// panics when n < 3.
//
// Edges are inserted in ascending source order: first the n inner
// cycle edges (0,1), ..., (n-1, 0); then the n outer cycle edges
// (n, n+1), ..., (2n-1, n); then the n rungs (0, n), (1, n+1),
// ..., (n-1, 2n-1).
func Prism(n int) Shape[int, int64] {
	if n < 3 {
		panic(fmt.Sprintf("shapegen: Prism requires n >= 3, got %d", n))
	}
	return structuredBase{
		name:  "structured.prism",
		knobs: []Knob{{Name: "n", Min: 3, Max: 1000, Default: 3}},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildPrism(g, n)
		},
	}
}

// buildPrism interns the 2n nodes of Y_n and inserts its 3n edges
// in the order documented at Prism. The first AddEdge error
// short-circuits every loop.
func buildPrism(g *lpg.Graph[int, int64], n int) error {
	err := addNodesRange(g, 2*n)
	for i := 0; i < n && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode((i+1)%n), unweightedSentinel)
	}
	for i := 0; i < n && err == nil; i++ {
		err = g.AddEdge(canonicalNode(n+i), canonicalNode(n+(i+1)%n), unweightedSentinel)
	}
	for i := 0; i < n && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode(n+i), unweightedSentinel)
	}
	return err
}

// Theta returns a Shape that builds the theta graph θ_{a,b,c}: two
// "pole" vertices joined by three internally vertex-disjoint paths
// of lengths a, b, c (lengths measured in edges). The graph is
// undirected and simple; cfg.Directed and cfg.Multigraph are
// overridden to false.
//
// Node ids:
//
//   - 0       is pole s.
//   - 1       is pole t.
//   - 2..a    are the a-1 internal vertices of the first path
//     (a edges, so a-1 internal vertices between s and t).
//   - a+1..a+b-1 are the b-1 internal vertices of the second path.
//   - a+b..a+b+c-2 are the c-1 internal vertices of the third path.
//
// Total Order = 2 + (a-1) + (b-1) + (c-1) = a + b + c - 1.
// Total Size  = a + b + c (each path contributes its length in
// edges; the paths share only s and t, which contribute zero
// duplicates because the internal vertices are disjoint).
//
// Theta declares three knobs "a", "b", "c" over [1, 1000]. The
// lower bound is 1 because a length-1 path collapses to the single
// edge (s, t). When two or more of (a, b, c) are 1 the simple-graph
// regime collapses the duplicate (s, t) edges to one, which would
// violate the closed-form Size: the constructor therefore panics
// when more than one of (a, b, c) equals 1. The remaining
// degenerate case (exactly one length-1 path) is well-defined and
// produces a "lollipop with two arcs and one chord" graph.
//
// Edges are inserted path by path in ascending source order:
//
//  1. path A:  (0, 2), (2, 3), ..., (a, 1)        — a edges total.
//  2. path B:  (0, a+1), (a+1, a+2), ..., (a+b-1, 1)
//  3. path C:  (0, a+b), (a+b, a+b+1), ..., (a+b+c-2, 1)
//
// When a path has length 1 its only edge is (0, 1) directly.
func Theta(a, b, c int) Shape[int, int64] {
	if a < 1 || b < 1 || c < 1 {
		panic(fmt.Sprintf("shapegen: Theta requires a, b, c >= 1, got a=%d b=%d c=%d", a, b, c))
	}
	ones := 0
	if a == 1 {
		ones++
	}
	if b == 1 {
		ones++
	}
	if c == 1 {
		ones++
	}
	if ones > 1 {
		panic(fmt.Sprintf("shapegen: Theta requires at most one of a, b, c to equal 1, got a=%d b=%d c=%d", a, b, c))
	}
	return structuredBase{
		name: "structured.theta",
		knobs: []Knob{
			{Name: "a", Min: 1, Max: 1000, Default: 2},
			{Name: "b", Min: 1, Max: 1000, Default: 3},
			{Name: "c", Min: 1, Max: 1000, Default: 4},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildTheta(g, a, b, c)
		},
	}
}

// buildTheta interns the a+b+c-1 nodes of θ_{a,b,c} and inserts
// its a+b+c edges in the order documented at Theta. The first
// AddEdge error short-circuits every loop.
func buildTheta(g *lpg.Graph[int, int64], a, b, c int) error {
	order := a + b + c - 1
	err := addNodesRange(g, order)
	// Path A: a edges. Internal vertices live at ids 2..a.
	if err == nil {
		err = addThetaPath(g, 0, 1, 2, a)
	}
	// Path B: b edges. Internal vertices live at ids a+1..a+b-1.
	if err == nil {
		err = addThetaPath(g, 0, 1, a+1, b)
	}
	// Path C: c edges. Internal vertices live at ids a+b..a+b+c-2.
	if err == nil {
		err = addThetaPath(g, 0, 1, a+b, c)
	}
	return err
}

// addThetaPath inserts the edges of a single internally-disjoint
// s-t path of length pathLen. When pathLen == 1 the path is the
// direct edge (s, t); otherwise the path traverses pathLen-1
// internal vertices starting at internalStart and continuing in
// ascending order, with edges (s, internalStart),
// (internalStart, internalStart+1), ..., (internalStart+pathLen-2,
// t). The first AddEdge error short-circuits the loop.
func addThetaPath(g *lpg.Graph[int, int64], s, t, internalStart, pathLen int) error {
	if pathLen == 1 {
		return g.AddEdge(canonicalNode(s), canonicalNode(t), unweightedSentinel)
	}
	err := g.AddEdge(canonicalNode(s), canonicalNode(internalStart), unweightedSentinel)
	for i := 0; i < pathLen-2 && err == nil; i++ {
		err = g.AddEdge(canonicalNode(internalStart+i), canonicalNode(internalStart+i+1), unweightedSentinel)
	}
	if err == nil {
		err = g.AddEdge(canonicalNode(internalStart+pathLen-2), canonicalNode(t), unweightedSentinel)
	}
	return err
}
