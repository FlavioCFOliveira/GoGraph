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
//     pooled EncodePool;
//     - bolt/server         — a real Bolt v5 server on a real TCP socket,
//     driven by the official neo4j-go-driver through an autocommit read, a
//     committed explicit transaction and a rolled-back one, so the
//     per-message latency histograms and the connection/transaction
//     counters are populated by genuine wire traffic;
//     - relationship count-store — a CREATE / DELETE / SET-label /
//     REMOVE-label write burst that lights the count-store observability
//     counters (delta.applied, relabel.dirtied) and the reopen recompute
//     histogram, and samples write throughput with the store active so its
//     neutrality to the write path is observable;
//     - the MVCC substrate — four concurrent writers, a deliberate
//     write-write conflict, and a read snapshot pinned across a write burst,
//     so the writer gauge, the commit/abort counts, the conflict rate and
//     its per-store attribution, the retained chain-depth distribution and
//     the background vacuum's own series are all populated.
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
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/metrics"
	"github.com/FlavioCFOliveira/GoGraph/search"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
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
	prof := exprof.Bind(flag.CommandLine)
	flag.Parse()

	if err := prof.Run(os.Stdout, func() error {
		return run(context.Background(), os.Stdout, cfg)
	}); err != nil {
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

	// 7. Relationship count-store — drive a write workload that CREATEs and
	//    DELETEs CALLS edges and SETs/REMOVEs a DEGRADED label, so the
	//    count-store observability counters (delta.applied, relabel.dirtied)
	//    light up alongside the reopen recompute histogram, and sample write
	//    throughput with the store active so its neutrality is observable.
	if err = driveCountStore(ctx, w, eng); err != nil {
		return fmt.Errorf("count-store workload: %w", err)
	}

	// 8. The Bolt network surface — a real server on a real socket, driven by
	//    the official neo4j driver, so the per-message latency histograms are
	//    populated by genuine wire traffic (rmp #2715).
	if err = driveBoltServer(ctx, w, eng); err != nil {
		return fmt.Errorf("bolt server workload: %w", err)
	}

	// 9. The MVCC substrate — concurrency control is MVCC and nothing else, so
	//    its health IS the module's health. Drive the write side deliberately so
	//    the writer, commit, abort, conflict and chain-depth series are all
	//    non-trivially populated rather than left at their zero values.
	if err = driveMVCC(ctx, w, g); err != nil {
		return fmt.Errorf("mvcc workload: %w", err)
	}
	return nil
}

// mvccWriters and mvccWritesPerWriter size the concurrent write burst below. Small
// enough to keep the example instant in the short layer, large enough that several
// writers are genuinely in flight at once and the vacuum has real churn to sweep.
const (
	mvccWriters         = 4
	mvccWritesPerWriter = 64
	// mvccRetryBudget bounds the retry loop below. A serialization conflict is
	// retriable by contract, but a retry loop with no bound is an unbounded
	// resource, which this module does not admit anywhere.
	//
	// A TIME budget and not an attempt count, deliberately. A fixed spin was tried
	// first and it failed under coverage instrumentation: the conflict this loop
	// retries is the contiguous frontier waiting for ANOTHER writer to publish, and
	// under -cover that writer is slow enough that eight immediate attempts can all
	// land before it does. An attempt count measures this machine's speed; a
	// deadline measures the thing actually being waited for. The budget is generous
	// because it is a backstop, not a tuning knob — the loop normally succeeds on
	// its second attempt.
	mvccRetryBudget = 30 * time.Second
)

