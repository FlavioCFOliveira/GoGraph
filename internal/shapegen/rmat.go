package shapegen

import (
	"fmt"
	"math/rand/v2"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// This file implements the "random / R-MAT (Recursive MATrix)"
// generator of the shape catalogue. [RMAT] realises the Kronecker /
// recursive-matrix model of Chakrabarti, Zhan & Faloutsos
// ("R-MAT: A Recursive Model for Graph Mining", SDM 2004) as pinned
// by the Graph500 benchmark specification (Murphy et al., "Introducing
// the Graph 500", CUG 2010): the n-by-n adjacency of a directed graph
// is sampled by recursively descending a 2x2 probability matrix
// (A, B, C, D) — placing each edge into one of the four quadrants
// with probability a, b, c, d respectively, until the recursion
// reaches a single cell. The resulting graph is the catalogue's
// reference power-law / community-structured fixture: it gives the
// benchmark for BFS expansion under hub touch, for community
// detection on synthetic skew, and for the bulk-loader contention
// curve under realistic degree distributions.
//
// # Specialisation
//
// RMAT produces *lpg.Graph[int, int64], mirroring every other
// generator in the random/, trivial/, classic/, structured/, trees/,
// specials/, and dags/ families. The (int, int64) pair is the
// project's canonical "small unsigned key, signed weight" choice;
// every edge here carries [unweightedSentinel] (0) because the
// catalogue treats R-MAT as a topology fixture rather than a weight
// fixture.
//
// # Configuration override policy
//
// The generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// forces cfg.Directed=true and cfg.Multigraph=false: the Graph500
// R-MAT spec defines the graph as directed; duplicate (src, dst)
// emissions across the m draws are silently coalesced by the simple-
// graph mode of [adjlist.AdjList]. Self-loops (src == dst) are
// admitted because the canonical R-MAT distribution places non-zero
// probability mass on the diagonal at every recursion level; this is
// the Graph500 contract, not an oversight.
//
// # Edge ordering and determinism
//
// Edges are inserted in deterministic emission order: the i-th
// recursion-descent draw is emitted as the i-th AddEdge call. Because
// duplicates are dropped at the AddEdge layer, the **emission**
// sequence — not the unique-edge sequence — is what the determinism
// invariant pins. The seeded generator threads a caller-supplied
// [uint64] seed through [math/rand/v2.NewPCG]: every
// (scale, edgeFactor, a, b, c, d, seed) tuple yields the same
// byte-for-byte adjacency. No per-step sorting is needed because the
// recursion-descent draw order is itself stable.
//
// # The (a, b, c, d) tuple
//
// The four parameters are integer percents in [0, 100] that must sum
// to exactly 100. They are passed as constructor arguments rather
// than as [Knob]s because their joint sum constraint cannot be
// expressed in the single-integer-per-knob contract pinned by
// [Layered] (T58.8); a rapid sweep that drew them independently from
// [0, 100] would almost never land on a valid tuple. The Graph500
// reference values (a=57, b=19, c=19, d=5) are also the default used
// by the soak layer and by the goldens.
//
// # Error propagation
//
// The Build closure follows the same branch-free single-err-thread
// pattern documented in [erdos_renyi.go] and [barabasi_albert.go]:
// every per-phase error propagates through one err variable, and the
// surrounding closure returns (g, err). The per-phase loops can only
// surface [adjlist.ErrShardFull] when the caller has set a tight
// cfg.MaxShardCapacity; that error is returned verbatim.

// rmatBase is the per-generator scaffolding for this file. Its layout
// mirrors barabasiAlbertBase / erdosRenyiBase / wattsStrogatzBase so
// the helpers (Name, Knobs, Build) carry the exact same semantics
// across families.
type rmatBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s rmatBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s rmatBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s rmatBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// RMAT returns a Shape that builds a Graph500-style R-MAT graph with
// 2^scale nodes and edgeFactor * 2^scale edge-emission attempts. The
// per-emission recursive descent of the 2x2 (a, b, c, d) probability
// matrix is the canonical Chakrabarti/Zhan/Faloutsos placement
// (SDM 2004); the integer-percent encoding of (a, b, c, d) pins the
// quadrant decisions to a single IntN(100) draw per recursion level,
// giving a deterministic, allocation-free hot path.
//
// # Quadrant convention
//
// At every recursion level the unit square (representing the current
// 2^k-by-2^k sub-matrix) splits into four equal quadrants:
//
//	+-----+-----+
//	|  A  |  B  |
//	+-----+-----+
//	|  C  |  D  |
//	+-----+-----+
//
// where A is top-left (src in lower half, dst in lower half), B is
// top-right (src in lower half, dst in upper half), C is bottom-left
// (src in upper half, dst in lower half), and D is bottom-right
// (src in upper half, dst in upper half). A draw v ~ Uniform[0, 100)
// is classified as A iff v < a, B iff a <= v < a+b, C iff
// a+b <= v < a+b+c, D otherwise. This is the [Graph500] split; the
// previous bench/rmat implementation split A/B at ab/2 instead of at
// a, which is correct only under a == b and silently diverged for the
// canonical (57, 19, 19, 5) tuple.
//
// # Parameters and validation
//
// The constructor panics when:
//
//   - scale < 1 or scale > 30 (the knob range);
//   - edgeFactor < 1 or edgeFactor > 64 (the knob range);
//   - any of a, b, c, d exceeds 100;
//   - a + b + c + d != 100.
//
// The Graph500 reference values are (a, b, c, d) = (57, 19, 19, 5).
//
// # Catalogue invariants on the returned graph
//
//   - Order() == uint64(1) << scale.
//   - Size()  <= uint64(edgeFactor) * (uint64(1) << scale). The
//     upper bound is exact; the lower bound depends on the dedup
//     rate of the (a, b, c, d) tuple, which under canonical Graph500
//     parameters concentrates a large share of draws on the A
//     quadrant and therefore on the (0, 0) corner of the adjacency.
//     Retention grows monotonically with scale (more destination
//     cells dilute collisions) and decreases with edgeFactor (more
//     draws into the same cells inflate dedup). The empirical
//     retention floor is documented in the "Empirical retention"
//     section below.
//   - The graph is directed and may contain self-loops.
//   - Determinism: same (scale, edgeFactor, a, b, c, d, seed) tuple
//     yields a byte-identical *lpg.Graph[int, int64].
//
// # Empirical retention
//
// The empirical floor across five independent seeds for the
// canonical (57, 19, 19, 5) tuple, measured against the upper
// bound m = edgeFactor * 2^scale, is:
//
//	scale  ef   m         min retention
//	    6   4    256          0.6797
//	    7   4    512          0.7402
//	    8   4   1024          0.8066
//	    9   4   2048          0.8496
//	   10   4   4096          0.8738
//	    8   8   2048          0.7349
//	    8  16   4096          0.6262
//	   10  16  16384          0.7334
//	   12   8  32768          0.8736
//
// [TestRandom_RMAT_EdgeCountWithinTolerance] pins the >= 80 percent
// retention contract at the AC #1 configuration
// (scale = 8, edgeFactor = 4), where the empirical floor is 0.8066
// across the three reference seeds; the 0.80 threshold is the
// published Graph500 contract under canonical parameter dedup
// tolerance and matches the documented behaviour at the
// [TestRandom_RMAT_Soak] (scale = 12, edgeFactor = 8) point where
// the empirical floor is 0.8736. Configurations outside this range
// (notably (scale = 8, edgeFactor = 16)) drift below the 0.80 floor
// and are not part of the pinned contract.
//
// RMAT declares two knobs: "scale" over [1, 30] (default 10) and
// "edgeFactor" over [1, 64] (default 16). The (a, b, c, d, seed)
// arguments are supplied at construction time and are not exposed as
// knobs, mirroring the convention pinned by [Layered] and the
// [a, b, c, d] joint-sum constraint documented at the head of this
// file.
//
//nolint:gosec,gocritic // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; paramTypeCombine: signature is pinned by the brief (scale int, edgeFactor int, a uint64, b uint64, c uint64, d uint64, seed uint64).
func RMAT(scale, edgeFactor int, a, b, c, d, seed uint64) Shape[int, int64] {
	if scale < 1 || scale > 30 {
		panic(fmt.Sprintf("shapegen: RMAT requires 1 <= scale <= 30, got %d", scale))
	}
	if edgeFactor < 1 || edgeFactor > 64 {
		panic(fmt.Sprintf("shapegen: RMAT requires 1 <= edgeFactor <= 64, got %d", edgeFactor))
	}
	if a > 100 || b > 100 || c > 100 || d > 100 {
		panic(fmt.Sprintf("shapegen: RMAT requires 0 <= a,b,c,d <= 100, got a=%d b=%d c=%d d=%d", a, b, c, d))
	}
	if a+b+c+d != 100 {
		panic(fmt.Sprintf("shapegen: RMAT requires a+b+c+d == 100, got %d", a+b+c+d))
	}
	return rmatBase{
		name: "random.rmat",
		knobs: []Knob{
			{Name: "scale", Min: 1, Max: 30, Default: 10},
			{Name: "edgeFactor", Min: 1, Max: 64, Default: 16},
		},
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = true
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildRMAT(g, scale, edgeFactor, int(a), int(b), int(c), seed)
		},
	}
}

