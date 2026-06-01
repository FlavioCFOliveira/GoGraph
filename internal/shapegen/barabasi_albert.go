package shapegen

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// This file implements the "random / Barabási-Albert" generator of
// the shape catalogue. [BarabasiAlbert] realises the canonical
// preferential-attachment model (Barabási & Albert, "Emergence of
// Scaling in Random Networks", Science 286(5439), 1999): a small
// seed clique grows by adding nodes one at a time, each new node
// drawing m0 edges to existing nodes with probability proportional
// to their current degree. The resulting graph is the catalogue's
// reference scale-free fixture: its power-law degree tail exposes
// Mapper hot-shard contention, PageRank/betweenness skew, and BFS
// frontier explosion on hub touch.
//
// # Specialisation
//
// BarabasiAlbert produces *lpg.Graph[int, int64], mirroring every
// other generator in the random/, trivial/, classic/, structured/,
// trees/, specials/, and dags/ families. The (int, int64) pair is
// the project's canonical "small unsigned key, signed weight" choice;
// every edge here carries [unweightedSentinel] (0) because the
// catalogue treats Barabási-Albert as a topology fixture rather than
// a weight fixture.
//
// # Configuration override policy
//
// The generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// forces cfg.Directed=false and cfg.Multigraph=false: the
// Barabási-Albert model is an undirected simple graph (no parallel
// edges, no self-loops) by definition.
//
// # Edge ordering and determinism
//
// Edges are inserted in a deterministic order so the goldens stay
// byte-for-byte reproducible across builds and across platforms.
// The seeded generator threads a caller-supplied [uint64] seed
// through [math/rand/v2.NewPCG]: every (n, m0, seed) tuple yields
// the same byte-for-byte output. Within each preferential-attachment
// step the m0 sampled targets are sorted ascending before insertion,
// pinning the golden bytes regardless of the draw permutation.
//
// # Error propagation
//
// The Build closure uses the same branch-free single-err-thread
// pattern as the other families: every per-phase error propagates
// through one err variable, and the surrounding closure returns
// (g, err). The per-phase loops can only surface
// [adjlist.ErrShardFull] when the caller has set a tight
// cfg.MaxShardCapacity; that error is returned verbatim.

