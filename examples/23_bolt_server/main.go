// Example 23_bolt_server is the Bolt v5 extreme-concurrency laboratory: it
// starts the embedded [bolt/server] over an in-memory labelled property graph,
// connects the official neo4j-go-driver/v5 as a real client, drives the wire
// path at a chosen number of CONCURRENT CONNECTIONS, and writes out every
// artefact needed to attribute what that cost.
//
// It is both a didactic end-to-end demonstration of serving Cypher over Bolt
// and a controlled instrument: the same binary runs a small deterministic
// default that a regression test pins, and sweeps the module's published
// concurrency ladder — 1, 8, 64, 256 and 1024 connections — plus a saturation
// rung that deliberately offers more connections than the server's
// [bolt/server.Options.MaxConnections] semaphore admits.
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
// # Two client dimensions, and why they are not one
//
//   - -sessions is the driver's own concurrency: how many logical Bolt sessions
//     issue queries. How many SOCKETS that opens is the driver pool's decision.
//   - -connections is the socket count itself: each connection is an
//     independent driver whose pool holds exactly one connection, so the number
//     of live server-side connections is exactly what was asked for.
//
// Only the second dimension can reach the server's MaxConnections semaphore,
// which is why it exists. With -connections 0 (the default) the example keeps
// its original shape: one shared driver pool of -sessions concurrent sessions.
//
// # Saturation
//
// Offering more connections than the semaphore admits drives the reject branch
// of Serve (bolt/server/serve.go, the select on s.sem). The refusal is visible
// nowhere on the client — a rejected connection simply fails to connect — so
// the example installs a metrics sink and reports bolt.server.conn.rejected
// directly:
//
//	go run ./examples/23_bolt_server -connections 256 -max-connections 128 -queries 20000
//
// Every connection handshakes and then waits at a barrier until every other
// offered connection has resolved, so the server really does hold them all at
// once; without that barrier the early connections would release their
// semaphore slots before the late ones dialled, and nothing would be refused.
//
// # Evidence
//
// With -artifact-dir set, a run writes cpu.pprof, heap.pprof, mutex.pprof,
// block.pprof, goroutine.pprof, trace.out, metrics.json, host.json and
// bench.txt. The goroutine profile is taken AT the peak, while every connection
// is live; a profile taken after teardown would show an idle server. The
// unprofiled repetitions and the profiled window are reported separately:
// full-rate contention profiling perturbs what it measures, so the profiled
// window supplies attribution and never a throughput number.
//
// With -ladder, one invocation sweeps the whole ladder, running each rung in a
// fresh child process — the mutex, block and allocation profiles accumulate for
// a process's lifetime with no way to reset them, so two rungs in one process
// would contaminate each other:
//
//	go run ./examples/23_bolt_server -ladder -artifact-dir /tmp/boltlab -queries 20000 -repetitions 5
//
// # Teardown
//
// The listener binds to 127.0.0.1:0 so the kernel assigns a free port and a
// test run never collides. Serve runs under a cancellable context; on
// completion the client drivers are closed, the server is gracefully shut down,
// and the serve goroutine is drained. Serve only returns after every
// per-connection goroutine has finished, so the drain guarantees no goroutine
// leaks — the same teardown discipline as bolt/server/example_test.go.
//
// # Reading the output
//
// Bare lines carry deterministic facts, reproducible for a fixed -seed. Lines
// prefixed with "# " carry volatile telemetry — throughput, latency
// percentiles, counters, heap, and the host's load average — which varies per
// run and per machine and is never pinned by a test. A run is called idle only
// when its pre-run load average was actually read and was below the threshold;
// an unread load average is reported as not certified idle, never as idle.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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

