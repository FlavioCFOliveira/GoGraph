// Example 10_dimacs9_routing — build a deterministic synthetic road
// network with the DIMACS 9 harness, freeze it into an immutable CSR
// snapshot, run a concrete single-source shortest-paths query
// (search.Dijkstra) that reconstructs a route, and measure search
// performance with a distribution of random probe queries.
//
// # Model
//
// The graph is produced by [dimacs9.Synthetic], which approximates a
// road network: every vertex points to its next-numbered neighbours
// (a chain that keeps the whole graph reachable from node 0) plus a
// few longer "shortcut" edges, all with int64 weights derived purely
// from the endpoint indices. The number of vertices and edges are the
// two scale knobs (-vertices, -edges); the average out-degree is
// edges/vertices (clamped to a minimum of 2).
//
// # Seed independence of the topology
//
// This is the one subtlety worth stating plainly: the DIMACS 9
// synthetic topology takes NO seed. It is a pure deterministic function
// of (vertices, edges) — destinations and weights are arithmetic on the
// endpoint indices — so the graph, and therefore every shortest path
// over it, is identical on every run and every machine for a fixed
// (-vertices, -edges).
//
// The -seed flag controls ONLY the random probe workload: which
// source->target node pairs the timed Dijkstra queries run between, to
// build the throughput and p50/p95/p99 latency distribution. Changing
// -seed changes which pairs are probed (and therefore the volatile
// timing telemetry), but it does NOT change the graph, the fixed
// concrete route, the distance, the reachable count, or any other
// deterministic fact. Those facts are reproducible for a fixed
// (vertices, edges) regardless of -seed.
//
// # Evidence
//
// The example reports two kinds of line:
//
//   - Deterministic facts (bare key=value lines, pinned by the test):
//     the node and edge counts; one fixed concrete route from node 0 to
//     a fixed target with its exact distance and reconstructed path; how
//     many nodes are reachable from node 0; and how many of the random
//     probe pairs were feasible (target reachable from source).
//   - Volatile telemetry (lines prefixed with "# "): the search
//     throughput in queries/s and the p50/p95/p99 query-latency
//     distribution, plus the live Go heap. These vary per run and per
//     machine and are never pinned by the regression test.
//
// # Scale
//
// Run with no flags, the example builds a 2,000-node, 12,000-edge
// network (average out-degree 6) and times 200 random probe queries —
// small enough to finish in milliseconds yet large enough that the
// latency distribution is meaningful. Every dimension is a flag, so the
// same binary scales up to a size where search performance is genuinely
// observable:
//
//	go run ./examples/10_dimacs9_routing -vertices 200000 -edges 1000000 -probes 5000 -seed 7
//
// The deterministic facts are reproducible for a fixed (-vertices,
// -edges); only the telemetry (lines prefixed with "# ") varies between
// runs, machines, and -seed values.
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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bench/dimacs9"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// srcNode is the fixed anchor of the deterministic concrete route. The
// synthetic generator chains every vertex to its successor, so node 0
// reaches every other vertex; routing from it keeps the example's fixed
// route well defined at any scale.
const srcNode = 0

// config captures every scale and shape knob of the example. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	vertices int   // number of road-network vertices (topology dimension)
	edges    int   // number of directed edges (topology dimension)
	target   int   // destination node value of the fixed concrete route from node 0
	probes   int   // number of random source->target probe queries to time
	seed     int64 // RNG seed for the probe workload ONLY (not the topology)
}

// defaultConfig returns the small, deterministic default the regression
// test pins: a 2,000-node, 12,000-edge synthetic road network (average
// out-degree 6) with 200 timed probe queries. It builds and queries in
// milliseconds, well under the short-layer 60 s package budget, yet is
// large enough for the latency distribution to be meaningful.
func defaultConfig() config {
	return config{
		vertices: 2_000,
		edges:    12_000,
		target:   50,
		probes:   200,
		seed:     1,
	}
}

// validate rejects a configuration that cannot produce a well-defined
// run. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.vertices <= 0:
		return fmt.Errorf("vertices must be > 0, got %d", c.vertices)
	case c.edges < 0:
		return fmt.Errorf("edges must be >= 0, got %d", c.edges)
	case c.target < 0 || c.target >= c.vertices:
		return fmt.Errorf("target (%d) must be in [0, vertices=%d)", c.target, c.vertices)
	case c.target == srcNode:
		return fmt.Errorf("target (%d) must differ from the source node %d", c.target, srcNode)
	case c.probes < 0:
		return fmt.Errorf("probes must be >= 0, got %d", c.probes)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.vertices, "vertices", cfg.vertices, "number of road-network vertices (fixes the topology)")
	flag.IntVar(&cfg.edges, "edges", cfg.edges, "number of directed edges (fixes the topology)")
	flag.IntVar(&cfg.target, "target", cfg.target, "destination node value of the fixed concrete route from node 0")
	flag.IntVar(&cfg.probes, "probes", cfg.probes, "number of random source->target probe queries to time")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed for the probe workload only (the topology is seed-independent)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run builds the synthetic road network described by cfg, freezes it
