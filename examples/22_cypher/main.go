// Example 22_cypher — the GoGraph Cypher engine, the module's flagship
// (100% openCypher TCK compliant at the execution level), driven over a
// realistic, seeded social graph.
//
// It builds a labelled property graph that models a small social network and
// exercises the engine across three families of Cypher work:
//
//	Reads — a label scan with a property projection and ORDER BY (the oldest
//	users, with a deterministic name tiebreak); a WHERE filter over a node
//	property (users older than a threshold); and a directed relationship
//	pattern (the KNOWS friendships, plus a bound-relationship read of the
//	since date).
//
//	Traversal — shortestPath and allShortestPaths over the undirected KNOWS
//	relation between two seeded users, cross-checked against an independent
//	in-Go oracle. The oracle mirrors the same KNOWS edges into an undirected
//	graph/csr.CSR and searches it with search.BiBFS; a hand-written BFS over
//	that CSR picks the farthest reachable user as the destination. If the
//	engine and the oracle disagree on the path length that is a module bug, so
//	the agreement is asserted as a fact (sp.len_matches_bibfs=1).
//
//	Writes — the mutation surface via Engine.RunInTx, each effect verified by
//	a follow-up read: a multi-pattern CREATE (a two-hop KNOWS path hung off an
//	existing user); an UNWIND batch create driven by a list-of-maps parameter;
//	MERGE with ON CREATE / ON MATCH run twice to prove idempotency (the second
//	pass creates nothing and takes the ON MATCH branch); SET and REMOVE of a
//	property; and DELETE of a relationship followed by DETACH DELETE of a node.
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
// # Determinism of the writes
//
// Each run builds a fresh graph, so the write battery's per-step deltas are
// independent of prior runs and reproducible for a fixed -seed: the
// multi-pattern CREATE adds two users and two KNOWS edges, the UNWIND adds
// -batch users, the first MERGE adds one user and the second adds none, and
// the DELETE / DETACH DELETE remove one relationship and one node (together
// with that node's remaining edge). The reported deltas and the final counts
// are therefore fixed facts. Because the writes add two KNOWS edges and later
// remove two, the final KNOWS count returns to the build total.
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
	"github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/search"
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
	batch    int   // number of :USER rows the UNWIND write transaction creates
	maxHops  int   // upper bound N for the shortestPath varlen pattern (KNOWS*..N)
	seed     int64 // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins: fifty users, three-to-six acquaintances each, a WHERE
