// Example 07_graphml_roundtrip — a GraphML interchange round-trip over a
// realistic, seeded link graph.
//
// It generates a directed link graph in memory (a miniature "web" of
// documents that cite one another), serialises it to GraphML, parses
// that GraphML back with [graphml.ReadInto] so the reader is genuinely
// exercised, and then re-serialises the parsed graph to both GraphML and
// Graphviz DOT. It reports the deterministic shape of the data and the
// round-trip invariant (node/edge counts and a weight checksum survive
// the trip) as bare fact lines, and the volatile interchange telemetry —
// bytes in/out per format, parse and serialise throughput, and Go heap —
// as "# "-prefixed lines.
//
// # Model
//
//	(page)-[link {weight}]->(page)   // a directed, weighted link graph
//
// Pages are 12-character lowercase hex ids drawn from the seeded RNG.
// The graph is grown by preferential attachment: page i links to a small
// number of distinct earlier pages, with an earlier page chosen with
// probability proportional to the in-degree it has already accumulated.
// That yields a heavy-tailed in-degree distribution — a handful of
// "authority" pages collect most links — which is the realistic shape of
// a citation or hyperlink web and a more interesting interchange payload
// than a uniform-random graph. Every link carries a positive integer
// weight in [1, maxWeight], stored as the GraphML <data key="w"> long
// that [graphml.ReadInto] reads back and [dot.Write] renders as a
// label="..." edge attribute.
//
// The graph is a simple directed graph (no self-loops, no parallel
// edges), so the GraphML reader — which collapses parallel edges and is
// directed when edgedefault is not "undirected" — re-materialises it
// edge-for-edge. That makes the round-trip exact: re-reading the written
// GraphML yields the same node count, the same edge count, and the same
// weight sum.
//
// # Scale
//
// Run with no flags, the example builds a small, deterministic default
// (a few hundred pages) that the regression test pins and that finishes
// in well under the short-test budget. Every dimension is a flag, so the
// same binary scales up to where parse/serialise throughput and the
// interchange byte footprint become observable:
//
//	go run ./examples/07_graphml_roundtrip -nodes 1000000 -edges 12 -seed 7
//
// The deterministic facts are reproducible for a fixed -seed; only the
// telemetry (lines prefixed with "# ") varies between runs and machines.
// At the default scale the example does not dump the GraphML or DOT
// documents to stdout — it serialises to in-memory buffers and reports
// their byte sizes — but the -sample flag prints the first few lines of
// each serialised format for a quick visual check.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/dot"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
)

// config captures every scale and shape knob of the round-trip. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	nodes        int   // number of pages (:node) to generate
	edgesPerNode int   // out-degree target per page (capped at the earlier-page count)
	maxWeight    int   // link weights are drawn uniformly from [1, maxWeight]
	seed         int64 // RNG seed; fixes the deterministic data shape
	sampleLines  int   // when > 0, print this many leading lines of each serialised format
}