// barabasiAlbertBase is the per-generator scaffolding for this file.
// Its layout mirrors erdosRenyiBase, dagsBase, treesBase, etc. so
// the helpers (Name, Knobs, Build) carry the exact same semantics
// across families.
type barabasiAlbertBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s barabasiAlbertBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s barabasiAlbertBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s barabasiAlbertBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// BarabasiAlbert returns a Shape that builds a Barabási-Albert
// preferential-attachment graph on n nodes seeded with a complete
// graph on the first m0 nodes. Each subsequent node i in [m0, n)
// is connected to m0 distinct existing nodes drawn with probability
// proportional to their current degree (Barabási & Albert,
// "Emergence of Scaling in Random Networks", Science 286(5439),
// 1999). The graph is undirected and simple (no parallel edges,
// no self-loops); cfg.Directed and cfg.Multigraph are overridden
// to false.
//
// The PRNG is a deterministically-seeded [math/rand/v2.PCG], so
// every (n, m0, seed) tuple yields the same byte-for-byte adjacency.
//
// Catalogue invariants on the returned graph:
//
//   - Order() == uint64(n).
//   - Size()  == uint64(m0*(m0-1)/2 + (n-m0)*m0).
//     The first term counts the edges of the initial K_{m0} clique;
//     the second counts the m0 edges contributed by every node
//     added after the seed.
//   - The graph is undirected and simple.
//   - The degree distribution fits a power-law tail with exponent
//     gamma in [2.5, 3.5] at n=10000, m0=3 over five independent
//     seeds. The statistical test lives in the soak layer
//     ([TestRandom_BarabasiAlbert_PowerLawExponent]); the regression
//     method is described in its godoc.
//
// BarabasiAlbert declares two knobs: "n" over [2, 100000] (default
// 50) and "m0" over [1, 50] (default 3). The "seed" parameter is
// supplied at construction time as a uint64 and is not exposed as
// a knob, mirroring the convention pinned by [Layered]. The
// constructor panics when m0 < 1, when n < m0, when n > 100000, or
// when m0 > 50; the catalogue does not define the model outside
// these bounds.
//
// Sampling strategy: standard cumulative-sum preferential attachment.
// For each new node i in [m0, n) the generator
//
//  1. builds a per-step rejection set of node IDs already chosen
//     within the same step (so the m0 edges out of i are distinct);
//  2. draws u uniformly from [0, totalDegree); the half-open range
//     is the sum of current degrees over all existing nodes;
//  3. recovers the target node via cumulative-degree binary search;
//  4. retries the draw when the target is already in the rejection
//     set, until m0 distinct targets have been accepted.
//
// The cumulative-degree array is rebuilt once per step from the
// per-node degree slice, taking O(i) time at step i. The binary
// search inside the draw is O(log i). The total cost is therefore
// O(n*m0 + n^2) = O(n^2) for m0 fixed, well inside the catalogue's
// short-layer ceiling (the soak layer goes up to n=10000, which
// runs comfortably in seconds on a modern machine).
//
// Rejection is bounded because at step i there are i existing
// nodes and only m0 must be drawn; with m0 <= 50 and i >= m0 the
// expected number of retries per accepted target stays below a
// small constant. The catalogue does not measure or pin a worst-case
// retry bound — the soak-layer power-law test is the empirical
// guard that the sampling did not degenerate.
//
// After each new node i is fully wired, its m0 sampled targets are
// sorted ascending and inserted with i as the source. Because the
// graph is undirected, every AddEdge updates both endpoints'
// adjacency lists; the per-node degree slice is therefore
// incremented for both i and each target after the AddEdge call
// succeeds, so the cumulative-sum sampling at step i+1 sees the
// up-to-date totals.
//
//nolint:gosec,gocritic // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; paramTypeCombine: signature is pinned by the brief (n int, m0 int, seed uint64).
func BarabasiAlbert(n int, m0 int, seed uint64) Shape[int, int64] {
	if m0 < 1 || m0 > 50 {
		panic(fmt.Sprintf("shapegen: BarabasiAlbert requires 1 <= m0 <= 50, got %d", m0))
	}
	if n < m0 || n > 100_000 {
		panic(fmt.Sprintf("shapegen: BarabasiAlbert requires m0 <= n <= 100000, got n=%d m0=%d", n, m0))
	}
	return barabasiAlbertBase{
		name: "random.barabasi-albert",
		knobs: []Knob{
			{Name: "n", Min: 2, Max: 100_000, Default: 50},
			{Name: "m0", Min: 1, Max: 50, Default: 3},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildBarabasiAlbert(g, n, m0, seed)
		},
	}
}

// buildBarabasiAlbert interns nodes 0..n-1 in g and grows the graph
// from a complete K_{m0} seed clique by preferential attachment. At
// each step i in [m0, n) it draws m0 distinct existing targets with
// probability proportional to their current degree, sorts them
// ascending, and inserts the edges (i, t) into g. The first AddEdge
// error short-circuits every loop.
//
// The per-node degree slice deg[0..n-1] is maintained incrementally:
// after each successful AddEdge the source and target degrees are
// both incremented by one. This keeps the cumulative-degree array
// rebuild at step i+1 a pure function of the previous step's
// outcomes, so the seed-to-output map is a pure function of the
// (n, m0, seed) tuple.
//
// The PRNG is consumed exactly once per draw attempt — including
// rejected duplicates — so the seed-to-output map is stable under
// any change to the rejection-set representation.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see BarabasiAlbert godoc.
func buildBarabasiAlbert(g *lpg.Graph[int, int64], n, m0 int, seed uint64) error {
	err := addNodesRange(g, n)
	// deg[v] is the current undirected degree of node v. The slice
	// is sized to n so the cumulative-sum build at step i can stop
	// at deg[:i] without bounds re-checks. Allocating deg before the
	// err==nil guard keeps the layout uniform with [buildErdosRenyiNP]:
	// the per-phase loops below are gated on err == nil, so a failure
	// from addNodesRange short-circuits the work without an explicit
	// early return.
	deg := make([]int, n)
	// Phase 1: seed K_{m0}. Insert every unordered pair (a, b) with
	// a < b < m0 in lexicographic order, incrementing both endpoint
	// degrees on success.
	for a := 0; a < m0 && err == nil; a++ {
		for b := a + 1; b < m0 && err == nil; b++ {
			err = g.AddEdge(canonicalNode(a), canonicalNode(b), unweightedSentinel)
			if err == nil {
				deg[a]++
				deg[b]++
			}
		}
	}
	if err != nil || n == m0 {
		return err
	}
	r := rand.New(rand.NewPCG(seed, seed))
	// Phase 2: grow by preferential attachment. cumDeg is reused
	// across steps; targets and chosen are per-step working buffers.
	cumDeg := make([]int, 0, n)
	targets := make([]int, 0, m0)
	chosen := make(map[int]struct{}, m0)
	for i := m0; i < n && err == nil; i++ {
		err = barabasiAlbertStep(g, deg, cumDeg, targets, chosen, i, m0, r)
		// targets and chosen are reset inside barabasiAlbertStep so
		// they may be reused; cumDeg is rebuilt from deg[:i] at
		// every entry, so leftover capacity does no harm.
		targets = targets[:0]
		for k := range chosen {
			delete(chosen, k)
		}
	}
	return err
}

