// Example 16_centrality_analytics — runs two analytics over one shared,
// immutable CSR snapshot: exact Brandes betweenness centrality and
// label-propagation community detection — with deterministic
// tie-breaking, and reports per-analysis evidence.
//
// The example builds a seeded, scale-parametrised synthetic graph with the
// mutable adjlist builder, freezes it into an immutable CSR snapshot, then
// runs both analytics against that one snapshot. It reports the
// deterministic results (the top-k betweenness node ids, the number of
// communities found, and that partition's size distribution) as bare fact
// lines, and the volatile cost of each analysis (wall-clock, transient
// allocations, live heap) as "# "-prefixed telemetry.
//
// # Topology — why this generator
//
// The generator was chosen on the advice of the graph-theory-expert
// sub-agent, so that BOTH analytics produce a meaningful, dramatically
// non-uniform result from one graph: betweenness must concentrate on a few
// obvious cut vertices, and label propagation must recover a sensible
// partition without collapsing to one giant label or fragmenting.
//
// The graph is a CHAIN of dense clusters joined by single bridge edges:
//
//		C0 == C1 == C2 == … == C(K-1)
//
//	  - Each cluster is a dense Erdős–Rényi-style subgraph laid down on top of
//	    a random spanning tree. The spanning tree GUARANTEES the cluster is one
//	    connected component (no reliance on luck), and the extra edges at
//	    intra-density p-in make it internally dense — dense enough that label
//	    propagation keeps each cluster as a single distinct label rather than
//	    fragmenting it.
//	  - Consecutive clusters are joined by exactly ONE bridge edge, between the
//	    right gateway of cluster c and the left gateway of cluster c+1. A single
//	    edge across the cut keeps the effective inter-cluster density near zero,
//	    which is the most stable regime for label propagation (no dense
//	    inter-coupling for a label to flood across), so the clusters survive as
//	    ~K separate communities.
//
// The gateways being the betweenness winners is a theorem here, not a
// heuristic: each bridge is the UNIQUE edge across its cut, so every shortest
// path between the two sides must traverse both gateway endpoints. Each
// gateway therefore carries Θ(n_c^2) pair-dependencies while every interior
// node carries only O(n_c), so the gateways dominate the betweenness ranking
// for every seed — the argument depends only on the fixed cut structure, not
// on the random intra-cluster edges. A CHAIN (rather than a ring) is used on
// purpose: a ring gives two equal-length arcs between far clusters, which
// splits the pair-dependency between them and blurs the signal; the chain
// gives a unique inter-cluster path, so the betweenness winners are maximally
// unambiguous and test-assertable.
//
// The graph is built UNDIRECTED: Brandes betweenness is classically read on
// undirected graphs, and label propagation is defined on undirected
// neighbourhoods. Both analytics here are unweighted (Brandes counts
// shortest-path hops; label propagation counts neighbour labels), so the
// edges carry no weight — the snapshot is purely structural.
//
// # Scale
//
// Run with no flags, the example builds a small deterministic default
// (6 clusters of 50 nodes at intra-density 0.30 — 300 nodes, roughly 1.5k
// edges) that the regression test pins and that runs in milliseconds. Exact
// Brandes is O(V*E), so the default is deliberately small. Every dimension is
// a flag, so the same binary scales up to a size where the per-analysis cost
// is actually observable:
//
//	go run ./examples/16_centrality_analytics -communities 20 -nodes 200 -intra-density 0.2 -seed 7
//
// That is ~4 000 nodes and tens of thousands of edges; because exact Brandes
// is O(V*E), pushing the cluster size into the hundreds moves the betweenness
// pass into seconds. The deterministic facts are reproducible for a fixed
// -seed; only the telemetry (lines prefixed with "# ") varies between runs
// and machines.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
	"github.com/FlavioCFOliveira/GoGraph/search/community"
)

// config captures every scale and shape knob of the graph generator. The
// zero value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression test).
type config struct {
	communities       int     // number of clusters in the chain
	nodesPerCommunity int     // nodes in each cluster (>= 2: two are gateways)
	intraDensity      float64 // probability of each extra intra-cluster edge (Erdős–Rényi p_in)
	topK              int     // how many top-betweenness node ids to report
	seed              int64   // RNG seed; fixes the deterministic graph shape
}