// defaultConfig returns the small, deterministic default the regression
// test pins: a few hundred pages with a modest out-degree, enough to
// exercise the reader and writer on a non-trivial heavy-tailed graph
// while staying far under the short-test budget.
func defaultConfig() config {
	return config{
		nodes:        300,
		edgesPerNode: 4,
		maxWeight:    100,
		seed:         1,
		sampleLines:  0,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.nodes <= 0:
		return fmt.Errorf("nodes must be > 0, got %d", c.nodes)
	case c.edgesPerNode < 0:
		return fmt.Errorf("edges must be >= 0, got %d", c.edgesPerNode)
	case c.maxWeight < 1:
		return fmt.Errorf("max-weight must be >= 1, got %d", c.maxWeight)
	case c.sampleLines < 0:
		return fmt.Errorf("sample must be >= 0, got %d", c.sampleLines)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of pages (nodes) to generate")
	flag.IntVar(&cfg.edgesPerNode, "edges", cfg.edgesPerNode, "out-degree target per page (capped at the earlier-page count)")
	flag.IntVar(&cfg.maxWeight, "max-weight", cfg.maxWeight, "link weights are drawn uniformly from [1, max-weight]")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.IntVar(&cfg.sampleLines, "sample", cfg.sampleLines, "print this many leading lines of each serialised format (0: none)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the link graph described by cfg, round-trips it through
// GraphML, re-serialises the parsed graph to GraphML and DOT, and writes
// a report to w. Bare lines carry deterministic facts (counts and the
// weight checksum, reproducible for a fixed seed); lines prefixed with
// "# " carry volatile telemetry (byte sizes, throughput, and heap) that
// vary per run and per machine. All output goes to w so a test can
// capture and assert on the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.edges_per_node=%d\n", cfg.edgesPerNode)
	fmt.Fprintf(w, "config.max_weight=%d\n", cfg.maxWeight)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// 1. Generate the source graph in memory.
	src, srcEdges, srcWeightSum, err := generate(ctx, cfg)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	srcNodes := src.Order()
	fmt.Fprintf(w, "nodes=%d\n", srcNodes)
	fmt.Fprintf(w, "edges=%d\n", srcEdges)
	fmt.Fprintf(w, "weight.sum=%d\n", srcWeightSum)

	// 2. Serialise the source graph to GraphML (the interchange payload).
	var graphmlIn bytes.Buffer
	serStart := time.Now()
	if err := graphml.WriteCtx(ctx, &graphmlIn, src); err != nil {
		return fmt.Errorf("graphml.Write (source): %w", err)
	}
	serIn := time.Since(serStart)

	// 3. Parse that GraphML back, exercising the reader. ReadIntoCtx is
	//    given the freshly written document so the parse path runs over a
	//    realistic, generated payload rather than a hand-written literal.
	parseStart := time.Now()
	parsed, parsedEdges, err := graphml.ReadIntoCtx(ctx, bytes.NewReader(graphmlIn.Bytes()))
	if err != nil {
		return fmt.Errorf("graphml.ReadInto: %w", err)
	}
	parseDur := time.Since(parseStart)
	parsedNodes := parsed.Order()
	parsedWeightSum := weightSum(parsed)

	fmt.Fprintf(w, "graphml.parsed_nodes=%d\n", parsedNodes)
	fmt.Fprintf(w, "graphml.parsed_edges=%d\n", parsedEdges)

	// 4. Re-serialise the parsed graph to GraphML and DOT, edges and
	//    weights intact.
	var graphmlOut bytes.Buffer
	serOutStart := time.Now()
	if err := graphml.WriteCtx(ctx, &graphmlOut, parsed); err != nil {
		return fmt.Errorf("graphml.Write (parsed): %w", err)
	}
	serOut := time.Since(serOutStart)

	var dotOut bytes.Buffer
	dotStart := time.Now()
	if err := dot.WriteCtx(ctx, &dotOut, parsed); err != nil {
		return fmt.Errorf("dot.Write: %w", err)
	}
	dotDur := time.Since(dotStart)

	// 5. Deterministic round-trip invariant: re-reading the written
	//    GraphML must yield the same node count, the same edge count, and
	//    the same weight checksum as the source graph. roundtrip.ok=1
	//    asserts all three at once; the individual figures are surfaced so
	//    a mismatch is diagnosable.
	roundTripOK := parsedNodes == srcNodes &&
		parsedEdges == srcEdges &&
		parsedWeightSum == srcWeightSum
	fmt.Fprintf(w, "graphml.written_bytes=%d\n", graphmlOut.Len())
	fmt.Fprintf(w, "dot.written_edges=%d\n", parsedEdges)
	fmt.Fprintf(w, "roundtrip.weight_sum=%d\n", parsedWeightSum)
	fmt.Fprintf(w, "roundtrip.ok=%d\n", boolToInt(roundTripOK))

	// Telemetry: interchange byte footprint, throughput, and live heap.
	cur := readMem()
	fmt.Fprintf(w, "# graphml.in_bytes=%s\n", humanLen(graphmlIn.Len()))
	fmt.Fprintf(w, "# graphml.out_bytes=%s\n", humanLen(graphmlOut.Len()))
	fmt.Fprintf(w, "# dot.out_bytes=%s\n", humanLen(dotOut.Len()))
	fmt.Fprintf(w, "# graphml.parse.elapsed=%s\n", parseDur.Round(time.Microsecond))
	fmt.Fprintf(w, "# graphml.parse.node_rate=%.0f nodes/s\n", rate(parsedNodes, parseDur))
	fmt.Fprintf(w, "# graphml.parse.throughput=%s\n", mibPerSec(graphmlIn.Len(), parseDur))
	fmt.Fprintf(w, "# graphml.serialise.elapsed=%s\n", serIn.Round(time.Microsecond))
	fmt.Fprintf(w, "# graphml.serialise.throughput=%s\n", mibPerSec(graphmlIn.Len(), serIn))
	fmt.Fprintf(w, "# graphml.reserialise.elapsed=%s\n", serOut.Round(time.Microsecond))
	fmt.Fprintf(w, "# graphml.reserialise.throughput=%s\n", mibPerSec(graphmlOut.Len(), serOut))
	fmt.Fprintf(w, "# dot.serialise.elapsed=%s\n", dotDur.Round(time.Microsecond))
	fmt.Fprintf(w, "# dot.serialise.throughput=%s\n", mibPerSec(dotOut.Len(), dotDur))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(cur.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(cur.HeapAlloc-base.HeapAlloc))

	// Optional human-readable sample of each serialised format.
	if cfg.sampleLines > 0 {
		printSample(w, "GraphML", graphmlOut.String(), cfg.sampleLines)
		printSample(w, "DOT", dotOut.String(), cfg.sampleLines)
	}

	return nil
}

// generate builds the seeded, preferential-attachment link graph
// described by cfg into a fresh simple directed adjacency list. It
// returns the graph, the exact number of edges added, and the sum of all
// edge weights — the two checksums the round-trip invariant pins. The
// build honours ctx cancellation on a periodic check.
func generate(ctx context.Context, cfg config) (*adjlist.AdjList[string, int64], int, int64, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))

	// A simple directed graph: no self-loops and no parallel edges, so the
	// GraphML reader (which is directed and collapses parallel edges)
	// re-materialises it edge-for-edge and the round-trip is exact.
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})

	ids := make([]string, cfg.nodes)
	seen := make(map[string]struct{}, cfg.nodes)

	// inDegree drives preferential attachment: an earlier page is chosen as
	// a link target with probability proportional to the in-degree it has
	// already accumulated, plus one (so a page with no links yet can still
	// be chosen). cumulative is the running prefix-sum scratch reused per
	// source to avoid per-edge allocation.
	inDegree := make([]int, cfg.nodes)
	cumulative := make([]int64, 0, cfg.nodes)

	var edges int
	var weightSum int64
	targets := make(map[int]struct{}, cfg.edgesPerNode)

	for i := range cfg.nodes {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
		}
		id := uniqueHexID(rng, seen)
		ids[i] = id
		if err := a.AddNode(id); err != nil {
			return nil, 0, 0, fmt.Errorf("AddNode %s: %w", id, err)
		}

		// Link page i to up to edgesPerNode distinct earlier pages. The
		// first page has no earlier pages, so it is link-free; the second
		// can have at most one link, and so on, which is the natural shape
		// of a growing citation web.
		degree := min(cfg.edgesPerNode, i)
		if degree == 0 {
			continue
		}
		clear(targets)
		for len(targets) < degree {
			j := preferentialPick(rng, inDegree[:i], &cumulative)
			if _, dup := targets[j]; dup {
				continue
			}
			targets[j] = struct{}{}
		}
		for j := range targets {
			weight := int64(1 + rng.Intn(cfg.maxWeight))
			if err := a.AddEdge(id, ids[j], weight); err != nil {
				return nil, 0, 0, fmt.Errorf("AddEdge %s->%s: %w", id, ids[j], err)
			}
			inDegree[j]++
			edges++
			weightSum += weight
		}
	}
	return a, edges, weightSum, nil
}