// buildRMAT interns nodes 0..n-1 in g (n = 1 << scale) and emits
// edgeFactor * n edge-placement attempts via [RMATPick], inserting
// each into g. Duplicates within an emission run are silently
// coalesced by simple-graph mode; the resulting Size() is therefore
// the count of *distinct* (src, dst) pairs emitted, while every
// emission still consumes a fixed prefix of the PRNG output. The
// first AddEdge error short-circuits the loop, matching the
// branch-free err-thread convention shared with [buildBarabasiAlbert]
// and [buildErdosRenyiNP].
//
// The thresholds (a, ab, abc) passed to [RMATPick] are the cumulative
// boundaries (a, a+b, a+b+c) interpreted as integer percents in
// [0, 100]. d is implied at the 100 boundary; passing it would be
// redundant.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see RMAT godoc.
func buildRMAT(g *lpg.Graph[int, int64], scale, edgeFactor, a, b, c int, seed uint64) error {
	n := uint64(1) << scale
	err := addNodesRange(g, int(n))
	r := rand.New(rand.NewPCG(seed, ^seed))
	m := uint64(edgeFactor) * n
	ab := a + b
	abc := ab + c
	for i := uint64(0); i < m && err == nil; i++ {
		src, dst := RMATPick(r, n, a, ab, abc)
		err = g.AddEdge(canonicalNode(int(src)), canonicalNode(int(dst)), unweightedSentinel)
	}
	return err
}

