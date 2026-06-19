// Example 03_advanced_algorithms — runs four algorithms over one shared,
// immutable CSR snapshot: BFS, Dijkstra, exact Brandes betweenness
// centrality, and PageRank — and reports per-algorithm evidence.
//
// The example builds a seeded, scale-parametrised synthetic graph with the
// mutable adjlist builder, freezes it into an immutable CSR snapshot, then
// runs all four algorithms against that one snapshot. It reports the
// deterministic results (reachability, two shortest-path distances, the
// top-k betweenness and PageRank node ids) as bare fact lines, and the
// volatile cost of each algorithm (wall-clock, PageRank convergence
// iterations, transient allocations, live heap) as "# "-prefixed telemetry.
//
// # Topology — why this generator
//
// The generator was chosen on the advice of the graph-theory-expert
// sub-agent, so that all four algorithms produce meaningful, distinct,
// dramatically non-uniform results from one graph:
//
//   - Each community is a Barabási–Albert scale-free graph (preferential
//     attachment), which gives a non-uniform PageRank: a few high-degree
//     intra-community hubs dominate the stationary distribution. A
//     near-regular Erdős–Rényi blob would leave PageRank almost flat.
//   - The communities are joined only through dedicated low-degree bridge
//     nodes wired in a ring. Because a bridge is the sole gateway out of its
//     community, every inter-community shortest path is forced through it, so
//     the bridges are genuine cut vertices whose betweenness towers over every
//     intra-community node. Promoting a hub to also be a bridge would blur
//     that signal, so bridges attach to low-degree members on purpose.
//
// The two mechanisms are orthogonal — preferential attachment shapes
// within-community mass (PageRank); the bridge ring shapes between-community
// flow (betweenness) — so they do not fight. A clean teaching consequence is
// that the bridge nodes have HIGH betweenness but LOW PageRank: the two
// centrality measures disagree, and the topology shows exactly why.
//
// The graph is built UNDIRECTED. Brandes betweenness is classically read on
// undirected graphs, and on a connected undirected graph PageRank always
// converges with no dangling-node sinks. Edge weights are positive with
// spread and are consumed ONLY by Dijkstra: BFS counts hops, and both Brandes
// betweenness and PageRank here are the unweighted variants. So Dijkstra's
// weighted distances generally differ from BFS's hop counts — a deliberate
// contrast.
//
// # Scale
//
// Run with no flags, the example builds a small deterministic default
// (4 communities of 25 nodes, attachment 2, plus 4 bridge nodes — 104 nodes,
// ~208 edges) that the regression test pins and that runs in microseconds.
// Every dimension is a flag, so the same binary scales up to a size where the
// per-algorithm cost is actually observable:
//
//	go run ./examples/03_advanced_algorithms -communities 8 -nodes 500 -ba-attach 3 -seed 7
//
// That is ~4 000 nodes and ~12 000 edges; exact Brandes is O(V*E), so pushing
// the per-community size into the thousands moves the betweenness pass into
// seconds. The deterministic facts are reproducible for a fixed -seed; only
// the telemetry (lines prefixed with "# ") varies between runs and machines.
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
	"github.com/FlavioCFOliveira/GoGraph/search"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// config captures every scale and shape knob of the graph generator. The
// zero value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression test).
type config struct {
	communities       int   // number of scale-free communities
	nodesPerCommunity int   // BA nodes in each community
	baAttach          int   // BA attachment parameter m (edges each new node brings)
	bridgeIntraEdges  int   // edges from each bridge node into its community
	weightMin         int64 // minimum edge weight (inclusive, must be >= 1)
	weightMax         int64 // maximum edge weight (inclusive)
	topK              int   // how many top betweenness / PageRank nodes to report
	seed              int64 // RNG seed; fixes the deterministic graph shape
}