// preferentialPick returns the index of an earlier page chosen with
// probability proportional to (inDegree+1), implementing preferential
// attachment. cumulative is a caller-owned scratch slice reused across
// calls; it is rebuilt as the prefix sum of (inDegree[k]+1) and then
// binary-searched for the random draw.
func preferentialPick(rng *rand.Rand, inDegree []int, cumulative *[]int64) int {
	cum := (*cumulative)[:0]
	var total int64
	for _, d := range inDegree {
		total += int64(d) + 1
		cum = append(cum, total)
	}
	*cumulative = cum
	r := rng.Int63n(total)
	// Smallest index whose cumulative weight is strictly greater than r.
	lo, hi := 0, len(cum)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if cum[mid] <= r {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// checkEvery bounds how often the build polls ctx for cancellation:
// often enough that a cancelled large build stops promptly, rare enough
// that the check is free relative to the surrounding work.
const checkEvery = 4096

// weightSum returns the sum of every edge weight in a, iterating the
// adjacency by ascending NodeID. It is the checksum the round-trip
// invariant compares against the source graph's sum.
func weightSum(a *adjlist.AdjList[string, int64]) int64 {
	var sum int64
	maxID := uint64(a.MaxNodeID())
	for id := uint64(0); id < maxID; id++ {
		_, ws := a.LoadEntry(graph.NodeID(id))
		for _, w := range ws {
			sum += w
		}
	}
	return sum
}

// uniqueHexID returns a 12-character lowercase hex id (6 random bytes)
// that has not been handed out before, recording it in seen. The bytes
// are drawn from the seeded rng via Rand.Read — which is deterministic
// for a fixed seed and always succeeds for a *math/rand.Rand — so the
// whole dataset is reproducible.
func uniqueHexID(rng *rand.Rand, seen map[string]struct{}) string {
	var b [6]byte
	for {
		_, _ = rng.Read(b[:]) // *rand.Rand.Read never fails; result is deterministic
		id := hex.EncodeToString(b[:])
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		return id
	}
}

// printSample writes the first n lines of doc to w under a labelled,
// "# "-prefixed header, so the sample never collides with the
// deterministic fact lines a test asserts.
func printSample(w io.Writer, label, doc string, n int) {
	fmt.Fprintf(w, "# --- %s sample (first %d lines) ---\n", label, n)
	lines := strings.SplitN(doc, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	for _, line := range lines {
		fmt.Fprintf(w, "# %s\n", line)
	}
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
func rate(count uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// mibPerSec formats nbytes/elapsed as a MiB/s throughput string, or
// "0.00 MiB/s" for a zero-length interval.
func mibPerSec(nbytes int, elapsed time.Duration) string {
	if elapsed <= 0 {
		return "0.00 MiB/s"
	}
	mib := float64(nbytes) / (1024 * 1024)
	return fmt.Sprintf("%.2f MiB/s", mib/elapsed.Seconds())
}

// humanLen formats a non-negative buffer length (a bytes.Buffer.Len, which
// is never negative) with a binary suffix. The negatives-to-zero guard
// makes the int-to-uint64 widening provably in range.
func humanLen(n int) string {
	if n < 0 {
		n = 0
	}
	return humanBytes(uint64(n))
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

// boolToInt maps true to 1 and false to 0 so a boolean invariant can be
// reported as a bare integer fact line (roundtrip.ok=1).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
