// Example 23_bolt_server drives the GoGraph Bolt v5 server end to end: it
// starts the embedded [bolt/server] over an in-memory labelled property
// graph, connects the official neo4j-go-driver/v5 as a real client, runs a
// battery of Cypher queries over driver sessions, and shuts everything down
// cleanly with no goroutine left behind.
//
// Unlike a hello-world round-trip, this example seeds the served graph from a
// seeded, scale-parametrised generator and then puts the wire path under
// load, so it doubles as a Bolt-throughput benchmark. It reports the evidence
// that matters for the Bolt/Cypher subject — query throughput, a p50/p95/p99
// latency distribution, and live Go heap — as volatile telemetry, while the
// deterministic results (a label-scan count equal to the known node count and
// the number of queries that succeeded) are printed as bare facts a
// regression test can pin.
//
// # Model
//
//	(:Person {id, name})                        // id is a 24-char hex string
//	(:Person)-[:KNOWS {since}]->(:Person)        // knowsMin..knowsMax per person
//
// The graph is a directed social network: every person is given a random
// out-degree in [knowsMin, knowsMax] to distinct other people (no self-loops,
// no duplicate targets). Every KNOWS edge carries a mandatory since date,
// stored as an ISO-8601 (YYYY-MM-DD) string drawn from the seeded RNG and
// anchored to a fixed reference date, so it is reproducible for a given -seed
// and the cypher.Engine reads it back as a non-null, chronologically sortable
// value (lpg.TimeValue is not used: the Cypher reader maps it to null, whereas
// the tagged date strings round-trip).
//
// # Scale and load
//
// Run with no flags, the example seeds a small deterministic graph (2000
// people) and fires 2000 read queries from a pool of driver sessions, fast
// enough to stay well under the 60 s short-test budget. Every dimension is a
// flag, so the same binary scales the dataset and the query load up to where
// the Bolt path's behaviour is actually observable:
//
//	go run ./examples/23_bolt_server -nodes 200000 -queries 50000 -sessions 16 -seed 7
//
// The deterministic facts are reproducible for a fixed -seed; only the
// telemetry (lines prefixed with "# ") — throughput, latency percentiles, and
// heap — varies between runs and machines.
//
// # Teardown
//
// The listener binds to 127.0.0.1:0 so the kernel assigns a free port and a
// test run never collides. Serve runs under a cancellable context; on
// completion the client driver is closed, the server is gracefully shut down,
// and the serve goroutine is drained. Serve only returns after every
// per-connection goroutine has finished, so the drain guarantees no goroutine
// leaks — the same teardown discipline as bolt/server/example_test.go.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Node label and relationship type. Centralised so the model is described in
// exactly one place and a rename surfaces as a compile error everywhere.
const (
	labelPerson = "Person"

	relKnows = "KNOWS" // (:Person)-[:KNOWS {since}]->(:Person)

	// propKnowsSince is the mandatory per-relationship date property: every
	// KNOWS records when the acquaintance began. Stored as an ISO-8601 date
	// string (see isoEdgeDate).
	propKnowsSince = "since"
)