// config captures every scale, shape, and instrument knob of the laboratory.
// The zero value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression test).
//
// Fields added for the connection laboratory tolerate their zero value on
// purpose: a config built by hand in a test names only the dimensions it cares
// about, and the accessors below (reps, dialTimeout, sessionsPerConn,
// effectiveMaxConnections) turn a zero into the documented default rather than
// into a failure.
type config struct {
	nodes    int   // number of :Person nodes
	knowsMin int   // minimum KNOWS out-degree per person (inclusive)
	knowsMax int   // maximum KNOWS out-degree per person (inclusive)
	queries  int   // number of read queries to fire over the wire
	sessions int   // driver sessions: concurrent in pooled mode, per connection otherwise
	seed     int64 // RNG seed; fixes the deterministic data shape

	// connections is the independent-connection dimension: 0 keeps the shared
	// driver pool, and any positive value opens exactly that many sockets, one
	// driver each. It is the only dimension that can reach the server's
	// MaxConnections semaphore.
	connections int
	// maxConnections is bolt/server Options.MaxConnections verbatim; 0 derives
	// it from the offered load (see effectiveMaxConnections).
	maxConnections int
	// repetitions is how many UNPROFILED measurement windows a rung runs, so
	// benchstat has a spread to compare. 0 means one.
	repetitions int
	// connectTimeout bounds the client's dial and connection acquisition. It is
	// what keeps a saturation run from stalling on the driver's 60 s default
	// once the server starts refusing connections.
	connectTimeout time.Duration

	// mutexFraction and blockRate configure the profiled window's contention
	// profilers; 0 disables either one.
	mutexFraction int
	blockRate     int

	// serverLog selects the logger handed to bolt/server Options.Logger. It
	// exists so the cost of the server's own logging can be measured rather
	// than assumed: the accept loop logs a WARN for every connection the
	// MaxConnections semaphore refuses, and that emission sits on the accept
	// goroutine, between one Accept and the next.
	//
	// The three values decompose that cost into its parts, and only the parts
	// differ between them:
	//
	//   - "default" — Options.Logger nil, so the server uses slog.Default(),
	//     which formats the record and writes it to stderr. This is what an
	//     embedder that configures nothing actually runs.
	//   - "discard" — the same handler at the same level over io.Discard: the
	//     record is still formatted, and nothing is written. The difference
	//     from "default" is the write syscall alone.
	//   - "error"   — the same handler at LevelError, so Logger.Warn returns
	//     before building the record. The difference from "discard" is slog's
	//     own formatting.
	//
	// No arm can remove the cost of EVALUATING the log call's arguments, which
	// the language performs before the call in every arm.
	serverLog string

	// artifactDir, when set, turns the run into an instrumented one: the full
	// artefact set is written there.
	artifactDir string
	// label is the benchstat sub-name; empty derives one from the shape.
	label string

	// ladder sweeps the published concurrency ladder, one child process per
	// rung. It requires artifactDir.
	ladder       bool
	ladderLevels string
	// satOffer and satAdmit define the ladder's saturation rung: satOffer
	// connections are offered to a server that admits satAdmit. Either at 0
	// drops the rung.
	satOffer int
	satAdmit int
}

// Server-logger modes for -server-log. See config.serverLog for what each one
// removes and why the difference between them is the measurement.
const (
	serverLogDefault = "default"
	serverLogDiscard = "discard"
	serverLogError   = "error"
)

// serverLogModes is the accepted set, in the order the decomposition reads:
// each mode removes one more layer than the one before it.
var serverLogModes = [...]string{serverLogDefault, serverLogDiscard, serverLogError}

// validServerLog reports whether mode is one of serverLogModes. The empty
// string is accepted and means the default, so a config built by hand in a test
// need not name this dimension.
func validServerLog(mode string) bool {
	if mode == "" {
		return true
	}
	for _, m := range serverLogModes {
		if m == mode {
			return true
		}
	}
	return false
}

// serverLogMode is the configured mode with the empty string resolved to the
// default, so the machine-readable record never carries an ambiguous value.
func (c *config) serverLogMode() string {
	if c.serverLog == "" {
		return serverLogDefault
	}
	return c.serverLog
}

// serverLogger builds the logger for bolt/server Options.Logger.
//
// A nil return is the documented way to ask the server for slog.Default(); the
// other two arms build a handler over the same writer shape so that the only
// difference between the three is the layer named in config.serverLog.
func (c *config) serverLogger() *slog.Logger {
	switch c.serverLogMode() {
	case serverLogDiscard:
		return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	case serverLogError:
		return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	default:
		return nil
	}
}

// defaultConfig returns a small, deterministic default: a 2000-person graph
// queried 2000 times over four sessions of one shared pool. It is the shape the
// regression test pins and is fast enough to stay well under the short-layer
// 60 s budget. The laboratory dimensions default to off, so `go run` with no
// flags still runs the plain demonstration.
func defaultConfig() config {
	return config{
		nodes:          2000,
		knowsMin:       5,
		knowsMax:       8,
		queries:        2000,
		sessions:       4,
		seed:           42,
		connections:    0,
		maxConnections: 0,
		repetitions:    1,
		connectTimeout: 5 * time.Second,
		mutexFraction:  1,
		blockRate:      1,
		serverLog:      serverLogDefault,
		ladderLevels:   defaultLadderLevels,
		satOffer:       256,
		satAdmit:       128,
	}
}

