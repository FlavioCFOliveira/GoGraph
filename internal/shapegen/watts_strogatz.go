package shapegen

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// This file implements the "random / Watts-Strogatz small-world"
// generator of the shape catalogue. [WattsStrogatz] realises the
// canonical small-world model (Watts & Strogatz, "Collective dynamics
// of 'small-world' networks", Nature 393(6684), 1998): a k-nearest-
// neighbour ring lattice in which each edge is independently rewired
// to a uniformly chosen non-self, non-duplicate target with probability
// beta. The resulting graph is the catalogue's reference small-world
// fixture: it gives the benchmark for clustering + low-diameter
// community algorithms (Leiden, label propagation) and for
// shortest-path expansion on rewired lattices.
//
// # Specialisation
//
// WattsStrogatz produces *lpg.Graph[int, int64], mirroring every other
// generator in the random/, trivial/, classic/, structured/, trees/,
// specials/, and dags/ families. The (int, int64) pair is the
// project's canonical "small unsigned key, signed weight" choice;
// every edge here carries [unweightedSentinel] (0) because the
// catalogue treats Watts-Strogatz as a topology fixture rather than a
// weight fixture.
//
// # Configuration override policy
//
// The generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// forces cfg.Directed=false and cfg.Multigraph=false: the
// Watts-Strogatz model is an undirected simple graph (no parallel
// edges, no self-loops) by definition.
//
// # Edge ordering and determinism
//
// Edges are inserted in a deterministic order so the goldens stay
// byte-for-byte reproducible across builds and across platforms. The
// seeded generator threads a caller-supplied [uint64] seed through
// [math/rand/v2.NewPCG]: every (n, k, betaPercent, seed) tuple yields
// the same byte-for-byte output.
//
// The rewire order is the canonical "left-shifted standard order"
// pinned by Watts & Strogatz (1998): the algorithm walks the ring
// lattice one "shift" at a time, where shift s in [1, k/2] selects
// the family of edges (i, (i + s) mod n) for i in [0, n). For each
// edge, a single PCG draw decides whether to rewire; if so, a second
// draw selects the new endpoint uniformly from the set of valid
// targets (any node other than i that is not already a neighbour of
// i in the current adjacency snapshot). The original edge is
// discarded and the new edge is inserted. After all k/2 shifts have
// been processed, the resulting adjacency is dumped into the lpg
// graph in lexicographic (u, v) order with u < v so the golden
// bytes stay stable regardless of the draw permutation.
//
// # Edge count invariance
//
// Because rewiring replaces an existing edge rather than adding or
// removing one, the total edge count is preserved exactly:
// Size() == n * k / 2 for any beta in [0, 100]. This is the
// catalogue's AC #1 and is checked by both the short-layer
// invariant test and the soak-layer uniformity test.
//
// # Error propagation
//
// The Build closure uses the same branch-free single-err-thread
// pattern as the other families: every per-phase error propagates
// through one err variable, and the surrounding closure returns
// (g, err). The per-phase loops can only surface
// [adjlist.ErrShardFull] when the caller has set a tight
// cfg.MaxShardCapacity; that error is returned verbatim.