// config captures every scale and shape knob of the benchmark. The zero value
// is not valid; build one with defaultConfig and override fields from flags
// (see main) or construct one directly (see the regression test).
type config struct {
	nodes    int   // number of :Person nodes
	knowsMin int   // minimum KNOWS out-degree per person (inclusive)
	knowsMax int   // maximum KNOWS out-degree per person (inclusive)
	queries  int   // number of read queries to fire over the wire
	sessions int   // number of concurrent driver sessions issuing queries
	seed     int64 // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns a small, deterministic default: a 2000-person graph
// queried 2000 times over four sessions. It is the shape the regression test
// pins and is fast enough to stay well under the short-layer 60 s budget.
func defaultConfig() config {
	return config{
		nodes:    2000,
		knowsMin: 5,
		knowsMax: 8,
		queries:  2000,
		sessions: 4,
		seed:     42,
	}
}

// validate rejects a configuration that cannot produce the requested shape —
// for instance more acquaintances than there are other people. It is checked
// once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.nodes <= 0:
		return fmt.Errorf("nodes must be > 0, got %d", c.nodes)
	case c.knowsMin < 0 || c.knowsMax < c.knowsMin:
		return fmt.Errorf("require 0 <= knowsMin <= knowsMax, got [%d,%d]", c.knowsMin, c.knowsMax)
	case c.knowsMax >= c.nodes:
		return fmt.Errorf("knowsMax (%d) exceeds nodes-1 (%d): not enough distinct acquaintances", c.knowsMax, c.nodes-1)
	case c.queries <= 0:
		return fmt.Errorf("queries must be > 0, got %d", c.queries)
	case c.sessions <= 0:
		return fmt.Errorf("sessions must be > 0, got %d", c.sessions)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of :Person nodes to seed")
	flag.IntVar(&cfg.knowsMin, "knows-min", cfg.knowsMin, "minimum KNOWS out-degree per person")
	flag.IntVar(&cfg.knowsMax, "knows-max", cfg.knowsMax, "maximum KNOWS out-degree per person")
	flag.IntVar(&cfg.queries, "queries", cfg.queries, "number of read queries to fire over the wire")
	flag.IntVar(&cfg.sessions, "sessions", cfg.sessions, "number of concurrent driver sessions")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run seeds the social graph described by cfg, starts a Bolt v5 server on an
// ephemeral port, drives cfg.queries read queries from cfg.sessions concurrent
// driver sessions, and writes a report to w before tearing everything down
// cleanly. Bare lines carry deterministic facts (counts and query results,
// reproducible for a fixed seed); lines prefixed with "# " carry volatile
// telemetry (throughput, latency percentiles, heap) that varies per run and
// per machine. All output goes to w so a test can capture and assert on it;
// run returns wrapped errors rather than terminating the process, and honours
// ctx cancellation.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.knows=[%d,%d]\n", cfg.knowsMin, cfg.knowsMax)
	fmt.Fprintf(w, "config.queries=%d\n", cfg.queries)
	fmt.Fprintf(w, "config.sessions=%d\n", cfg.sessions)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	// Engine over an in-memory labelled property graph, seeded from cfg.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	stats, err := seed(ctx, g, cfg)
	if err != nil {
		return fmt.Errorf("seed graph: %w", err)
	}
	fmt.Fprintf(w, "nodes.person=%d\n", stats.persons)
	fmt.Fprintf(w, "edges.knows=%d\n", stats.knowsEdges)

	// Bolt v5 server with a per-connection idle timeout. The explicit
	// NoAuthHandler{} value is the opt-in that lets this development example
	// run without credentials; the server is secure-by-default and otherwise
	// refuses to start with a nil Auth handler. A production deployment would
	// instead set Options.Auth to a real AuthHandler and start from
	// server.DefaultTLSConfig() with a certificate as Options.TLSConfig.
	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: cfg.sessions + 4,
		ConnTimeout:    30 * time.Second,
		Auth:           server.NoAuthHandler{},
	})
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}

	// Kernel-assigned port; ln.Addr() reveals the chosen port for the client,
	// so a parallel test run never collides on a fixed port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().String()

	// Serve in the background under a cancellable context so Serve exits
	// cleanly once the load finishes and the context is cancelled.
	serveCtx, serveCancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx, ln) }()

	// Drive the load and gather evidence. driveLoad connects the driver, fires
	// the queries across sessions, verifies the deterministic result of each,
	// and closes its own driver before returning.
	report, loadErr := driveLoad(ctx, addr, cfg)

	// Graceful shutdown with a deadline, then cancel Serve and drain its
	// goroutine. Serve only returns after every connection goroutine has
	// finished, so the drain guarantees no leaked goroutine.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	shutErr := srv.Shutdown(shutCtx)
	shutCancel()
	serveCancel()
	serveDrain := <-serveErr

	// Surface the first meaningful failure, preferring the client load.
	if loadErr != nil {
		return fmt.Errorf("load: %w", loadErr)
	}
	if shutErr != nil {
		return fmt.Errorf("shutdown: %w", shutErr)
	}
	if serveDrain != nil {
		return fmt.Errorf("serve: %w", serveDrain)
	}

	// Deterministic facts: the fixed query's result over the seeded data (the
	// label-scan count equals the known node count) and how many of the fired
	// queries succeeded. Both are reproducible for a fixed seed and are the
	// lines the regression test pins.
	fmt.Fprintf(w, "q.count_person=%d\n", report.countPerson)
	fmt.Fprintf(w, "queries.ok=%d\n", report.ok)

	// Volatile telemetry: query throughput, the latency distribution, and live
	// heap. These vary per run and per machine, so they are "# "-prefixed and
	// never pinned by the test.
	fmt.Fprintf(w, "# load.elapsed=%s\n", report.elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "# load.throughput=%.0f queries/s\n", rate(report.ok, report.elapsed))
	fmt.Fprintf(w, "# load.latency_p50=%s\n", report.p50.Round(time.Microsecond))
	fmt.Fprintf(w, "# load.latency_p95=%s\n", report.p95.Round(time.Microsecond))
	fmt.Fprintf(w, "# load.latency_p99=%s\n", report.p99.Round(time.Microsecond))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(readMem().HeapAlloc))

	fmt.Fprintln(w, "# server shut down cleanly")
	return nil
}