// validate rejects a configuration that cannot produce the requested shape —
// for instance more acquaintances than there are other people, or a saturation
// rung that offers no more connections than it admits. It is checked once, at
// the boundary, before any work.
func (c *config) validate() error {
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
	case c.connections < 0:
		return fmt.Errorf("connections must be >= 0, got %d", c.connections)
	case c.maxConnections < 0:
		return fmt.Errorf("max-connections must be >= 0, got %d", c.maxConnections)
	case c.repetitions < 0:
		return fmt.Errorf("repetitions must be >= 0, got %d", c.repetitions)
	case c.connectTimeout < 0:
		return fmt.Errorf("connect-timeout must be >= 0, got %s", c.connectTimeout)
	case c.mutexFraction < 0:
		return fmt.Errorf("mutex-fraction must be >= 0, got %d", c.mutexFraction)
	case c.blockRate < 0:
		return fmt.Errorf("block-rate must be >= 0, got %d", c.blockRate)
	case !validServerLog(c.serverLog):
		return fmt.Errorf("server-log %q: want one of %s", c.serverLog, strings.Join(serverLogModes[:], ", "))
	case c.satOffer < 0 || c.satAdmit < 0:
		return fmt.Errorf("saturation offer/admit must be >= 0, got %d/%d", c.satOffer, c.satAdmit)
	case c.ladder && c.artifactDir == "":
		return fmt.Errorf("-ladder writes one artefact directory per rung and needs -artifact-dir")
	case c.ladder && c.satOffer > 0 && c.satAdmit > 0 && c.satOffer <= c.satAdmit:
		return fmt.Errorf("saturation offers %d connections to a semaphore of %d: nothing would be refused", c.satOffer, c.satAdmit)
	}
	return nil
}

// reps is the number of unprofiled measurement windows, treating the zero value
// as one.
func (c *config) reps() int { return max(1, c.repetitions) }

// dialTimeout is the client's dial and acquisition bound, treating the zero
// value as the default.
func (c *config) dialTimeout() time.Duration {
	if c.connectTimeout <= 0 {
		return 5 * time.Second
	}
	return c.connectTimeout
}

// sessionsPerConn is how many sessions each independent connection opens in
// turn over its share of the queries.
func (c *config) sessionsPerConn() int { return max(1, c.sessions) }

// effectiveMaxConnections is the semaphore the server is given. An explicit
// -max-connections wins; otherwise it is the offered load plus a small headroom
// for the driver's own connectivity probe, which preserves the value this
// example used before the connection dimension existed (sessions + 4).
func (c *config) effectiveMaxConnections() int {
	if c.maxConnections > 0 {
		return c.maxConnections
	}
	if c.connections > 0 {
		return c.connections + connHeadroom
	}
	return c.sessions + connHeadroom
}

// expectRejections reports whether this run offers more connections than the
// semaphore admits. When it does, a failed establish is the experiment's result
// and is counted rather than returned as an error.
func (c *config) expectRejections() bool {
	return c.connections > 0 && c.connections > c.effectiveMaxConnections()
}

// rungLabel is the benchstat sub-name for this run.
func (c *config) rungLabel() string {
	switch {
	case c.label != "":
		return c.label
	case c.connections > 0:
		return "conn=" + strconv.Itoa(c.connections)
	default:
		return "pooled"
	}
}

// asJSON renders the configuration for the machine-readable record.
func (c *config) asJSON() configJSON {
	return configJSON{
		Nodes:            c.nodes,
		KnowsMin:         c.knowsMin,
		KnowsMax:         c.knowsMax,
		Queries:          c.queries,
		Sessions:         c.sessions,
		Seed:             c.seed,
		Connections:      c.connections,
		MaxConnections:   c.effectiveMaxConnections(),
		Repetitions:      c.reps(),
		ConnectTimeout:   c.dialTimeout().String(),
		MutexFraction:    c.mutexFraction,
		BlockRate:        c.blockRate,
		ServerLog:        c.serverLogMode(),
		ExpectRejections: c.expectRejections(),
	}
}

