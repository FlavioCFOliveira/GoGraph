// Example 22_cypher — the GoGraph Cypher engine, the module's flagship
// (100% openCypher TCK compliant at the execution level), driven over a
// realistic, seeded social graph.
//
// It builds a labelled property graph that models a small social network
// and then exercises the engine with the four Cypher idioms this example
// teaches:
//
//   - a label scan with a property projection and ORDER BY (the oldest
//     users, with a deterministic name tiebreak);
//   - a WHERE filter over a node property (users older than a threshold);
//   - a directed relationship pattern (the KNOWS friendships, plus a
//     bound-relationship read of the since date property);
//   - a CREATE inside a write transaction, whose effect is verified by a
//     follow-up read (the user count increments by exactly one).
//
// Every value is read back from the result record and rendered in
// human-readable form — names, ages and dates, never raw node IDs.
//
// # Model
//
//	(:USER {id, name, age, city})           // id is a 24-char hex string
//	(:USER)-[:KNOWS {since}]->(:USER)        // knowsMin..knowsMax per user
//
// KNOWS is a directed out-edge: every user is given a random out-degree
// in [knowsMin, knowsMax] to distinct other users (no self-loops, no
// duplicate targets). Each KNOWS carries exactly one mandatory date
// property, since, recording when the acquaintance began.
//
// since is stored as an ISO-8601 (YYYY-MM-DD) string — the representation
// examples 25 and 26 use for Cypher-queryable dates — so the cypher.Engine
// reads it back as a non-null value and, because ISO-8601 sorts
// chronologically, ORDER BY and range predicates over since behave as
// dates. (lpg.TimeValue is not used: the Cypher reader maps it to null,
// whereas the tagged date string round-trips.) The dates are drawn from
// the seeded RNG, anchored to a fixed reference date rather than the wall
// clock, so they are reproducible for a given -seed.
//
// # Scale
//
// Run with no flags, the example builds a small, deterministic default —
// fifty users with three-to-six acquaintances each — so the run is instant
// and the deterministic facts are pinned by the regression test. Every
// dimension is a flag, so the same binary scales up to a size where the
// per-query latency and live-heap telemetry become observable:
//
//	go run ./examples/22_cypher -users 200000 -knows-max 30 -seed 7
//
// The deterministic data shape is reproducible for a fixed -seed; only the
// telemetry (lines prefixed with "# ") varies between runs and machines.
//
// # Determinism of the CREATE
//
// The CREATE mutates the graph, so the example reads the :USER count
// immediately before and after the write and reports the delta as a fact
// (create.user_delta=1). Each run builds a fresh graph, so re-runs are
// independent and the delta is always exactly one.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"runtime"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Node labels, relationship types, and property keys. Centralised so the
// model is described in exactly one place and a rename surfaces as a
// compile error everywhere it is used.
const (
	labelUser = "USER"

	relKnows = "KNOWS" // (:USER)-[:KNOWS {since}]->(:USER)

	propID    = "id"
	propName  = "name"
	propAge   = "age"
	propCity  = "city"
	propSince = "since" // mandatory KNOWS date (ISO-8601), see isoEdgeDate
)

// config captures every scale and shape knob of the example. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	users    int   // number of :USER nodes
	knowsMin int   // minimum KNOWS out-degree per user (inclusive)
	knowsMax int   // maximum KNOWS out-degree per user (inclusive)
	minAge   int64 // WHERE-filter threshold: keep users with age > minAge
	top      int   // ORDER BY ... LIMIT row count for the oldest-users query
	seed     int64 // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins: fifty users, three-to-six acquaintances each, a WHERE