// wattsStrogatzBase is the per-generator scaffolding for this file.
// Its layout mirrors barabasiAlbertBase, erdosRenyiBase, dagsBase,
// treesBase, etc. so the helpers (Name, Knobs, Build) carry the
// exact same semantics across families.
type wattsStrogatzBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s wattsStrogatzBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s wattsStrogatzBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s wattsStrogatzBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// WattsStrogatz returns a Shape that builds a Watts-Strogatz
// small-world graph on n nodes. The construction starts from a
// k-nearest-neighbour ring lattice — every node i is connected to
// k/2 left-neighbours and k/2 right-neighbours on the ring — and
// rewires each edge independently with probability betaPercent / 100
// (Watts & Strogatz, "Collective dynamics of 'small-world' networks",
// Nature 393(6684), 1998). The graph is undirected and simple (no
// parallel edges, no self-loops); cfg.Directed and cfg.Multigraph are
// overridden to false.
//
// The PRNG is a deterministically-seeded [math/rand/v2.PCG], so every
// (n, k, betaPercent, seed) tuple yields the same byte-for-byte
// adjacency.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n).
//   - Size()  == uint64(n * k / 2).
//     The rewiring preserves edge count exactly: each rewire replaces
//     one edge with another, never adds or removes one.
//   - The graph is undirected and simple.
//   - When betaPercent == 0 the graph equals the k-nearest-neighbour
//     ring lattice (byte-equal across runs at fixed seed).
//   - When betaPercent == 100 the rewiring distribution is
//     approximately uniform over the C(n, 2) candidate unordered
//     pairs — Watts-Strogatz at beta=1 is approximately G(n, p),
//     not strictly so: the ring-origin bias (every rewire starts
//     from a ring-lattice edge and the rejection-sampling step
//     forbids duplicates against the current neighbour set)
//     leaves a small but measurable excess on pairs of small ring
//     distance. The soak-layer chi-squared test
//     [TestRandom_WattsStrogatz_UniformAtBeta100ChiSquared] pins
//     the empirical guarantee at the alpha ~= 1e-100 tail so
//     catastrophic regressions are caught without falsely
//     rejecting the documented approximate-uniformity contract.
//   - The global clustering coefficient decreases monotonically as
//     betaPercent sweeps {0, 1, 10, 100}; the soak-layer test
//     [TestRandom_WattsStrogatz_ClusteringDecreasesMonotonically]
//     pins the catalogue contract.
//
// WattsStrogatz declares three knobs: "n" over [4, 10000] (default
// 20), "k" over [2, 50] (default 4), and "beta" over [0, 100]
// (default 10). The "seed" parameter is supplied at construction time
// as a uint64 and is not exposed as a knob, mirroring the convention
// pinned by [Layered] and [BarabasiAlbert]. The constructor panics
// when:
//
//   - n < 4 or n > 10000 (catalogue out-of-range);
//   - k < 2 or k > 50 (catalogue out-of-range);
//   - k is odd (the ring lattice requires k/2 left + k/2 right
//     neighbours, which forces k to be even);
//   - k >= n (every node would have to connect to itself or
//     duplicate a neighbour);
//   - betaPercent < 0 or betaPercent > 100.
//
// Rewire algorithm: the canonical Watts & Strogatz (1998) procedure.
// For each shift distance s in [1, k/2], for each node i in [0, n),
// consider the edge (i, (i + s) mod n) from the original ring
// lattice. Draw u from IntN(100): if u < betaPercent, replace the
// endpoint j = (i + s) mod n with a uniformly drawn t != i that is
// not already a neighbour of i in the current adjacency. Otherwise
// keep the original edge. The PRNG is consumed exactly once per edge
// for the rewire decision, plus zero or more times per rewire to
// resolve a valid target — the latter is by rejection sampling
// (draw a candidate from IntN(n), retry on self-loop or duplicate
// against the current per-node neighbour set).
//
// The rejection-sampling loop is bounded because at any point in the
// rewire the source node has at most k neighbours and n - 1 - k
// targets remain admissible. Under the constructor's k < n
// invariant the rejection probability per attempt is at most
// (k + 1) / n < 1; for the catalogue's (n, k) sweep this stays well
// below 0.1, so the expected number of attempts per accepted
// rewire is below 1.2. The catalogue does not pin a worst-case retry
// bound — the soak-layer uniformity test is the empirical guard
// that the sampling did not degenerate.
//
// After the rewire phase, the per-node neighbour sets are walked in
// ascending node order; for each source u, the neighbour ids v with
// v > u are emitted in ascending order so the final adjacency is
// inserted into the lpg graph in lexicographic (u, v) order with
// u < v. This pins the golden bytes regardless of the rewire draw
// permutation.
//
//nolint:gosec,gocritic // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; paramTypeCombine: signature is pinned by the brief (n int, k int, betaPercent int, seed uint64).
func WattsStrogatz(n, k, betaPercent int, seed uint64) Shape[int, int64] {
	if n < 4 || n > 10_000 {
		panic(fmt.Sprintf("shapegen: WattsStrogatz requires 4 <= n <= 10000, got %d", n))
	}
	if k < 2 || k > 50 {
		panic(fmt.Sprintf("shapegen: WattsStrogatz requires 2 <= k <= 50, got %d", k))
	}
	if k%2 != 0 {
		panic(fmt.Sprintf("shapegen: WattsStrogatz requires even k, got %d", k))
	}
	if k >= n {
		panic(fmt.Sprintf("shapegen: WattsStrogatz requires k < n, got k=%d n=%d", k, n))
	}
	if betaPercent < 0 || betaPercent > 100 {
		panic(fmt.Sprintf("shapegen: WattsStrogatz requires 0 <= betaPercent <= 100, got %d", betaPercent))
	}
	return wattsStrogatzBase{
		name: "random.watts-strogatz",
		knobs: []Knob{
			{Name: "n", Min: 4, Max: 10_000, Default: 20},
			{Name: "k", Min: 2, Max: 50, Default: 4},
			{Name: "beta", Min: 0, Max: 100, Default: 10},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildWattsStrogatz(g, n, k, betaPercent, seed)
		},
	}
}

