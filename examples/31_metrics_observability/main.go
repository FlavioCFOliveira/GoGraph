// Example 31_metrics_observability — GoGraph's observability surface,
// driven end-to-end over a realistic, seeded service-mesh call graph.
//
// GoGraph instruments every public blocking API with a latency observation
// and a paired error/utilisation counter (docs/metrics.md). The whole
// surface is dark until a consumer installs a metrics.Backend: the public
// metrics.NewPrometheusRegistry + metrics.SetBackend facade activates
// dispatch and exposes the observations in Prometheus text-exposition
// format over an HTTP endpoint. This example is the one demonstration of
// that surface — the project mandates "latency histograms on every public
// blocking API", and this shows an operator how to turn them on, drive a
// mixed workload across subsystems, and scrape the result.
//
// # What it does
//
//  1. Installs a Prometheus registry as the global metrics backend
//     (metrics.NewPrometheusRegistry + metrics.SetBackend), and restores
//     the no-op default on exit so no global state leaks.
//  2. Generates a seeded service-mesh call graph and materialises it into
//     three representations that feed instrumented APIs.
//  3. Runs a mixed workload that touches instrumented entry points across
//     subsystems:
//     - cypher.Engine.Run   — a label-scan count, run twice so the plan
//     cache records a miss then a hit;
//     - cypher.Engine.RunInTx — a canary CREATE, its effect verified by a
//     follow-up count;
//     - search.Dijkstra     — shortest-latency single-source paths over a
//     CSR built from the call graph;
//     - graph/io/csv        — a WriteCtx + ReadIntoCtx round-trip;
//     - bolt/packstream     — an encoder acquired from and returned to the
//     pooled EncodePool.
//  4. Serves reg.Handler() over a local httptest server and GETs /metrics,
//     exactly as an operator's Prometheus scrape would.
//  5. Parses the exposition and reports, as deterministic FACT lines,
//     whether every expected metric NAME is present (the schema
//     <package-path>.<Symbol> from docs/metrics.md, rendered by the backend
//     with dots mapped to underscores). The observed latencies and counts
//     are volatile and reported as "# " telemetry.
//
// # Facts vs telemetry
//
// Metric NAMES are deterministic: a fixed workload always touches the same
// instrumented sites, so the presence facts are reproducible and pinned by
// the regression test. Metric VALUES (histogram sums and bucket counts,
// scrape byte size, wall-clock latency, live heap) vary per run and per
// machine, so they are emitted as "# "-prefixed telemetry and never pinned.
//
// # Scale
//
// Run with no flags, the example builds a small deterministic default —
// two hundred services with two-to-six downstream calls each — so the run
// is instant and the presence facts are pinned by the regression test.
// Every dimension is a flag, so the same binary scales up to a size where
// the latency histograms carry a meaningful distribution:
//
//	go run ./examples/31_metrics_observability -services 200000 -calls-max 12 -seed 7
//
// The metric names are identical at every scale; only the "# " telemetry
// varies between runs and machines.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/metrics"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

const (
	labelService = "SERVICE"
	relCalls     = "CALLS" // (:SERVICE)-[:CALLS {latency_ms}]->(:SERVICE)

	propTier      = "tier"
	propLatencyMs = "latency_ms"

	// canaryName is the SERVICE created by the write transaction; its tier
	// distinguishes it from the generated services.
	canaryName = "svc-canary"
	canaryTier = "canary"

	// countQuery counts SERVICE nodes. Running it twice back-to-back yields
	// a plan-cache miss (first compile) then a hit (second, cached) — the
	// deterministic driver for the cypher.plan_cache.{misses,hits} counters.
	countQuery = "MATCH (s:SERVICE) RETURN count(s) AS c"
)

// tiers are the deployment tiers a service can belong to; assigned round
// robin by index so the property-graph nodes carry a realistic label.
var tiers = []string{"gateway", "edge", "core", "data"}