// threshold of 30, a top-five oldest-users projection, a three-row UNWIND
// batch, and a ten-hop cap on the shortestPath search. It builds and queries
// instantly, well under the short-layer 60 s package budget.
func defaultConfig() config {
	return config{
		users:    50,
		knowsMin: 3,
		knowsMax: 6,
		minAge:   30,
		top:      5,
		batch:    3,
		maxHops:  10,
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
	case c.knowsMin < 1:
		return fmt.Errorf("knowsMin must be >= 1 so every user has an acquaintance to traverse, got %d", c.knowsMin)
	case c.top <= 0:
		return fmt.Errorf("top must be > 0, got %d", c.top)
	case c.batch <= 0:
		return fmt.Errorf("batch must be > 0, got %d", c.batch)
	case c.maxHops <= 0:
		return fmt.Errorf("maxHops must be > 0, got %d", c.maxHops)
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
	flag.IntVar(&cfg.batch, "batch", cfg.batch, "number of :USER rows the UNWIND write transaction creates")
	flag.IntVar(&cfg.maxHops, "max-hops", cfg.maxHops, "upper bound N for the shortestPath varlen pattern (KNOWS*..N)")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	prof := exprof.Bind(flag.CommandLine)
	flag.Parse()

	if err := prof.Run(os.Stdout, func() error {
		return run(context.Background(), os.Stdout, cfg)
	}); err != nil {
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

	// Multigraph: true is required for openCypher semantics — CREATE always adds
	// a relationship, including a parallel edge between an existing node pair.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
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
	if err := runQueries(ctx, eng, g, cfg, stats, w); err != nil {
		return fmt.Errorf("queries: %w", err)
	}
	return nil
}

// buildStats reports the realised shape of a build (the random degrees
// mean the edge total is not known until the graph is materialised) plus
// the wall-clock cost and an anchor user whose name is unique within the
// dataset so a relationship-pattern sample is deterministic.
type buildStats struct {
	anchorName string // name of userIDs[0]; unique, used to anchor a sample read
	anchorID   string // id of userIDs[0]; unique, anchors the shortestPath source
	users      int
	knowsEdges int
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
		anchorID:   userIDs[0],
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
			b[i] = byte(rng.Intn(256)) //nolint:gosec // G115: rng.Intn(256) returns [0,256), exactly the byte range.
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

// runQueries executes the example's Cypher query set against eng. It reports
// deterministic result facts as bare lines and per-query latency as "# "
// telemetry, and renders a small human-readable sample of the projection and
// relationship-pattern results as "# " sample lines. The reads run first, then
// the traversal cross-check, then the write battery, so every read observes the
// pristine seeded graph before any mutation.
func runQueries(ctx context.Context, eng *cypher.Engine, g *lpg.Graph[string, float64], cfg config, stats buildStats, w io.Writer) error {
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

	// 4. shortestPath / allShortestPaths over undirected KNOWS, cross-checked
	//    against an independent search.BiBFS oracle built from the same edges.
	if err := queryShortestPaths(ctx, eng, g, cfg, stats, w); err != nil {
		return err
	}

	// 5. The write battery: CREATE, UNWIND, MERGE (idempotency), SET, REMOVE,
	//    DELETE and DETACH DELETE, each verified by a follow-up read.
	return queryWrites(ctx, eng, cfg, stats, w)
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

// ─────────────────────────────────────────────────────────────────────────────
// Traversal: shortestPath / allShortestPaths, cross-checked against an oracle
// ─────────────────────────────────────────────────────────────────────────────

// queryShortestPaths runs the engine's shortestPath and allShortestPaths over
// the undirected KNOWS relation and cross-checks the result against a search
// oracle built from the same edges.
//
// It first mirrors the graph's KNOWS edges into an undirected graph/csr.CSR (a
// snapshot independent of the engine's query path), runs a hand-written BFS to
// pick the farthest reachable user from the anchor as the destination, and
// confirms the distance with search.BiBFS. It then asks the engine for
// shortestPath((anchor)-[:KNOWS*..maxHops]-(dst)). If the engine's length
// disagrees with the oracle, that is a module bug: the mismatch is reported as
// a fact (sp.len_matches_bibfs=0) and surfaced as an error. allShortestPaths is
// checked for the invariant that every returned path is a shortest one.
func queryShortestPaths(ctx context.Context, eng *cypher.Engine, g *lpg.Graph[string, float64], cfg config, stats buildStats, w io.Writer) error {
	oracle, err := buildUndirectedKnows(ctx, g)
	if err != nil {
		return fmt.Errorf("shortest_path oracle: %w", err)
	}
	oracleCSR := csr.BuildFromAdjList(oracle)

	src, ok := oracle.Mapper().Lookup(stats.anchorID)
	if !ok {
		return fmt.Errorf("shortest_path: anchor user %q has no KNOWS edges", stats.anchorID)
	}

	// Hand-written BFS over the oracle CSR: pick the farthest reachable user
	// (ties broken by the smallest id) as the destination. Sharing no code
	// with the engine, it is an independent shortest-distance reference.
	dist, err := bfsDistances(ctx, oracleCSR, src)
	if err != nil {
		return err
	}
	dstID, oracleDist := farthest(oracleCSR, oracle, dist)
	if oracleDist < 0 {
		return fmt.Errorf("shortest_path: anchor user %q reaches no other user", stats.anchorID)
	}

	// search.BiBFS on the same pair: the module's own bidirectional search, a
	// second independent path from the Cypher shortestPath operator.
	dstNode, _ := oracle.Mapper().Lookup(dstID)
	bfsPath, err := search.BiBFSCtx(ctx, oracleCSR, src, dstNode)
	if err != nil {
		return fmt.Errorf("shortest_path BiBFS: %w", err)
	}
	biHops := len(bfsPath) - 1

	// Cypher shortestPath over undirected KNOWS, bounded by -max-hops and
	// anchored on the two users by their unique id property.
	params := map[string]expr.Value{
		"aid": expr.StringValue(stats.anchorID),
		"bid": expr.StringValue(dstID),
	}
	spQuery := fmt.Sprintf(
		"MATCH (a:USER {id:$aid}),(b:USER {id:$bid}) "+
			"MATCH p = shortestPath((a)-[:KNOWS*..%d]-(b)) RETURN length(p) AS len", cfg.maxHops)
	spStart := time.Now()
	cypherLen, spRows, err := scalarLen(ctx, eng, spQuery, params)
	if err != nil {
		return fmt.Errorf("shortest_path: %w", err)
	}
	spLatency := time.Since(spStart)
	if spRows == 0 {
		return fmt.Errorf("shortest_path: engine found no path within %d hops, but the oracle found one of length %d — module inconsistency", cfg.maxHops, oracleDist)
	}

	// allShortestPaths: every enumerated path must be a shortest path.
	aspQuery := fmt.Sprintf(
		"MATCH (a:USER {id:$aid}),(b:USER {id:$bid}) "+
			"MATCH p = allShortestPaths((a)-[:KNOWS*..%d]-(b)) RETURN length(p) AS len", cfg.maxHops)
	aspStart := time.Now()
	aspLens, err := collectLens(ctx, eng, aspQuery, params)
	if err != nil {
		return fmt.Errorf("all_shortest_paths: %w", err)
	}
	aspLatency := time.Since(aspStart)
	allMin := len(aspLens) > 0
	for _, l := range aspLens {
		if l != cypherLen {
			allMin = false
		}
	}

	match := cypherLen == int64(biHops) && oracleDist == biHops
	fmt.Fprintf(w, "sp.len=%d\n", cypherLen)
	fmt.Fprintf(w, "sp.len_matches_bibfs=%d\n", boolToInt(match))
	fmt.Fprintf(w, "asp.count=%d\n", len(aspLens))
	fmt.Fprintf(w, "asp.all_min_length=%d\n", boolToInt(allMin))
	fmt.Fprintf(w, "# sp.oracle_bfs_dist=%d bibfs_hops=%d\n", oracleDist, biHops)
	fmt.Fprintf(w, "# sp.latency=%s\n", spLatency.Round(time.Microsecond))
	fmt.Fprintf(w, "# asp.latency=%s\n", aspLatency.Round(time.Microsecond))

	if !match {
		return fmt.Errorf("shortest_path length disagreement: cypher=%d bibfs=%d oracle_bfs=%d — module bug", cypherLen, biHops, oracleDist)
	}
	return nil
}

// buildUndirectedKnows returns an undirected adjacency holding exactly the
// KNOWS edges currently stored in g, mirroring each directed KNOWS into an
// undirected edge. It reads the same adjacency the engine queries, so a search
// over it exercises the identical edge set on an independent code path.
// (csr.BuildFromAdjList on g's own adjacency would yield a directed CSR, which
// would not match the undirected shortestPath pattern.)
func buildUndirectedKnows(ctx context.Context, g *lpg.Graph[string, float64]) (*adjlist.AdjList[string, float64], error) {
	oracle := adjlist.New[string, float64](adjlist.Config{Directed: false})
	adj := g.AdjList()
	var walkErr error
	visited := 0
	adj.Mapper().Walk(func(nid graph.NodeID, key string) bool {
		if visited%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				walkErr = err
				return false
			}
		}
		visited++
		neighbours, _ := adj.LoadEntry(nid)
		for _, nb := range neighbours {
			nbKey, ok := adj.Mapper().Resolve(nb)
			if !ok {
				continue
			}
			if err := oracle.AddEdge(key, nbKey, 1); err != nil {
				walkErr = fmt.Errorf("AddEdge %s-%s: %w", key, nbKey, err)
				return false
			}
		}
		return true
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return oracle, nil
}

// bfsDistances computes the unweighted hop distance from src to every node of
// the CSR, returning a slice indexed by NodeID with -1 for unreachable nodes.
// It is a plain textbook BFS, deliberately sharing no code with the engine.
func bfsDistances(ctx context.Context, c *csr.CSR[float64], src graph.NodeID) ([]int, error) {
	dist := make([]int, int(c.MaxNodeID())+1) //nolint:gosec // G115: MaxNodeID is bounded by the node count of the CSR just built, which fits addressable memory.
	for i := range dist {
		dist[i] = -1
	}
	dist[src] = 0
	queue := make([]graph.NodeID, 0, len(dist))
	queue = append(queue, src)
	for head := 0; head < len(queue); head++ {
		if head%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		cur := queue[head]
		for nb := range c.NeighboursByID(cur) {
			if dist[nb] < 0 {
				dist[nb] = dist[cur] + 1
				queue = append(queue, nb)
			}
		}
	}
	return dist, nil
}

// farthest returns the id and distance of the reachable node at the greatest
// hop distance in dist, breaking ties by the smallest id so the choice is
// deterministic regardless of the mapper's internal node ordering. The source
// itself (distance 0) is excluded. dstID is empty and dist -1 when no other
// node is reachable.
func farthest(c *csr.CSR[float64], m *adjlist.AdjList[string, float64], dist []int) (dstID string, dstDist int) {
	dstDist = -1
	for id := graph.NodeID(0); id <= c.MaxNodeID(); id++ {
		d := dist[id]
		if d <= 0 {
			continue
		}
		key, ok := m.Mapper().Resolve(id)
		if !ok {
			continue
		}
		if d > dstDist || (d == dstDist && key < dstID) {
			dstDist, dstID = d, key
		}
	}
	return dstID, dstDist
}

// scalarLen runs a query whose rows each carry a single integer column len and
// returns the last such value together with the number of rows seen.
func scalarLen(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value) (int64, int, error) {
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = res.Close() }()

	var val int64
	rows := 0
	for res.Next() {
		v := res.Record()["len"]
		iv, ok := v.(expr.IntegerValue)
		if !ok {
			return 0, 0, fmt.Errorf("column len is %T, want expr.IntegerValue", v)
		}
		val = int64(iv)
		rows++
	}
	if err := res.Err(); err != nil {
		return 0, 0, err
	}
	return val, rows, nil
}

// collectLens runs a query whose rows each carry a single integer column len
// and returns every value in row order.
func collectLens(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value) ([]int64, error) {
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()

	var out []int64
	for res.Next() {
		v := res.Record()["len"]
		iv, ok := v.(expr.IntegerValue)
		if !ok {
			return nil, fmt.Errorf("column len is %T, want expr.IntegerValue", v)
		}
		out = append(out, int64(iv))
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Writes: the full mutation surface via Engine.RunInTx
// ─────────────────────────────────────────────────────────────────────────────

// Fixed identifiers and property values used by the write battery. The ids are
// deliberately not 24-char hex, so they never collide with a generated user id;
// MERGE and every write anchor on id, so the writes are independent of the
// seeded names.
const (
	writeSince = "2020-06-15" // since date carried by the newly created KNOWS edges

	pathBID   = "cx-path-b" // multi-pattern CREATE: first new user (b)
	pathBName = "Grace Path"
	pathBAge  = 33
	pathCID   = "cx-path-c" // multi-pattern CREATE: second new user (c)
	pathCName = "Henry Path"
	pathCAge  = 34

	mergeID      = "cx-merge-probe" // MERGE / SET / REMOVE target
	mergeName    = "Merl Probe"
	mergeAge     = 50 // age set by ON CREATE
	mergeSetAge  = 77 // age written by the SET write
	mergeCityDef = "Lisbon"
)

// queryWrites drives the full Cypher write surface through Engine.RunInTx over
// the seeded graph. Each mutation's effect is verified by a follow-up read and
// reported as a deterministic fact: node/edge deltas, the MERGE idempotency
// contract (a second identical MERGE creates nothing and takes ON MATCH), the
// SET and REMOVE read-backs, and the DELETE / DETACH DELETE removals. The final
// :USER and KNOWS counts pin the aggregate end state.
func queryWrites(ctx context.Context, eng *cypher.Engine, cfg config, stats buildStats, w io.Writer) error {
	// A single latched error keeps the sequential body readable: every helper
	// becomes a no-op once something has failed, and the error surfaces at the
	// end. Each step still verifies its own effect.
	var ferr error
	users := func() int64 {
		if ferr != nil {
			return 0
		}
		n, _, err := scalarCount(ctx, eng, "MATCH (u:USER) RETURN count(u) AS c", nil)
		if err != nil {
			ferr = err
		}
		return n
	}
	knows := func() int64 {
		if ferr != nil {
			return 0
		}
		n, _, err := scalarCount(ctx, eng, "MATCH (:USER)-[:KNOWS]->(:USER) RETURN count(*) AS c", nil)
		if err != nil {
			ferr = err
		}
		return n
	}
	do := func(label, query string, params map[string]expr.Value) {
		if ferr != nil {
			return
		}
		if err := execWrite(ctx, eng, query, params); err != nil {
			ferr = fmt.Errorf("%s: %w", label, err)
		}
	}
	ageOf := func(id string) int64 {
		if ferr != nil {
			return 0
		}
		n, _, err := scalarCount(ctx, eng, "MATCH (u:USER {id:$id}) RETURN u.age AS c", map[string]expr.Value{"id": expr.StringValue(id)})
		if err != nil {
			ferr = err
		}
		return n
	}
	tagOf := func(id string) (string, bool) {
		if ferr != nil {
			return "", true
		}
		v, isNull, err := scalarStringOrNull(ctx, eng,
			"MATCH (u:USER {id:$id}) RETURN u.mergeTag AS v", map[string]expr.Value{"id": expr.StringValue(id)})
		if err != nil {
			ferr = err
		}
		return v, isNull
	}

	start := time.Now()
	u0, k0 := users(), knows()

	// W1 — multi-pattern CREATE: a two-hop KNOWS path (anchor → b → c) hung off
	// an existing user, adding two users and two relationships in one statement.
	do("create_path",
		"MATCH (a:USER {id:$aid}) "+
			"CREATE (a)-[:KNOWS {since:$since}]->"+
			"(b:USER {id:$bid, name:$bname, age:$bage, city:$city})-[:KNOWS {since:$since}]->"+
			"(c:USER {id:$cid, name:$cname, age:$cage, city:$city})",
		map[string]expr.Value{
			"aid": expr.StringValue(stats.anchorID), "since": expr.StringValue(writeSince),
			"bid": expr.StringValue(pathBID), "bname": expr.StringValue(pathBName), "bage": expr.IntegerValue(pathBAge),
			"cid": expr.StringValue(pathCID), "cname": expr.StringValue(pathCName), "cage": expr.IntegerValue(pathCAge),
			"city": expr.StringValue(mergeCityDef),
		})
	u1, k1 := users(), knows()

	// W2 — UNWIND batch create driven by a list-of-maps parameter.
	rows := make(expr.ListValue, cfg.batch)
	for i := 0; i < cfg.batch; i++ {
		rows[i] = expr.MapValue{
			"id":   expr.StringValue(fmt.Sprintf("cx-batch-%d", i)),
			"name": expr.StringValue(fmt.Sprintf("Batch User %d", i)),
			"age":  expr.IntegerValue(int64(minUserAge + i)),
			"city": expr.StringValue(cities[i%len(cities)]),
		}
	}
	do("unwind_create",
		"UNWIND $rows AS row CREATE (:USER {id: row.id, name: row.name, age: row.age, city: row.city})",
		map[string]expr.Value{"rows": rows})
	u2 := users()

	// W3 — MERGE, first pass: the probe user does not exist, so ON CREATE fires.
	mergeQuery := "MERGE (u:USER {id:$mid}) " +
		"ON CREATE SET u.name=$name, u.age=$age, u.mergeTag='created' " +
		"ON MATCH SET u.mergeTag='matched'"
	mergeParams := map[string]expr.Value{
		"mid": expr.StringValue(mergeID), "name": expr.StringValue(mergeName), "age": expr.IntegerValue(mergeAge),
	}
	do("merge_create", mergeQuery, mergeParams)
	u3 := users()
	tagAfterCreate, _ := tagOf(mergeID)

	// W4 — MERGE, second pass: the identical MERGE now matches, so it creates
	// nothing and takes the ON MATCH branch. This proves idempotency.
	do("merge_match", mergeQuery, mergeParams)
	u4 := users()
	tagAfterMatch, _ := tagOf(mergeID)

	// W5 — SET a property, then read it back.
	do("set_property",
		"MATCH (u:USER {id:$mid}) SET u.age=$age",
		map[string]expr.Value{"mid": expr.StringValue(mergeID), "age": expr.IntegerValue(mergeSetAge)})
	ageAfterSet := ageOf(mergeID)

	// W6 — REMOVE a property, then confirm it reads back as null.
	do("remove_property",
		"MATCH (u:USER {id:$mid}) REMOVE u.mergeTag",
		map[string]expr.Value{"mid": expr.StringValue(mergeID)})
	nullCount := int64(0)
	if ferr == nil {
		nullCount, _, ferr = scalarCount(ctx, eng,
			"MATCH (u:USER {id:$mid}) WHERE u.mergeTag IS NULL RETURN count(u) AS c",
			map[string]expr.Value{"mid": expr.StringValue(mergeID)})
	}

	// W7 — DELETE a relationship (the b → c edge); both endpoint nodes survive.
	uPreDel, kPreDel := users(), knows()
	do("delete_rel",
		"MATCH (:USER {id:$bid})-[r:KNOWS]->(:USER {id:$cid}) DELETE r",
		map[string]expr.Value{"bid": expr.StringValue(pathBID), "cid": expr.StringValue(pathCID)})
	uPostDel, kPostDel := users(), knows()

	// W8 — DETACH DELETE a node (b), which still has the incoming anchor → b
	// edge: DETACH DELETE removes the node and that remaining relationship.
	do("detach_delete",
		"MATCH (u:USER {id:$bid}) DETACH DELETE u",
		map[string]expr.Value{"bid": expr.StringValue(pathBID)})
	uFinal, kFinal := users(), knows()

	if ferr != nil {
		return ferr
	}

	fmt.Fprintf(w, "create.node_delta=%d\n", u1-u0)
	fmt.Fprintf(w, "create.edge_delta=%d\n", k1-k0)
	fmt.Fprintf(w, "unwind.created=%d\n", u2-u1)
	fmt.Fprintf(w, "merge.created=%d\n", u3-u2)
	fmt.Fprintf(w, "merge.created_second_pass=%d\n", u4-u3)
	fmt.Fprintf(w, "merge.matched_second_pass=%d\n", boolToInt(u4-u3 == 0 && tagAfterMatch == "matched"))
	fmt.Fprintf(w, "set.updated=%d\n", boolToInt(ageAfterSet == mergeSetAge))
	fmt.Fprintf(w, "remove.cleared=%d\n", boolToInt(nullCount == 1))
	fmt.Fprintf(w, "delete.rel_removed=%d\n", kPreDel-kPostDel)
	fmt.Fprintf(w, "delete.nodes_kept=%d\n", boolToInt(uPreDel == uPostDel))
	fmt.Fprintf(w, "detach.node_removed=%d\n", uPostDel-uFinal)
	fmt.Fprintf(w, "detach.rel_removed=%d\n", kPostDel-kFinal)
	fmt.Fprintf(w, "users.final=%d\n", uFinal)
	fmt.Fprintf(w, "knows.final=%d\n", kFinal)
	fmt.Fprintf(w, "# write.merge_tag_after_create=%s\n", tagAfterCreate)
	fmt.Fprintf(w, "# write.merge_tag_after_match=%s\n", tagAfterMatch)
	fmt.Fprintf(w, "# write.set_age=%d\n", ageAfterSet)
	fmt.Fprintf(w, "# write.elapsed=%s\n", time.Since(start).Round(time.Microsecond))
	return nil
}

// execWrite runs a write query inside a transaction and drains it to
// completion, applying the mutation atomically. Any rows are discarded; the
// caller verifies the effect with a separate read.
func execWrite(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value) error {
	res, err := eng.RunInTx(ctx, query, params)
	if err != nil {
		return err
	}
	for res.Next() { // drain to apply the write; result rows are not needed here.
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// scalarStringOrNull runs a query whose single row carries a column v and
// returns its string value, or isNull=true when the column is null (or no row
// is produced).
func scalarStringOrNull(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value) (val string, isNull bool, err error) {
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = res.Close() }()

	got := false
	for res.Next() {
		got = true
		v := res.Record()["v"]
		if ev, ok := v.(expr.Value); ok && expr.IsNull(ev) {
			isNull = true
			continue
		}
		if s, ok := v.(expr.StringValue); ok {
			val = string(s)
		}
	}
	if e := res.Err(); e != nil {
		return "", false, e
	}
	if !got {
		return "", true, nil
	}
	return val, isNull, nil
}

// boolToInt renders a boolean invariant as a pinnable 0/1 fact value.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
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