// seedStats reports the realised shape of a seeded graph (the random degrees
// mean the edge total is not known until the graph is materialised).
type seedStats struct {
	persons    int
	knowsEdges int
}

// seed materialises the social network described by cfg into g via the
// in-process property-graph API. It first creates every :Person node (so KNOWS
// targets exist before the edges reference them), then the KNOWS edges. Person
// ids are 24-char hex strings drawn from the seeded RNG; names are realistic
// strings assembled from fixed word lists. The seed honours ctx cancellation
// between phases and on a periodic check.
func seed(ctx context.Context, g *lpg.Graph[string, float64], cfg config) (seedStats, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))

	ids := make([]string, cfg.nodes)
	seen := make(map[string]struct{}, cfg.nodes)

	// People.
	for i := 0; i < cfg.nodes; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return seedStats{}, err
			}
		}
		id := uniqueHexID(rng, seen)
		ids[i] = id
		if err := addPerson(g, id, realisticName(rng)); err != nil {
			return seedStats{}, err
		}
	}

	// KNOWS edges: each person gets a random out-degree in [knowsMin, knowsMax]
	// to distinct other people, each tagged with a mandatory since date.
	knowsEdges := 0
	targets := make(map[int]struct{}, cfg.knowsMax)
	for i := 0; i < cfg.nodes; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return seedStats{}, err
			}
		}
		degree := cfg.knowsMin + rng.Intn(cfg.knowsMax-cfg.knowsMin+1)
		clear(targets)
		for len(targets) < degree {
			j := rng.Intn(cfg.nodes)
			if j == i {
				continue
			}
			targets[j] = struct{}{}
		}
		for j := range targets {
			if err := addKnows(g, ids[i], ids[j], isoEdgeDate(rng)); err != nil {
				return seedStats{}, err
			}
			knowsEdges++
		}
	}

	return seedStats{persons: cfg.nodes, knowsEdges: knowsEdges}, nil
}

// checkEvery bounds how often the seed loop polls ctx for cancellation: often
// enough that a cancelled large seed stops promptly, rare enough that the
// check is free relative to the surrounding work.
const checkEvery = 4096