// buildWattsStrogatz interns nodes 0..n-1 in g, constructs the
// k-nearest-neighbour ring lattice in an in-memory per-node neighbour
// set, rewires each edge with probability betaPercent / 100, then
// inserts the resulting adjacency into g in lexicographic (u, v)
// order. The first AddEdge error short-circuits the insertion loop.
//
// The neighbour-set representation is a slice of map[int]struct{}.
// At catalogue sizes (n <= 10_000, k <= 50) the per-node working set
// stays small (at most k entries on average after rewire); the map
// gives O(1) membership lookup during the rejection-sampling step
// without an asymptotic cost penalty over a sorted slice. The
// catalogue's short-layer ceiling (n=10000, k=4) needs 5*10_000 =
// 50_000 map entries at peak, well inside the per-package time and
// memory budget.
//
// The PRNG is consumed exactly once per edge for the rewire decision
// (yielding betaPercent / 100 acceptance), and zero or more times
// per accepted rewire to resolve a valid target via rejection
// sampling against IntN(n). The seed-to-output map is therefore a
// pure function of the (n, k, betaPercent, seed) tuple.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see WattsStrogatz godoc.
func buildWattsStrogatz(g *lpg.Graph[int, int64], n, k, betaPercent int, seed uint64) error {
	err := addNodesRange(g, n)
	// Per-node neighbour sets: neigh[v] is the set of current
	// undirected neighbours of v. Edges are stored on both endpoints
	// because the underlying ring lattice is undirected; the rewire
	// step maintains the invariant by adding/removing the new/old
	// target on both endpoints.
	//
	// The allocation is performed before the err==nil guard so the
	// layout mirrors [buildBarabasiAlbert]: the per-phase loops below
	// are gated on err == nil, so a failure from addNodesRange short-
	// circuits the work without an explicit early return. The
	// in-memory neighbour set is purely a per-build working buffer;
	// it is discarded when the function returns, so allocating it on
	// an error path costs only a constant amount of garbage.
	neigh := make([]map[int]struct{}, n)
	for i := 0; i < n; i++ {
		neigh[i] = make(map[int]struct{}, k)
	}
	// Phase 1: build the k-nearest-neighbour ring lattice. For each
	// shift s in [1, k/2], connect every node i to (i + s) mod n.
	// Each undirected edge is added once and recorded on both
	// endpoints; the result is a 2-regular graph at s=1, 4-regular
	// at s=2 (cumulatively), ..., k-regular at s=k/2.
	half := k / 2
	if err == nil {
		for s := 1; s <= half; s++ {
			for i := 0; i < n; i++ {
				j := (i + s) % n
				neigh[i][j] = struct{}{}
				neigh[j][i] = struct{}{}
			}
		}
	}
	// Phase 2: rewire. Walk the original ring-lattice edges in the
	// same (shift, source) order as Phase 1. The per-edge work is
	// delegated to wattsStrogatzRewireStep so the cyclomatic
	// complexity of buildWattsStrogatz stays inside the project's
	// gocyclo budget; the helper also serves as the unit of reuse
	// when adding alternative rewire policies (none planned, but the
	// seam is here for the same reason barabasiAlbertStep is split
	// out from buildBarabasiAlbert).
	r := rand.New(rand.NewPCG(seed, seed))
	if err == nil {
		for s := 1; s <= half; s++ {
			for i := 0; i < n; i++ {
				wattsStrogatzRewireStep(r, neigh, i, s, n, betaPercent)
			}
		}
	}
	// Phase 3: insert the rewired adjacency into g in lexicographic
	// (u, v) order with u < v. The neighbour sets are unordered (map
	// iteration order is not stable in Go), so we sort the v ids for
	// each source u before insertion. The pin on insertion order is
	// what keeps the golden bytes stable.
	for u := 0; u < n && err == nil; u++ {
		targets := make([]int, 0, len(neigh[u]))
		for v := range neigh[u] {
			if v > u {
				targets = append(targets, v)
			}
		}
		sort.Ints(targets)
		for idx := 0; idx < len(targets) && err == nil; idx++ {
			err = g.AddEdge(canonicalNode(u), canonicalNode(targets[idx]), unweightedSentinel)
		}
	}
	return err
}

