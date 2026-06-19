// Example 13_network_reliability — two resilience analyses over ONE
// synthetic communication backbone, derived from a single capacitated
// edge list:
//
//  1. Structural single points of failure — the articulation points
//     (gateway sites) and bridges (links) whose individual loss
//     partitions the network, found with search.HopcroftTarjanBCC over
//     an immutable CSR snapshot.
//  2. Throughput and its bottleneck — the maximum flow from a source
//     site to a sink site via Dinic's max-flow (search/flow), followed
//     by the minimum cut: the saturated links that cap that throughput.
//
// Both analyses run on the SAME node set and the SAME capacitated edge
// list. The structural analysis sees the links through a CSR snapshot;
// the flow analysis sees the very same links as a capacitated
// flow.Network indexed by the same node space, so the two views describe
// one network rather than two unrelated graphs.
//
// # Topology
//
// The backbone is a deterministic, seeded "transit-stub" clustered
// network. The model is grounded in the Internet-topology literature —
// the GT-ITM transit-stub model (Zegura, Calvert & Bhattacharjee, "How
// to Model an Internetwork", IEEE INFOCOM '96; tool: Calvert & Zegura,
// "GT-ITM: Georgia Tech Internetwork Topology Models"). Intra-cluster
// density follows the planted-partition (stochastic block model)
// intuition p_in >> p_out; a Hamiltonian cycle per cluster is added to
// GUARANTEE 2-vertex-connectivity, which the SBM only gives
// probabilistically. Min-cut == max-flow is the Ford-Fulkerson theorem
// (Ford & Fulkerson 1956; CLRS ch. 26).
//
// The topology is built so that, for every seed, it has genuine
// reliability structure that both analyses can observe:
//
//   - K dense clusters of s sites each, laid out as a Hamiltonian cycle
//     plus c random chords. The cycle alone makes a cluster
//     2-vertex-connected: removing any single site leaves a path on the
//     rest, so a cluster has NO internal articulation point and NO
//     internal bridge. Adding chords keeps it 2-connected (open-ear
//     decomposition theorem) and raises intra-cluster capacity well
//     above the inter-cluster boundaries.
//   - The clusters form a spine PATH (cluster 0 .. K-1). Consecutive
//     clusters are joined by w_i parallel inter-cluster links: one
//     interior boundary is deliberately the NARROWEST (exactly two
//     links), every other spine boundary has three or more. Because the
//     cluster graph is a tree (a path), every spine boundary is a
//     genuine source-to-sink cut.
//   - One extra STUB cluster hangs off a spine cluster by a SINGLE link.
//     Because the cluster graph stays a tree, that single link is the
//     unique path to the stub: it IS a bridge and BOTH its endpoints ARE
//     articulation points. The stub sits OFF the source-sink spine, so
//     the bridge never enters the source-sink min-cut.
//   - Capacities are stratified H >> M > L (intra-cluster H, spine link
//     M, bridge link L). The source is an interior site of cluster 0 and
//     the sink an interior site of cluster K-1, each with intra-degree
//     >= 3, so their incident capacity dwarfs any boundary. This defeats
//     the trivial "isolate the source/sink" degree cut and forces the
//     global min-cut to be the narrowest interior spine boundary — a SET
//     of two saturated links, strictly cheaper than either terminal's
//     incident capacity.
//
// The deterministic facts this produces — the articulation-point count,
// the bridge count, the max-flow value, the min-cut size, and the
// max-flow == min-cut equality — are pinned by the regression test. They
// are reproducible for a fixed -seed; only the telemetry (lines prefixed
// with "# ") varies between runs and machines.
//
// # Scale
//
// Run with no flags the example builds a small, fast, deterministic
// default (5 spine clusters of 8 sites, ~45 sites). Every dimension is a
// flag, so the same binary scales up to where the analyses' wall-clock
// and heap footprint become observable:
//
//	go run ./examples/13_network_reliability -clusters 200 -cluster-size 64 -seed 7
//
// # Why in-memory
//
// The example measures structural-analysis and max-flow wall-clock and
// the live-heap footprint of the snapshot and flow network, so it builds
// everything in memory. It does not exercise the WAL/recovery stack;
// persistence is demonstrated by examples 04, 17, 24 and 25.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
	"github.com/FlavioCFOliveira/GoGraph/search/flow"
)

// Capacity strata (Gb/s). The separation H >> M > L is what forces the
// global min-cut to be an interior spine boundary (capacity a multiple
// of M) rather than a cut inside a dense cluster (>= H) or the off-spine
// bridge (L). Kept as named constants so the model is stated once.
const (
	capIntra  = 100 // H: intra-cluster link (cycle + chords)
	capSpine  = 10  // M: one inter-cluster spine link
	capBridge = 5   // L: the single off-spine stub link
)