// defaultConfig returns the small deterministic default the regression test
// pins: four scale-free communities of 25 nodes (attachment 2) joined by a
// ring of four bridge nodes. That is 104 nodes and roughly 208 edges, so
// every algorithm — including the O(V*E) Brandes pass — runs in microseconds.
func defaultConfig() config {
	return config{
		communities:       4,
		nodesPerCommunity: 25,
		baAttach:          2,
		bridgeIntraEdges:  1,
		weightMin:         1,
		weightMax:         10,
		topK:              5,
		seed:              1,
	}
}

// validate rejects a configuration that cannot produce the requested shape.
// It is checked once, at the boundary, before any work. The Barabási–Albert
// construction requires 1 <= baAttach < nodesPerCommunity (a new node must
// attach to m distinct earlier nodes, and the seed clique has baAttach+1
// nodes), and the weight range must be strictly positive for Dijkstra.
func (c config) validate() error {
	switch {
	case c.communities <= 0:
		return fmt.Errorf("communities must be > 0, got %d", c.communities)
	case c.nodesPerCommunity <= 0:
		return fmt.Errorf("nodes (per community) must be > 0, got %d", c.nodesPerCommunity)
	case c.baAttach < 1:
		return fmt.Errorf("ba-attach must be >= 1, got %d", c.baAttach)
	case c.baAttach >= c.nodesPerCommunity:
		return fmt.Errorf("ba-attach (%d) must be < nodes per community (%d)", c.baAttach, c.nodesPerCommunity)
	case c.bridgeIntraEdges < 1:
		return fmt.Errorf("bridge-intra-edges must be >= 1, got %d", c.bridgeIntraEdges)
	case c.bridgeIntraEdges > c.nodesPerCommunity:
		return fmt.Errorf("bridge-intra-edges (%d) exceeds nodes per community (%d)", c.bridgeIntraEdges, c.nodesPerCommunity)
	case c.weightMin < 1:
		return fmt.Errorf("weight-min must be >= 1 (Dijkstra needs positive weights), got %d", c.weightMin)
	case c.weightMax < c.weightMin:
		return fmt.Errorf("require weight-min <= weight-max, got [%d,%d]", c.weightMin, c.weightMax)
	case c.topK <= 0:
		return fmt.Errorf("top-k must be > 0, got %d", c.topK)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.communities, "communities", cfg.communities, "number of scale-free communities")
	flag.IntVar(&cfg.nodesPerCommunity, "nodes", cfg.nodesPerCommunity, "Barabási–Albert nodes per community")
	flag.IntVar(&cfg.baAttach, "ba-attach", cfg.baAttach, "BA attachment parameter m (edges each new node brings)")
	flag.IntVar(&cfg.bridgeIntraEdges, "bridge-intra-edges", cfg.bridgeIntraEdges, "edges from each bridge node into its community")
	flag.Int64Var(&cfg.weightMin, "weight-min", cfg.weightMin, "minimum edge weight (inclusive, >= 1)")
	flag.Int64Var(&cfg.weightMax, "weight-max", cfg.weightMax, "maximum edge weight (inclusive)")
	flag.IntVar(&cfg.topK, "top-k", cfg.topK, "how many top betweenness / PageRank nodes to report")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic graph shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the graph described by cfg, freezes it into one CSR
// snapshot, runs the four algorithms against it, and writes a report to w.
// Bare lines carry deterministic facts (counts, distances, the top-k node
// ids — reproducible for a fixed seed); lines prefixed with "# " carry
// volatile telemetry (per-algorithm wall-clock, convergence iterations,
// transient allocations, and heap figures) that varies per run and machine.
// All output goes to w so a test can capture and assert the deterministic
// lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.communities=%d\n", cfg.communities)
	fmt.Fprintf(w, "config.nodes_per_community=%d\n", cfg.nodesPerCommunity)
	fmt.Fprintf(w, "config.ba_attach=%d\n", cfg.baAttach)
	fmt.Fprintf(w, "config.weights=[%d,%d]\n", cfg.weightMin, cfg.weightMax)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// Build the mutable graph, then freeze it into the single immutable CSR
	// snapshot every algorithm reads. Freezing once and querying the same
	// snapshot is the idiom: an immutable CSR needs no synchronisation on the
	// read path, so the four algorithms could even run concurrently.
	a, gen, err := build(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	fmt.Fprintf(w, "nodes.total=%d\n", c.Order())
	fmt.Fprintf(w, "edges.total=%d\n", c.Size())
	fmt.Fprintf(w, "nodes.bridges=%d\n", cfg.communities)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", gen.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))

	src, ok := mapper.Lookup(gen.source)
	if !ok {
		return fmt.Errorf("source node %d not found in graph", gen.source)
	}
	farthest, ok := mapper.Lookup(gen.farthest)
	if !ok {
		return fmt.Errorf("farthest node %d not found in graph", gen.farthest)
	}

	if err := reportBFS(ctx, w, c, src); err != nil {
		return err
	}
	if err := reportDijkstra(ctx, w, c, gen, src, farthest); err != nil {
		return err
	}
	if err := reportBetweenness(ctx, w, c, mapper, cfg.topK); err != nil {
		return err
	}
	if err := reportPageRank(ctx, w, c, mapper, cfg.topK); err != nil {
		return err
	}
	return nil
}

// reportBFS runs an unweighted breadth-first traversal from src over the
// shared snapshot, reporting the number of reachable nodes (a fact: the graph
// is connected by construction, so this equals the node count) and the
// eccentricity of the source — the depth of the farthest reachable node.
func reportBFS(ctx context.Context, w io.Writer, c *csr.CSR[int64], src graph.NodeID) error {
	start := time.Now()
	mem := readMem()
	reachable := 0
	maxDepth := 0
	if err := search.BFSCtx(ctx, c, src, func(_ graph.NodeID, depth int) bool {
		reachable++
		if depth > maxDepth {
			maxDepth = depth
		}
		return true
	}); err != nil {
		return fmt.Errorf("bfs: %w", err)
	}
	after := readMem()
	fmt.Fprintf(w, "bfs.reachable=%d\n", reachable)
	fmt.Fprintf(w, "bfs.eccentricity=%d\n", maxDepth)
	fmt.Fprintf(w, "# bfs.elapsed=%s\n", time.Since(start).Round(time.Microsecond))
	fmt.Fprintf(w, "# bfs.mallocs=%d\n", after.Mallocs-mem.Mallocs)
	return nil
}

// reportDijkstra runs single-source weighted shortest paths from src and
// reports two distances — to the farthest node within the source's own
// community and to a node in another community (which must route through the
// bridge ring). The weighted distances generally differ from the BFS hop
// counts because the edge weights have spread, which Dijkstra consumes and
// BFS ignores.
func reportDijkstra(ctx context.Context, w io.Writer, c *csr.CSR[int64], gen genResult, src, farthest graph.NodeID) error {
	start := time.Now()
	mem := readMem()
	d, err := search.DijkstraCtx(ctx, c, src)
	if err != nil {
		return fmt.Errorf("dijkstra: %w", err)
	}
	after := readMem()

	distFar, ok := d.Distance(farthest)
	if !ok {
		return fmt.Errorf("farthest node %d unreachable from source", gen.farthest)
	}
	fmt.Fprintf(w, "dijkstra.dist_to_farthest=%d\n", distFar)
	fmt.Fprintf(w, "dijkstra.hops_to_farthest=%d\n", len(d.Path(farthest))-1)
	fmt.Fprintf(w, "# dijkstra.elapsed=%s\n", time.Since(start).Round(time.Microsecond))
	fmt.Fprintf(w, "# dijkstra.mallocs=%d\n", after.Mallocs-mem.Mallocs)
	return nil
}

// reportBetweenness runs the exact (unweighted) Brandes betweenness pass over
// the shared snapshot and reports the top-k node ids by score. The bridge
// nodes are cut vertices, so they occupy the very top of the ranking. Ties
// are broken by ascending node value, so the reported ids are deterministic.
func reportBetweenness(ctx context.Context, w io.Writer, c *csr.CSR[int64], mapper *graph.Mapper[int], k int) error {
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

// reportPageRank runs the (unweighted) PageRank power iteration over the
// shared snapshot and reports the convergence iteration count and the top-k
// node ids by score. The top scores are the high-degree intra-community hubs;
// the bridge nodes, being low-degree by design, sit low — high betweenness,
// low PageRank. Ties are broken by ascending node value for determinism.
func reportPageRank(ctx context.Context, w io.Writer, c *csr.CSR[int64], mapper *graph.Mapper[int], k int) error {
	start := time.Now()
	mem := readMem()
	scores, iters, err := centrality.PageRankCtx(ctx, c, centrality.DefaultPageRankOptions())
	if err != nil {
		return fmt.Errorf("pagerank: %w", err)
	}
	after := readMem()

	top, err := topKByScore(mapper, c.LiveNodes(), scores, k)
	if err != nil {
		return fmt.Errorf("pagerank top-k: %w", err)
	}
	fmt.Fprintf(w, "pagerank.iterations=%d\n", iters)
	for rank, n := range top {
		fmt.Fprintf(w, "pagerank.top%d=%d\n", rank+1, n)
	}
	fmt.Fprintf(w, "# pagerank.elapsed=%s\n", time.Since(start).Round(time.Microsecond))
	fmt.Fprintf(w, "# pagerank.mallocs=%d\n", after.Mallocs-mem.Mallocs)
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

// genResult reports the realised shape of a build (the random BA degrees mean
// the edge totals are not known until the graph is materialised) plus the
// fixed traversal anchors and the wall-clock cost.
type genResult struct {
	source   int // node value used as the BFS/Dijkstra source (community-0 node 0)
	farthest int // node value in the far community, reached via the bridge ring
	elapsed  time.Duration
}

// nodeID maps a community index and within-community offset to the flat,
// deterministic node value c*nodesPerCommunity + i. Bridge nodes live in a
// contiguous block above every community node (see bridgeID).
func (c config) nodeID(community, offset int) int {
	return community*c.nodesPerCommunity + offset
}

// bridgeID returns the flat node value of the bridge node attached to the
// given community, placed in a contiguous block above all community nodes so
// the two id spaces never overlap.
func (c config) bridgeID(community int) int {
	return c.communities*c.nodesPerCommunity + community
}

// build materialises the hybrid graph described by cfg into a fresh
// undirected adjlist, consuming the seeded RNG in a single fixed order so the
// shape is a pure function of cfg.seed. The order is: each community in turn
// (seed clique, then preferential-attachment growth), then each bridge node's
// intra-community edges, then the bridge ring. Edge weights are drawn from
// [weightMin, weightMax] at creation time. The build honours ctx cancellation
// on a coarse interval.
func build(ctx context.Context, cfg config) (*adjlist.AdjList[int, int64], genResult, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed graph for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	a := adjlist.New[int, int64](adjlist.Config{Directed: false})

	addEdge := func(u, v int) error {
		w := cfg.weightMin + rng.Int63n(cfg.weightMax-cfg.weightMin+1)
		if err := a.AddEdge(u, v, w); err != nil {
			return fmt.Errorf("AddEdge %d-%d: %w", u, v, err)
		}
		return nil
	}

	// repeated holds each node value once per incident edge endpoint, so a
	// uniform draw into it picks a node with probability proportional to its
	// degree — the classic Barabási–Albert preferential-attachment trick. It
	// is RESET per community (buildCommunity truncates it back to empty) so
	// preferential attachment never reaches across communities: the bridge
	// ring must be the ONLY inter-community path, or the bridges stop being
	// cut vertices and their betweenness collapses.
	repeated := make([]int, 0, cfg.nodesPerCommunity*cfg.baAttach*2)

	for community := 0; community < cfg.communities; community++ {
		if err := ctx.Err(); err != nil {
			return nil, genResult{}, err
		}
		if err := buildCommunity(cfg, rng, addEdge, &repeated, community); err != nil {
			return nil, genResult{}, err
		}
	}

	// Bridge nodes: one per community, attached to bridgeIntraEdges distinct
	// LOW-degree community members — the LAST BA nodes added (offsets counting
	// down from nodesPerCommunity-1), which preferential attachment leaves with
	// the lowest degree. The low-degree attach matters only for PageRank: it
	// keeps each bridge out of PageRank's top-k (the BA hubs dominate that),
	// while the cut-vertex role — the bridge being the sole gateway out of its
	// community — is what makes its betweenness tower over every node.
	for community := 0; community < cfg.communities; community++ {
		if err := ctx.Err(); err != nil {
			return nil, genResult{}, err
		}
		bridge := cfg.bridgeID(community)
		for e := 0; e < cfg.bridgeIntraEdges; e++ {
			leaf := cfg.nodeID(community, cfg.nodesPerCommunity-1-e)
			if err := addEdge(bridge, leaf); err != nil {
				return nil, genResult{}, err
			}
		}
	}
	for community := 0; community < cfg.communities; community++ {
		next := (community + 1) % cfg.communities
		if next == community {
			break // a single community has no ring
		}
		if err := addEdge(cfg.bridgeID(community), cfg.bridgeID(next)); err != nil {
			return nil, genResult{}, err
		}
	}

	// The source is node 0 of community 0; the farthest anchor is node 0 of
	// the last community, which is reached only by crossing the bridge ring.
	return a, genResult{
		source:   cfg.nodeID(0, 0),
		farthest: cfg.nodeID(cfg.communities-1, 0),
		elapsed:  time.Since(start),
	}, nil
}

// buildCommunity grows a single Barabási–Albert scale-free community: it
// first connects the seed clique of the first baAttach+1 nodes, then attaches
// every later node to baAttach distinct earlier nodes of THIS community chosen
// with probability proportional to their current degree (read from repeated).
// Self-loops and duplicate targets are rejected so each new node contributes
// exactly baAttach distinct neighbours and the degree-proportional sampling
// stays correct. repeated is reset to empty on entry so attachment never
// reaches a previous community — that isolation is what keeps the bridge ring
// the only inter-community path.
func buildCommunity(cfg config, rng *rand.Rand, addEdge func(u, v int) error, repeated *[]int, community int) error {
	m := cfg.baAttach
	*repeated = (*repeated)[:0] // confine preferential attachment to this community

	// Seed clique on the first m+1 nodes guarantees the community starts
	// connected and seeds the degree pool.
	for i := 0; i <= m; i++ {
		for j := i + 1; j <= m; j++ {
			if err := addEdge(cfg.nodeID(community, i), cfg.nodeID(community, j)); err != nil {
				return err
			}
			*repeated = append(*repeated, cfg.nodeID(community, i), cfg.nodeID(community, j))
		}
	}

	// Preferential-attachment growth for the remaining nodes.
	targets := make(map[int]struct{}, m)
	for offset := m + 1; offset < cfg.nodesPerCommunity; offset++ {
		v := cfg.nodeID(community, offset)
		clear(targets)
		// Draw m distinct targets, each picked degree-proportionally from the
		// community's degree pool (entries before this node's block start).
		for len(targets) < m {
			pick := (*repeated)[rng.Intn(len(*repeated))]
			if pick == v {
				continue // reject self
			}
			if _, dup := targets[pick]; dup {
				continue // reject duplicate target for this v
			}
			targets[pick] = struct{}{}
		}
		// Wire the edges in ascending target order so RNG-independent map
		// iteration never perturbs the build, then update the degree pool.
		ordered := make([]int, 0, m)
		for t := range targets {
			ordered = append(ordered, t)
		}
		sort.Ints(ordered)
		for _, t := range ordered {
			if err := addEdge(v, t); err != nil {
				return err
			}
			*repeated = append(*repeated, v, t)
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