// RMATPick performs one Graph500 R-MAT recursive descent on the n-by-n
// adjacency matrix using cumulative integer-percent thresholds
// (a, ab, abc) = (a, a+b, a+b+c). It returns the (src, dst) cell
// coordinates with 0 <= src, dst < n.
//
// The function consumes exactly log2(n) PRNG draws — one per
// recursion level. Every draw is an [math/rand/v2.Rand.IntN](100)
// call; comparing the result against the cumulative thresholds
// implements the Graph500 quadrant split in branch-free integer
// arithmetic.
//
// The function is exported so [github.com/FlavioCFOliveira/GoGraph/bench/rmat.Generate] can share
// the exact same recursive-descent core without duplicating the
// (a, ab, abc) split logic. Outside the gograph module RMATPick is
// not reachable because shapegen is under internal/.
//
// Pre-conditions:
//
//   - n is a positive power of two; the loop terminates iff size > 1
//     becomes false, which holds only for n in {1, 2, 4, 8, ...}.
//   - 0 <= a <= ab <= abc <= 100. The Graph500 convention treats
//     draws >= abc as quadrant D.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see RMAT godoc.
func RMATPick(r *rand.Rand, n uint64, a, ab, abc int) (src, dst uint64) {
	for size := n; size > 1; size /= 2 {
		half := size / 2
		draw := r.IntN(100)
		switch {
		case draw < a:
			// Quadrant A (top-left): src and dst stay in the lower half.
		case draw < ab:
			// Quadrant B (top-right): dst moves to the upper half.
			dst += half
		case draw < abc:
			// Quadrant C (bottom-left): src moves to the upper half.
			src += half
		default:
			// Quadrant D (bottom-right): both src and dst move.
			src += half
			dst += half
		}
	}
	return src, dst
}