// Boundary multiplicities along the spine. Exactly one interior boundary
// is the narrowest (two links); every other spine boundary has three, so
// the narrowest boundary is the unique global source-to-sink min-cut.
const (
	spineLinksNarrow = 2 // the narrowest interior boundary -> the min-cut
	spineLinksWide   = 3 // every other spine boundary
)

// config captures every scale and shape knob of the backbone. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	clusters    int   // number of spine clusters (K); >= 2 for a non-trivial spine
	clusterSize int   // sites per cluster (s); >= 3 so clusters are 2-connectable
	chords      int   // extra random chords per cluster beyond the Hamiltonian cycle
	seed        int64 // RNG seed; fixes the deterministic topology exactly
}

// defaultConfig returns the small, fast, deterministic default the
// regression test pins: five spine clusters of eight sites plus one stub
// cluster, roughly forty-eight sites.
func defaultConfig() config {
	return config{
		clusters:    5,
		clusterSize: 8,
		chords:      8,
		seed:        1,
	}
}

// validate rejects a configuration that cannot produce the required
// reliability structure. It is checked once, at the boundary, before any
// work. The spine needs at least two clusters for an interior boundary to
// exist. A cluster must be large enough to (a) be 2-vertex-connected
// (>= 3 sites for a Hamiltonian cycle) and (b) host the widest spine
// boundary on distinct interior sites while reserving its first site for a
// plain terminal — that is, clusterSize > spineLinksWide.
func (c config) validate() error {
	switch {
	case c.clusters < 2:
		return fmt.Errorf("clusters must be >= 2 (need an interior spine boundary), got %d", c.clusters)
	case c.clusterSize <= spineLinksWide:
		return fmt.Errorf("cluster-size must be > %d (need distinct interior endpoints for the widest boundary), got %d", spineLinksWide, c.clusterSize)
	case c.chords < 0:
		return fmt.Errorf("chords must be >= 0, got %d", c.chords)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.clusters, "clusters", cfg.clusters, "number of spine clusters (K)")
	flag.IntVar(&cfg.clusterSize, "cluster-size", cfg.clusterSize, "sites per cluster (s)")
	flag.IntVar(&cfg.chords, "chords", cfg.chords, "extra random chords per cluster beyond the Hamiltonian cycle")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic topology)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the backbone described by cfg, runs both resilience
// analyses, and writes a report to w. Bare lines carry deterministic
// facts (counts, the flow value, the cut size, the conservation
// invariant — reproducible for a fixed seed); lines prefixed with "# "
// carry volatile telemetry (durations and heap figures) that vary per
// run and per machine. All output goes to w so a test can capture and
// assert on the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.clusters=%d\n", cfg.clusters)
	fmt.Fprintf(w, "config.cluster_size=%d\n", cfg.clusterSize)
	fmt.Fprintf(w, "config.chords=%d\n", cfg.chords)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	net, err := generate(ctx, cfg)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	fmt.Fprintf(w, "nodes.sites=%d\n", net.sites)
	fmt.Fprintf(w, "edges.links=%d\n", len(net.links))

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", net.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# build.site_rate=%.0f sites/s\n", rate(net.sites, net.elapsed))
	fmt.Fprintf(w, "# build.link_rate=%.0f links/s\n", rate(len(net.links), net.elapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))

	if err := reportSPOF(ctx, w, net); err != nil {
		return fmt.Errorf("spof: %w", err)
	}
	if err := reportThroughput(ctx, w, net); err != nil {
		return fmt.Errorf("throughput: %w", err)
	}
	return nil
}