// defaultConfig returns the small deterministic default the regression test
// pins: six clusters of fifty nodes at intra-density 0.30, joined in a chain
// by five single bridge edges. That is 300 nodes and roughly 1.5k edges, so
// the O(V*E) Brandes pass runs in milliseconds, well under the 60 s short-test
// budget. topK is 2*(communities-1) so the report covers every gateway: each
// of the K-1 bridges has two endpoints, and those 2*(K-1) gateways are the
// betweenness winners.
func defaultConfig() config {
	return config{
		communities:       6,
		nodesPerCommunity: 50,
		intraDensity:      0.30,
		topK:              10, // 2*(6-1): every gateway of the chain
		seed:              1,
	}
}

// validate rejects a configuration that cannot produce the requested shape.
// It is checked once, at the boundary, before any work. Each cluster needs at
// least two nodes (a left and a right gateway), intraDensity is a probability
// in [0,1], and the chain needs at least one cluster.
func (c config) validate() error {
	switch {
	case c.communities <= 0:
		return fmt.Errorf("communities must be > 0, got %d", c.communities)
	case c.nodesPerCommunity < 2:
		return fmt.Errorf("nodes (per community) must be >= 2 (left and right gateway), got %d", c.nodesPerCommunity)
	case c.intraDensity < 0 || c.intraDensity > 1:
		return fmt.Errorf("intra-density must be in [0,1], got %g", c.intraDensity)
	case c.topK <= 0:
		return fmt.Errorf("top-k must be > 0, got %d", c.topK)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.communities, "communities", cfg.communities, "number of clusters in the chain")
	flag.IntVar(&cfg.nodesPerCommunity, "nodes", cfg.nodesPerCommunity, "nodes per cluster (>= 2: two are gateways)")
	flag.Float64Var(&cfg.intraDensity, "intra-density", cfg.intraDensity, "Erdős–Rényi p_in for extra intra-cluster edges, in [0,1]")
	flag.IntVar(&cfg.topK, "top-k", cfg.topK, "how many top-betweenness node ids to report")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic graph shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the chain-of-clusters graph described by cfg, freezes it into
// one immutable CSR snapshot, runs Brandes betweenness and label-propagation
// community detection against it, and writes a report to w. Bare lines carry
// deterministic facts (the top-k betweenness node ids, the community count and
// its size distribution — reproducible for a fixed seed); lines prefixed with
// "# " carry volatile telemetry (per-analysis wall-clock, transient
// allocations and heap figures) that varies per run and machine. All output
// goes to w so a test can capture and assert the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.communities=%d\n", cfg.communities)
	fmt.Fprintf(w, "config.nodes_per_community=%d\n", cfg.nodesPerCommunity)
	fmt.Fprintf(w, "config.intra_density=%g\n", cfg.intraDensity)
	fmt.Fprintf(w, "config.top_k=%d\n", cfg.topK)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// Build the mutable graph, then freeze it into the single immutable CSR
	// snapshot both analytics read. Freezing once and querying the same
	// snapshot is the idiom: an immutable CSR needs no synchronisation on the
	// read path, so the two analyses could even run concurrently.
	a, gen, err := build(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	fmt.Fprintf(w, "nodes.total=%d\n", c.Order())
	fmt.Fprintf(w, "edges.total=%d\n", c.Size())
	fmt.Fprintf(w, "nodes.gateways=%d\n", gen.gatewayCount)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", gen.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))

	if err := reportBetweenness(ctx, w, c, mapper, cfg.topK); err != nil {
		return err
	}
	if err := reportCommunities(ctx, w, c); err != nil {
		return err
	}
	return nil
}

// reportBetweenness runs the exact (unweighted) Brandes betweenness pass over
// the shared snapshot and reports the top-k node ids by score. The gateway
// nodes are the unique edges across each cut, so they dominate the ranking and
// occupy the very top. Ties are broken by ascending node value, so the
// reported ids are deterministic for a fixed seed.
func reportBetweenness(ctx context.Context, w io.Writer, c *csr.CSR[struct{}], mapper *graph.Mapper[int], k int) error {
	start := time.Now()
	mem := readMem()
	scores, err := centrality.BetweennessCtx(ctx, c)
	if err != nil {
		return fmt.Errorf("betweenness: %w", err)
	}
	after := readMem()

	top, err := topKByScore(mapper, c.LiveNodes(), scores, k)
	if err != nil {
		return fmt.Errorf("betweenness top-k: %w", err)
	}
	for rank, n := range top {
		fmt.Fprintf(w, "betweenness.top%d=%d\n", rank+1, n)
	}
	fmt.Fprintf(w, "# betweenness.elapsed=%s\n", time.Since(start).Round(time.Microsecond))
	fmt.Fprintf(w, "# betweenness.mallocs=%d\n", after.Mallocs-mem.Mallocs)
	return nil
}

// reportCommunities runs label-propagation community detection over the shared
// snapshot and reports the deterministic shape of the partition: the number of
// communities found and that partition's size distribution (the sorted list of
// community sizes). On the chain-of-dense-clusters topology label propagation
// recovers one community per cluster, so the count equals cfg.communities and
// every size equals cfg.nodesPerCommunity. Reporting the sorted size
// distribution — rather than per-node labels, whose numeric ids are an
// internal artefact — keeps the fact deterministic and meaningful.
func reportCommunities(ctx context.Context, w io.Writer, c *csr.CSR[struct{}]) error {
	start := time.Now()
	mem := readMem()
	p, err := community.LabelPropagationCtx(ctx, c, community.DefaultLabelPropagationOptions())
	if err != nil {
		return fmt.Errorf("label propagation: %w", err)
	}
	after := readMem()

	// Count the members of each live community. Ghost slots carry the sentinel
	// -1 (see community.Partition) and are skipped.
	sizes := make(map[int]int, p.NumCommunities)
	for _, cid := range p.Community {
		if cid < 0 {
			continue
		}
		sizes[cid]++
	}
	dist := make([]int, 0, len(sizes))
	for _, n := range sizes {
		dist = append(dist, n)
	}
	sort.Ints(dist)

	fmt.Fprintf(w, "communities.count=%d\n", p.NumCommunities)
	fmt.Fprintf(w, "communities.sizes=%v\n", dist)
	fmt.Fprintf(w, "# communities.elapsed=%s\n", time.Since(start).Round(time.Microsecond))
	fmt.Fprintf(w, "# communities.mallocs=%d\n", after.Mallocs-mem.Mallocs)
	return nil
}

// topKByScore resolves each live NodeID to its user-facing node value, ranks
// the values by descending score (breaking ties by ascending node value so the
// result is deterministic for a fixed seed), and returns the top k node values.
// scores is a NodeID-indexed slice as returned by the centrality algorithms.
func topKByScore(mapper *graph.Mapper[int], ids []graph.NodeID, scores []float64, k int) ([]int, error) {
	type scored struct {
		node  int
		score float64
	}
	ranked := make([]scored, 0, len(ids))
	for _, id := range ids {
		node, ok := mapper.Resolve(id)
		if !ok {
			return nil, fmt.Errorf("unresolved live node id %d", id)
		}
		if uint64(id) >= uint64(len(scores)) {
			return nil, fmt.Errorf("node id %d out of range for score slice of len %d", id, len(scores))
		}
		ranked = append(ranked, scored{node: node, score: scores[id]})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].node < ranked[j].node
	})
	if k > len(ranked) {
		k = len(ranked)
	}
	out := make([]int, k)
	for i := 0; i < k; i++ {
		out[i] = ranked[i].node
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Seeded generator
// ─────────────────────────────────────────────────────────────────────────────

// genResult reports the realised shape of a build (the random intra-cluster
// edges mean the edge total is not known until the graph is materialised) plus
// the count of gateway nodes and the wall-clock cost.
type genResult struct {
	gatewayCount int // number of gateway nodes (2 per bridge = 2*(communities-1))
	elapsed      time.Duration
}

// nodeID maps a cluster index and within-cluster offset to the flat,
// deterministic node value c*nodesPerCommunity + offset. Within a cluster,
// offset 0 is the LEFT gateway and offset 1 is the RIGHT gateway; offsets
// 2..nodesPerCommunity-1 are interior members.
func (c config) nodeID(cluster, offset int) int {
	return cluster*c.nodesPerCommunity + offset
}

// build materialises the chain-of-clusters graph described by cfg into a fresh
// undirected adjlist, consuming the seeded RNG in a single fixed order so the
// shape is a pure function of cfg.seed. The order is: each cluster in turn (a
// random spanning tree first for guaranteed connectivity, then Erdős–Rényi
// extra edges at intraDensity), then the chain of single bridge edges between
// consecutive clusters' gateways. The build honours ctx cancellation on a
// coarse interval.
func build(ctx context.Context, cfg config) (*adjlist.AdjList[int, struct{}], genResult, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed graph for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})

	addEdge := func(u, v int) error {
		if err := a.AddEdge(u, v, struct{}{}); err != nil {
			return fmt.Errorf("AddEdge %d-%d: %w", u, v, err)
		}
		return nil
	}

	for cluster := 0; cluster < cfg.communities; cluster++ {
		if err := ctx.Err(); err != nil {
			return nil, genResult{}, err
		}
		if err := buildCluster(ctx, cfg, rng, addEdge, cluster); err != nil {
			return nil, genResult{}, err
		}
	}

	// Chain bridges: one single edge between the RIGHT gateway (offset 1) of
	// cluster c and the LEFT gateway (offset 0) of cluster c+1. A single edge
	// across the cut makes each gateway the unique articulation point between
	// the two sides, which is what concentrates betweenness on the gateways and
	// keeps inter-cluster density near zero for label propagation.
	for cluster := 0; cluster+1 < cfg.communities; cluster++ {
		if err := addEdge(cfg.nodeID(cluster, 1), cfg.nodeID(cluster+1, 0)); err != nil {
			return nil, genResult{}, err
		}
	}

	gateways := 0
	if cfg.communities > 1 {
		gateways = 2 * (cfg.communities - 1)
	}
	return a, genResult{
		gatewayCount: gateways,
		elapsed:      time.Since(start),
	}, nil
}