// barabasiAlbertStep performs one preferential-attachment step for
// the new node i: draws m0 distinct existing targets weighted by
// current degree, sorts them ascending, inserts the m0 undirected
// edges (i, target) into g, and updates the degree slice. It returns
// the first AddEdge error encountered (or nil).
//
// The function expects:
//
//   - deg[0..i) carries the current undirected degrees of the
//     existing nodes; deg[i..n) is zero.
//   - cumDeg is a working buffer with capacity >= i; the function
//     rewrites it in place to hold the prefix sums of deg[:i].
//   - targets is a working buffer with capacity >= m0; on entry it
//     must be empty. On exit it carries the m0 ascending target
//     ids; the caller is responsible for resetting it for the next
//     step.
//   - chosen is a working set with capacity hint m0; on entry it
//     must be empty. The caller is responsible for clearing it for
//     the next step.
//
// totalDegree is the sum of deg[:i]; it is recomputed from the last
// cumulative-sum entry (cumDeg[i-1]) after the build, so the call
// site does not need to maintain it explicitly.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see BarabasiAlbert godoc.
func barabasiAlbertStep(
	g *lpg.Graph[int, int64],
	deg, cumDeg, targets []int,
	chosen map[int]struct{},
	i, m0 int,
	r *rand.Rand,
) error {
	cumDeg = cumDeg[:0]
	running := 0
	for v := 0; v < i; v++ {
		running += deg[v]
		cumDeg = append(cumDeg, running)
	}
	totalDegree := running
	for len(chosen) < m0 {
		// totalDegree > 0 holds at every step except the very first
		// one under m0 == 1, where the seed "clique" K_1 has zero
		// edges and the lone existing node has degree 0. The only
		// admissible target in that case is node 0; falling through
		// to IntN(0) would panic, so we short-circuit explicitly.
		// For every other (m0, i) pair the seed clique on m0 >= 2
		// nodes contributes m0*(m0-1) > 0 to the total degree, or —
		// when m0 == 1 — earlier steps have already attached edges
		// that push totalDegree to >= 2.
		if totalDegree == 0 {
			chosen[0] = struct{}{}
			break
		}
		u := r.IntN(totalDegree)
		// cumDeg is strictly increasing on positive degrees; a
		// binary search recovers the smallest v with cumDeg[v] > u.
		// sort.SearchInts on cumDeg with target u+1 returns exactly
		// that index.
		v := sort.SearchInts(cumDeg, u+1)
		if _, dup := chosen[v]; dup {
			continue
		}
		chosen[v] = struct{}{}
	}
	// Sort the chosen ids ascending so the edges are inserted in a
	// stable order; this pins the golden bytes regardless of the
	// draw permutation.
	for v := range chosen {
		targets = append(targets, v)
	}
	sort.Ints(targets)
	var err error
	for k := 0; k < len(targets) && err == nil; k++ {
		t := targets[k]
		err = g.AddEdge(canonicalNode(i), canonicalNode(t), unweightedSentinel)
		if err == nil {
			deg[i]++
			deg[t]++
		}
	}
	return err
}