// config captures every scale and shape knob of the example. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	services int   // number of :SERVICE nodes
	callsMin int   // minimum CALLS out-degree per service (inclusive)
	callsMax int   // maximum CALLS out-degree per service (inclusive)
	seed     int64 // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins: two hundred services, two-to-six downstream calls each. It
// builds and drives the whole workload instantly, well under the
// short-layer 60 s package budget.
func defaultConfig() config {
	return config{
		services: 200,
		callsMin: 2,
		callsMax: 6,
		seed:     1,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.services <= 0:
		return fmt.Errorf("services must be > 0, got %d", c.services)
	case c.callsMin < 1:
		return fmt.Errorf("callsMin must be >= 1 so every service has a downstream call, got %d", c.callsMin)
	case c.callsMax < c.callsMin:
		return fmt.Errorf("require callsMin <= callsMax, got [%d,%d]", c.callsMin, c.callsMax)
	case c.callsMax >= c.services:
		return fmt.Errorf("callsMax (%d) exceeds services-1 (%d): not enough distinct callees", c.callsMax, c.services-1)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.services, "services", cfg.services, "number of SERVICE nodes")
	flag.IntVar(&cfg.callsMin, "calls-min", cfg.callsMin, "minimum CALLS out-degree per service")
	flag.IntVar(&cfg.callsMax, "calls-max", cfg.callsMax, "maximum CALLS out-degree per service")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run installs the Prometheus metrics backend, builds the seeded call
// graph, drives the mixed workload across instrumented subsystems, scrapes
// the exposition, and writes a report to w. Bare lines carry deterministic
// facts (domain counts, conservation laws, and which metric names are
// present); lines prefixed with "# " carry volatile telemetry (observed
// latencies and counts, scrape size, wall-clock, live heap). run honours
// ctx cancellation, returns wrapped errors rather than terminating the
// process, and always restores the no-op metrics backend before returning.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Install the Prometheus registry as the global metrics backend BEFORE
	// any instrumented API runs. The defer restores the no-op default so the
	// process is left with no global metrics state (metrics.SetBackend is a
	// process-global atomic.Pointer swap).
	reg := metrics.NewPrometheusRegistry()
	metrics.SetBackend(reg)
	defer metrics.SetBackend(nil)

	fmt.Fprintf(w, "config.services=%d\n", cfg.services)
	fmt.Fprintf(w, "config.calls=[%d,%d]\n", cfg.callsMin, cfg.callsMax)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	start := time.Now()
	calls := generate(cfg)

	g, err := buildLPG(ctx, calls, cfg)
	if err != nil {
		return fmt.Errorf("build lpg: %w", err)
	}
	adj, err := buildAdjList(ctx, calls, cfg)
	if err != nil {
		return fmt.Errorf("build adjlist: %w", err)
	}
	csrGraph := csr.BuildFromAdjList(adj)

	fmt.Fprintf(w, "nodes.services=%d\n", cfg.services)
	fmt.Fprintf(w, "edges.calls=%d\n", len(calls))

	if err := driveWorkload(ctx, w, g, adj, csrGraph, cfg); err != nil {
		return fmt.Errorf("workload: %w", err)
	}

	// Scrape the exposition exactly as a Prometheus server would: serve the
	// registry's HTTP handler and GET /metrics.
	text, err := scrape(ctx, reg)
	if err != nil {
		return fmt.Errorf("scrape: %w", err)
	}

	reportMetrics(w, text)

	fmt.Fprintf(w, "# workload.elapsed=%s\n", time.Since(start).Round(time.Microsecond))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(readMem().HeapAlloc))
	fmt.Fprintf(w, "# scrape.bytes=%d\n", len(text))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Seeded generator — a service-mesh call graph
// ─────────────────────────────────────────────────────────────────────────────

// call is one directed RPC dependency: service src calls service dst, and
// the call is attributed a synthetic latency (microseconds) drawn from the
// seeded RNG. Microseconds keep the weight an integer for the int64-weighted
// adjacency list that feeds Dijkstra and the CSV round-trip; the LPG graph
// stores the same figure as float milliseconds.
type call struct {
	src, dst      int
	latencyMicros int64
}

