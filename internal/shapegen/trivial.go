package shapegen

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// This file implements the "trivial / degenerate" family of the
// shape catalogue. These are the smallest, most reduced graphs that
// every algorithm in search/ must accept as a no-op input, and that
// every persistence path in store/ must round-trip without surprise.
// They are also the first shapes the rapid-driven generators exercise,
// because corner-case failures most often live here.
//
// # Specialisation
//
// Every generator in this file produces a *lpg.Graph[int, int64].
// The (int, int64) pair was chosen to mirror the examples in
// graph/adjlist (int user keys, int64 weights). The brief allowed an
// alternative Shape[int, struct{}] specialisation for an "unweighted"
// SingleEdge variant; this file keeps the single specialisation to
// preserve a uniform registry and uses int64(0) as the canonical
// "no-weight" sentinel for the unweighted case. The decision is
// documented per constructor.
//
// # Configuration override policy
//
// Each generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity, but overrides
// cfg.Directed and cfg.Multigraph whenever the catalogue definition
// fixes them. For example, ParallelDigon must force Multigraph=true
// regardless of cfg.Multigraph; otherwise the catalogue invariant
// Size=k would silently degrade to Size=1 under simple-graph
// deduplication. Where a flag is overridden, the godoc records both
// the override and the rationale.
//
// # Error propagation
//
// The Build closures never wrap errors from g.AddNode / g.AddEdge.
// Both methods currently return only [adjlist.ErrShardFull] (and
// only when the caller has set cfg.MaxShardCapacity); the trivial
// generators have nothing useful to add to that signal. Errors are
// returned verbatim, alongside a nil graph, so callers can
// errors.Is them against [adjlist.ErrShardFull] without unwrapping.

// unweightedSentinel is the int64 weight assigned to edges in
// generators that the catalogue treats as "unweighted". The value 0
// is deliberate: it is the additive identity for the int64 weight
// monoid, so algorithms that sum weights (Dijkstra, Bellman-Ford)
// observe no contribution from these edges.
const unweightedSentinel int64 = 0

// weightedSentinel is the int64 weight assigned to edges in
// generators when the caller asks for a weighted variant. The value
// 1 is chosen as a small, positive, non-default constant so tests
// that scan for weight propagation can distinguish a weighted edge
// from an unweighted one without resorting to inequality with 0.
const weightedSentinel int64 = 1

// canonicalNode returns the canonical user-facing node value for the
// i-th node in a trivial-family graph. The convention is the integer
// i itself; this keeps node identity readable in golden files and
// matches the indexing used by every catalogue acceptance criterion
// in this family.
func canonicalNode(i int) int { return i }

// trivialBase carries the shared scaffolding for every generator in
// this file: a stable Name and the closure that performs the actual
// Build given a fully resolved [adjlist.Config]. Using a single
// concrete type keeps the registry single-specialisation and lets
// helpers like Knobs delegate to a per-generator slice.
type trivialBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s trivialBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s trivialBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
// The closure is responsible for any topology-specific cfg overrides.
func (s trivialBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// EmptyGraph returns a Shape that builds the empty graph E0: zero
// nodes, zero edges. The graph is directed. EmptyGraph declares no
// knobs.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 0
//   - Size()  == 0
//
// The graph respects cfg.MaxShardCapacity but always sets
// Directed=true: an empty directed graph is the canonical E0 across
// the catalogue, and any reverse-edge mirroring would be vacuous
// regardless.
func EmptyGraph() Shape[int, int64] {
	return trivialBase{
		name: "trivial.empty",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			return lpg.New[int, int64](cfg), nil
		},
	}
}

// SingleNode returns a Shape that builds K1: one node, zero edges.
// The canonical node value is 0. SingleNode declares no knobs.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 1
//   - Size()  == 0
//
// The underlying graph is directed; the lone node has no outgoing
// adjacency entry yet (it is interned only via the mapper).
//
// The g.AddNode call cannot fail in the current [adjlist] contract —
// AddNode does not touch the shard slot array, so cfg.MaxShardCapacity
// has no observable effect on it. SingleNode therefore returns the
// error verbatim, with no wrapping, in the interest of leaving zero
// dead branches in the build closure.
func SingleNode() Shape[int, int64] {
	return trivialBase{
		name: "trivial.k1",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			err := g.AddNode(canonicalNode(0))
			return g, err
		},
	}
}