// driveMVCC exercises the MVCC substrate's WRITE side and its reclamation, so every
// series in the "graph/lpg — the MVCC substrate" section of docs/metrics.md is
// populated by the time the exposition is scraped.
//
// It does four things the rest of this example does not:
//
//   - runs CONCURRENT writers against one graph, so the writer gauge, the commit
//     count and the transaction-latency histogram describe more than one writer;
//   - drives a deliberate CONFLICT on one hot node, so the conflict series and the
//     per-store attribution move. The conflict is provoked rather than hoped for: two
//     transactions are opened against the same node and the first to commit wins, so
//     the second is refused;
//   - holds a READ SNAPSHOT open across a burst of writes to the same key, so the
//     versions cannot be reclaimed and the chain-depth distribution has a chain
//     deeper than one to report;
//   - reclaims synchronously afterwards, because the depth distribution is filled BY
//     the reclaimer and a scrape taken before any sweep would find it empty.
//
// The deterministic facts it writes are the ones that hold regardless of scheduling;
// the volatile counts go out as "# " telemetry.
func driveMVCC(ctx context.Context, w io.Writer, g *lpg.Graph[string, float64]) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before := g.MVCCStats()

	// A read snapshot pinned across the burst: it holds the reclamation watermark
	// below every version written from here on, which is what gives the chain-depth
	// histogram something to measure.
	snap := g.BeginRead()

	// Concurrent writers, each on its own key, so they contend for nothing and the
	// scaling the sprint exists to deliver is what is being exercised.
	var wg sync.WaitGroup
	var retried atomic.Int64
	errs := make([]error, mvccWriters)
	for i := 0; i < mvccWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// ONE SESSION PER WRITER. A session is what guarantees a caller observes
			// its own committed writes: without one, a writer's next transaction can
			// begin below its own previous commit, because the commit frontier is
			// contiguous and ApplyVersioned returns before the instant publishes
			// (rmp #2328). These writers touch DISJOINT keys, so with a session they
			// contend for nothing and never conflict; the retry below is kept because a
			// serialization conflict remains part of the contract for a workload that
			// DOES share keys, and an example that cannot handle one would be teaching
			// the wrong thing.
			sess := g.NewSession()
			key := fmt.Sprintf("mvcc-writer-%d", id)
			for n := 0; n < mvccWritesPerWriter; n++ {
				// RETRY on a serialization conflict, because that is the contract a
				// client has to implement: under MVCC a write is refused rather than
				// blocked, and the caller retries.
				var err error
				deadline := time.Now().Add(mvccRetryBudget)
				for attempt := 0; ; attempt++ {
					err = sess.ApplyVersioned(func(tx lpg.WriteTx) error {
						wv := g.Writer(tx)
						if n == 0 && attempt == 0 {
							if e := wv.AddNode(key); e != nil {
								return e
							}
						}
						return wv.SetNodeProperty(key, "seq", lpg.Int64Value(int64(n)))
					})
					if err == nil || !errors.Is(err, mvcc.ErrSerializationConflict) {
						break
					}
					if time.Now().After(deadline) {
						break
					}
					retried.Add(1)
					// Yield: the transaction this one is waiting on is another
					// goroutine's, and spinning without giving it the processor is how
					// a retry loop turns a microsecond wait into a full budget.
					runtime.Gosched()
				}
				if err != nil {
					errs[id] = err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			g.EndRead(snap)
			return fmt.Errorf("writer %d: %w", i, err)
		}
	}

	// The deliberate conflict. Two transactions read the same node and both write
	// it; the first to publish wins and the second is refused with a retriable
	// serialization error, which is exactly the contract a client must handle.
	conflicts, err := driveMVCCConflict(g)
	if err != nil {
		g.EndRead(snap)
		return err
	}

	// Reclaim WITH the reader still registered, so the sweep truncates only what is
	// genuinely unreachable and the surviving chains are the ones the reader pinned.
	g.ReclaimNow()
	depth := g.ChainDepths()

	g.EndRead(snap)
	// And once more with the reader gone, so the vacuum's own series show a pass that
	// actually freed something.
	g.ReclaimNow()

	after := g.MVCCStats()
	// Deterministic facts: whatever the scheduling, this many transactions committed,
	// at least one conflicted, and no writer is left in flight.
	fmt.Fprintf(w, "mvcc.commits.delta=%d\n", after.Write.Commits-before.Write.Commits)
	fmt.Fprintf(w, "mvcc.conflicts.observed=%d\n", conflicts)
	fmt.Fprintf(w, "mvcc.writers.settled=%d\n", after.Write.Writers)
	fmt.Fprintf(w, "mvcc.chain_depth.deepest_at_least_two=%d\n", boolFact(depth.Deepest >= 2))

	// Telemetry: the shape of the substrate after the burst.
	fmt.Fprintf(w, "# mvcc.aborts=%d retries=%d\n", after.Write.Aborts, retried.Load())
	fmt.Fprintf(w, "# mvcc.conflict_rate=%.4f\n", after.Write.ConflictRate())
	fmt.Fprintf(w, "# mvcc.chain_depth.chains=%d deepest=%d\n", depth.Chains(), depth.Deepest)
	fmt.Fprintf(w, "# mvcc.chain_depth.buckets=%v\n", depth.Buckets)
	fmt.Fprintf(w, "# mvcc.versions.total=%d bound=%d\n", after.Total, after.Bound)
	fmt.Fprintf(w, "# mvcc.snapshots.active=%d capacity=%d\n", after.ActiveSnapshots, after.SnapshotCapacity)
	vs := g.VacuumStats()
	fmt.Fprintf(w, "# mvcc.vacuum.passes=%d reclaimed=%d mean_pass=%s\n",
		vs.Passes, vs.Reclaimed, vs.MeanPass().Round(time.Microsecond))
	return nil
}