// callLatencyMin and callLatencyMax bound the synthetic per-call latency the
// generator draws from (microseconds, inclusive): 0.2 ms to 5 ms.
const (
	callLatencyMin = 200
	callLatencyMax = 5000
)

// generate produces the deterministic call graph described by cfg: each
// service is given a random out-degree in [callsMin, callsMax] to distinct
// other services, and each call a synthetic latency. The result is a flat
// edge list; the same list feeds every representation so the LPG graph, the
// adjacency list, and the CSR describe the same topology.
func generate(cfg config) []call {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	calls := make([]call, 0, cfg.services*cfg.callsMax)
	targets := make(map[int]struct{}, cfg.callsMax)
	for i := 0; i < cfg.services; i++ {
		degree := cfg.callsMin + rng.Intn(cfg.callsMax-cfg.callsMin+1)
		clear(targets)
		for len(targets) < degree {
			j := rng.Intn(cfg.services)
			if j == i {
				continue
			}
			targets[j] = struct{}{}
		}
		// Materialise the target set in ascending order so the edge list is a
		// deterministic function of the seed (map iteration order is not).
		ordered := make([]int, 0, len(targets))
		for j := range targets {
			ordered = append(ordered, j)
		}
		sort.Ints(ordered)
		for _, j := range ordered {
			lat := int64(callLatencyMin + rng.Intn(callLatencyMax-callLatencyMin+1))
			calls = append(calls, call{src: i, dst: j, latencyMicros: lat})
		}
	}
	return calls
}

// svcName returns the deterministic, unique node key for service index i.
func svcName(i int) string { return fmt.Sprintf("svc-%04d", i) }

// checkEvery bounds how often the builders poll ctx for cancellation.
const checkEvery = 4096

// buildLPG materialises the call graph into a labelled property graph for
// the Cypher workload: (:SERVICE {tier}) nodes and (:SERVICE)-[:CALLS
// {latency_ms}]->(:SERVICE) edges. Multigraph is enabled so the write
// transaction's CREATE follows openCypher semantics.
func buildLPG(ctx context.Context, calls []call, cfg config) (*lpg.Graph[string, float64], error) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < cfg.services; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		name := svcName(i)
		if err := g.AddNode(name); err != nil {
			return nil, fmt.Errorf("AddNode %s: %w", name, err)
		}
		if err := g.SetNodeLabel(name, labelService); err != nil {
			return nil, fmt.Errorf("SetNodeLabel %s: %w", name, err)
		}
		if err := g.SetNodeProperty(name, propTier, lpg.StringValue(tiers[i%len(tiers)])); err != nil {
			return nil, fmt.Errorf("SetNodeProperty %s: %w", name, err)
		}
	}
	for k := range calls {
		if k%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		c := calls[k]
		ms := float64(c.latencyMicros) / 1000.0
		if err := g.AddEdgeLabeled(svcName(c.src), svcName(c.dst), ms, relCalls); err != nil {
			return nil, fmt.Errorf("AddEdgeLabeled: %w", err)
		}
		if err := g.SetEdgeProperty(svcName(c.src), svcName(c.dst), propLatencyMs, lpg.Float64Value(ms)); err != nil {
			return nil, fmt.Errorf("SetEdgeProperty: %w", err)
		}
	}
	g.AdjList().Compact(ctx)
	return g, nil
}