// into a CSR snapshot, runs a concrete Dijkstra route, then times a
// distribution of random probe queries, writing a report to w. Bare
// lines carry deterministic facts (counts, the fixed route, its distance
// and path — all reproducible for a fixed vertices/edges regardless of
// seed); lines prefixed with "# " carry volatile telemetry (throughput,
// latency percentiles, heap) that varies per run and per machine. All
// output goes to w so a test can capture and assert the deterministic
// lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.vertices=%d\n", cfg.vertices)
	fmt.Fprintf(w, "config.edges=%d\n", cfg.edges)
	fmt.Fprintf(w, "config.target=%d\n", cfg.target)
	fmt.Fprintf(w, "config.probes=%d\n", cfg.probes)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// Build the synthetic road network. The generator is deterministic
	// for a fixed (vertices, edges) and takes no seed: destinations and
	// weights are pure functions of the endpoint indices, so the graph —
	// and every shortest path over it — is identical on every run.
	// validate has already established vertices > 0 and edges >= 0, so the
	// widening to the harness's uint64 dimensions is in range.
	buildStart := time.Now()
	a, err := dimacs9.Synthetic(ctx, uintFromNonNeg(cfg.vertices), uintFromNonNeg(cfg.edges))
	if err != nil {
		return fmt.Errorf("dimacs9.Synthetic: %w", err)
	}
	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()
	buildElapsed := time.Since(buildStart)

	fmt.Fprintf(w, "graph.nodes=%d\n", c.Order())
	fmt.Fprintf(w, "graph.edges=%d\n", c.Size())

	// One fixed concrete route: node 0 -> cfg.target. Distance and the
	// reconstructed path are deterministic facts for a fixed topology.
	d, err := singleSource(ctx, c, mapper, srcNode)
	if err != nil {
		return fmt.Errorf("dijkstra from node %d: %w", srcNode, err)
	}
	dstID, ok := lookupVal(mapper, cfg.target)
	if !ok {
		return fmt.Errorf("target node %d not found in graph", cfg.target)
	}
	dist, reachable := d.Distance(dstID)
	if !reachable {
		return fmt.Errorf("target node %d is not reachable from node %d", cfg.target, srcNode)
	}
	route, err := routeNodes(mapper, d.Path(dstID))
	if err != nil {
		return fmt.Errorf("resolve route to %d: %w", cfg.target, err)
	}
	fmt.Fprintf(w, "route.src=%d\n", srcNode)
	fmt.Fprintf(w, "route.dst=%d\n", cfg.target)
	fmt.Fprintf(w, "route.distance=%d\n", dist)
	fmt.Fprintf(w, "route.hops=%d\n", len(d.Path(dstID))-1)
	fmt.Fprintf(w, "route.path=%s\n", route)

	// How many nodes the single-source query reached from node 0. The
	// chain edges keep the whole graph reachable, so this equals the
	// node count, but we measure it rather than assume it.
	fmt.Fprintf(w, "reach.from_src=%d\n", reachableCount(d, mapper, cfg.vertices))

	// Probe workload: time `probes` random source->target Dijkstra
	// queries. The pairs are drawn from a math/rand seeded by cfg.seed,
	// so the workload SHAPE is reproducible for a fixed seed; the
	// topology it runs over is seed-independent. We record the per-query
	// latency to build the distribution and count how many pairs were
	// feasible (target reachable from source) as a deterministic fact.
	prep, err := probe(ctx, c, mapper, cfg)
	if err != nil {
		return fmt.Errorf("probe workload: %w", err)
	}
	fmt.Fprintf(w, "probe.count=%d\n", prep.count)
	fmt.Fprintf(w, "probe.feasible=%d\n", prep.feasible)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", buildElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# build.node_rate=%.0f nodes/s\n", rate(c.Order(), buildElapsed))
	fmt.Fprintf(w, "# build.edge_rate=%.0f edges/s\n", rate(c.Size(), buildElapsed))
	if prep.count > 0 {
		fmt.Fprintf(w, "# probe.elapsed=%s\n", prep.elapsed.Round(time.Microsecond))
		fmt.Fprintf(w, "# probe.throughput=%.0f queries/s\n", rate(uintFromNonNeg(prep.count), prep.elapsed))
		fmt.Fprintf(w, "# probe.latency.p50=%s\n", percentile(prep.latencies, 0.50).Round(time.Nanosecond))
		fmt.Fprintf(w, "# probe.latency.p95=%s\n", percentile(prep.latencies, 0.95).Round(time.Nanosecond))
		fmt.Fprintf(w, "# probe.latency.p99=%s\n", percentile(prep.latencies, 0.99).Round(time.Nanosecond))
	}
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(saturatingSub(built.HeapAlloc, base.HeapAlloc)))
	fmt.Fprintf(w, "# mem.total_alloc=%s\n", humanBytes(built.TotalAlloc-base.TotalAlloc))
	fmt.Fprintf(w, "# mem.num_gc=%d\n", built.NumGC-base.NumGC)
	return nil
}