// driveMVCCConflict provokes exactly one write-write conflict and returns how many
// were observed, so the conflict series is populated by a refusal that actually
// happened rather than by one the workload hoped for.
//
// Two explicit transactions write the same node. The first publishes; the second is
// then displacing a version committed after it began, which is the definition of a
// serialization conflict, and its commit is refused.
func driveMVCCConflict(g *lpg.Graph[string, float64]) (int, error) {
	const hot = "mvcc-hotspot"
	if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
		return g.Writer(tx).AddNode(hot)
	}); err != nil {
		return 0, fmt.Errorf("create hotspot: %w", err)
	}

	// Both transactions begin before either writes, so both read the same instant.
	a := g.BeginVersionedTx()
	b := g.BeginVersionedTx()
	if err := g.Writer(a).SetNodeProperty(hot, "owner", lpg.StringValue("a")); err != nil {
		g.EndVersionedTx(a)
		g.EndVersionedTx(b)
		return 0, fmt.Errorf("a write: %w", err)
	}
	// A publishes first and wins.
	g.EndVersionedTx(a)

	conflicts := 0
	// B now tries to displace a version that committed after B began. The refusal is
	// recorded on the transaction; whether it is returned here depends on which
	// primitive the write went through, so both are treated as the conflict they are.
	if err := g.Writer(b).SetNodeProperty(hot, "owner", lpg.StringValue("b")); err != nil {
		conflicts++
	}
	g.EndVersionedTx(b)
	return conflicts, nil
}

// countWriteBatch is the number of CALLS-edge CREATEs the count-store throughput
// sample drives. It is a realistic mini-burst — one service repeatedly calling
// another — sized to keep the short-layer example instant while still yielding a
// meaningful ops/sec figure that shows the count-store maintenance is neutral to
// write throughput.
const countWriteBatch = 200

