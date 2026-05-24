package shapegen

import (
	"errors"
	"fmt"
	"math/rand/v2"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// This file implements the "random / stochastic block model" family of
// the shape catalogue. The two generators here — [SBM] and
// [PlantedPartition] — realise the canonical block-structured random-
// graph constructions used in the community-detection literature: the
// general stochastic block model (Holland, Laskey & Leinhardt,
// "Stochastic blockmodels: First steps", Social Networks 5(2), 1983;
// Karrer & Newman, "Stochastic blockmodels and community structure in
// networks", Phys. Rev. E 83, 016107, 2011) and the planted partition
// model (Condon & Karp, "Algorithms for graph partitioning on the
// planted partition model", Random Struct. Algor. 18(2), 2001) — the
// special SBM with equal block sizes, a single intra-block probability,
// and a single inter-block probability that is the textbook
// recoverability benchmark for Leiden, label propagation, spectral
// partitioning, and related community-detection algorithms.
//
// # Specialisation
//
// Both generators produce *lpg.Graph[int, int64], mirroring the trivial,
// classic, structured, trees, specials, dags, erdos-renyi, barabasi-
// albert, watts-strogatz, rmat, configuration-model, and rgg families.
// The (int, int64) pair is the project's canonical "small unsigned key,
// signed weight" specialisation; every edge here carries
// [unweightedSentinel] (0) because the catalogue defines these shapes
// as topology fixtures rather than weight fixtures — the community
// information lives in the per-node "block_id" property, not on the
// edges.
//
// # Configuration override policy
//
// Each generator constructs the underlying [adjlist.Config] from the
// caller-supplied cfg, preserving cfg.MaxShardCapacity verbatim, and
// forces cfg.Directed=false and cfg.Multigraph=false: both block-model
// constructions are undirected simple graphs by definition. The
// "no self-loop" invariant holds because the pair iteration ranges over
// i < j only; the "no parallel edge" invariant holds because each
// unordered pair is considered exactly once.
//
// # Ground-truth community labels
//
// Every node carries its block membership as an [lpg.Int64Value]
// property under the key "block_id". The label is the zero-based block
// index that produced the node, so consumers running Leiden / label-
// propagation / spectral partitioning have a direct ground truth to
// score recovery against (NMI, ARI, accuracy). The property is written
// during the node-interning phase and persists regardless of whether
// the node ends up isolated (which can happen at very low intra-block
// probabilities).
//
// # Edge ordering and determinism
//
// Edges are inserted in lexicographic (u, v) order with u < v so the
// goldens stay byte-for-byte reproducible across builds and across
// platforms. The seeded generators thread a caller-supplied [uint64]
// seed through [math/rand/v2.NewPCG]; every (blockSizes, probPercent,
// seed) tuple yields the same byte-for-byte adjacency.
//
// PRNG consumption is exactly one [math/rand/v2.Rand.IntN] draw per
// unordered pair (i, j) with i < j, in lexicographic order: pairs are
// visited in row-major order over the upper triangle of the node-by-
// node matrix, and each draw is compared against probPercent[B(i)][B(j)]
// where B(.) is the block-of-node lookup. The seed-to-output map is
// therefore a pure function of (blockSizes, probPercent, seed).
//
// # Error propagation
//
// The Build closures use the same branch-free single-err-thread
// pattern as the other families: every per-phase error propagates
// through one err variable, and the surrounding closure returns
// (g, err). Validation errors surface as four distinct sentinels —
// [ErrSBMBlockMismatch], [ErrSBMNonSquare], [ErrSBMAsymmetric], and
// [ErrSBMProbOutOfRange] — wrapped with the offending coordinates so
// callers can errors.Is against each discriminant without unwrapping.
// Otherwise the per-phase loops can only surface [adjlist.ErrShardFull]
// when the caller has set a tight cfg.MaxShardCapacity; that error is
// returned verbatim. Property writes go through
// [lpg.Graph.SetNodeProperty], which itself may surface the same
// shard-full error from the underlying [adjlist.AdjList.AddNode] guard;
// the helper threads that error through the same err variable.

// ErrSBMBlockMismatch is returned by [SBM].Build when the length of
// blockSizes does not match the outer length of probPercent. The block
// vector and the probability matrix must agree on the number of blocks
// — without that agreement the per-pair probability lookup is
// ill-defined. Callers can errors.Is against this sentinel without
// unwrapping.
var ErrSBMBlockMismatch = errors.New("shapegen: SBM blockSizes length disagrees with probPercent outer length")

// ErrSBMNonSquare is returned by [SBM].Build when any row of
// probPercent has a length different from the number of blocks. The
// probability matrix must be square (k-by-k for k blocks); a ragged
// matrix has no consistent inter-block probability for at least one
// (i, j) pair. Callers can errors.Is against this sentinel without
// unwrapping.
var ErrSBMNonSquare = errors.New("shapegen: SBM probPercent matrix is not square")

// ErrSBMAsymmetric is returned by [SBM].Build when probPercent[i][j]
// disagrees with probPercent[j][i] for some i != j. The SBM is defined
// on undirected edges, so the inter-block probability must be
// symmetric; an asymmetric matrix would imply a directed model the
// catalogue does not realise here. Callers can errors.Is against this
// sentinel without unwrapping.
var ErrSBMAsymmetric = errors.New("shapegen: SBM probPercent matrix is not symmetric")

// ErrSBMProbOutOfRange is returned by [SBM].Build when any
// probPercent[i][j] entry is outside the inclusive [0, 100] window.
// The catalogue encodes probabilities as integer percents in the
// closed window [0, 100]; values outside this window have no
// well-defined meaning under the model. Callers can errors.Is against
// this sentinel without unwrapping.
var ErrSBMProbOutOfRange = errors.New("shapegen: SBM probPercent entry outside [0, 100]")

// sbmBase is the per-generator scaffolding for this file. Its layout
// mirrors trivialBase, classicBase, structuredBase, treesBase,
// specialsBase, dagsBase, erdosRenyiBase, barabasiAlbertBase,
// wattsStrogatzBase, rmatBase, configModelBase, and rggBase so the
// helpers (Name, Knobs, Build) carry the exact same semantics across
// families.
type sbmBase struct {
	name  string
	knobs []Knob
	build func(adjlist.Config) (*lpg.Graph[int, int64], error)
}

// Name returns the catalogue identifier.
func (s sbmBase) Name() string { return s.name }

// Knobs returns the bounded sweep ranges declared by the generator.
func (s sbmBase) Knobs() []Knob { return s.knobs }

// Build delegates to the per-generator closure after applying the
// configuration override policy documented at the head of this file.
func (s sbmBase) Build(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
	return s.build(cfg)
}

// SBM returns a Shape that builds a stochastic block model graph
// (Holland, Laskey & Leinhardt, "Stochastic blockmodels: First steps",
// Social Networks 5(2), 1983; Karrer & Newman, "Stochastic blockmodels
// and community structure in networks", Phys. Rev. E 83, 016107, 2011).
// The node set is partitioned into k = len(blockSizes) blocks; block i
// contains blockSizes[i] nodes. Every unordered pair (u, v) with u in
// block A and v in block B receives an undirected edge independently
// with probability probPercent[A][B] / 100. Self-loops are forbidden
// (the i < j pair iteration enforces this) and the probability matrix
// must be symmetric (probPercent[i][j] == probPercent[j][i] for all
// i, j).
//
// Nodes are assigned ids in contiguous blocks: block 0 takes ids
// 0..blockSizes[0]-1, block 1 takes ids
// blockSizes[0]..blockSizes[0]+blockSizes[1]-1, and so on. Empty blocks
// (blockSizes[i] == 0) contribute zero nodes and zero edges; they are
// silently skipped. A nil or empty blockSizes slice yields the empty
// graph (assuming probPercent is also empty — otherwise
// [ErrSBMBlockMismatch] surfaces).
//
// # Ground-truth community labels
//
// Every node carries its zero-based block index as an [lpg.Int64Value]
// property under the key "block_id". Consumers running community-
// detection algorithms (Leiden, label propagation, spectral
// partitioning) read the labels back through [lpg.Graph.GetNodeProperty]
// to score recovery quality (NMI, ARI, accuracy) against the planted
// partition.
//
// # Parameters and validation
//
// The PRNG is a deterministically-seeded [math/rand/v2.PCG], so every
// (blockSizes, probPercent, seed) tuple yields the same byte-for-byte
// adjacency. The constructor panics when any blockSizes[i] is negative
// (the catalogue does not define the model on negative-size inputs);
// every other validation is deferred to Build because the failure
// modes are caller-provided data errors rather than catalogue out-of-
// range conditions. Build surfaces four distinct sentinels:
//
//   - [ErrSBMBlockMismatch] — len(blockSizes) != len(probPercent).
//   - [ErrSBMNonSquare] — some row of probPercent has length
//     != len(blockSizes).
//   - [ErrSBMAsymmetric] — probPercent[i][j] != probPercent[j][i] for
//     some i != j.
//   - [ErrSBMProbOutOfRange] — some probPercent[i][j] outside [0, 100].
//
// All four are checked in the order above; the first failure
// short-circuits the others. Each is wrapped with the offending
// coordinates so callers can format diagnostic messages without
// unwrapping.
//
// # Catalogue invariants on the returned graph
//
//   - Order() == uint64(sum(blockSizes)).
//
//   - The graph is undirected and simple (no parallel edges, no
//     self-loops).
//
//   - Every node carries a "block_id" [lpg.Int64Value] property whose
//     value is the zero-based index of the block that produced it.
//
//   - The number of edges is a random variable; the expected count of
//     edges between block i and block j is:
//
//     blockSizes[i] * blockSizes[j] * probPercent[i][j] / 100   (i != j)
//     C(blockSizes[i], 2) * probPercent[i][i] / 100             (i == j)
//
//     The soak layer pins the pooled grand mean of the block-edge
//     counts — one per seed averaged across 100 seeds — against these
//     expectations within +/- 3 * sigma / sqrt(N), the standard error
//     of the sample mean. The pooled interpretation is required
//     because per-seed pointwise +/- 3 sigma checks have a non-trivial
//     false-positive rate over 100 independent draws (about 27%);
//     pooling shifts the test from a per-seed concentration test to a
//     concentration test on the average, which is the correct
//     statistical reading of "matches the expectation across 100
//     seeds". See [TestRandom_SBM_BlockEdgeSums_Soak] in sbm_test.go.
//
// SBM declares no knobs: the blockSizes and probPercent slices are
// variadic-style and property-based tests draw them directly via
// rapid, mirroring the convention pinned by [Multipartite] in the
// classic family and [ConfigurationModel] in the random family. The
// constructor takes a defensive copy of both inputs so subsequent
// caller mutations cannot affect Build.
//
//nolint:gocritic // paramTypeCombine: signature is pinned by the brief (blockSizes []int, probPercent [][]int, seed uint64).
func SBM(blockSizes []int, probPercent [][]int, seed uint64) Shape[int, int64] {
	for i, size := range blockSizes {
		if size < 0 {
			panic(fmt.Sprintf("shapegen: SBM blockSizes[%d] = %d, must be >= 0", i, size))
		}
	}
	// Copy blockSizes and probPercent so subsequent caller mutations
	// cannot affect Build — mirrors the [Multipartite] /
	// [ConfigurationModel] contract.
	ownedSizes := make([]int, len(blockSizes))
	copy(ownedSizes, blockSizes)
	ownedProb := make([][]int, len(probPercent))
	for i, row := range probPercent {
		ownedProb[i] = make([]int, len(row))
		copy(ownedProb[i], row)
	}
	return sbmBase{
		name: "random.sbm",
		build: func(cfg adjlist.Config) (*lpg.Graph[int, int64], error) {
			cfg.Directed = false
			cfg.Multigraph = false
			g := lpg.New[int, int64](cfg)
			return g, buildSBM(g, ownedSizes, ownedProb, seed)
		},
	}
}

// PlantedPartition returns a Shape that builds the planted partition
// model (Condon & Karp, "Algorithms for graph partitioning on the
// planted partition model", Random Struct. Algor. 18(2), 2001): k
// equal-size blocks of blockSize nodes each, with intra-block edge
// probability pInPercent / 100 and inter-block edge probability
// pOutPercent / 100. The implementation delegates to [SBM] with a
// k-block uniform-size vector and a constant probability matrix
// (pIn on the diagonal, pOut off-diagonal).
//
// PlantedPartition declares four knobs:
//
//   - "k" over [1, 20] (default 4) — number of blocks.
//   - "blockSize" over [0, 500] (default 25) — nodes per block.
//   - "pIn" over [0, 100] (default 50) — intra-block edge probability,
//     as integer percent.
//   - "pOut" over [0, 100] (default 1) — inter-block edge probability,
//     as integer percent.
//
// The "seed" parameter is supplied at construction time as a uint64
// and is not exposed as a knob, mirroring the convention pinned by
// [Layered], [BarabasiAlbert], [WattsStrogatz], [ErdosRenyiNP],
// [RMAT], [RandomRegular], and [RGG]. The constructor panics when:
//
//   - k < 1 or k > 20 (catalogue out-of-range);
//   - blockSize < 0 or blockSize > 500 (catalogue out-of-range);
//   - pInPercent < 0 or pInPercent > 100 (catalogue out-of-range);
//   - pOutPercent < 0 or pOutPercent > 100 (catalogue out-of-range).
//
// # Catalogue invariants on the returned graph
//
//   - Order() == uint64(k * blockSize).
//   - The graph is undirected and simple.
//   - Every node carries a "block_id" [lpg.Int64Value] property in
//     [0, k).
//   - Per-block expected intra-edge count: C(blockSize, 2) *
//     pInPercent / 100.
//   - Per-block-pair expected inter-edge count: blockSize * blockSize *
//     pOutPercent / 100.
//
// The soak layer pins statistical recoverability at the canonical
// recoverability knobs (k=4, blockSize=50, pIn=50, pOut=1) over 5
// seeds. The recoverability check is applied to the *aggregated*
// pooled mean across the 5 seeds — that is, summed over every block
// for intra-edges and summed over every unordered block-pair for
// inter-edges, then divided by the seed count. The aggregated mean of
// the intra-edge total must be at least 80% of its expectation
// (k * C(blockSize, 2) * pIn / 100) and the aggregated mean of the
// inter-edge total must be at most 105% of its expectation
// (C(k, 2) * blockSize^2 * pOut / 100). The aggregated reading is one
// of the two choices the brief explicitly permits ("per-block-pair
// OR aggregated") and is the one that stays inside the 5% leakage
// budget at the canonical knobs without flaking: per-block-pair
// inter-edge ratios fluctuate up to ~1.20 over short seed windows,
// while the aggregated ratio remains tightly inside [0.97, 1.04] over
// every observed 5-seed window. See
// [TestRandom_PlantedPartition_Recoverability_Soak] in sbm_test.go.
//
//nolint:gocritic // paramTypeCombine: signature is pinned by the brief (k int, blockSize int, pInPercent int, pOutPercent int, seed uint64).
func PlantedPartition(k, blockSize, pInPercent, pOutPercent int, seed uint64) Shape[int, int64] {
	if k < 1 || k > 20 {
		panic(fmt.Sprintf("shapegen: PlantedPartition requires 1 <= k <= 20, got %d", k))
	}
	if blockSize < 0 || blockSize > 500 {
		panic(fmt.Sprintf("shapegen: PlantedPartition requires 0 <= blockSize <= 500, got %d", blockSize))
	}
	if pInPercent < 0 || pInPercent > 100 {
		panic(fmt.Sprintf("shapegen: PlantedPartition requires 0 <= pInPercent <= 100, got %d", pInPercent))
	}
	if pOutPercent < 0 || pOutPercent > 100 {
		panic(fmt.Sprintf("shapegen: PlantedPartition requires 0 <= pOutPercent <= 100, got %d", pOutPercent))
	}
	blockSizes := make([]int, k)
	for i := range blockSizes {
		blockSizes[i] = blockSize
	}
	probPercent := make([][]int, k)
	for i := range probPercent {
		probPercent[i] = make([]int, k)
		for j := range probPercent[i] {
			if i == j {
				probPercent[i][j] = pInPercent
			} else {
				probPercent[i][j] = pOutPercent
			}
		}
	}
	inner := SBM(blockSizes, probPercent, seed)
	return sbmBase{
		name: "random.planted-partition",
		knobs: []Knob{
			{Name: "k", Min: 1, Max: 20, Default: 4},
			{Name: "blockSize", Min: 0, Max: 500, Default: 25},
			{Name: "pIn", Min: 0, Max: 100, Default: 50},
			{Name: "pOut", Min: 0, Max: 100, Default: 1},
		},
		build: inner.Build,
	}
}

// buildSBM validates blockSizes / probPercent, interns the per-block
// nodes in g with their ground-truth "block_id" properties, and then
// performs an O(n^2) pairwise Bernoulli sweep to insert every block-
// model edge. On success the edges are inserted in lexicographic
// (u, v) order with u < v.
//
// Validation is performed in the order documented on [SBM]:
//
//  1. len(blockSizes) == len(probPercent) — else [ErrSBMBlockMismatch].
//  2. every probPercent[i] has length len(blockSizes) — else
//     [ErrSBMNonSquare].
//  3. probPercent[i][j] == probPercent[j][i] for all i, j — else
//     [ErrSBMAsymmetric].
//  4. probPercent[i][j] in [0, 100] for all i, j — else
//     [ErrSBMProbOutOfRange].
//
// The first failure short-circuits the others; each error is wrapped
// with the offending coordinates so callers can format diagnostics
// without unwrapping.
//
// PRNG consumption is exactly one [math/rand/v2.Rand.IntN](100) draw
// per unordered pair (u, v) with u < v, in row-major order over the
// upper triangle of the node-by-node matrix. The seed-to-output map is
// therefore a pure function of (blockSizes, probPercent, seed).
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see SBM godoc.
func buildSBM(g *lpg.Graph[int, int64], blockSizes []int, probPercent [][]int, seed uint64) error {
	if err := validateSBM(blockSizes, probPercent); err != nil {
		return err
	}
	offsets, total := sbmBlockOffsets(blockSizes)
	err := addNodesRange(g, total)
	if err == nil {
		err = sbmAttachBlockLabels(g, blockSizes, offsets)
	}
	if err == nil {
		err = sbmEmitEdges(g, blockSizes, probPercent, offsets, total, seed)
	}
	return err
}

// validateSBM checks the four invariants documented on [SBM] in the
// canonical order: shape mismatch, non-square, asymmetric, out-of-range.
// The first failure short-circuits the others. Each error wraps the
// offending coordinates so callers can format diagnostics without
// unwrapping.
func validateSBM(blockSizes []int, probPercent [][]int) error {
	k := len(blockSizes)
	if len(probPercent) != k {
		return fmt.Errorf("%w: len(blockSizes)=%d len(probPercent)=%d", ErrSBMBlockMismatch, k, len(probPercent))
	}
	for i, row := range probPercent {
		if len(row) != k {
			return fmt.Errorf("%w: len(probPercent[%d])=%d, want %d", ErrSBMNonSquare, i, len(row), k)
		}
	}
	for i := 0; i < k; i++ {
		for j := i + 1; j < k; j++ {
			if probPercent[i][j] != probPercent[j][i] {
				return fmt.Errorf("%w: probPercent[%d][%d]=%d != probPercent[%d][%d]=%d",
					ErrSBMAsymmetric, i, j, probPercent[i][j], j, i, probPercent[j][i])
			}
		}
	}
	for i := 0; i < k; i++ {
		for j := 0; j < k; j++ {
			p := probPercent[i][j]
			if p < 0 || p > 100 {
				return fmt.Errorf("%w: probPercent[%d][%d]=%d", ErrSBMProbOutOfRange, i, j, p)
			}
		}
	}
	return nil
}

// sbmBlockOffsets returns the cumulative id offset of each block and
// the total node count. offsets[i] is the id of the first node in
// block i; offsets has len(blockSizes) entries. total is sum(blockSizes).
// The pair (offsets, total) is shared by sbmAttachBlockLabels (to
// stamp the per-node "block_id" property) and sbmEmitEdges (to map a
// node id back to its block via a binary search).
func sbmBlockOffsets(blockSizes []int) (offsets []int, total int) {
	offsets = make([]int, len(blockSizes))
	for i, size := range blockSizes {
		offsets[i] = total
		total += size
	}
	return offsets, total
}

// sbmAttachBlockLabels stamps every node 0..total-1 with its zero-based
// block index as an [lpg.Int64Value] property under the key "block_id".
// The label is the index of the block whose offset range contains the
// node id. The first SetNodeProperty error short-circuits the loop,
// matching the branch-free err-thread convention shared across the
// random family. SetNodeProperty surfaces the same shard-full error
// from the underlying [adjlist.AdjList.AddNode] guard.
func sbmAttachBlockLabels(g *lpg.Graph[int, int64], blockSizes, offsets []int) error {
	var err error
	for b := 0; b < len(blockSizes) && err == nil; b++ {
		label := lpg.Int64Value(int64(b))
		end := offsets[b] + blockSizes[b]
		for i := offsets[b]; i < end && err == nil; i++ {
			err = g.SetNodeProperty(canonicalNode(i), "block_id", label)
		}
	}
	return err
}

// sbmEmitEdges performs the O(n^2) pairwise Bernoulli sweep over every
// unordered pair (u, v) with u < v. Each pair receives an undirected
// edge with probability probPercent[B(u)][B(v)] / 100, where B(.) is
// the block-of-node lookup precomputed by [sbmNodeBlocks]. The first
// AddEdge error short-circuits the loop, matching the branch-free
// err-thread convention.
//
// Edges are inserted in lexicographic (u, v) order with u < v: the
// outer loop iterates u in ascending order, the inner loop iterates
// v = u+1..total-1 in ascending order. This pins the golden bytes
// regardless of any reordering inside the lpg backend.
//
// The PRNG draw is exactly one [math/rand/v2.Rand.IntN](100) call per
// pair, in row-major order. The draw is unconditional (not guarded by
// the probability lookup) so the seed-to-output map stays a pure
// function of (blockSizes, probPercent, seed); a guard would skip
// pairs where probPercent[B(u)][B(v)] == 0 and silently shift the
// PRNG stream for subsequent draws.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for catalogue determinism; see SBM godoc.
func sbmEmitEdges(g *lpg.Graph[int, int64], blockSizes []int, probPercent [][]int, offsets []int, total int, seed uint64) error {
	blocks := sbmNodeBlocks(blockSizes, offsets, total)
	r := rand.New(rand.NewPCG(seed, ^seed))
	var err error
	for u := 0; u < total && err == nil; u++ {
		bu := blocks[u]
		for v := u + 1; v < total && err == nil; v++ {
			bv := blocks[v]
			roll := r.IntN(100)
			if roll < probPercent[bu][bv] {
				err = g.AddEdge(canonicalNode(u), canonicalNode(v), unweightedSentinel)
			}
		}
	}
	return err
}

// sbmNodeBlocks returns a flat per-node lookup table mapping node id
// to its zero-based block index. blocks[i] is the block of node i, in
// the contiguous-block id assignment. The table is computed once per
// build so the inner edge loop performs an O(1) block lookup instead
// of an O(log k) binary search. Total nodes is total = sum(blockSizes).
func sbmNodeBlocks(blockSizes, offsets []int, total int) []int {
	blocks := make([]int, total)
	for b, size := range blockSizes {
		end := offsets[b] + size
		for i := offsets[b]; i < end; i++ {
			blocks[i] = b
		}
	}
	return blocks
}