// pickRewireTarget draws a uniform random integer in [0, n) by
// rejection sampling until it lands on a node t that is neither the
// source i nor already in srcNeigh. It returns t on success, or -1
// when the rejection set is saturated (every other node is already a
// neighbour). The latter is reachable only when len(srcNeigh) ==
// n - 1, a degenerate case discussed in [buildWattsStrogatz].
//
// The rejection probability per attempt is
// (len(srcNeigh) + 1) / n, which under the catalogue's k < n
// invariant stays strictly below 1 — the loop is therefore
// guaranteed to terminate. The expected number of attempts per
// accepted draw is n / (n - 1 - len(srcNeigh)); for the catalogue's
// (n, k) sweep with len(srcNeigh) <= k this stays below 1.2 even at
// the tightest configurations, so the rejection-sampling cost is
// effectively constant.
//
// PRNG consumption is exactly one IntN(n) per attempt — including
// rejected duplicates — so the seed-to-output map stays stable under
// any change to the neighbour-set representation.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see WattsStrogatz godoc.
func pickRewireTarget(r *rand.Rand, srcNeigh map[int]struct{}, i, n int) int {
	if len(srcNeigh)+1 >= n {
		return -1
	}
	for {
		t := r.IntN(n)
		if t == i {
			continue
		}
		if _, dup := srcNeigh[t]; dup {
			continue
		}
		return t
	}
}

// wattsStrogatzRewireStep evaluates the rewire decision for the
// canonical ring-lattice edge (i, (i + s) mod n) on the in-memory
// adjacency snapshot held in neigh. It performs exactly one
// PRNG IntN(100) draw — regardless of whether the edge is rewired —
// so the seed-to-output map of [buildWattsStrogatz] stays a pure
// function of (n, k, betaPercent, seed). When the draw selects the
// rewire branch and [pickRewireTarget] returns a valid target t, the
// helper removes the (i, j) edge from neigh[i] and neigh[j] and
// inserts (i, t) on neigh[i] and neigh[t]; otherwise the original
// edge survives unchanged.
//
// The helper carries no error return because every operation on the
// in-memory neighbour set is total: deletions of present keys are
// idempotent, insertions on map[int]struct{} cannot fail, and the
// rejection-sampling inside pickRewireTarget is bounded by the
// constructor's k < n invariant. Errors from g.AddEdge are surfaced
// by Phase 3 of [buildWattsStrogatz], not here.
//
// At the moment this helper is invoked for shift s and source i, the
// original ring-lattice neighbour j = (i + s) mod n is guaranteed to
// still be in neigh[i]. Proof: only the rewire step removes
// neighbours, and it removes only the j visited by that very
// iteration. Rewires from other (s', i') iterations add only
// non-neighbour targets, which by construction cannot collide with
// the about-to-be-visited j of this iteration. The
// `delete(neigh[i], j)` and `delete(neigh[j], i)` calls below are
// therefore always effective when the rewire branch fires.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see WattsStrogatz godoc.
func wattsStrogatzRewireStep(
	r *rand.Rand,
	neigh []map[int]struct{},
	i, s, n, betaPercent int,
) {
	j := (i + s) % n
	// One PCG draw per edge regardless of outcome — the comparison
	// against betaPercent realises the Bernoulli decision.
	if r.IntN(100) >= betaPercent {
		return
	}
	t := pickRewireTarget(r, neigh[i], i, n)
	if t < 0 {
		// All other nodes are already neighbours: rewire is
		// impossible. Reachable only when neigh[i] has saturated to
		// n - 1 entries; the catalogue does not generate this case
		// under the constructor's k < n invariant, but the guard is
		// kept defensive — we keep the original edge and return.
		return
	}
	delete(neigh[i], j)
	delete(neigh[j], i)
	neigh[i][t] = struct{}{}
	neigh[t][i] = struct{}{}
}