// driveCountStore exercises the relationship count-store's maintenance paths over
// the seeded call graph and samples the write throughput with the store active.
// It writes one deterministic fact — that the store holds live cells after the
// workload — and emits the volatile cell counts and throughput as "# " telemetry.
// The count-store observability metrics themselves surface through the installed
// Prometheus backend and are reported by reportMetrics, exactly like every other
// instrumented series.
func driveCountStore(ctx context.Context, w io.Writer, eng *cypher.Engine) error {
	cellsBefore := eng.CountStoreCells()

	// A relabel that marks the count-store's IN X-scoped cells dirty, then its
	// inverse — one increment each of the relabel.dirtied counter.
	if err := cypherWrite(ctx, eng,
		"MATCH (n:SERVICE)-[:CALLS]->() WITH n LIMIT 1 SET n:DEGRADED", nil); err != nil {
		return fmt.Errorf("mark degraded: %w", err)
	}
	if err := cypherWrite(ctx, eng,
		"MATCH (n:DEGRADED) WITH n LIMIT 1 REMOVE n:DEGRADED", nil); err != nil {
		return fmt.Errorf("clear degraded: %w", err)
	}

	// Retire a handful of monitored calls — DELETE decrements the E/D/T cells.
	for i := 0; i < 8; i++ {
		if err := cypherWrite(ctx, eng,
			"MATCH (a:SERVICE)-[r:CALLS]->(b:SERVICE) WITH r LIMIT 1 DELETE r", nil); err != nil {
			return fmt.Errorf("retire call: %w", err)
		}
	}

	// Throughput sample: a burst of new monitored calls, each an autocommit write
	// that fans its count deltas onto the store. Timing only the write loop keeps
	// the figure attributable to the write path.
	const createCall = "MATCH (a:SERVICE)-[:CALLS]->(b:SERVICE) WITH a,b LIMIT 1 " +
		"CREATE (a)-[:CALLS {latency_ms:1.0}]->(b)"
	start := time.Now()
	for i := 0; i < countWriteBatch; i++ {
		if err := cypherWrite(ctx, eng, createCall, nil); err != nil {
			return fmt.Errorf("record call %d: %w", i, err)
		}
	}
	elapsed := time.Since(start)

	cellsAfter := eng.CountStoreCells()
	var opsPerSec float64
	if s := elapsed.Seconds(); s > 0 {
		opsPerSec = float64(countWriteBatch) / s
	}

	// Deterministic fact: the store holds live cells after the workload.
	fmt.Fprintf(w, "countstore.cells_positive=%d\n", boolFact(cellsAfter > 0))
	// Volatile telemetry: exact cell counts and the write throughput.
	fmt.Fprintf(w, "# countstore.cells_before=%d\n", cellsBefore)
	fmt.Fprintf(w, "# countstore.cells_after=%d\n", cellsAfter)
	fmt.Fprintf(w, "# countstore.write_batch=%d\n", countWriteBatch)
	fmt.Fprintf(w, "# countstore.write_elapsed=%s\n", elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# countstore.write_throughput_ops_per_sec=%.0f\n", opsPerSec)
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
	// Size the byte cap to what was just written. csv.DefaultMaxBytes is a 128 MiB
	// memory-exhaustion bound for input of unknown provenance; this buffer was
	// written on the line above and its length is known exactly, so the default
	// would only put a hidden ceiling on the -services knob (rmp #2375). Raised,
	// never disabled — MaxBytes <= 0 removes the bound altogether.
	readOpts := opts
	readOpts.MaxBytes = int64(buf.Len())
	_, read, err := csv.ReadIntoCtx(ctx, &buf, readOpts)
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
	// The Bolt network surface, per message type (rmp #2715). These are the
	// only series in this list produced by traffic over a real socket, and they
	// are the module's own measurement of its network latency — before them the
	// p50/p99 quoted for Bolt came from a CLIENT-side stopwatch that no
	// deployment can emit.
	//
	// Seven of the thirteen names in the closed set appear here, because seven
	// is what this workload's three session shapes deterministically send: the
	// official driver opens with HELLO + LOGON, an autocommit read is RUN +
	// PULL, and the two explicit transactions add BEGIN, COMMIT and ROLLBACK.
	// DISCARD, RESET, ROUTE, LOGOFF and GOODBYE are NOT pinned — the driver
	// does not send them on these paths, or does not send them reliably, and a
	// presence fact that depends on a client library's discretion is not a
	// fact. Their names are proven closed, distinct and correctly attributed by
	// bolt/server/msgmetrics_internal_test.go and driven over the raw wire by
	// bolt/server/msgmetrics_wire_test.go.
	{"bolt.server.HandleMessage.message.hello", "histogram", "Bolt HELLO over the wire"},
	{"bolt.server.HandleMessage.message.logon", "histogram", "Bolt LOGON over the wire"},
	{"bolt.server.HandleMessage.message.run", "histogram", "Bolt RUN over the wire"},
	{"bolt.server.HandleMessage.message.pull", "histogram", "Bolt PULL over the wire"},
	{"bolt.server.HandleMessage.message.begin", "histogram", "Bolt BEGIN over the wire"},
	{"bolt.server.HandleMessage.message.commit", "histogram", "Bolt COMMIT over the wire"},
	{"bolt.server.HandleMessage.message.rollback", "histogram", "Bolt ROLLBACK over the wire"},
	{"bolt.server.conn.accepted", "counter", "bolt/server accept loop"},
	{"bolt.server.conn.closed", "counter", "bolt/server connection teardown"},
	{"bolt.server.tx.opened", "counter", "Bolt BEGIN that acquired a transaction"},
	{"bolt.server.tx.closed", "counter", "Bolt COMMIT / ROLLBACK"},
	{"cypher.countstore.recompute", "histogram", "Engine reopen recompute (#2087)"},
	{"cypher.countstore.delta.applied", "counter", "count-store commit fan-out (#2087)"},
	{"cypher.countstore.relabel.dirtied", "counter", "count-store relabel (#2087)"},
	// The MVCC substrate (rmp #2312). Concurrency control is MVCC and nothing
	// else, so these are not an optional extra: they are how an operator sees
	// whether the mechanism the whole module rests on is working.
	{"graph.lpg.ApplyVersioned", "histogram", "Graph.ApplyVersioned (one write transaction)"},
	{"graph.lpg.EndVersionedTx", "histogram", "Graph.EndVersionedTx (publish)"},
	{"lpg.mvcc.writers.active", "gauge", "write transactions in flight"},
	{"lpg.mvcc.commits", "gauge", "transactions that published an instant"},
	{"lpg.mvcc.aborts", "gauge", "transactions refused publication"},
	{"lpg.mvcc.conflict_rate", "gauge", "conflicts / (commits + aborts)"},
	{"lpg.mvcc.conflicts", "counter", "one per doomed transaction"},
	{"lpg.mvcc.conflicts.store.node_properties", "counter", "attributed to the contended store"},
	{"lpg.mvcc.chain_depth.deepest", "gauge", "deepest retained version chain"},
	{"lpg.mvcc.chain_depth.bucket.1", "gauge", "chains of retained depth 1"},
	{"lpg.mvcc.oldest_snapshot_age", "gauge", "now - reclamation watermark"},
	{"lpg.mvcc.snapshots.active", "gauge", "snapshots registered with the horizon"},
	{"lpg.mvcc.vacuum.passes", "gauge", "completed reclamation passes"},
	{"lpg.mvcc.vacuum.pass", "histogram", "one reclamation pass, start to finish"},
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

// ─────────────────────────────────────────────────────────────────────────────
// The Bolt network surface
// ─────────────────────────────────────────────────────────────────────────────

// driveBoltServer starts a real bolt/server over a real TCP socket, connects
// the official neo4j-go-driver as a real client, and drives one session of
// each shape — an autocommit read, an explicit transaction committed, and an
// explicit transaction rolled back — so the per-message latency histograms
// (bolt.server.HandleMessage.message.<type>, rmp #2715) are populated by
// genuine wire traffic rather than by a direct call.
//
// Why over a socket and not by calling Session.HandleMessage directly: the
// histograms exist so a DEPLOYED GoGraph can report its own Bolt latency. A
// demonstration that skipped the listener, the handshake and the chunked
// framing would show the metric working on a path no operator runs. The
// numbers it produces are the server's own, which is the gap this closes — the
// p50/p99 previously quoted for the Bolt surface came from a client-side
// stopwatch in examples/23_bolt_server and no deployment can emit them.
//
// The facts it writes are deterministic (row counts and the committed write
// delta); the observed latencies are volatile and appear only as "# "
// telemetry, through the histograms themselves.
func driveBoltServer(ctx context.Context, w io.Writer, eng *cypher.Engine) error {
	// The explicit NoAuthHandler{} value is the opt-in that lets a development
	// example run without credentials; the server is secure-by-default and
	// refuses to start with a nil Auth handler.
	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: 8,
		ConnTimeout:    30 * time.Second,
		Auth:           server.NoAuthHandler{},
	})
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().String()

	serveCtx, serveCancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx, ln) }()

	driveErr := driveBoltClient(ctx, w, addr)

	// Shut down with a deadline, then cancel Serve and drain its goroutine.
	// Serve returns only once every connection goroutine has finished, so the
	// drain is what makes the example goroutine-leak clean.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	shutErr := srv.Shutdown(shutCtx)
	shutCancel()
	serveCancel()
	<-serveErr

	if driveErr != nil {
		return driveErr
	}
	if shutErr != nil {
		return fmt.Errorf("shutdown: %w", shutErr)
	}
	return nil
}

