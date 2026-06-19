// Example 06_csv_import — an interchange round-trip benchmark for the
// edge-list serialisers: generate a seeded follower graph as CSV in
// memory, parse it back with [csv.ReadIntoCtx], then re-serialise the
// resulting graph as CSV with [csv.WriteCtx] and as newline-delimited
// JSON (JSON Lines) with [jsonl.WriteCtx], measuring each leg.
//
// It exists to exercise the io/csv and io/jsonl packages on a realistic,
// scale-parametrised dataset and to report the evidence that matters for
// an interchange subject — parse and serialise throughput (rows/s,
// MiB/s), bytes in and out for each format, and live heap — while pinning
// a deterministic data shape with a regression test.
//
// # Model
//
// The dataset is a directed follower graph: handles follow other
// handles. Each node is a 24-char hex id; each edge is one CSV row of
//
//	src,dst,weight
//
// where weight is a small integer in [1, weightMax] (think interaction
// strength). Every user is given a random out-degree in
// [followsMin, followsMax] to distinct other users — no self-loops and no
// duplicate (src,dst) pairs — so the graph is simple and the row count is
// exactly the edge count. This matters for the round-trip invariant: a
// simple directed graph re-serialises to exactly as many CSV rows as were
// ingested, with none collapsed by parallel-edge deduplication.
//
// # Pipeline
//
//  1. Generate a CSV edge list from the seeded RNG into an in-memory
//     buffer (this is the "bytes in").
//  2. Parse it back with [csv.ReadIntoCtx], genuinely exercising the CSV
//     reader (this is the parse leg).
//  3. Serialise the parsed graph to an in-memory buffer with
//     [csv.WriteCtx] (the CSV serialise leg; "bytes out, csv").
//  4. Serialise the same graph with [jsonl.WriteCtx] (the JSON Lines
//     serialise leg; "bytes out, jsonl"). JSON Lines emits one record per
//     node followed by one record per edge, so its line count is
//     nodes + edges.
//  5. Re-parse the written CSV with [csv.ReadIntoCtx] and confirm the
//     edge count is unchanged — the round-trip invariant.
//
// At the default scale the example does NOT dump the CSV or JSON Lines to
// stdout — that would be large and would read as non-deterministic. The
// round-trip output is written to in-memory buffers; only the
// deterministic facts and the volatile "# " telemetry are printed, plus a
// few sample lines of each format so a reader can see the shape.
//
// # Scale
//
// Run with no flags, the example builds a small deterministic default
// (1000 users, 3-6 follows each) that stays well under the short-test
// budget. Every dimension is a flag, so the same binary scales up to a
// size where the serialisers' throughput is actually observable:
//
//	go run ./examples/06_csv_import -nodes 1000000 -follows-max 12 -seed 7
//
// The deterministic data shape is reproducible for a fixed -seed; only
// the telemetry (lines prefixed with "# ") varies between runs and
// machines.
package main