// reportSPOF freezes the network into an immutable CSR snapshot, runs the
// Hopcroft-Tarjan biconnected-components analysis over it, and prints the
// articulation points and bridges — the structural single points of
// failure. The counts are deterministic facts; the analysis wall-clock is
// telemetry.
func reportSPOF(ctx context.Context, w io.Writer, net *network) error {
	c := csr.BuildFromAdjList(net.adj)

	start := time.Now()
	res, err := search.HopcroftTarjanBCCCtx(ctx, c)
	if err != nil {
		return fmt.Errorf("HopcroftTarjanBCC: %w", err)
	}
	elapsed := time.Since(start)

	fmt.Fprintf(w, "spof.articulation_points=%d\n", len(res.Articulation))
	fmt.Fprintf(w, "spof.bridges=%d\n", len(res.Bridges))
	fmt.Fprintf(w, "# spof.elapsed=%s\n", elapsed.Round(time.Microsecond))

	// Resolve a few articulation sites and the bridges back to names so a
	// reader sees the structure; the names depend on the RNG draw, so they
	// are surfaced as telemetry, not pinned facts.
	for _, id := range sortedSites(res.Articulation) {
		name, ok := net.mapper.Resolve(id)
		if !ok {
			return fmt.Errorf("unresolved articulation node id %d", id)
		}
		fmt.Fprintf(w, "# spof.articulation_point=%s\n", name)
	}
	for _, b := range sortedBridges(res.Bridges) {
		u, ok := net.mapper.Resolve(b[0])
		if !ok {
			return fmt.Errorf("unresolved bridge endpoint id %d", b[0])
		}
		v, ok := net.mapper.Resolve(b[1])
		if !ok {
			return fmt.Errorf("unresolved bridge endpoint id %d", b[1])
		}
		fmt.Fprintf(w, "# spof.bridge=%s--%s\n", u, v)
	}
	return nil
}

// reportThroughput computes the maximum flow from source to sink over the
// SAME link list (indexed by the same site space the structural analysis
// uses) and derives the minimum cut that limits it.
//
// The flow is computed two ways and the two must agree: the library's
// Dinic max-flow (search/flow) is the authoritative oracle, and a small
// in-line residual solver computes the same value while ALSO exposing the
// settled residual graph — which is what lets the example derive the
// minimum cut (the library's Network does not expose its residual). The
// max-flow value, the min-cut size, and the max-flow == min-cut equality
// are deterministic facts; the flow wall-clock is telemetry.
func reportThroughput(ctx context.Context, w io.Writer, net *network) error {
	res := newResidual(net.sites)
	for _, l := range net.links {
		res.addUndirected(l.a, l.b, l.cap)
	}

	start := time.Now()
	flowValue := res.maxFlow(net.source, net.sink)
	elapsed := time.Since(start)

	// Cross-check against the library's Dinic max-flow built from the
	// identical link list: the example's residual solver and search/flow
	// must agree on the answer. Each undirected link is a pair of opposing
	// directed arcs of equal capacity.
	g := flow.NewNetwork(net.sites)
	for _, l := range net.links {
		g.AddEdge(l.a, l.b, l.cap)
		g.AddEdge(l.b, l.a, l.cap)
	}
	libValue, err := flow.MaxFlowCtx(ctx, g, net.source, net.sink)
	if err != nil {
		return fmt.Errorf("MaxFlow: %w", err)
	}
	if libValue != flowValue {
		return fmt.Errorf("max-flow mismatch: residual solver=%d, search/flow=%d", flowValue, libValue)
	}

	// The minimum cut is the set of links crossing from the source-side
	// reachable set (in the settled residual graph) to the rest. Its
	// capacity must equal the max flow (max-flow min-cut theorem); this
	// conservation law is asserted by the regression test.
	cut, cutCap := res.minCut(net.source, net.links)
	if cutCap != flowValue {
		return fmt.Errorf("min-cut capacity %d != max flow %d (max-flow min-cut theorem violated)", cutCap, flowValue)
	}

	fmt.Fprintf(w, "flow.max_value=%d\n", flowValue)
	fmt.Fprintf(w, "flow.min_cut_size=%d\n", len(cut))
	fmt.Fprintf(w, "flow.min_cut_capacity=%d\n", cutCap)
	fmt.Fprintf(w, "flow.maxflow_eq_mincut=%t\n", cutCap == flowValue)
	fmt.Fprintf(w, "# flow.elapsed=%s\n", elapsed.Round(time.Microsecond))
	for _, l := range cut {
		ua, _ := net.mapper.Resolve(net.idOf[l.a])
		ub, _ := net.mapper.Resolve(net.idOf[l.b])
		fmt.Fprintf(w, "# flow.saturated_link=%s--%s (%d Gb/s)\n", ua, ub, l.cap)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// rate returns count/elapsed in units per second, or 0 for a
// zero-length interval.
func rate(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
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

// sortedSites returns a copy of ids in ascending order so the telemetry
// site list is stable for a fixed topology.
func sortedSites(ids []graph.NodeID) []graph.NodeID {
	out := make([]graph.NodeID, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sortedBridges returns a copy of bridges in ascending (a, b) order so the
// telemetry bridge list is stable for a fixed topology.
func sortedBridges(bridges [][2]graph.NodeID) [][2]graph.NodeID {
	out := make([][2]graph.NodeID, len(bridges))
	copy(out, bridges)
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
