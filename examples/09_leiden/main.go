// Example 09_leiden — modularity-optimising community detection with
// [community.Leiden] over a realistic, seeded planted-partition graph.
//
// It generates a symmetric stochastic block model (SBM) — K equal-sized
// planted communities with a high intra-community edge probability and a
// low inter-community edge probability — freezes it into an immutable CSR
// snapshot, runs Leiden, and reports how well Leiden recovers the planted
// structure: the number of communities found and the Newman modularity Q
// of the returned partition, alongside the volatile build/detect timing.
//
// # Model
//
// The graph is undirected and unweighted. The N = communities ×
// communitySize nodes are partitioned into K equal blocks. For every
// unordered node pair (i, j) an edge is drawn independently with
// probability pIn when i and j share a block and pOut when they do not.
// This is the planted-partition / symmetric-SBM construction; fixing
// -seed fixes the drawn edge set, hence the graph shape, exactly.
//
// # Why these parameters
//
// The graph-theory-expert sub-agent supplied the regime (recorded inline
// at sbmParams and computeModularity):
//
//   - Detectability follows the Kesten–Stigum / Decelle–Krzakala–Moore–
//     Zdeborová threshold for the symmetric SBM: with intra-degree
//     a = pIn·(s−1) and per-other-block inter-degree b = pOut·s over K
//     blocks, weak recovery is possible iff
//     SNR = (a−b)² / [K·(a + (K−1)·b)] > 1. The defaults sit ≈3× above
//     that threshold, deep in the "easy" regime, so Leiden reliably
//     recovers the planting.
//   - Each block is an Erdős–Rényi G(s, pIn) subgraph; it stays internally
//     connected with high probability when pIn·(s−1) ≥ 2·ln(s), which
//     validate enforces so a planted community cannot fragment and inflate
//     the community count.
//   - The planted partition's expected modularity is
//     Q ≈ (intra-edge fraction) − 1/K, which the small default targets at
//     ≈0.70 (theoretical max 1 − 1/K = 0.75 for K=4).
//
// # Scale
//
// Run with no flags, the example builds the small deterministic default —
// four communities of twenty-five nodes (100 nodes, ~700 edges) — which
// completes in well under a second and is pinned by the regression test.
// Every dimension is a flag, so the same binary scales up to a size where
// the detect cost is observable:
//
//	go run ./examples/09_leiden -communities 8 -community-size 500 -p-in 0.06 -p-out 0.0008 -seed 7
//
// The deterministic facts (community count and the modularity reported to
// two decimals) are reproducible for a fixed -seed; only the telemetry
// (lines prefixed with "# ") varies between runs and machines.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"runtime"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/community"
)