// probeResult is the realised outcome of the random probe workload: how
// many queries ran, how many were feasible (target reachable from
// source), the per-query latencies, and the total wall-clock time.
type probeResult struct {
	count     int
	feasible  int
	latencies []time.Duration
	elapsed   time.Duration
}

// probe runs cfg.probes random source->target Dijkstra queries over the
// frozen CSR snapshot c. The source/target pairs are drawn from a
// math/rand seeded by cfg.seed — that fixes the WORKLOAD shape, while
// the topology it runs against is seed-independent. The whole-source
// distances are computed per probe (a single-source query) and the
// target's reachability is read back, so feasible counts the pairs whose
// target is reachable. The build honours ctx cancellation on a coarse
// interval.
func probe(ctx context.Context, c *csr.CSR[int64], mapper *graph.Mapper[uint32], cfg config) (probeResult, error) {
	if cfg.probes == 0 {
		return probeResult{}, nil
	}
	//nolint:gosec // G404: a seeded math/rand is intentional — the probe
	// workload must reproduce a fixed pair sequence for a given -seed;
	// crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))

	res := probeResult{latencies: make([]time.Duration, 0, cfg.probes)}
	start := time.Now()
	for i := 0; i < cfg.probes; i++ {
		if i%probeCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return probeResult{}, err
			}
		}
		srcVal := rng.Intn(cfg.vertices)
		dstVal := rng.Intn(cfg.vertices)
		src, ok := lookupVal(mapper, srcVal)
		if !ok {
			return probeResult{}, fmt.Errorf("probe source %d not found", srcVal)
		}
		dst, ok := lookupVal(mapper, dstVal)
		if !ok {
			return probeResult{}, fmt.Errorf("probe target %d not found", dstVal)
		}
		t0 := time.Now()
		d, err := search.Dijkstra(c, src)
		elapsed := time.Since(t0)
		if err != nil {
			return probeResult{}, fmt.Errorf("dijkstra from %d: %w", srcVal, err)
		}
		res.latencies = append(res.latencies, elapsed)
		res.count++
		if _, reachable := d.Distance(dst); reachable {
			res.feasible++
		}
	}
	res.elapsed = time.Since(start)
	return res, nil
}

// probeCheckEvery bounds how often the probe loop polls ctx for
// cancellation: often enough that a cancelled large run stops promptly,
// rare enough that the check is free relative to a Dijkstra call.
const probeCheckEvery = 16

// singleSource runs a single-source Dijkstra query from the given node
// value, resolving it to its compact NodeID through the mapper first.
func singleSource(ctx context.Context, c *csr.CSR[int64], mapper *graph.Mapper[uint32], from int) (*search.Distances[int64], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src, ok := lookupVal(mapper, from)
	if !ok {
		return nil, fmt.Errorf("source node %d not found in graph", from)
	}
	d, err := search.Dijkstra(c, src)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// reachableCount counts how many of the cfg.vertices node values are
// reachable from the source the distances were computed for.
func reachableCount(d *search.Distances[int64], mapper *graph.Mapper[uint32], vertices int) int {
	n := 0
	for i := 0; i < vertices; i++ {
		id, ok := lookupVal(mapper, i)
		if !ok {
			continue
		}
		if _, reachable := d.Distance(id); reachable {
			n++
		}
	}
	return n
}

// lookupVal resolves an int node value to its compact NodeID through the
// mapper. The synthetic generator keys nodes by uint32, so the value is
// range-checked against uint32 before the lookup; an out-of-range value
// can never be a node and is reported as not found.
func lookupVal(mapper *graph.Mapper[uint32], v int) (graph.NodeID, bool) {
	if v < 0 || int64(v) > math.MaxUint32 {
		return 0, false
	}
	return mapper.Lookup(uint32(v))
}

// uintFromNonNeg widens a non-negative int to uint64. Callers pass values
// the boundary validation has already proven non-negative (vertices,
// edges, probe counts), so a negative input is a programmer error; it is
// clamped to 0 rather than wrapping to a huge unsigned value.
func uintFromNonNeg(v int) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// routeNodes resolves a path of NodeIDs back to a "0 -> 1 -> 2" string
// of node values through the mapper. It returns an error if any id
// cannot be resolved, which would indicate a corrupted result.
func routeNodes(mapper *graph.Mapper[uint32], path []graph.NodeID) (string, error) {
	parts := make([]string, len(path))
	for i, id := range path {
		val, ok := mapper.Resolve(id)
		if !ok {
			return "", fmt.Errorf("unresolved node id %d", id)
		}
		parts[i] = strconv.FormatUint(uint64(val), 10)
	}
	return strings.Join(parts, " -> "), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers
// ─────────────────────────────────────────────────────────────────────────────

// percentile returns the latency at the given fractional rank (0.5 =
// median, 0.95 = p95) using the nearest-rank method. It copies and sorts
// the input so callers need not pre-sort. Returns 0 for an empty slice.
func percentile(latencies []time.Duration, p float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

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
func rate(count uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// saturatingSub returns a-b, or 0 when b > a (heap can shrink between
// the two snapshots, which would otherwise underflow the unsigned
// subtraction).
func saturatingSub(a, b uint64) uint64 {
	if b > a {
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