// SingleEdge returns a Shape that builds K2 or — when selfLoop is
// true — the single self-loop graph. The three boolean flags select
// the variant:
//
//   - directed:  true → cfg.Directed=true; false → cfg.Directed=false.
//   - weighted:  true → edge weight is [weightedSentinel] (1);
//     false → edge weight is [unweightedSentinel] (0).
//   - selfLoop:  true → one node with one (0→0) edge; the directed
//     flag is ignored in this case because a single self-loop has the
//     same topology in directed and undirected graphs.
//
// Name is "trivial.k2" in the non-selfLoop case and
// "trivial.k1.selfloop" when selfLoop=true.
//
// Catalogue invariants on the returned graph:
//
//   - selfLoop=false: Order=2, Size=1, HasEdge(0,1)=true.
//     When directed=false, HasEdge(1,0)=true as well; when
//     directed=true, HasEdge(1,0)=false.
//   - selfLoop=true:  Order=1, Size=1, HasEdge(0,0)=true.
//
// SingleEdge declares no Knobs: every combination of (directed,
// weighted, selfLoop) is a discrete shape rather than a numeric
// sweep, so the property-based test enumerates the eight-cell
// constructor matrix directly. Multigraph is always set to false
// because K2 and the self-loop graph are simple by definition.
//
// The single g.AddEdge call inside Build can in principle return
// [adjlist.ErrShardFull] when the caller has set a tight
// cfg.MaxShardCapacity; that error is returned verbatim with no
// extra wrapping. With the canonical node ids 0 and 1 the error
// path is not reachable for any maxCap >= 1.
func SingleEdge(directed, weighted, selfLoop bool) Shape[int, int64] {
	w := unweightedSentinel
	if weighted {
		w = weightedSentinel
	}
	if selfLoop {
		return trivialBase{
			name: "trivial.k1.selfloop",
			build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
				// A self-loop is its own reverse, so directedness has
				// no observable effect on this topology. We pick
				// Directed=true to give a deterministic configuration
				// for the underlying adjlist; the catalogue invariant
				// (HasEdge(0,0)=true, Order=1, Size=1) holds either way.
				cfg.Directed = true
				cfg.Multigraph = false
				g := lpg.New[int, int64](cfg)
				err := g.AddEdge(canonicalNode(0), canonicalNode(0), w)
				return g, err
			},
		}
	}
	return trivialBase{
		name: "trivial.k2",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = directed
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			err := g.AddEdge(canonicalNode(0), canonicalNode(1), w)
			return g, err
		},
	}
}

// ParallelDigon returns a Shape that builds the multigraph M_k on
// two nodes: two nodes (0 and 1) joined by k parallel directed edges
// 0 → 1. Every edge carries the same weight [unweightedSentinel];
// algorithms that care about edge multiplicity rather than weight
// (path counts, multigraph traversals) get a clean fixture.
//
// k is the parameter set at construction time; the knob "k" exposes
// the bounded sweep [1, 1000] to property-based tests, with a
// default of 2 (the smallest k that distinguishes this shape from
// the simple K2). The constructor panics when k < 1 because the
// catalogue does not define ParallelDigon(0) — the zero-edge
// degenerate case is the responsibility of [EmptyGraph] or, on two
// nodes, of [IsolatedOnly](2).
//
// Catalogue invariants on the returned graph:
//
//   - Order() == 2
//   - Size()  == uint64(k)
//   - HasEdge(0,1) == true; HasEdge(1,0) == false.
//
// The build forces Directed=true and Multigraph=true regardless of
// cfg, because the catalogue definition fixes both. cfg.MaxShardCapacity
// is preserved so the caller can still bound shard growth.
//
// Errors from g.AddEdge are returned verbatim from the first failing
// iteration; the build aborts at that point and the partial graph
// is discarded by returning nil.
func ParallelDigon(k int) Shape[int, int64] {
	if k < 1 {
		panic(fmt.Sprintf("shapegen: ParallelDigon requires k >= 1, got %d", k))
	}
	return trivialBase{
		name: "trivial.parallel-digon",
		knobs: []Knob{
			{Name: "k", Min: 1, Max: 1000, Default: 2},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = true
			g := lpg.New[int, int64](cfg)
			return g, addParallelEdges(g, canonicalNode(0), canonicalNode(1), unweightedSentinel, k)
		},
	}
}