import (
	"bufio"
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

	"github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// config captures every scale and shape knob of the benchmark. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	nodes      int   // number of follower nodes (:USER handles)
	followsMin int   // minimum out-degree per node (inclusive)
	followsMax int   // maximum out-degree per node (inclusive)
	weightMax  int   // edge weight is drawn from [1, weightMax]
	sampleN    int   // how many sample lines of each format to print
	seed       int64 // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins: a 1000-node follower graph with 3-6 follows per node. It is
// large enough to exercise the parse and serialise paths on thousands of
// rows yet builds and round-trips well under the short-layer 60 s budget.
func defaultConfig() config {
	return config{
		nodes:      1000,
		followsMin: 3,
		followsMax: 6,
		weightMax:  9,
		sampleN:    3,
		seed:       1,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape — for instance more follows than there are other nodes to follow.
// It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.nodes <= 0:
		return fmt.Errorf("nodes must be > 0, got %d", c.nodes)
	case c.followsMin < 0 || c.followsMax < c.followsMin:
		return fmt.Errorf("require 0 <= followsMin <= followsMax, got [%d,%d]", c.followsMin, c.followsMax)
	case c.followsMax >= c.nodes:
		return fmt.Errorf("followsMax (%d) exceeds nodes-1 (%d): not enough distinct nodes to follow", c.followsMax, c.nodes-1)
	case c.weightMax < 1:
		return fmt.Errorf("weightMax must be >= 1, got %d", c.weightMax)
	case c.sampleN < 0:
		return fmt.Errorf("sampleN must be >= 0, got %d", c.sampleN)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of follower nodes")
	flag.IntVar(&cfg.followsMin, "follows-min", cfg.followsMin, "minimum out-degree per node")
	flag.IntVar(&cfg.followsMax, "follows-max", cfg.followsMax, "maximum out-degree per node")
	flag.IntVar(&cfg.weightMax, "weight-max", cfg.weightMax, "edge weight drawn from [1, weight-max]")
	flag.IntVar(&cfg.sampleN, "sample", cfg.sampleN, "number of sample lines of each format to print")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the follower-graph edge list described by cfg, parses it
// back, re-serialises it as CSV and JSON Lines, and writes a report to w.
// Bare lines carry deterministic facts (counts and the round-trip
// invariant, reproducible for a fixed seed); lines prefixed with "# "
// carry volatile telemetry (durations, throughput, bytes, and heap
// figures) that vary per run and per machine. All output goes to w so a
// test can capture and assert on the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.follows=[%d,%d]\n", cfg.followsMin, cfg.followsMax)
	fmt.Fprintf(w, "config.weight_max=%d\n", cfg.weightMax)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// 1. Generate a CSV edge list into an in-memory buffer. genEdges is the
	// number of rows generated; because the generator emits a simple graph
	// (distinct targets, no self-loops) this equals the edge count the
	// reader will produce.
	var srcCSV bytes.Buffer
	genEdges, err := generateCSV(ctx, &srcCSV, cfg)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	bytesIn := srcCSV.Len()

	// 2. Parse the generated CSV back, genuinely exercising the reader.
	parseStart := time.Now()
	a, ingested, err := csv.ReadIntoCtx(ctx, bytes.NewReader(srcCSV.Bytes()), csv.DefaultOptions())
	if err != nil {
		return fmt.Errorf("csv.ReadIntoCtx: %w", err)
	}
	parseElapsed := time.Since(parseStart)

	// Order/Size are uint64; keep them so to avoid a narrowing conversion.
	nodes := a.Order()
	edges := a.Size()

	// 3. Serialise the parsed graph back to CSV in memory.
	var csvOut bytes.Buffer
	csvStart := time.Now()
	csvRows, err := csv.WriteCtx(ctx, &csvOut, a, csv.DefaultOptions())
	if err != nil {
		return fmt.Errorf("csv.WriteCtx: %w", err)
	}
	csvElapsed := time.Since(csvStart)
	bytesOutCSV := csvOut.Len()

	// 4. Serialise the same graph as JSON Lines in memory. jsonl emits one
	// record per node then one per edge, so jsonlRecords == nodes + edges.
	var jsonlOut bytes.Buffer
	jsonlStart := time.Now()
	jsonlRecords, err := jsonl.WriteCtx(ctx, &jsonlOut, a)
	if err != nil {
		return fmt.Errorf("jsonl.WriteCtx: %w", err)
	}
	jsonlElapsed := time.Since(jsonlStart)
	bytesOutJSONL := jsonlOut.Len()

	// 5. Round-trip invariant: re-parse the written CSV and confirm the
	// edge count is unchanged.
	rt, rtEdges, err := csv.ReadIntoCtx(ctx, bytes.NewReader(csvOut.Bytes()), csv.DefaultOptions())
	if err != nil {
		return fmt.Errorf("csv.ReadIntoCtx (round-trip): %w", err)
	}
	roundTripEdges := rt.Size()

	// Deterministic facts — pinned by the regression test.
	fmt.Fprintf(w, "generated.rows=%d\n", genEdges)
	fmt.Fprintf(w, "ingested.rows=%d\n", ingested)
	fmt.Fprintf(w, "graph.nodes=%d\n", nodes)
	fmt.Fprintf(w, "graph.edges=%d\n", edges)
	fmt.Fprintf(w, "csv.rows_out=%d\n", csvRows)
	fmt.Fprintf(w, "jsonl.records_out=%d\n", jsonlRecords)
	fmt.Fprintf(w, "jsonl.expected_records=%d\n", nodes+edges)
	fmt.Fprintf(w, "roundtrip.csv_reread_rows=%d\n", rtEdges)
	fmt.Fprintf(w, "roundtrip.edges=%d\n", roundTripEdges)

	// Volatile telemetry — never pinned.
	fmt.Fprintf(w, "# parse.elapsed=%s\n", parseElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# parse.row_rate=%.0f rows/s\n", rate(ingested, parseElapsed))
	fmt.Fprintf(w, "# parse.throughput=%s/s\n", humanBytesRate(bytesIn, parseElapsed))
	fmt.Fprintf(w, "# csv.serialise.elapsed=%s\n", csvElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# csv.serialise.row_rate=%.0f rows/s\n", rate(csvRows, csvElapsed))
	fmt.Fprintf(w, "# csv.serialise.throughput=%s/s\n", humanBytesRate(bytesOutCSV, csvElapsed))
	fmt.Fprintf(w, "# jsonl.serialise.elapsed=%s\n", jsonlElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# jsonl.serialise.row_rate=%.0f records/s\n", rate(jsonlRecords, jsonlElapsed))
	fmt.Fprintf(w, "# jsonl.serialise.throughput=%s/s\n", humanBytesRate(bytesOutJSONL, jsonlElapsed))
	fmt.Fprintf(w, "# bytes.in_csv=%s\n", humanBytesInt(bytesIn))
	fmt.Fprintf(w, "# bytes.out_csv=%s\n", humanBytesInt(bytesOutCSV))
	fmt.Fprintf(w, "# bytes.out_jsonl=%s\n", humanBytesInt(bytesOutJSONL))
	fmt.Fprintf(w, "# bytes.per_edge_csv=%.1f\n", safeDiv(float64(bytesOutCSV), float64(edges)))
	fmt.Fprintf(w, "# bytes.per_record_jsonl=%.1f\n", safeDiv(float64(bytesOutJSONL), float64(jsonlRecords)))

	built := readMem()
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))
	fmt.Fprintf(w, "# mem.total_alloc=%s\n", humanBytes(built.TotalAlloc-base.TotalAlloc))
	fmt.Fprintf(w, "# mem.num_gc=%d\n", built.NumGC-base.NumGC)

	// A few sample lines of each format, so a reader can see the shape
	// without dumping the whole (potentially huge) serialisation.
	if cfg.sampleN > 0 {
		fmt.Fprintf(w, "# sample.csv (first %d rows):\n", cfg.sampleN)
		writeSample(w, csvOut.String(), cfg.sampleN)
		fmt.Fprintf(w, "# sample.jsonl (first %d records):\n", cfg.sampleN)
		writeSample(w, jsonlOut.String(), cfg.sampleN)
	}

	return nil
}

// generateCSV writes a seeded directed follower-graph edge list to w in
// src,dst,weight order and returns the number of rows written. Each of
// cfg.nodes handles is given a random out-degree in
// [followsMin, followsMax] to distinct other handles (no self-loops, no
// duplicate targets), so the result is a simple directed graph and the
// row count equals the edge count. A leading "# " comment row documents
// the file; csv.ReadInto skips it. The build honours ctx cancellation on
// a periodic check.
func generateCSV(ctx context.Context, w io.Writer, cfg config) (int, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))

	// Pre-assign every handle so each edge's endpoints are drawn from the
	// same fixed, reproducible id set.
	ids := make([]string, cfg.nodes)
	seen := make(map[string]struct{}, cfg.nodes)
	for i := range ids {
		ids[i] = uniqueHexID(rng, seen)
	}

	bw := bufio.NewWriter(w)
	if _, err := fmt.Fprintf(bw, "# follower graph: %d nodes, %d..%d follows each, seed %d\n",
		cfg.nodes, cfg.followsMin, cfg.followsMax, cfg.seed); err != nil {
		return 0, err
	}

	rows := 0
	targets := make(map[int]struct{}, cfg.followsMax)
	for i := 0; i < cfg.nodes; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return rows, err
			}
		}
		degree := cfg.followsMin + rng.Intn(cfg.followsMax-cfg.followsMin+1)
		clear(targets)
		for len(targets) < degree {
			j := rng.Intn(cfg.nodes)
			if j == i {
				continue
			}
			targets[j] = struct{}{}
		}
		for j := range targets {
			weight := 1 + rng.Intn(cfg.weightMax)
			if _, err := fmt.Fprintf(bw, "%s,%s,%d\n", ids[i], ids[j], weight); err != nil {
				return rows, err
			}
			rows++
		}
	}
	if err := bw.Flush(); err != nil {
		return rows, err
	}
	return rows, nil
}