// bindFlags registers every flag of the laboratory on fs against cfg and
// returns the profiling configuration exprof binds alongside them.
//
// It is one function rather than a block inside main so that the ladder's
// child-process arguments can be parsed back through exactly the same flag set
// they were rendered for: a flag renamed on one side and not the other is then
// a test failure rather than a sweep that silently runs every rung at the
// default.
func bindFlags(fs *flag.FlagSet, cfg *config) *exprof.Config {
	fs.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of :Person nodes to seed")
	fs.IntVar(&cfg.knowsMin, "knows-min", cfg.knowsMin, "minimum KNOWS out-degree per person")
	fs.IntVar(&cfg.knowsMax, "knows-max", cfg.knowsMax, "maximum KNOWS out-degree per person")
	fs.IntVar(&cfg.queries, "queries", cfg.queries, "number of read queries to fire over the wire")
	fs.IntVar(&cfg.sessions, "sessions", cfg.sessions,
		"driver sessions: concurrent sessions in pooled mode, sessions opened per connection otherwise")
	fs.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")

	fs.IntVar(&cfg.connections, "connections", cfg.connections,
		"number of INDEPENDENT Bolt connections to open (0 = one shared driver pool of -sessions sessions)")
	fs.IntVar(&cfg.maxConnections, "max-connections", cfg.maxConnections,
		"bolt/server Options.MaxConnections (0 = derive from the offered load)")
	fs.IntVar(&cfg.repetitions, "repetitions", cfg.repetitions,
		"unprofiled measurement windows per run, so benchstat has a spread")
	fs.DurationVar(&cfg.connectTimeout, "connect-timeout", cfg.connectTimeout,
		"client dial and connection-acquisition timeout")

	fs.IntVar(&cfg.mutexFraction, "mutex-fraction", cfg.mutexFraction,
		"runtime.SetMutexProfileFraction for the profiled window (0 disables)")
	fs.IntVar(&cfg.blockRate, "block-rate", cfg.blockRate,
		"runtime.SetBlockProfileRate in ns for the profiled window (0 disables)")
	fs.StringVar(&cfg.serverLog, "server-log", cfg.serverLog,
		"logger handed to bolt/server Options.Logger: "+strings.Join(serverLogModes[:], " | "))
	fs.StringVar(&cfg.artifactDir, "artifact-dir", cfg.artifactDir,
		"if set, write this run's full artefact set here (five profiles, trace, metrics.json, host.json, bench.txt)")
	fs.StringVar(&cfg.label, "label", cfg.label,
		"benchstat sub-name for this run (default: conn=<n>, or pooled)")

	fs.BoolVar(&cfg.ladder, "ladder", cfg.ladder,
		"sweep the published concurrency ladder, one child process per rung (requires -artifact-dir)")
	fs.StringVar(&cfg.ladderLevels, "ladder-levels", cfg.ladderLevels,
		"comma-separated connection ladder for -ladder")
	fs.IntVar(&cfg.satOffer, "saturation-offer", cfg.satOffer,
		"connections offered by the ladder's saturation rung (0 drops the rung)")
	fs.IntVar(&cfg.satAdmit, "saturation-admit", cfg.satAdmit,
		"Options.MaxConnections for the ladder's saturation rung")

	return exprof.Bind(fs)
}

func main() {
	cfg := defaultConfig()
	prof := bindFlags(flag.CommandLine, &cfg)
	flag.Parse()

	// -artifact-dir owns every profile of an instrumented run, including the
	// trace; letting exprof's own flags run alongside it would start a second
	// CPU profile and fail, or silently write the trace somewhere else.
	if cfg.artifactDir != "" && prof.Enabled() {
		log.Fatal("-artifact-dir owns this run's profiles; do not combine it with -profile-dir or -trace")
	}

	if err := prof.Run(os.Stdout, func() error {
		return run(context.Background(), os.Stdout, cfg)
	}); err != nil {
		log.Fatal(err)
	}
}

// run validates cfg and dispatches: a ladder sweep forks one child per rung,
// and anything else is a single rung measured in this process.
//
// It writes every byte of its output to w and returns wrapped errors rather
// than terminating the process, so a test can capture and assert on both.
//
//nolint:gocritic // hugeParam: run takes the config BY VALUE because docs/examples-standard.md fixes this signature for every example; it is called once per process and hands a pointer to everything below it.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.ladder {
		return runLadder(ctx, w, &cfg)
	}
	return runRung(ctx, w, &cfg)
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
func seed(ctx context.Context, g *lpg.Graph[string, float64], cfg *config) (seedStats, error) {
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