// threshold of 30, and a top-five oldest-users projection. It builds and
// queries instantly, well under the short-layer 60 s package budget.
func defaultConfig() config {
	return config{
		users:    50,
		knowsMin: 3,
		knowsMax: 6,
		minAge:   30,
		top:      5,
		seed:     1,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape — for instance more acquaintances than there are other users to
// know. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.users <= 0:
		return fmt.Errorf("users must be > 0, got %d", c.users)
	case c.knowsMin < 0 || c.knowsMax < c.knowsMin:
		return fmt.Errorf("require 0 <= knowsMin <= knowsMax, got [%d,%d]", c.knowsMin, c.knowsMax)
	case c.knowsMax >= c.users:
		return fmt.Errorf("knowsMax (%d) exceeds users-1 (%d): not enough distinct acquaintances", c.knowsMax, c.users-1)
	case c.top <= 0:
		return fmt.Errorf("top must be > 0, got %d", c.top)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.users, "users", cfg.users, "number of USER nodes")
	flag.IntVar(&cfg.knowsMin, "knows-min", cfg.knowsMin, "minimum KNOWS out-degree per user")
	flag.IntVar(&cfg.knowsMax, "knows-max", cfg.knowsMax, "maximum KNOWS out-degree per user")
	flag.Int64Var(&cfg.minAge, "min-age", cfg.minAge, "WHERE threshold: keep users with age greater than this")
	flag.IntVar(&cfg.top, "top", cfg.top, "row count for the oldest-users ORDER BY ... LIMIT query")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run builds the social network described by cfg, exercises the Cypher
// query set against it, and writes a report to w. Bare lines carry
// deterministic facts (counts and query results, reproducible for a fixed
// seed); lines prefixed with "# " carry volatile telemetry (per-query
// latency and live-heap figures) that vary per run and per machine. All
// output goes to w so a test can capture and assert on the deterministic
// lines. run honours ctx cancellation and returns wrapped errors rather
// than terminating the process.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.users=%d\n", cfg.users)
	fmt.Fprintf(w, "config.knows=[%d,%d]\n", cfg.knowsMin, cfg.knowsMax)
	fmt.Fprintf(w, "config.min_age=%d\n", cfg.minAge)
	fmt.Fprintf(w, "config.top=%d\n", cfg.top)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	stats, err := build(ctx, g, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Build-then-query workload: the graph is fully assembled above and
	// only read from here on (until the single CREATE). Compact right-sizes
	// the adjacency backing arrays so the live-heap telemetry reflects the
	// tight arrays the query phase runs against.
	if err := ctx.Err(); err != nil {
		return err
	}
	g.AdjList().Compact(ctx)

	fmt.Fprintf(w, "nodes.users=%d\n", stats.users)
	fmt.Fprintf(w, "edges.knows=%d\n", stats.knowsEdges)

	mem := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", stats.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(mem.HeapAlloc))

	eng := cypher.NewEngine(g)
	if err := runQueries(ctx, eng, cfg, stats, w); err != nil {
		return fmt.Errorf("queries: %w", err)
	}
	return nil
}

// buildStats reports the realised shape of a build (the random degrees
// mean the edge total is not known until the graph is materialised) plus
// the wall-clock cost and an anchor user whose name is unique within the
// dataset so a relationship-pattern sample is deterministic.
type buildStats struct {
	users      int
	knowsEdges int
	anchorName string // name of userIDs[0]; unique, used to anchor a sample read
	elapsed    time.Duration
}

// build materialises the graph described by cfg into g. It first creates
// every user (so KNOWS targets exist before the edges reference them),
// then the KNOWS edges. User ids are 24-char hex strings drawn from the
// seeded RNG; names and cities are realistic strings from fixed word
// lists; ages are drawn from a plausible adult range. The build honours
// ctx cancellation on a periodic check.
func build(ctx context.Context, g *lpg.Graph[string, float64], cfg config) (buildStats, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	userIDs := make([]string, cfg.users)
	names := make([]string, cfg.users)
	seenID := make(map[string]struct{}, cfg.users)

	// Users. Each carries a unique hex id plus name/age/city properties.
	// Names are made unique by appending the index so the anchored sample
	// query below matches exactly one user regardless of word-list reuse.
	for i := 0; i < cfg.users; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return buildStats{}, err
			}
		}
		id := uniqueHexID(rng, seenID)
		userIDs[i] = id
		name := fmt.Sprintf("%s #%d", realisticName(rng), i)
		names[i] = name
		age := int64(minUserAge + rng.Intn(maxUserAge-minUserAge+1))
		if err := addUser(g, id, name, age, cities[rng.Intn(len(cities))]); err != nil {
			return buildStats{}, err
		}
	}

	// KNOWS edges: each user gets a random out-degree in [knowsMin,
	// knowsMax] to distinct other users.
	knowsEdges := 0
	targets := make(map[int]struct{}, cfg.knowsMax)
	for i := 0; i < cfg.users; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return buildStats{}, err
			}
		}
		degree := cfg.knowsMin + rng.Intn(cfg.knowsMax-cfg.knowsMin+1)
		clear(targets)
		for len(targets) < degree {
			j := rng.Intn(cfg.users)
			if j == i {
				continue
			}
			targets[j] = struct{}{}
		}
		for j := range targets {
			if err := addKnows(g, userIDs[i], userIDs[j], isoEdgeDate(rng)); err != nil {
				return buildStats{}, err
			}
			knowsEdges++
		}
	}

	return buildStats{
		users:      cfg.users,
		knowsEdges: knowsEdges,
		anchorName: names[0],
		elapsed:    time.Since(start),
	}, nil
}