// addPerson adds a single :Person node carrying its id and name.
func addPerson(g *lpg.Graph[string, float64], id, name string) error {
	if err := g.AddNode(id); err != nil {
		return fmt.Errorf("AddNode %s: %w", id, err)
	}
	if err := g.SetNodeLabel(id, labelPerson); err != nil {
		return fmt.Errorf("SetNodeLabel %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, "id", lpg.StringValue(id)); err != nil {
		return fmt.Errorf("SetNodeProperty id %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, "name", lpg.StringValue(name)); err != nil {
		return fmt.Errorf("SetNodeProperty name %s: %w", id, err)
	}
	return nil
}

// addKnows adds a directed, weight-1 KNOWS edge tagged with the relationship
// type (via AddEdgeLabeled, so the type lands in the edge's inline slot at
// insertion time) and its mandatory since date.
func addKnows(g *lpg.Graph[string, float64], src, dst, since string) error {
	if err := g.AddEdgeLabeled(src, dst, 1, relKnows); err != nil {
		return fmt.Errorf("AddEdgeLabeled %s-[%s]->%s: %w", src, relKnows, dst, err)
	}
	if err := g.SetEdgeProperty(src, dst, propKnowsSince, lpg.StringValue(since)); err != nil {
		return fmt.Errorf("SetEdgeProperty %s on %s-[%s]->%s: %w", propKnowsSince, src, relKnows, dst, err)
	}
	return nil
}

// edgeDateWindowDays bounds how far before the fixed reference date a KNOWS
// edge may be dated: every since falls within [edgeDateRef-edgeDateWindowDays,
// edgeDateRef]. ~6 years.
const edgeDateWindowDays = 2192

// edgeDateRef is the fixed reference date the synthetic edge dates count back
// from. Anchoring to a constant — never the wall clock — keeps the dataset
// reproducible for a given -seed.
var edgeDateRef = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

// isoEdgeDate returns a deterministic calendar date in ISO-8601 form
// (YYYY-MM-DD) drawn from rng as a whole-day offset back from edgeDateRef.
func isoEdgeDate(rng *rand.Rand) string {
	return edgeDateRef.AddDate(0, 0, -rng.Intn(edgeDateWindowDays+1)).Format("2006-01-02")
}

// uniqueHexID returns a 24-character lowercase hex id (12 random bytes) that
// has not been handed out before, recording it in seen. Drawing from the
// seeded rng keeps the whole dataset reproducible.
func uniqueHexID(rng *rand.Rand, seen map[string]struct{}) string {
	var b [12]byte
	for {
		// rng.Read fills b directly from the seeded stream — no per-byte
		// narrowing cast — and is documented to always succeed.
		_, _ = rng.Read(b[:])
		id := hex.EncodeToString(b[:])
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		return id
	}
}

// realisticName assembles a plausible "First Last" personal name from fixed
// word lists. Names are intentionally allowed to repeat — the unique key is
// the hex id, not the name, which mirrors reality.
func realisticName(rng *rand.Rand) string {
	return firstNames[rng.Intn(len(firstNames))] + " " + lastNames[rng.Intn(len(lastNames))]
}

// ─────────────────────────────────────────────────────────────────────────────
// Load driver — the Bolt-throughput evidence
// ─────────────────────────────────────────────────────────────────────────────

// loadReport carries the deterministic results and the volatile telemetry of a
// load run.
type loadReport struct {
	countPerson int64         // result of the fixed label-scan query (deterministic)
	ok          int           // number of queries that succeeded (deterministic)
	elapsed     time.Duration // wall-clock of the load phase (telemetry)
	p50         time.Duration // per-query latency percentiles (telemetry)
	p95         time.Duration
	p99         time.Duration
}

// countPersonQuery is the fixed query every load worker runs. Its result — the
// count of :Person nodes — is deterministic over the seeded data (it equals
// cfg.nodes), which makes it the regression baseline the test pins.
const countPersonQuery = "MATCH (n:Person) RETURN count(n) AS c"

// driveLoad connects a neo4j-go-driver client to the server at addr and fires
// cfg.queries instances of the fixed count query, spread across cfg.sessions
// concurrent sessions. It records every successful query's latency, verifies
// each returns the known :Person count, and reports throughput plus a
// p50/p95/p99 latency distribution. The driver, every session, and every
// worker goroutine are torn down before driveLoad returns, so it leaks no
// goroutine. It honours ctx cancellation.
func driveLoad(ctx context.Context, addr string, cfg config) (loadReport, error) {
	driver, err := neo4j.NewDriverWithContext("bolt://"+addr, neo4j.NoAuth())
	if err != nil {
		return loadReport{}, fmt.Errorf("driver: %w", err)
	}
	defer driver.Close(ctx) //nolint:errcheck // best-effort close on teardown

	// Verify connectivity once up front so a connection failure is reported
	// here rather than as a confusing per-query error storm.
	if err := driver.VerifyConnectivity(ctx); err != nil {
		return loadReport{}, fmt.Errorf("verify connectivity: %w", err)
	}

	// Spread cfg.queries as evenly as possible across cfg.sessions workers.
	perWorker := splitWork(cfg.queries, cfg.sessions)

	var (
		mu        sync.Mutex      // guards latencies and the first worker error
		latencies []time.Duration // one entry per successful query
		firstErr  error
		okCount   atomic.Int64
		want      = int64(cfg.nodes) // the deterministic expected :Person count
	)
	latencies = make([]time.Duration, 0, cfg.queries)

	start := time.Now()
	var wg sync.WaitGroup
	for _, n := range perWorker {
		if n == 0 {
			continue
		}
		wg.Add(1)
		go func(count int) {
			defer wg.Done()
			local, ok, err := runWorker(ctx, driver, count, want)
			mu.Lock()
			latencies = append(latencies, local...)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			okCount.Add(int64(ok))
		}(n)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if firstErr != nil {
		return loadReport{}, firstErr
	}

	p50, p95, p99 := percentiles(latencies)
	return loadReport{
		countPerson: want,
		ok:          int(okCount.Load()),
		elapsed:     elapsed,
		p50:         p50,
		p95:         p95,
		p99:         p99,
	}, nil
}

// runWorker opens one driver session and runs the fixed count query count
// times over it, returning the per-query latencies and how many returned the
// expected count. It stops early on the first error (including ctx
// cancellation) and always closes its session before returning. Reusing a
// single session for the whole worker keeps the connection hot, which is what
// a real client pool does.
func runWorker(ctx context.Context, driver neo4j.DriverWithContext, count int, want int64) ([]time.Duration, int, error) {
	sess := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx) //nolint:errcheck // best-effort close on teardown

	latencies := make([]time.Duration, 0, count)
	ok := 0
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return latencies, ok, err
		}
		qStart := time.Now()
		got, err := queryCount(ctx, sess)
		if err != nil {
			return latencies, ok, fmt.Errorf("query %d: %w", i, err)
		}
		latencies = append(latencies, time.Since(qStart))
		if got != want {
			return latencies, ok, fmt.Errorf("query %d: count=%d, want %d", i, got, want)
		}
		ok++
	}
	return latencies, ok, nil
}