// addParallelEdges inserts cnt parallel edges from src to dst into
// g, returning the first error encountered (or nil). It exists so
// that the ParallelDigon build closure stays a single expression
// with no in-function branching to cover.
func addParallelEdges(g *lpg.Graph[int, int64], src, dst int, w int64, cnt int) error {
	var err error
	for i := 0; i < cnt && err == nil; i++ {
		err = g.AddEdge(src, dst, w)
	}
	return err
}

// IsolatedOnly returns a Shape that builds the graph on n isolated
// nodes: nodes 0..n-1 with no edges. When n is zero the result is
// observationally indistinguishable from [EmptyGraph]; the catalogue
// still treats this as a distinct shape because the construction
// path (explicit AddNode calls) exercises a different code path than
// the no-op build of EmptyGraph.
//
// n is the parameter set at construction time; the knob "n" exposes
// the bounded sweep [0, 1000] to property-based tests, with a
// default of 5 (a small but non-trivial count that any catalogue
// inspection can comfortably enumerate). The constructor panics
// when n < 0 because the catalogue does not define a graph with a
// negative number of nodes.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n)
//   - Size()  == 0
//   - HasEdge(i, j) == false for every pair (i, j).
//
// The build forces Directed=true (the catalogue's canonical
// orientation for this family) and Multigraph=false. cfg.MaxShardCapacity
// is preserved.
//
// AddNode cannot fail in the current adjlist contract (it only
// interns the user value with the Mapper, never touches shard
// slots). Errors are returned verbatim anyway, so the closure stays
// branch-free.
func IsolatedOnly(n int) Shape[int, int64] {
	if n < 0 {
		panic(fmt.Sprintf("shapegen: IsolatedOnly requires n >= 0, got %d", n))
	}
	return trivialBase{
		name: "trivial.isolated",
		knobs: []Knob{
			{Name: "n", Min: 0, Max: 1000, Default: 5},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, addNodesRange(g, n)
		},
	}
}

// addNodesRange interns nodes 0..n-1 in g, returning the first
// error from g.AddNode (or nil). Like [addParallelEdges] it exists
// to keep the build closure free of in-function branches.
func addNodesRange(g *lpg.Graph[int, int64], n int) error {
	var err error
	for i := 0; i < n && err == nil; i++ {
		err = g.AddNode(canonicalNode(i))
	}
	return err
}

// UniversalSelfLoops returns a Shape that builds the "universal
// self-loops" graph on n nodes: n nodes 0..n-1 with every node
// carrying exactly one self-loop. When weighted is true every loop
// carries weight [weightedSentinel] (1); otherwise every loop
// carries weight [unweightedSentinel] (0). When n is zero the build
// is a no-op and the result coincides with [EmptyGraph].
//
// n is the parameter set at construction time; the knob "n" exposes
// the bounded sweep [0, 1000] to property-based tests, with a
// default of 4. The constructor panics when n < 0; see
// [IsolatedOnly] for the rationale.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n)
//   - Size()  == uint64(n)
//   - HasEdge(v, v) == true for every v in 0..n-1.
//   - HasEdge(u, v) == false for every u != v.
//
// The build forces Directed=true and Multigraph=false; a self-loop
// is its own reverse so the directed flag has no observable effect,
// but fixing it removes a useless degree of freedom from the
// configuration matrix. cfg.MaxShardCapacity is preserved.
func UniversalSelfLoops(n int, weighted bool) Shape[int, int64] {
	if n < 0 {
		panic(fmt.Sprintf("shapegen: UniversalSelfLoops requires n >= 0, got %d", n))
	}
	w := unweightedSentinel
	if weighted {
		w = weightedSentinel
	}
	return trivialBase{
		name: "trivial.self-loop-universe",
		knobs: []Knob{
			{Name: "n", Min: 0, Max: 1000, Default: 4},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, addSelfLoops(g, n, w)
		},
	}
}

// addSelfLoops inserts a self-loop at every node 0..n-1 in g with
// the supplied weight, returning the first error (or nil). Same
// rationale as [addParallelEdges].
func addSelfLoops(g *lpg.Graph[int, int64], n int, w int64) error {
	var err error
	for i := 0; i < n && err == nil; i++ {
		err = g.AddEdge(canonicalNode(i), canonicalNode(i), w)
	}
	return err
}