// boltCanaryName is the SERVICE created over the Bolt wire. It is distinct from
// the in-process canary so the two write paths cannot be confused for each
// other in the node counts.
const boltCanaryName = "svc-bolt-canary"

// driveBoltClient connects the official driver to addr and runs the three
// session shapes. It closes its own driver before returning, which is what
// sends GOODBYE and lets that histogram populate too.
func driveBoltClient(ctx context.Context, w io.Writer, addr string) error {
	driver, err := neo4j.NewDriverWithContext("bolt://"+addr, neo4j.NoAuth())
	if err != nil {
		return fmt.Errorf("bolt driver: %w", err)
	}
	defer func() { _ = driver.Close(ctx) }()

	// 1. Autocommit read — RUN then PULL, with no transaction messages.
	sess := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	before, err := boltCount(ctx, sess)
	if err != nil {
		_ = sess.Close(ctx)
		return fmt.Errorf("bolt autocommit read: %w", err)
	}
	if err := sess.Close(ctx); err != nil {
		return fmt.Errorf("bolt read session close: %w", err)
	}

	// 2. An explicit transaction, COMMITted — BEGIN, RUN, PULL, COMMIT.
	wsess := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = wsess.Close(ctx) }()
	tx, err := wsess.BeginTransaction(ctx)
	if err != nil {
		return fmt.Errorf("bolt begin (commit arm): %w", err)
	}
	if _, err := tx.Run(ctx, "CREATE (s:SERVICE {name:$name})",
		map[string]any{"name": boltCanaryName}); err != nil {
		return fmt.Errorf("bolt tx create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("bolt commit: %w", err)
	}

	// 3. An explicit transaction, ROLLED BACK — BEGIN, RUN, PULL, ROLLBACK. The
	//    write it attempts must leave no trace, which the count below proves.
	rtx, err := wsess.BeginTransaction(ctx)
	if err != nil {
		return fmt.Errorf("bolt begin (rollback arm): %w", err)
	}
	if _, err := rtx.Run(ctx, "CREATE (s:SERVICE {name:$name})",
		map[string]any{"name": boltCanaryName + "-rolled-back"}); err != nil {
		return fmt.Errorf("bolt tx create (rollback arm): %w", err)
	}
	if err := rtx.Rollback(ctx); err != nil {
		return fmt.Errorf("bolt rollback: %w", err)
	}

	after, err := boltCount(ctx, wsess)
	if err != nil {
		return fmt.Errorf("bolt autocommit read (after): %w", err)
	}

	fmt.Fprintf(w, "bolt.services_before=%d\n", before)
	fmt.Fprintf(w, "bolt.services_after=%d\n", after)
	fmt.Fprintf(w, "bolt.commit_delta=%d\n", after-before)
	return nil
}

// boltCount runs the SERVICE count as an autocommit read over the Bolt wire and
// returns it. It is the assertion that the wire path produced a real result:
// without it the example would light the histograms just as happily against a
// server that answered nothing.
func boltCount(ctx context.Context, sess neo4j.SessionWithContext) (int64, error) {
	res, err := sess.Run(ctx, "MATCH (s:"+labelService+") RETURN count(s) AS n", nil)
	if err != nil {
		return 0, err
	}
	rec, err := res.Single(ctx)
	if err != nil {
		return 0, err
	}
	v, ok := rec.Get("n")
	if !ok {
		return 0, errors.New("count query returned no column n")
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("count column n is %T, want int64", v)
	}
	return n, nil
}