// checkEvery bounds how often the intra-cluster edge loops poll ctx for
// cancellation: often enough that a cancelled large build stops promptly, rare
// enough that the check is free relative to the surrounding work.
const checkEvery = 4096

// buildCluster materialises one dense cluster of cfg.nodesPerCommunity nodes.
// It first lays down a random spanning tree — each node 1..n-1 attached to a
// uniformly random earlier node — which GUARANTEES the cluster is a single
// connected component without relying on the Erdős–Rényi draw. It then adds
// each remaining pair (i, j) with i < j, that is not already a tree edge,
// independently with probability intraDensity, making the cluster internally
// dense. Both phases consume the RNG in a fixed (ascending) order, so the
// cluster shape is a pure function of the seed and no parallel edges are
// created. The gateways (offsets 0 and 1) are ordinary cluster members here;
// the single bridge edge wired later is what gives them their role. The pair
// loop polls ctx on a coarse interval so even one very large cluster stays
// cancellable.
func buildCluster(ctx context.Context, cfg config, rng *rand.Rand, addEdge func(u, v int) error, cluster int) error {
	n := cfg.nodesPerCommunity

	// Random spanning tree: attach each later node to a uniformly random
	// earlier one. n-1 edges, guaranteed connected. parent[offset] records the
	// tree edge so the Erdős–Rényi phase below can skip it and never create a
	// parallel edge.
	parent := make([]int, n)
	for offset := 1; offset < n; offset++ {
		parent[offset] = rng.Intn(offset)
		if err := addEdge(cfg.nodeID(cluster, offset), cfg.nodeID(cluster, parent[offset])); err != nil {
			return err
		}
	}

	// Erdős–Rényi extra edges: every unordered pair (i, j) with i < j that is
	// not already the (j, parent[j]) tree edge, added independently with
	// probability intraDensity.
	checks := 0
	for j := 1; j < n; j++ {
		for i := 0; i < j; i++ {
			checks++
			if checks%checkEvery == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if i == parent[j] {
				continue // already present as a tree edge
			}
			if rng.Float64() < cfg.intraDensity {
				if err := addEdge(cfg.nodeID(cluster, i), cfg.nodeID(cluster, j)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc reflects
// live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
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