// checkEvery bounds how often the build polls ctx for cancellation: often
// enough that a cancelled large build stops promptly, rare enough that the
// check is free relative to the surrounding work.
const checkEvery = 4096

// minUserAge and maxUserAge bound the plausible adult age range the
// generator draws from (inclusive).
const (
	minUserAge = 18
	maxUserAge = 80
)

// addUser adds a single :USER node carrying its id plus the name, age and
// city properties. age is an integer; the rest are strings.
func addUser(g *lpg.Graph[string, float64], id, name string, age int64, city string) error {
	if err := g.AddNode(id); err != nil {
		return fmt.Errorf("AddNode %s: %w", id, err)
	}
	if err := g.SetNodeLabel(id, labelUser); err != nil {
		return fmt.Errorf("SetNodeLabel %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, propID, lpg.StringValue(id)); err != nil {
		return fmt.Errorf("SetNodeProperty id %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, propName, lpg.StringValue(name)); err != nil {
		return fmt.Errorf("SetNodeProperty name %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, propAge, lpg.Int64Value(age)); err != nil {
		return fmt.Errorf("SetNodeProperty age %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, propCity, lpg.StringValue(city)); err != nil {
		return fmt.Errorf("SetNodeProperty city %s: %w", id, err)
	}
	return nil
}

// addKnows adds a directed, weight-1 KNOWS edge and sets its mandatory
// since date property. AddEdgeLabeled lands the relationship type in the
// edge's inline slot at insertion time (a single O(1)-amortised append),
// so Cypher patterns like [:KNOWS] match. SetEdgeProperty then stores the
// ISO-8601 date the engine reads back as r.since.
func addKnows(g *lpg.Graph[string, float64], src, dst, since string) error {
	if err := g.AddEdgeLabeled(src, dst, 1, relKnows); err != nil {
		return fmt.Errorf("AddEdgeLabeled %s-[%s]->%s: %w", src, relKnows, dst, err)
	}
	if err := g.SetEdgeProperty(src, dst, propSince, lpg.StringValue(since)); err != nil {
		return fmt.Errorf("SetEdgeProperty %s on %s-[%s]->%s: %w", propSince, src, relKnows, dst, err)
	}
	return nil
}

// edgeDateWindowDays bounds how far before the fixed reference date a
// KNOWS may be dated: every since falls within
// [edgeDateRef-edgeDateWindowDays, edgeDateRef]. ~6 years.
const edgeDateWindowDays = 2192

// edgeDateRef is the fixed reference date the synthetic edge dates count
// back from. Anchoring to a constant — never the wall clock — keeps the
// dataset reproducible for a given -seed.
var edgeDateRef = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

// isoEdgeDate returns a deterministic calendar date in ISO-8601 form
// (YYYY-MM-DD) drawn from rng as a whole-day offset back from edgeDateRef.
// ISO-8601 strings sort chronologically, so storing the dates as strings
// keeps ORDER BY and range predicates over since behaving as dates.
func isoEdgeDate(rng *rand.Rand) string {
	return edgeDateRef.AddDate(0, 0, -rng.Intn(edgeDateWindowDays+1)).Format("2006-01-02")
}

// uniqueHexID returns a 24-character lowercase hex id (12 random bytes)
// that has not been handed out before, recording it in seen. Drawing from
// the seeded rng keeps the whole dataset reproducible.
func uniqueHexID(rng *rand.Rand, seen map[string]struct{}) string {
	var b [12]byte
	for {
		for i := range b {
			b[i] = byte(rng.Intn(256))
		}
		id := hex.EncodeToString(b[:])
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		return id
	}
}

// realisticName assembles a plausible "First Last" personal name from
// fixed word lists. build appends a unique index suffix so the stored name
// is unique even when the word draw repeats.
func realisticName(rng *rand.Rand) string {
	return firstNames[rng.Intn(len(firstNames))] + " " + lastNames[rng.Intn(len(lastNames))]
}

// ─────────────────────────────────────────────────────────────────────────────
// Query battery
// ─────────────────────────────────────────────────────────────────────────────

// runQueries executes the example's Cypher query set against eng. It
// reports deterministic result facts as bare lines and per-query latency
// as "# " telemetry, and renders a small human-readable sample of the
// projection and relationship-pattern results as "# " sample lines.
func runQueries(ctx context.Context, eng *cypher.Engine, cfg config, stats buildStats, w io.Writer) error {
	// 1. Label scan + projection + ORDER BY ... LIMIT — the oldest users.
	//    ORDER BY age DESC with a name tiebreak makes the top rows fully
	//    deterministic: the count is pinned and the first row is rendered
	//    as a human-readable sample.
	if err := queryOldestUsers(ctx, eng, cfg, w); err != nil {
		return err
	}

	// 2. WHERE filter over a node property — count users older than the
	//    threshold, passed as a query parameter.
	if err := queryOlderThanCount(ctx, eng, cfg, w); err != nil {
		return err
	}

	// 3. Relationship pattern — count the KNOWS friendships, then read one
	//    bound-relationship's since date from the anchor user as a sample.
	if err := queryKnows(ctx, eng, stats, w); err != nil {
		return err
	}

	// 4. CREATE inside a write transaction, verified by a follow-up read:
	//    the :USER count must increment by exactly one.
	return queryCreate(ctx, eng, w)
}

// queryOldestUsers runs the label-scan projection ordered by descending
// age with a name tiebreak, reporting the row count as a fact and the top
// row as a human-readable "# " sample.
func queryOldestUsers(ctx context.Context, eng *cypher.Engine, cfg config, w io.Writer) error {
	query := fmt.Sprintf(
		"MATCH (u:USER) RETURN u.name AS name, u.age AS age "+
			"ORDER BY age DESC, name ASC LIMIT %d", cfg.top)
	start := time.Now()
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return fmt.Errorf("oldest_users: %w", err)
	}
	defer func() { _ = res.Close() }()

	rows := 0
	var topName string
	var topAge int64
	for res.Next() {
		rec := res.Record()
		name, err := stringCell(rec, "name")
		if err != nil {
			return err
		}
		age, err := intCell(rec, "age")
		if err != nil {
			return err
		}
		if rows == 0 {
			topName, topAge = name, age
		}
		rows++
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("oldest_users result: %w", err)
	}
	d := time.Since(start)
	fmt.Fprintf(w, "q.oldest_users.rows=%d\n", rows)
	fmt.Fprintf(w, "# q.oldest_users.latency=%s\n", d.Round(time.Microsecond))
	fmt.Fprintf(w, "# q.oldest_users.sample=%q age %d\n", topName, topAge)
	return nil
}

// queryOlderThanCount runs the WHERE-filter count of users older than the
// configured threshold (passed as a parameter), reporting the count as a
// fact and the latency as telemetry.
func queryOlderThanCount(ctx context.Context, eng *cypher.Engine, cfg config, w io.Writer) error {
	const query = "MATCH (u:USER) WHERE u.age > $min RETURN count(u) AS c"
	params := map[string]expr.Value{"min": expr.IntegerValue(cfg.minAge)}
	n, d, err := scalarCount(ctx, eng, query, params)
	if err != nil {
		return fmt.Errorf("older_than: %w", err)
	}
	fmt.Fprintf(w, "q.older_than=%d\n", n)
	fmt.Fprintf(w, "# q.older_than.latency=%s\n", d.Round(time.Microsecond))
	return nil
}

// queryKnows counts the KNOWS relationships (a directed relationship
// pattern) and then reads the since date of one acquaintance of the anchor
// user as a deterministic human-readable sample. The sample query orders
// by since then the friend's name so its first row is stable.
func queryKnows(ctx context.Context, eng *cypher.Engine, stats buildStats, w io.Writer) error {
	const countQuery = "MATCH (:USER)-[:KNOWS]->(:USER) RETURN count(*) AS c"
	n, d, err := scalarCount(ctx, eng, countQuery, nil)
	if err != nil {
		return fmt.Errorf("knows_count: %w", err)
	}
	fmt.Fprintf(w, "q.knows_count=%d\n", n)
	fmt.Fprintf(w, "# q.knows_count.latency=%s\n", d.Round(time.Microsecond))

	const sampleQuery = "MATCH (a:USER {name:$name})-[r:KNOWS]->(b:USER) " +
		"RETURN b.name AS friend, r.since AS since ORDER BY since ASC, friend ASC LIMIT 1"
	params := map[string]expr.Value{"name": expr.StringValue(stats.anchorName)}
	start := time.Now()
	res, err := eng.Run(ctx, sampleQuery, params)
	if err != nil {
		return fmt.Errorf("knows_sample: %w", err)
	}
	defer func() { _ = res.Close() }()

	rows := 0
	var friend, since string
	for res.Next() {
		rec := res.Record()
		friend, err = stringCell(rec, "friend")
		if err != nil {
			return err
		}
		since, err = stringCell(rec, "since")
		if err != nil {
			return err
		}
		rows++
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("knows_sample result: %w", err)
	}
	fmt.Fprintf(w, "q.knows_sample.rows=%d\n", rows)
	fmt.Fprintf(w, "# q.knows_sample.latency=%s\n", time.Since(start).Round(time.Microsecond))
	if rows > 0 {
		fmt.Fprintf(w, "# q.knows_sample=%q KNOWS %q since %s\n", stats.anchorName, friend, since)
	}
	return nil
}

// queryCreate reads the :USER count, runs a CREATE inside a write
// transaction, then reads the count again. The CREATE's effect is verified
// by the follow-up read: create.user_delta is the post-minus-pre count and
// must be exactly one. Each run builds a fresh graph, so the delta is
// independent of prior runs.
func queryCreate(ctx context.Context, eng *cypher.Engine, w io.Writer) error {
	const countQuery = "MATCH (u:USER) RETURN count(u) AS c"
	before, _, err := scalarCount(ctx, eng, countQuery, nil)
	if err != nil {
		return fmt.Errorf("create_count_before: %w", err)
	}

	const createQuery = `CREATE (u:USER {name: "Frank Newcomer", age: 41, city: "Lisbon"})`
	start := time.Now()
	res, err := eng.RunInTx(ctx, createQuery, nil)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	for res.Next() { //nolint:revive // empty body: CREATE yields no rows; drain to completion.
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return fmt.Errorf("create result: %w", err)
	}
	if err := res.Close(); err != nil {
		return fmt.Errorf("create close: %w", err)
	}
	d := time.Since(start)

	after, _, err := scalarCount(ctx, eng, countQuery, nil)
	if err != nil {
		return fmt.Errorf("create_count_after: %w", err)
	}

	fmt.Fprintf(w, "q.users_before_create=%d\n", before)
	fmt.Fprintf(w, "q.users_after_create=%d\n", after)
	fmt.Fprintf(w, "create.user_delta=%d\n", after-before)
	fmt.Fprintf(w, "# q.create.latency=%s\n", d.Round(time.Microsecond))
	return nil
}

// scalarCount runs a query whose single row has a single integer column c
// and returns that integer plus the wall-clock time the query took.
func scalarCount(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value) (int64, time.Duration, error) {
	start := time.Now()
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = res.Close() }()

	var n int64
	var got bool
	for res.Next() {
		rec := res.Record()
		v, ok := rec["c"]
		if !ok {
			return 0, 0, fmt.Errorf("column %q missing", "c")
		}
		iv, ok := v.(expr.IntegerValue)
		if !ok {
			return 0, 0, fmt.Errorf("column c is %T, want expr.IntegerValue", v)
		}
		n = int64(iv)
		got = true
	}
	if err := res.Err(); err != nil {
		return 0, 0, err
	}
	if !got {
		return 0, 0, fmt.Errorf("query returned no rows")
	}
	return n, time.Since(start), nil
}

// stringCell reads column col from rec and returns its underlying Go
// string. The Cypher engine returns a projected string property as an
// expr.StringValue; this unwraps it to the bare string (printing the value
// directly would emit the quoted form).
func stringCell(rec map[string]any, col string) (string, error) {
	v, ok := rec[col]
	if !ok {
		return "", fmt.Errorf("column %q missing from record", col)
	}
	s, ok := v.(expr.StringValue)
	if !ok {
		return "", fmt.Errorf("column %q is %T, want expr.StringValue", col, v)
	}
	return string(s), nil
}

// intCell reads column col from rec and returns its underlying int64. The
// Cypher engine returns a projected integer property as an
// expr.IntegerValue.
func intCell(rec map[string]any, col string) (int64, error) {
	v, ok := rec[col]
	if !ok {
		return 0, fmt.Errorf("column %q missing from record", col)
	}
	n, ok := v.(expr.IntegerValue)
	if !ok {
		return 0, fmt.Errorf("column %q is %T, want expr.IntegerValue", col, v)
	}
	return int64(n), nil
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
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller",
	"Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez",
	"Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin",
	"Lee", "Perez", "Thompson", "White", "Harris", "Sanchez", "Clark",
}

var cities = []string{
	"Lisbon", "Porto", "Madrid", "Paris", "Berlin", "Rome", "Amsterdam",
	"Dublin", "Vienna", "Prague", "Warsaw", "Oslo", "Helsinki", "Athens",
}