// checkEvery bounds how often the generator polls ctx for cancellation:
// often enough that a cancelled large run stops promptly, rare enough
// that the check is free relative to the surrounding work.
const checkEvery = 4096

// uniqueHexID returns a 24-character lowercase hex id (12 random bytes)
// that has not been handed out before, recording it in seen. The bytes are
// drawn from the seeded rng (rand.Rand.Read fills the buffer directly and
// always succeeds), so the whole dataset is reproducible for a fixed seed.
func uniqueHexID(rng *rand.Rand, seen map[string]struct{}) string {
	var b [12]byte
	for {
		_, _ = rng.Read(b[:]) // never errors; deterministic for the seed
		id := hex.EncodeToString(b[:])
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		return id
	}
}

// writeSample writes the first n lines of s to w, each prefixed with
// "# " so the samples count as telemetry (they reflect the RNG draw and
// are not pinned by the regression test).
func writeSample(w io.Writer, s string, n int) {
	count := 0
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintf(w, "# %s\n", line)
		count++
		if count >= n {
			return
		}
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

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
func rate(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// humanBytesRate formats n bytes transferred over elapsed as a binary
// throughput (e.g. "131.01 MiB"); the caller appends "/s". It returns
// "0 B" for a zero-length interval.
func humanBytesRate(n int, elapsed time.Duration) string {
	if elapsed <= 0 || n < 0 {
		return humanBytes(0)
	}
	return humanBytes(uint64(float64(n) / elapsed.Seconds()))
}

// humanBytesInt formats a non-negative byte count (such as a
// bytes.Buffer length) with a binary suffix. A negative input — which
// cannot arise from a buffer length — formats as "0 B".
func humanBytesInt(n int) string {
	if n < 0 {
		return humanBytes(0)
	}
	return humanBytes(uint64(n))
}

// safeDiv divides a by b, returning 0 when b is 0.
func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
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