// queryCount runs the fixed count query over sess and returns the single
// integer it yields.
func queryCount(ctx context.Context, sess neo4j.SessionWithContext) (int64, error) {
	result, err := sess.Run(ctx, countPersonQuery, nil)
	if err != nil {
		return 0, fmt.Errorf("run: %w", err)
	}
	rec, err := result.Single(ctx)
	if err != nil {
		return 0, fmt.Errorf("single: %w", err)
	}
	v, ok := rec.Get("c")
	if !ok {
		return 0, fmt.Errorf("column 'c' missing")
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("column 'c': expected int64, got %T", v)
	}
	return n, nil
}

// splitWork divides total into parts buckets as evenly as possible: the first
// total%parts buckets get one extra unit. It is used to spread the query load
// across the session workers.
func splitWork(total, parts int) []int {
	out := make([]int, parts)
	base, extra := total/parts, total%parts
	for i := range out {
		out[i] = base
		if i < extra {
			out[i]++
		}
	}
	return out
}

// percentiles returns the p50, p95, and p99 of the given latencies. It sorts a
// copy so the caller's slice is left untouched; an empty input yields zeros.
func percentiles(latencies []time.Duration) (p50, p95, p99 time.Duration) {
	if len(latencies) == 0 {
		return 0, 0, 0
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return quantile(sorted, 0.50), quantile(sorted, 0.95), quantile(sorted, 0.99)
}

// quantile returns the q-quantile (0 <= q <= 1) of an already-sorted,
// non-empty slice using the nearest-rank method.
func quantile(sorted []time.Duration, q float64) time.Duration {
	idx := int(q * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
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

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
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

// ─────────────────────────────────────────────────────────────────────────────
// Realistic-data word lists. Fixed so the dataset is reproducible.
// ─────────────────────────────────────────────────────────────────────────────

var firstNames = []string{
	"Olivia", "Liam", "Emma", "Noah", "Ava", "Oliver", "Sophia", "Elijah",
	"Isabella", "James", "Mia", "Lucas", "Charlotte", "Mateo", "Amelia",
	"Ethan", "Harper", "Leo", "Evelyn", "Sebastian", "Abigail", "Daniel",
	"Emily", "Henry", "Ella", "Alexander", "Scarlett", "Jack", "Aria",
	"Benjamin", "Camila", "Theodore", "Luna", "Samuel", "Chloe", "David",
	"Sofia", "Joseph", "Layla", "Carter", "Nora", "Wyatt", "Zoe", "Julian",
	"Mila", "Levi", "Aurora", "Gabriel", "Hannah", "Anthony",
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller",
	"Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez",
	"Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin",
	"Lee", "Perez", "Thompson", "White", "Harris", "Sanchez", "Clark",
	"Ramirez", "Lewis", "Robinson", "Walker", "Young", "Allen", "King",
	"Wright", "Scott", "Torres", "Nguyen", "Hill", "Flores", "Green",
	"Adams", "Nelson", "Baker", "Hall", "Rivera", "Campbell", "Mitchell",
	"Carter", "Roberts",
}