// buildAdjList materialises the same topology into an int64-weighted
// directed adjacency list (weight = latency in microseconds). This single
// structure feeds both the CSV round-trip and, via csr.BuildFromAdjList,
// the Dijkstra shortest-latency query.
func buildAdjList(ctx context.Context, calls []call, cfg config) (*adjlist.AdjList[string, int64], error) {
	adj := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < cfg.services; i++ {
		if err := adj.AddNode(svcName(i)); err != nil {
			return nil, fmt.Errorf("AddNode %s: %w", svcName(i), err)
		}
	}
	for k := range calls {
		if k%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		c := calls[k]
		if err := adj.AddEdge(svcName(c.src), svcName(c.dst), c.latencyMicros); err != nil {
			return nil, fmt.Errorf("AddEdge: %w", err)
		}
	}
	adj.Compact(ctx)
	return adj, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Mixed workload — one call per instrumented subsystem
// ─────────────────────────────────────────────────────────────────────────────

// driveWorkload exercises one instrumented entry point in each subsystem and
// writes the deterministic domain facts each produces. The metric emissions
// are a side effect captured by the installed Prometheus backend; this
// function only records the domain-level results.
func driveWorkload(
	ctx context.Context,
	w io.Writer,
	g *lpg.Graph[string, float64],
	adj *adjlist.AdjList[string, int64],
	csrGraph *csr.CSR[int64],
	cfg config,
) error {
	eng := cypher.NewEngine(g)

	// 1+2. Cypher reads via Engine.Run. Run the count query twice, so the
	//      plan cache records exactly one miss (first compile) then one hit.
	before, err := cypherCount(ctx, eng, countQuery)
	if err != nil {
		return fmt.Errorf("count (miss): %w", err)
	}
	if _, err = cypherCount(ctx, eng, countQuery); err != nil {
		return fmt.Errorf("count (hit): %w", err)
	}

	// 3. Cypher write via Engine.RunInTx — create the canary SERVICE, then
	//    confirm the node count grew by exactly one.
	if err = cypherWrite(ctx, eng,
		"CREATE (s:SERVICE {name:$name, tier:$tier})",
		map[string]expr.Value{"name": expr.StringValue(canaryName), "tier": expr.StringValue(canaryTier)},
	); err != nil {
		return fmt.Errorf("canary create: %w", err)
	}
	after, err := cypherCount(ctx, eng, countQuery)
	if err != nil {
		return fmt.Errorf("count (after write): %w", err)
	}
	fmt.Fprintf(w, "cypher.services_before=%d\n", before)
	fmt.Fprintf(w, "cypher.services_after=%d\n", after)
	fmt.Fprintf(w, "cypher.write_delta=%d\n", after-before)

	// 4. search.Dijkstra over the CSR — shortest-latency paths from svc-0000.
	//    The reachable count is a deterministic function of the seed.
	src, ok := adj.Mapper().Lookup(svcName(0))
	if !ok {
		return fmt.Errorf("source %s not found in mapper", svcName(0))
	}
	dist, err := search.Dijkstra(csrGraph, src)
	if err != nil {
		return fmt.Errorf("dijkstra: %w", err)
	}
	reached := countReachable(adj, dist, cfg.services)
	fmt.Fprintf(w, "dijkstra.src_reached=%d\n", reached)

	// 5. graph/io/csv round-trip — write the edge list, read it back, and
	//    assert the edge count is conserved.
	match, err := csvRoundTrip(ctx, adj)
	if err != nil {
		return fmt.Errorf("csv round-trip: %w", err)
	}
	fmt.Fprintf(w, "csv.roundtrip.edges_match=%d\n", boolFact(match))

	// 6. bolt/packstream encoder pool — acquire, encode a small record,
	//    return. Exercises the pooled EncodePool get/put counters.
	if err = boltEncode(); err != nil {
		return fmt.Errorf("bolt encode: %w", err)
	}
	return nil
}

// cypherCount runs a scalar count query via Engine.Run and returns the
// integer in column c.
func cypherCount(ctx context.Context, eng *cypher.Engine, query string) (int64, error) {
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()

	var n int64
	var got bool
	for res.Next() {
		v, ok := res.Record()["c"]
		if !ok {
			return 0, fmt.Errorf("column %q missing", "c")
		}
		iv, ok := v.(expr.IntegerValue)
		if !ok {
			return 0, fmt.Errorf("column c is %T, want expr.IntegerValue", v)
		}
		n = int64(iv)
		got = true
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	if !got {
		return 0, fmt.Errorf("count query returned no rows")
	}
	return n, nil
}

// cypherWrite runs a mutation via Engine.RunInTx and drains the result.
func cypherWrite(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value) error {
	res, err := eng.RunInTx(ctx, query, params)
	if err != nil {
		return err
	}
	for res.Next() { //nolint:revive // drain any rows so the write commits cleanly.
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// countReachable returns the number of services reachable from the Dijkstra
// source (those with a finite distance), iterating the service keys so the
// count does not depend on NodeID layout.
func countReachable(adj *adjlist.AdjList[string, int64], dist *search.Distances[int64], services int) int64 {
	var reached int64
	for i := 0; i < services; i++ {
		id, ok := adj.Mapper().Lookup(svcName(i))
		if !ok {
			continue
		}
		if _, ok := dist.Distance(id); ok {
			reached++
		}
	}
	return reached
}

// csvRoundTrip writes adj as a CSV edge list and reads it back, returning
// whether the read-back edge count equals the written edge count.
func csvRoundTrip(ctx context.Context, adj *adjlist.AdjList[string, int64]) (bool, error) {
	opts := csv.DefaultOptions()
	opts.Directed = true

	var buf bytes.Buffer
	written, err := csv.WriteCtx(ctx, &buf, adj, opts)
	if err != nil {
		return false, fmt.Errorf("WriteCtx: %w", err)
	}
	_, read, err := csv.ReadIntoCtx(ctx, &buf, opts)
	if err != nil {
		return false, fmt.Errorf("ReadIntoCtx: %w", err)
	}
	return written == read, nil
}

// boltEncode acquires an Encoder from a pooled EncodePool, encodes a small
// PackStream record, and returns the encoder — exercising the pool's
// get/put utilisation counters.
func boltEncode() error {
	pool := packstream.NewEncodePool()
	var buf bytes.Buffer
	enc := pool.Get(&buf)
	defer pool.Put(enc)

	if err := enc.WriteMapHeader(2); err != nil {
		return err
	}
	if err := enc.WriteString("service"); err != nil {
		return err
	}
	if err := enc.WriteString(svcName(0)); err != nil {
		return err
	}
	if err := enc.WriteString("latency_ms"); err != nil {
		return err
	}
	if err := enc.WriteFloat(1.5); err != nil {
		return err
	}
	return enc.Flush()
}

// ─────────────────────────────────────────────────────────────────────────────
// Scrape + report
// ─────────────────────────────────────────────────────────────────────────────

// scrape serves reg.Handler() over a local httptest server and performs a
// single GET, returning the Prometheus text exposition — the exact path an
// operator's Prometheus scrape follows.
func scrape(ctx context.Context, reg *metrics.Registry) (string, error) {
	srv := httptest.NewServer(reg.Handler())
	defer srv.Close()

	client := &http.Client{}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/metrics", http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /metrics: status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// expectedMetric is a metric that the workload must have surfaced. name is
// the dotted schema name from docs/metrics.md; kind is the exposition TYPE
// it is declared under; api names the entry point that emits it.
type expectedMetric struct {
	name string
	kind string
	api  string
}

// expectedMetrics enumerates the instrumented metrics the mixed workload is
// guaranteed to touch, each verified against docs/metrics.md and the wiring
// in source. Their NAMES are deterministic; the presence facts below are
// pinned by the regression test.
var expectedMetrics = []expectedMetric{
	{"cypher.Run", "histogram", "Engine.Run"},
	{"cypher.RunInTx", "histogram", "Engine.RunInTx"},
	{"cypher.plan_cache.misses", "counter", "Engine.Run (first compile)"},
	{"cypher.plan_cache.hits", "counter", "Engine.Run (repeat query)"},
	{"search.Dijkstra", "histogram", "search.Dijkstra"},
	{"search.DijkstraCtx", "histogram", "search.Dijkstra (delegates to Ctx)"},
	{"search.pool.dijkstra.get", "counter", "search.Dijkstra scratch pool"},
	{"search.pool.dijkstra.put", "counter", "search.Dijkstra scratch pool"},
	{"graph.io.csv.Write", "histogram", "csv.WriteCtx"},
	{"graph.io.csv.ReadInto", "histogram", "csv.ReadIntoCtx"},
	{"bolt.pool.encoder.get", "counter", "packstream.EncodePool.Get"},
	{"bolt.pool.encoder.put", "counter", "packstream.EncodePool.Put"},
}

// reportMetrics parses the exposition and writes, for every expected
// metric, a deterministic presence fact (metric.present.<name>=true|false)
// plus a summary count; the observed values and any extra instrumented
// series discovered are written as "# " telemetry. A metric that is present
// under an unexpected TYPE is surfaced as a "# WARN" line.
func reportMetrics(w io.Writer, text string) {
	series := parseSeries(text)

	present := 0
	for _, m := range expectedMetrics {
		rendered := promName(m.name)
		kind, ok := series[rendered]
		if ok {
			present++
		}
		fmt.Fprintf(w, "metric.present.%s=%s\n", m.name, boolWord(ok))
		if ok && kind != m.kind {
			fmt.Fprintf(w, "# WARN metric.kind.mismatch %s: exposition TYPE=%q, docs/metrics.md=%q\n", m.name, kind, m.kind)
		}
	}
	fmt.Fprintf(w, "metric.present.count=%d\n", present)
	fmt.Fprintf(w, "metric.expected.count=%d\n", len(expectedMetrics))

	// Telemetry: the observed value behind each present expected metric.
	for _, m := range expectedMetrics {
		rendered := promName(m.name)
		if _, ok := series[rendered]; !ok {
			continue
		}
		if v, ok := seriesValue(text, rendered, m.kind); ok {
			fmt.Fprintf(w, "# observed.%s=%d (%s)\n", rendered, v, m.api)
		}
	}

	// Telemetry: any other instrumented series the workload lit up beyond
	// the pinned set — evidence of the wider surface a scrape captures.
	expectedSet := make(map[string]struct{}, len(expectedMetrics))
	for _, m := range expectedMetrics {
		expectedSet[promName(m.name)] = struct{}{}
	}
	var extra []string
	for name := range series {
		if _, ok := expectedSet[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	fmt.Fprintf(w, "# scrape.series.total=%d\n", len(series))
	fmt.Fprintf(w, "# scrape.series.extra=%d\n", len(extra))
	for _, name := range extra {
		fmt.Fprintf(w, "# extra.series=%s (%s)\n", name, series[name])
	}
}

// parseSeries returns the set of metric base-names present in the Prometheus
// text exposition, mapped to their declared TYPE (counter or histogram). It
// reads the "# TYPE <name> <kind>" declaration lines, which name exactly one
// series each.
func parseSeries(text string) map[string]string {
	series := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		series[fields[2]] = fields[3]
	}
	return series
}

// seriesValue returns the scalar behind a metric: a counter's value, or a
// histogram's total observation count (the <name>_count line).
func seriesValue(text, name, kind string) (uint64, bool) {
	target := name
	if kind == "histogram" {
		target = name + "_count"
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == target {
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

// promName mirrors the Prometheus backend's name sanitisation: every byte
// outside [a-zA-Z0-9_:] becomes '_', and a leading digit is prefixed with
// '_'. For GoGraph's ASCII dotted identifiers this maps each '.' to '_',
// matching how the backend renders a metric name in the exposition.
func promName(dotted string) string {
	var b strings.Builder
	b.Grow(len(dotted) + 1)
	for i, r := range dotted {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == ':':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// Small helpers
// ─────────────────────────────────────────────────────────────────────────────

// boolFact renders a boolean as the 1/0 an integer fact line carries.
func boolFact(b bool) int {
	if b {
		return 1
	}
	return 0
}

// boolWord renders a boolean as the true/false a presence fact carries.
func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live bytes rather than uncollected garbage.
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