// config captures every scale and shape knob of the planted-partition
// generator and the detection run. The zero value is not valid; build one
// with defaultConfig and override fields from flags (see main) or
// construct one directly (see the regression test).
type config struct {
	communities   int     // K — number of planted communities (>= 2)
	communitySize int     // s — nodes per community (>= 2)
	pIn           float64 // intra-community edge probability (0,1]
	pOut          float64 // inter-community edge probability [0,1)
	seed          int64   // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins: four planted communities of twenty-five nodes each, with a
// high intra-community and a low inter-community edge probability that
// place the partition ≈3× above the SBM detectability threshold (see the
// package doc comment). At this scale the build and detection are
// instantaneous, keeping `go test` well under the short-layer budget.
func defaultConfig() config {
	return config{
		communities:   4,
		communitySize: 25,
		pIn:           0.55,
		pOut:          0.01,
		seed:          1,
	}
}

// validate rejects a configuration that cannot produce a recoverable
// planted partition. It is checked once, at the boundary, before any work.
// The intra-degree floor (pIn·(s−1) ≥ 2·ln(s)) keeps every planted block
// internally connected with high probability so a community cannot
// fragment into singletons and inflate the recovered count; the
// pIn > pOut check guarantees there is planted structure to find.
func (c config) validate() error {
	switch {
	case c.communities < 2:
		return fmt.Errorf("communities must be >= 2, got %d", c.communities)
	case c.communitySize < 2:
		return fmt.Errorf("community-size must be >= 2, got %d", c.communitySize)
	case c.pIn <= 0 || c.pIn > 1:
		return fmt.Errorf("p-in must be in (0,1], got %g", c.pIn)
	case c.pOut < 0 || c.pOut >= 1:
		return fmt.Errorf("p-out must be in [0,1), got %g", c.pOut)
	case c.pIn <= c.pOut:
		return fmt.Errorf("require p-in > p-out (else no planted structure), got p-in=%g p-out=%g", c.pIn, c.pOut)
	}
	// Erdős–Rényi connectivity floor: each block is a G(s, pIn) subgraph and
	// stays connected w.h.p. when its expected intra-degree clears 2·ln(s).
	if minDeg := 2 * math.Log(float64(c.communitySize)); c.pIn*float64(c.communitySize-1) < minDeg {
		return fmt.Errorf("p-in (%g) too low for community-size %d: expected intra-degree %.2f < 2*ln(s)=%.2f, blocks may fragment",
			c.pIn, c.communitySize, c.pIn*float64(c.communitySize-1), minDeg)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.communities, "communities", cfg.communities, "number of planted communities (K)")
	flag.IntVar(&cfg.communitySize, "community-size", cfg.communitySize, "nodes per community (s)")
	flag.Float64Var(&cfg.pIn, "p-in", cfg.pIn, "intra-community edge probability")
	flag.Float64Var(&cfg.pOut, "p-out", cfg.pOut, "inter-community edge probability")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the planted-partition graph described by cfg, runs Leiden
// over its CSR snapshot, and writes a report to w. Bare lines carry
// deterministic facts (the configuration, the planted node/edge totals,
// the recovered community count, and the modularity to two decimals — all
// reproducible for a fixed seed); lines prefixed with "# " carry volatile
// telemetry (build/detect durations and heap figures) that vary per run
// and per machine. All output goes to w so a test can capture and assert
// on the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.communities=%d\n", cfg.communities)
	fmt.Fprintf(w, "config.community_size=%d\n", cfg.communitySize)
	fmt.Fprintf(w, "config.p_in=%g\n", cfg.pIn)
	fmt.Fprintf(w, "config.p_out=%g\n", cfg.pOut)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	buildStart := time.Now()
	a, planted, err := buildSBM(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	c := csr.BuildFromAdjList(a)
	buildElapsed := time.Since(buildStart)

	nodes := cfg.communities * cfg.communitySize
	fmt.Fprintf(w, "nodes=%d\n", nodes)
	fmt.Fprintf(w, "edges=%d\n", planted)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", buildElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# build.edge_rate=%.0f edges/s\n", rate(planted, buildElapsed))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(saturatingSub(built.HeapAlloc, base.HeapAlloc)))

	// Detect. LeidenCtx checks ctx at every pass boundary and returns a
	// wrapped ctx error on cancellation.
	detectStart := time.Now()
	part, err := community.LeidenCtx(ctx, c, community.DefaultLeidenOptions())
	if err != nil {
		return fmt.Errorf("leiden: %w", err)
	}
	detectElapsed := time.Since(detectStart)

	q := computeModularity(c, part)

	// Deterministic facts: the recovered community count (Leiden recovers
	// the K planted blocks) and the modularity to two decimals. Leiden's
	// output is deterministic for a fixed input, but its internals are
	// randomised by contract, so the report pins the count and rounds Q to
	// two decimals — the regression test asserts a count band and a Q
	// lower bound rather than an exact float, surviving an internal change
	// that preserves partition quality.
	fmt.Fprintf(w, "communities_found=%d\n", part.NumCommunities)
	fmt.Fprintf(w, "modularity=%.2f\n", q)

	fmt.Fprintf(w, "# detect.elapsed=%s\n", detectElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# detect.node_rate=%.0f nodes/s\n", rate(nodes, detectElapsed))
	fmt.Fprintf(w, "# modularity.exact=%.6f\n", q)

	return nil
}

// buildSBM materialises the symmetric stochastic block model described by
// cfg into a fresh undirected AdjList, returning it together with the
// realised edge count (the random draw means the total is not known until
// the graph is built). Node v in [0, K·s) belongs to planted community
// v / s. Every unordered pair (i, j) is offered an edge with probability
// pIn when i and j share a community and pOut otherwise; AddEdge mirrors
// each undirected edge internally. The build honours ctx cancellation on a
// coarse interval so a cancelled large run stops promptly.
func buildSBM(ctx context.Context, cfg config) (*adjlist.AdjList[int, struct{}], int, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed planted-partition for a given -seed; crypto/rand
	// would defeat the reproducibility the standard requires.
	rng := rand.New(rand.NewSource(cfg.seed))
	n := cfg.communities * cfg.communitySize

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Intern every node up front so isolated nodes (none expected at the
	// configured densities, but possible for adversarial parameters) still
	// count toward Order and receive a community assignment.
	for v := 0; v < n; v++ {
		if err := a.AddNode(v); err != nil {
			return nil, 0, fmt.Errorf("AddNode %d: %w", v, err)
		}
	}

	edges := 0
	for i := 0; i < n; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		ci := i / cfg.communitySize
		for j := i + 1; j < n; j++ {
			p := cfg.pOut
			if ci == j/cfg.communitySize {
				p = cfg.pIn
			}
			if rng.Float64() < p {
				if err := a.AddEdge(i, j, struct{}{}); err != nil {
					return nil, 0, fmt.Errorf("AddEdge %d-%d: %w", i, j, err)
				}
				edges++
			}
		}
	}
	return a, edges, nil
}

// checkEvery bounds how often the build polls ctx for cancellation: often
// enough that a cancelled large run stops promptly, rare enough that the
// check is free relative to the surrounding O(N²) pair enumeration.
const checkEvery = 256

// computeModularity returns the Newman modularity Q of partition part over
// the undirected, unweighted snapshot c, using the per-community form
//
//	Q = Σ_c [ L_c/m − (D_c/2m)² ]
//
// where m is the edge count, L_c the number of edges with both endpoints in
// community c (counted once), and D_c the summed degree of community c
// (Newman & Girvan, Phys. Rev. E 69, 026113, 2004). The CSR stores each
// undirected edge as two directed entries, so m is the total directed
// entry count halved and L_c counts only the u<v direction of an
// intra-community adjacency; getting that factor of two right is the one
// subtle point. Ghost NodeID slots (community -1 from sharded packing) are
// skipped. Runs in O(V + E).
func computeModularity(c *csr.CSR[struct{}], part community.Partition) float64 {
	offsets := c.VerticesSlice() // len == MaxNodeID()+1
	edges := c.EdgesSlice()
	maxID := c.MaxNodeID()

	twoM := len(edges) // each undirected edge contributes two directed entries
	if twoM == 0 {
		return 0
	}
	m := float64(twoM) / 2

	lc := make([]int, part.NumCommunities)    // intra edges per community (u<v)
	dc := make([]uint64, part.NumCommunities) // summed degree per community
	// Iterate NodeIDs directly (no int conversions): offsets/edges/Community
	// are all indexable by NodeID, and NodeIDs compare directly for the u<v
	// once-per-undirected-pair guard.
	for u := graph.NodeID(0); u < maxID; u++ {
		cu := part.Community[u]
		if cu < 0 {
			continue // ghost slot
		}
		dc[cu] += offsets[u+1] - offsets[u]
		for e := offsets[u]; e < offsets[u+1]; e++ {
			v := edges[e]
			if v <= u {
				continue // count each undirected pair once
			}
			if part.Community[v] == cu {
				lc[cu]++
			}
		}
	}

	q := 0.0
	for cid := 0; cid < part.NumCommunities; cid++ {
		frac := float64(dc[cid]) / (2 * m)
		q += float64(lc[cid])/m - frac*frac
	}
	return q
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers (copied from the reference example 26).
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
func rate(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// saturatingSub returns a−b, clamped to 0 when b > a (GC between the two
// snapshots can leave the later HeapAlloc below the earlier one).
func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// humanBytes formats a byte count with a binary (KiB/MiB/GiB) suffix.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
