// Example 26_social_scale_bench — a large-scale social-network benchmark
// for query performance and resource consumption.
//
// It builds a labelled property graph that models a social network and
// then runs a battery of representative Cypher queries against it,
// reporting both the deterministic shape of the data and the volatile
// telemetry — build throughput, per-query latency, and Go heap
// consumption — that make this example a benchmark rather than a
// demonstration.
//
// # Query battery
//
// The read battery spans the breadth of the engine, in three groups:
//
//   - Counts and traversal: label-scan and relationship counts, the
//     always-filled date-coverage counts, a friend-of-friend traversal, and
//     the trending-articles grouped aggregation.
//   - Analytical aggregation and subqueries: the friend out-degree
//     distribution via min / max / avg and percentileCont (median); an
//     EXISTS { } / NOT EXISTS { } subquery split; a CASE bucketing
//     projection; a UNION ALL of two label-count streams; an UNWIND $ids
//     batch point-read; and id() / elementId() on a matched node.
//   - Temporal functions: date() / datetime() constructors, the
//     duration.between family (duration.inDays / inSeconds) with duration
//     component access, Date − Duration arithmetic, and date.truncate — all
//     anchored to the fixed reference date so the results stay deterministic.
//
// # Model
//
//	(:USER    {id, name})                       // id is a 24-char hex string
//	(:ARTICLE {id, title})                      // id is a 24-char hex string
//	(:USER)-[:FRIEND {since}]->(:USER)          // friendsMin..friendsMax per user
//	(:USER)-[:LIKE   {when}]->(:ARTICLE)        // 0..likesMax per user
//
// FRIEND is modelled as a directed out-edge: every user is given a
// random out-degree in [friendsMin, friendsMax] to distinct other
// users (no self-loops, no duplicate targets). LIKE is a directed
// out-edge to between zero and likesMax distinct articles.
//
// Every relationship carries exactly one mandatory date property: a
// FRIEND records when the friendship was created in since and a LIKE
// records when the like happened in when. Both are always present.
// The dates are written with [lpg.DateValue], which stores a native
// Cypher Date: the storage tier folds it into a compact int32 epoch-day
// column (~4 bytes/value) and the cypher.Engine reads it back as a Date,
// so range and ORDER BY predicates over since/when behave as dates
// natively. (lpg.TimeValue is deliberately NOT used: the Cypher reader
// maps PropTime to null; and a plain ISO-8601 string would read back as
// a String and cost a ~16-byte header plus its backing text — the
// per-edge cost #1649 removed by switching this example to DateValue.)
// The dates are drawn from the seeded RNG, anchored to a fixed reference
// date rather than the wall clock, so they are reproducible for a given
// -seed.
//
// # Scale
//
// Run with no flags, the example builds the full specification — one
// million users, thirty thousand articles, 150-200 friends per user and
// up to 300 likes per user. That is roughly 1.03M nodes and on the
// order of 3.2 × 10^8 edges. The dominant resident cost is the per-edge
// property and adjacency store, which measures ~24.4 bytes per edge at
// 20k/2k after two optimizations: the int32 date column (#1649) brought it
// from the ISO-string column's ~61.8 to ~32.9, and the weightless adjacency
// mode (#1650 — this graph is queried only by relationship type and property,
// never by edge weight, so the per-edge float64 weight column is dead) brought
// it to ~24.4. So the full run needs on the order of ~8 GiB of live heap and
// several minutes to build, down from ~19 GiB before the date column landed.
// The implicit-type mode (-rel-types=false) saves only the small
// relationship-label store on top; the date-property store is identical in both
// modes. This is deliberate: the example exists to stress query performance and
// resource consumption at that scale. See the README's "Memory profile and
// optimizations" section for the per-edge breakdown and how these figures were
// measured.
//
// Every dimension is a flag, so the same binary scales down to a
// laptop-sized run:
//
//	go run ./examples/26_social_scale_bench -users 50000 -articles 5000
//
// The deterministic data shape is reproducible for a fixed -seed; only
// the telemetry (lines prefixed with "# ") varies between runs and
// machines.
//
// # Why in-memory
//
// The benchmark targets read-query latency and live-heap footprint, so
// it builds the graph in memory through the property-graph API and
// queries it with an in-memory [cypher.Engine]. It does not exercise the
// WAL/recovery stack: durably persisting ~3 × 10^8 edges is impractical
// for an example and orthogonal to what this one measures. The
// persistence path is demonstrated by examples 04, 17, 24 and 25.
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
	"strconv"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Node labels and relationship types. Centralised so the model is
// described in exactly one place and a rename surfaces as a compile
// error everywhere it is used.
const (
	labelUser    = "USER"
	labelArticle = "ARTICLE"

	relFriend = "FRIEND" // (:USER)-[:FRIEND {since}]->(:USER)
	relLike   = "LIKE"   // (:USER)-[:LIKE   {when}]->(:ARTICLE)

	// Mandatory per-relationship date properties. Every FRIEND carries a
	// since (when the friendship was created) and every LIKE a when (when
	// the like happened). Stored as native Cypher Dates via lpg.DateValue
	// (see edgeDate), folded into the compact int32 epoch-day column.
	propFriendSince = "since"
	propLikeWhen    = "when"
)

// config captures every scale and shape knob of the benchmark. The
// zero value is not valid; build one with defaultConfig and override
// fields from flags (see main) or construct one directly (see the
// regression test).
type config struct {
	users      int   // number of :USER nodes
	articles   int   // number of :ARTICLE nodes
	friendsMin int   // minimum FRIEND out-degree per user (inclusive)
	friendsMax int   // maximum FRIEND out-degree per user (inclusive)
	likesMax   int   // maximum LIKE out-degree per user (0..likesMax)
	seed       int64 // RNG seed; fixes the deterministic data shape

	// relTypes selects how the two relationship kinds are distinguished.
	// When true (the default, faithful to the model) every edge carries
	// an explicit FRIEND or LIKE relationship type and queries match on
	// it. When false the type is left implicit and inferred from the
	// endpoint labels — FRIEND is the only USER->USER edge and LIKE the
	// only USER->ARTICLE edge — so no per-edge label is stored at all.
	// The implicit mode trades model fidelity for a large cut in
	// resident memory; see the README.
	relTypes bool
}

// defaultConfig returns the full specification this example was written
// to exercise: one million users, thirty thousand articles, 150-200
// friends per user, and up to 300 likes per user.
func defaultConfig() config {
	return config{
		users:      1_000_000,
		articles:   30_000,
		friendsMin: 150,
		friendsMax: 200,
		likesMax:   300,
		seed:       1,
		relTypes:   true,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape — for instance more friends than there are other users to
// befriend. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.users <= 0:
		return fmt.Errorf("users must be > 0, got %d", c.users)
	case c.articles < 0:
		return fmt.Errorf("articles must be >= 0, got %d", c.articles)
	case c.friendsMin < 0 || c.friendsMax < c.friendsMin:
		return fmt.Errorf("require 0 <= friendsMin <= friendsMax, got [%d,%d]", c.friendsMin, c.friendsMax)
	case c.friendsMax >= c.users:
		return fmt.Errorf("friendsMax (%d) exceeds users-1 (%d): not enough distinct friends", c.friendsMax, c.users-1)
	case c.likesMax < 0:
		return fmt.Errorf("likesMax must be >= 0, got %d", c.likesMax)
	case c.likesMax > c.articles:
		return fmt.Errorf("likesMax (%d) exceeds articles (%d): not enough distinct articles to like", c.likesMax, c.articles)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.users, "users", cfg.users, "number of USER nodes")
	flag.IntVar(&cfg.articles, "articles", cfg.articles, "number of ARTICLE nodes")
	flag.IntVar(&cfg.friendsMin, "friends-min", cfg.friendsMin, "minimum FRIEND out-degree per user")
	flag.IntVar(&cfg.friendsMax, "friends-max", cfg.friendsMax, "maximum FRIEND out-degree per user")
	flag.IntVar(&cfg.likesMax, "likes-max", cfg.likesMax, "maximum LIKE out-degree per user")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.BoolVar(&cfg.relTypes, "rel-types", cfg.relTypes,
		"store explicit FRIEND/LIKE relationship types (false: infer type from endpoint labels, no per-edge label stored)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run builds the social network described by cfg, queries it, and
// writes a report to w. Bare lines carry deterministic facts (counts
// and query results, reproducible for a fixed seed); lines prefixed
// with "# " carry volatile telemetry (durations and heap figures) that
// vary per run and per machine. All output goes to w so a test can
// capture and assert on the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.users=%d\n", cfg.users)
	fmt.Fprintf(w, "config.articles=%d\n", cfg.articles)
	fmt.Fprintf(w, "config.friends=[%d,%d]\n", cfg.friendsMin, cfg.friendsMax)
	fmt.Fprintf(w, "config.likes=[0,%d]\n", cfg.likesMax)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)
	fmt.Fprintf(w, "config.rel_types=%t\n", cfg.relTypes)

	base := readMem()

	// Weightless: this social graph is queried only by relationship type and
	// edge properties via the Cypher engine — the FRIEND/LIKE edges carry the
	// constant weight 1 that nothing ever reads. Dropping the per-edge weight
	// column (W=float64) saves 8 B/edge of dead memory; the build is otherwise
	// identical (addEdge still passes weight 1, which is accepted and ignored).
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Weightless: true})
	stats, err := build(ctx, g, cfg, w)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// This is a build-then-query workload: the graph is fully assembled
	// above and only read from here on. Compact right-sizes the adjacency
	// backing arrays, reclaiming the ~21% slack that geometric (×2) append
	// growth leaves behind, so the resident-heap figures reported below
	// reflect the tight arrays the query phase actually runs against.
	if err := ctx.Err(); err != nil {
		return err
	}
	g.AdjList().Compact(ctx)

	fmt.Fprintf(w, "nodes.users=%d\n", stats.users)
	fmt.Fprintf(w, "nodes.articles=%d\n", stats.articles)
	fmt.Fprintf(w, "edges.friend=%d\n", stats.friendEdges)
	fmt.Fprintf(w, "edges.like=%d\n", stats.likeEdges)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", stats.elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "# build.node_rate=%.0f nodes/s\n", rate(stats.users+stats.articles, stats.elapsed))
	fmt.Fprintf(w, "# build.edge_rate=%.0f edges/s\n", rate(stats.friendEdges+stats.likeEdges, stats.elapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))
	fmt.Fprintf(w, "# mem.total_alloc=%s\n", humanBytes(built.TotalAlloc-base.TotalAlloc))
	fmt.Fprintf(w, "# mem.sys=%s\n", humanBytes(built.Sys))
	fmt.Fprintf(w, "# mem.num_gc=%d\n", built.NumGC-base.NumGC)
	fmt.Fprintf(w, "# bytes_per_edge=%.1f\n",
		safeDiv(float64(built.HeapAlloc-base.HeapAlloc), float64(stats.friendEdges+stats.likeEdges)))

	eng := cypher.NewEngine(g)
	if err := runQueries(ctx, eng, cfg, &stats, w); err != nil {
		return fmt.Errorf("queries: %w", err)
	}
	return nil
}

// buildStats reports the realised shape of a build (the random degrees
// mean the edge totals are not known until the graph is materialised)
// plus the wall-clock cost and a sample user to anchor traversal
// queries.
type buildStats struct {
	users       int
	articles    int
	friendEdges int
	likeEdges   int
	sampleUser  string   // id of an arbitrary, fixed user for FoF queries
	sampleIDs   []string // first unwindBatch user ids, for the UNWIND batch read
	elapsed     time.Duration
}

// build materialises the graph described by cfg into g. It first
// creates every node (so FRIEND/LIKE targets exist before the edges
// reference them), then the FRIEND and LIKE edges. Node and article ids
// are 24-char hex strings drawn from the seeded RNG; names and titles
// are realistic strings assembled from fixed word lists. The build
// honours ctx cancellation between phases and on a periodic check.
func build(ctx context.Context, g *lpg.Graph[string, float64], cfg config, _ io.Writer) (buildStats, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the benchmark
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	userIDs := make([]string, cfg.users)
	articleIDs := make([]string, cfg.articles)
	seen := make(map[string]struct{}, cfg.users+cfg.articles)

	// Users.
	for i := 0; i < cfg.users; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return buildStats{}, err
			}
		}
		id := uniqueHexID(rng, seen)
		userIDs[i] = id
		if err := addNode(g, id, labelUser, "name", realisticName(rng)); err != nil {
			return buildStats{}, err
		}
	}

	// Articles.
	for i := 0; i < cfg.articles; i++ {
		id := uniqueHexID(rng, seen)
		articleIDs[i] = id
		if err := addNode(g, id, labelArticle, "title", realisticTitle(rng)); err != nil {
			return buildStats{}, err
		}
	}

	// FRIEND edges: each user gets a random out-degree in
	// [friendsMin, friendsMax] to distinct other users.
	friendEdges := 0
	targets := make(map[int]struct{}, cfg.friendsMax)
	for i := 0; i < cfg.users; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return buildStats{}, err
			}
		}
		degree := cfg.friendsMin + rng.Intn(cfg.friendsMax-cfg.friendsMin+1)
		clear(targets)
		for len(targets) < degree {
			j := rng.Intn(cfg.users)
			if j == i {
				continue
			}
			targets[j] = struct{}{}
		}
		for j := range targets {
			if err := addEdge(g, userIDs[i], userIDs[j], relFriend, cfg.relTypes, propFriendSince, lpg.DateValue(edgeDate(rng))); err != nil {
				return buildStats{}, err
			}
			friendEdges++
		}
	}

	// LIKE edges: each user likes 0..likesMax distinct articles.
	likeEdges := 0
	likes := make(map[int]struct{}, cfg.likesMax)
	if cfg.articles > 0 && cfg.likesMax > 0 {
		for i := 0; i < cfg.users; i++ {
			if i%checkEvery == 0 {
				if err := ctx.Err(); err != nil {
					return buildStats{}, err
				}
			}
			degree := rng.Intn(cfg.likesMax + 1)
			clear(likes)
			for len(likes) < degree {
				likes[rng.Intn(cfg.articles)] = struct{}{}
			}
			for a := range likes {
				if err := addEdge(g, userIDs[i], articleIDs[a], relLike, cfg.relTypes, propLikeWhen, lpg.DateValue(edgeDate(rng))); err != nil {
					return buildStats{}, err
				}
				likeEdges++
			}
		}
	}

	return buildStats{
		users:       cfg.users,
		articles:    cfg.articles,
		friendEdges: friendEdges,
		likeEdges:   likeEdges,
		sampleUser:  userIDs[0],
		sampleIDs:   append([]string(nil), userIDs[:min(unwindBatch, cfg.users)]...),
		elapsed:     time.Since(start),
	}, nil
}

// unwindBatch is the fixed number of user ids the UNWIND $ids batch read
// (see analyticalQueries) looks up in one query. Kept small and constant so
// the deterministic default pins an exact matched-count fact.
const unwindBatch = 8

// checkEvery bounds how often the build polls ctx for cancellation:
// often enough that a cancelled multi-minute build stops promptly,
// rare enough that the check is free relative to the surrounding work.
const checkEvery = 4096

// addNode adds a single labelled node carrying its id plus one extra
// string property (name for users, title for articles).
func addNode(g *lpg.Graph[string, float64], id, label, propKey, propVal string) error {
	if err := g.AddNode(id); err != nil {
		return fmt.Errorf("AddNode %s: %w", id, err)
	}
	if err := g.SetNodeLabel(id, label); err != nil {
		return fmt.Errorf("SetNodeLabel %s/%s: %w", id, label, err)
	}
	if err := g.SetNodeProperty(id, "id", lpg.StringValue(id)); err != nil {
		return fmt.Errorf("SetNodeProperty id %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, propKey, lpg.StringValue(propVal)); err != nil {
		return fmt.Errorf("SetNodeProperty %s %s: %w", propKey, id, err)
	}
	return nil
}

// addEdge adds a directed, weight-1 edge and sets its mandatory date
// property (propKey=propVal). When withType is true it also tags the edge
// with the given relationship type so Cypher patterns like [:FRIEND] /
// [:LIKE] match; when false the type is left implicit (to be inferred from
// the endpoint labels) and no per-edge label is stored.
//
// The labelled case uses [lpg.Graph.AddEdgeLabeledWithProperty] so BOTH the type
// AND the date property land in the edge's inline slot AT insertion time — a
// single O(1)-amortised append — rather than AddEdge[Labeled] followed by a
// SetEdgeProperty that copies the whole per-source property column (which makes a
// bulk property-carrying build O(degree²) per source: the regression sprint 222
// #1646 fixes). The untyped case has no relationship label, so it uses AddEdge +
// SetEdgeProperty (no fused-with-label entry point applies); it is exercised only
// at small scale.
//
// The date property is a Cypher-visible Date built via [lpg.DateValue]: the
// caller pre-builds the PropertyValue so the value type is opaque here. It is
// the property tier the Cypher engine reads when materialising a matched
// relationship's properties, so the date is visible (as a native Date) at
// r.since / r.when in the query battery. Using a pair-level property is
// unambiguous here because every (src, dst) pair carries exactly one edge.
func addEdge(g *lpg.Graph[string, float64], src, dst, relType string, withType bool, propKey string, propVal lpg.PropertyValue) error {
	if withType {
		if err := g.AddEdgeLabeledWithProperty(src, dst, 1, relType, propKey, propVal); err != nil {
			return fmt.Errorf("AddEdgeLabeledWithProperty %s-[%s]->%s: %w", src, relType, dst, err)
		}
		return nil
	}
	if err := g.AddEdge(src, dst, 1); err != nil {
		return fmt.Errorf("AddEdge %s-[%s]->%s: %w", src, relType, dst, err)
	}
	if err := g.SetEdgeProperty(src, dst, propKey, propVal); err != nil {
		return fmt.Errorf("SetEdgeProperty %s on %s-[%s]->%s: %w", propKey, src, relType, dst, err)
	}
	return nil
}

// edgeDateWindowDays bounds how far before the fixed reference date a
// relationship may be dated: every FRIEND.since and LIKE.when falls within
// the window [edgeDateRef-edgeDateWindowDays, edgeDateRef]. ~6 years.
const edgeDateWindowDays = 2192

// edgeDateRef is the fixed reference date the synthetic edge dates count
// back from. Anchoring to a constant — never the wall clock — keeps the
// dataset reproducible for a given -seed. It is hoisted out of edgeDate
// so the per-edge build loop does not re-run time.Date's normalisation on
// every one of the hundreds of millions of edges. Immutable after init,
// like the word-list vars below.
var edgeDateRef = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

// edgeDate returns a deterministic calendar date drawn from rng as a whole-day
// offset back from edgeDateRef. The caller wraps it in [lpg.DateValue], which
// folds into the storage tier's compact int32 epoch-day column (~4 bytes/value)
// and reads back as a native Cypher Date — so range and ORDER BY predicates
// over since/when behave as dates while costing a fraction of the heap an
// ISO-8601 string column would (the per-edge string-header + backing text that
// dominated this example's heap before #1649).
func edgeDate(rng *rand.Rand) time.Time {
	return edgeDateRef.AddDate(0, 0, -rng.Intn(edgeDateWindowDays+1))
}

// uniqueHexID returns a 24-character lowercase hex id (12 random bytes)
// that has not been handed out before, recording it in seen. Drawing
// from the seeded rng keeps the whole dataset reproducible.
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
// fixed word lists. Names are intentionally allowed to repeat — the
// unique key is the hex id, not the name, which mirrors reality.
func realisticName(rng *rand.Rand) string {
	return firstNames[rng.Intn(len(firstNames))] + " " + lastNames[rng.Intn(len(lastNames))]
}

// realisticTitle assembles a plausible article headline of the form
// "<Adjective> <Noun>: <Phrase>" from fixed word lists.
func realisticTitle(rng *rand.Rand) string {
	return titleAdjectives[rng.Intn(len(titleAdjectives))] + " " +
		titleNouns[rng.Intn(len(titleNouns))] + ": " +
		titlePhrases[rng.Intn(len(titlePhrases))]
}

// ─────────────────────────────────────────────────────────────────────────────
// Query battery
// ─────────────────────────────────────────────────────────────────────────────

// runQueries executes the representative read-query suite against eng,
// printing one deterministic result line and one volatile latency line
// ("# ...") per query.
func runQueries(ctx context.Context, eng *cypher.Engine, cfg config, stats *buildStats, w io.Writer) error {
	// Scalar count aggregations over label scans and relationship
	// patterns — the bread-and-butter of analytics over a social graph.
	// The relationship patterns differ by mode: with explicit types they
	// match [:FRIEND] / [:LIKE]; without, the type is inferred from the
	// endpoint labels (FRIEND is the only USER->USER edge, LIKE the only
	// USER->ARTICLE edge), so the same shape is expressed untyped.
	friendPat, likePat := "-[:FRIEND]->", "-[:LIKE]->"
	if !cfg.relTypes {
		friendPat, likePat = "-->", "-->"
	}

	// Bound-relationship variants of the same two patterns, used to read
	// each relationship's mandatory date property. The variable r binds the
	// relationship in both modes; only the optional type token differs.
	friendRelPat, likeRelPat := "-[r:FRIEND]->", "-[r:LIKE]->"
	if !cfg.relTypes {
		friendRelPat, likeRelPat = "-[r]->", "-[r]->"
	}

	scalars := []struct {
		name  string
		query string
	}{
		{"count_users", "MATCH (u:USER) RETURN count(u) AS c"},
		{"count_articles", "MATCH (a:ARTICLE) RETURN count(a) AS c"},
		{"count_friend", "MATCH (:USER)" + friendPat + "(:USER) RETURN count(*) AS c"},
		{"count_like", "MATCH (:USER)" + likePat + "(:ARTICLE) RETURN count(*) AS c"},
	}
	for _, q := range scalars {
		n, d, err := scalarCount(ctx, eng, q.query, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", q.name, err)
		}
		fmt.Fprintf(w, "q.%s=%d\n", q.name, n)
		fmt.Fprintf(w, "# q.%s.latency=%s\n", q.name, d.Round(time.Microsecond))
	}

	// Mandatory date-property coverage. Every FRIEND carries a since date
	// and every LIKE a when date, so the count of relationships whose date
	// IS NOT NULL must equal the total relationship count. This exercises
	// the relationship-property read path and verifies the always-filled
	// invariant at the query level: the regression test asserts these equal
	// edges.friend and edges.like respectively.
	coverage := []struct {
		name  string
		query string
	}{
		{"friend_since_filled", "MATCH (:USER)" + friendRelPat + "(:USER) WHERE r.since IS NOT NULL RETURN count(*) AS c"},
		{"like_when_filled", "MATCH (:USER)" + likeRelPat + "(:ARTICLE) WHERE r.when IS NOT NULL RETURN count(*) AS c"},
	}
	for _, q := range coverage {
		n, d, err := scalarCount(ctx, eng, q.query, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", q.name, err)
		}
		fmt.Fprintf(w, "q.%s=%d\n", q.name, n)
		fmt.Fprintf(w, "# q.%s.latency=%s\n", q.name, d.Round(time.Microsecond))
	}

	// Friend-of-friend reach from a fixed sample user: a two-hop
	// traversal with DISTINCT, anchored by a property lookup. Without an
	// index the anchor is a label scan, so this also measures the cost
	// of point access at scale.
	{
		query := "MATCH (u:USER {id:$id})" + friendPat + "(:USER)" + friendPat + "(fof:USER) " +
			"RETURN count(DISTINCT fof) AS c"
		params := map[string]expr.Value{"id": expr.StringValue(stats.sampleUser)}
		n, d, err := scalarCount(ctx, eng, query, params)
		if err != nil {
			return fmt.Errorf("fof: %w", err)
		}
		fmt.Fprintf(w, "q.fof_reach=%d\n", n)
		fmt.Fprintf(w, "# q.fof_reach.latency=%s\n", d.Round(time.Microsecond))
	}

	// Top-liked articles: a grouped aggregation with ORDER BY ... DESC
	// and LIMIT, the canonical "trending" query. We assert on the row
	// count (deterministic) and surface the latency; the specific ids
	// depend on the RNG draw and are intentionally not pinned.
	{
		limit := 10
		if cfg.articles < limit {
			limit = cfg.articles
		}
		query := fmt.Sprintf("MATCH (:USER)"+likePat+"(a:ARTICLE) "+
			"RETURN a.id AS id, count(*) AS likes ORDER BY likes DESC, id ASC LIMIT %d", limit)
		rows, d, err := topArticles(ctx, eng, query)
		if err != nil {
			return fmt.Errorf("top_articles: %w", err)
		}
		fmt.Fprintf(w, "q.top_articles.rows=%d\n", rows)
		fmt.Fprintf(w, "# q.top_articles.latency=%s\n", d.Round(time.Microsecond))
	}

	if err := analyticalQueries(ctx, eng, friendPat, likePat, cfg, stats, w); err != nil {
		return err
	}
	return temporalQueries(ctx, eng, friendRelPat, w)
}

// ─────────────────────────────────────────────────────────────────────────────
// Analytical aggregation & subquery battery (rmp #1971)
// ─────────────────────────────────────────────────────────────────────────────

// analyticalQueries exercises the analytical breadth of the Cypher engine over
// the seeded graph: distribution aggregates beyond count/sum (avg / min / max /
// percentileCont median), an EXISTS { } (and NOT EXISTS { }) subquery filter, a
// CASE bucketing projection, a UNION ALL of two result streams, an
// UNWIND $ids batch point-read, and id() / elementId() on a matched node. Every
// deterministic result is emitted as a bare fact line; each query's wall-clock
// cost is a "# " telemetry line.
//
// friendPat / likePat carry the mode-appropriate relationship shape (explicit
// [:FRIEND] / [:LIKE] or endpoint-inferred -->), so the answers are identical in
// both -rel-types modes.
func analyticalQueries(ctx context.Context, eng *cypher.Engine, friendPat, likePat string, cfg config, stats *buildStats, w io.Writer) error {
	// Friend out-degree distribution: compute each user's FRIEND out-degree,
	// then aggregate the distribution with avg / min / max and the median via
	// percentileCont(deg, 0.5). Every user has >= friendsMin friends, so all
	// users contribute. This is the canonical "beyond count/sum" analytics query.
	{
		query := "MATCH (u:USER)" + friendPat + "(:USER) WITH u, count(*) AS deg " +
			"RETURN min(deg) AS mn, max(deg) AS mx, avg(deg) AS av, percentileCont(deg, 0.5) AS med"
		dist, err := degreeStats(ctx, eng, query)
		if err != nil {
			return fmt.Errorf("friend_degree: %w", err)
		}
		fmt.Fprintf(w, "q.friend_degree.min=%d\n", dist.minDeg)
		fmt.Fprintf(w, "q.friend_degree.max=%d\n", dist.maxDeg)
		fmt.Fprintf(w, "q.friend_degree.avg=%.4f\n", dist.avg)
		fmt.Fprintf(w, "q.friend_degree.median=%.4f\n", dist.median)
		fmt.Fprintf(w, "# q.friend_degree.latency=%s\n", dist.latency.Round(time.Microsecond))
	}

	// EXISTS { } subquery: count users who have at least one LIKE, and its
	// complement via NOT EXISTS { }. The two must sum to the user count — the
	// conservation invariant the regression test asserts.
	{
		with := "MATCH (u:USER) WHERE EXISTS { (u)" + likePat + "(:ARTICLE) } RETURN count(u) AS c"
		without := "MATCH (u:USER) WHERE NOT EXISTS { (u)" + likePat + "(:ARTICLE) } RETURN count(u) AS c"
		n, d, err := scalarCount(ctx, eng, with, nil)
		if err != nil {
			return fmt.Errorf("users_with_like: %w", err)
		}
		fmt.Fprintf(w, "q.users_with_like=%d\n", n)
		fmt.Fprintf(w, "# q.users_with_like.latency=%s\n", d.Round(time.Microsecond))
		n, d, err = scalarCount(ctx, eng, without, nil)
		if err != nil {
			return fmt.Errorf("users_without_like: %w", err)
		}
		fmt.Fprintf(w, "q.users_without_like=%d\n", n)
		fmt.Fprintf(w, "# q.users_without_like.latency=%s\n", d.Round(time.Microsecond))
	}

	// CASE projection: bucket each user's FRIEND out-degree into three named
	// bands and count per band. The band boundaries are the equal-width tertiles
	// of the configured degree range [friendsMin, friendsMax], so the split is
	// meaningful at any scale (not just the small default). The bands partition
	// the users, so the counts sum to the user count.
	{
		span := cfg.friendsMax - cfg.friendsMin
		t1 := cfg.friendsMin + span/3
		t2 := cfg.friendsMin + 2*span/3
		query := fmt.Sprintf("MATCH (u:USER)%s(:USER) WITH u, count(*) AS deg "+
			"WITH CASE WHEN deg <= %d THEN 'low' WHEN deg <= %d THEN 'mid' ELSE 'high' END AS band "+
			"RETURN band, count(*) AS c ORDER BY band", friendPat, t1, t2)
		rows, d, err := groupCounts(ctx, eng, query, "band", "c", nil)
		if err != nil {
			return fmt.Errorf("degree_band: %w", err)
		}
		for _, r := range rows {
			fmt.Fprintf(w, "q.degree_band.%s=%d\n", r.key, r.count)
		}
		fmt.Fprintf(w, "# q.degree_band.latency=%s\n", d.Round(time.Microsecond))
	}

	// UNION ALL: combine two label-count streams into one result set. The two
	// rows carry the user and article totals.
	{
		query := "MATCH (u:USER) RETURN 'users' AS kind, count(*) AS c " +
			"UNION ALL MATCH (a:ARTICLE) RETURN 'articles' AS kind, count(*) AS c"
		rows, d, err := groupCounts(ctx, eng, query, "kind", "c", nil)
		if err != nil {
			return fmt.Errorf("union: %w", err)
		}
		fmt.Fprintf(w, "q.union.rows=%d\n", len(rows))
		for _, r := range rows {
			fmt.Fprintf(w, "q.union.%s=%d\n", r.key, r.count)
		}
		fmt.Fprintf(w, "# q.union.latency=%s\n", d.Round(time.Microsecond))
	}

	// UNWIND $ids batch read: look up a list of known user ids in one query.
	// Every id is real, so the matched count equals the requested count.
	{
		ids := make(expr.ListValue, 0, len(stats.sampleIDs))
		for _, id := range stats.sampleIDs {
			ids = append(ids, expr.StringValue(id))
		}
		query := "UNWIND $ids AS wanted MATCH (u:USER {id: wanted}) RETURN count(u) AS c"
		n, d, err := scalarCount(ctx, eng, query, map[string]expr.Value{"ids": ids})
		if err != nil {
			return fmt.Errorf("unwind_batch: %w", err)
		}
		fmt.Fprintf(w, "q.unwind_requested=%d\n", len(stats.sampleIDs))
		fmt.Fprintf(w, "q.unwind_matched=%d\n", n)
		fmt.Fprintf(w, "# q.unwind_batch.latency=%s\n", d.Round(time.Microsecond))
	}

	// id() / elementId() on a matched node. id() is an Integer (the interned,
	// reopen-stable NodeID); elementId() is a String. idPair verifies those Go
	// kinds at runtime and returns the values, which are deterministic for the
	// fixed seed (the sample user is the first node inserted).
	{
		query := "MATCH (u:USER {id:$id}) RETURN id(u) AS iid, elementId(u) AS eid"
		params := map[string]expr.Value{"id": expr.StringValue(stats.sampleUser)}
		id, elemID, d, err := idPair(ctx, eng, query, params)
		if err != nil {
			return fmt.Errorf("id_pair: %w", err)
		}
		fmt.Fprintf(w, "q.sample_node_id=%d\n", id)
		fmt.Fprintf(w, "q.sample_element_id=%s\n", elemID)
		fmt.Fprintf(w, "# q.id_pair.latency=%s\n", d.Round(time.Microsecond))
	}

	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Temporal-function battery (rmp #1972)
// ─────────────────────────────────────────────────────────────────────────────

// temporalQueries exercises Cypher temporal constructors and arithmetic — not
// just reading stored Date values, but calling date() / datetime(), duration()
// / duration.between-family projections, Date − Duration arithmetic, duration
// component access, and date.truncate. Every predicate is anchored to a FIXED
// reference date (never the wall clock) so the facts are deterministic.
//
// friendRelPat carries the mode-appropriate bound-relationship shape
// (-[r:FRIEND]-> or -[r]->) so r.since is read in both -rel-types modes.
func temporalQueries(ctx context.Context, eng *cypher.Engine, friendRelPat string, w io.Writer) error {
	// Constructor sanity: duration.inDays(date(a), date(b)).days between two
	// literal dates six years apart is the known window span (2192 days), which
	// equals edgeDateWindowDays. Exercises date(String), the 2-arg duration.inDays
	// projection, and the .days duration accessor with a checkable answer.
	{
		query := "RETURN duration.inDays(date($lo), date($hi)).days AS c"
		params := map[string]expr.Value{
			"lo": expr.StringValue("2019-01-01"),
			"hi": expr.StringValue("2025-01-01"),
		}
		n, d, err := scalarCount(ctx, eng, query, params)
		if err != nil {
			return fmt.Errorf("window_days: %w", err)
		}
		fmt.Fprintf(w, "q.temporal.window_days=%d\n", n)
		fmt.Fprintf(w, "# q.temporal.window_days.latency=%s\n", d.Round(time.Microsecond))
	}

	// datetime() constructor + duration.inSeconds projection between two zoned
	// instants 90 minutes apart → 5400 seconds. Exercises datetime(String) and
	// the .seconds accessor with a checkable answer.
	{
		query := "RETURN duration.inSeconds(datetime($a), datetime($b)).seconds AS c"
		params := map[string]expr.Value{
			"a": expr.StringValue("2025-01-01T00:00:00Z"),
			"b": expr.StringValue("2025-01-01T01:30:00Z"),
		}
		n, d, err := scalarCount(ctx, eng, query, params)
		if err != nil {
			return fmt.Errorf("dt_span_seconds: %w", err)
		}
		fmt.Fprintf(w, "q.temporal.dt_span_seconds=%d\n", n)
		fmt.Fprintf(w, "# q.temporal.dt_span_seconds.latency=%s\n", d.Round(time.Microsecond))
	}

	// Friendship age in whole days back from the fixed reference date, over
	// every FRIEND edge: duration.inDays(r.since, date(ref)).days. since is drawn
	// from [ref-window, ref], so the ages span [0, window]. Reports min and max.
	{
		query := "MATCH (:USER)" + friendRelPat + "(:USER) " +
			"WITH duration.inDays(r.since, date($ref)).days AS ageDays " +
			"RETURN min(ageDays) AS mn, max(ageDays) AS mx"
		params := map[string]expr.Value{"ref": expr.StringValue("2025-01-01")}
		mn, mx, d, err := twoInts(ctx, eng, query, params, "mn", "mx")
		if err != nil {
			return fmt.Errorf("friend_age_days: %w", err)
		}
		fmt.Fprintf(w, "q.friend_age_days.min=%d\n", mn)
		fmt.Fprintf(w, "q.friend_age_days.max=%d\n", mx)
		fmt.Fprintf(w, "# q.friend_age_days.latency=%s\n", d.Round(time.Microsecond))
	}

	// "Created within the last 30 days" relative to the fixed reference date,
	// using Date − Duration arithmetic: r.since >= date(ref) - duration('P30D').
	// Exercises duration(String) (ISO-8601), Date − Duration → Date, and a
	// Date >= Date comparison.
	{
		query := "MATCH (:USER)" + friendRelPat + "(:USER) " +
			"WHERE r.since >= date($ref) - duration('P30D') RETURN count(*) AS c"
		params := map[string]expr.Value{"ref": expr.StringValue("2025-01-01")}
		n, d, err := scalarCount(ctx, eng, query, params)
		if err != nil {
			return fmt.Errorf("friend_recent_30d: %w", err)
		}
		fmt.Fprintf(w, "q.friend_recent_30d=%d\n", n)
		fmt.Fprintf(w, "# q.friend_recent_30d.latency=%s\n", d.Round(time.Microsecond))
	}

	// Friendships bucketed by calendar year, using date.truncate('year', …) and
	// the .year accessor. The per-year counts sum to the FRIEND edge total — the
	// conservation invariant the regression test asserts.
	{
		query := "MATCH (:USER)" + friendRelPat + "(:USER) " +
			"WITH date.truncate('year', r.since) AS yr RETURN yr.year AS y, count(*) AS c ORDER BY y"
		rows, d, err := groupCounts(ctx, eng, query, "y", "c", nil)
		if err != nil {
			return fmt.Errorf("friend_by_year: %w", err)
		}
		for _, r := range rows {
			fmt.Fprintf(w, "q.friend_by_year.%s=%d\n", r.key, r.count)
		}
		fmt.Fprintf(w, "# q.friend_by_year.latency=%s\n", d.Round(time.Microsecond))
	}

	return nil
}

// scalarCount runs a query whose single row has a single integer column
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

// topArticles runs the trending-articles aggregation and returns the
// number of rows it produced plus the elapsed time. The rows are fully
// drained so the timing covers the whole query.
func topArticles(ctx context.Context, eng *cypher.Engine, query string) (int, time.Duration, error) {
	start := time.Now()
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = res.Close() }()

	rows := 0
	for res.Next() {
		rec := res.Record()
		if _, ok := rec["id"]; !ok {
			return 0, 0, fmt.Errorf("column %q missing", "id")
		}
		if _, ok := rec["likes"]; !ok {
			return 0, 0, fmt.Errorf("column %q missing", "likes")
		}
		rows++
	}
	if err := res.Err(); err != nil {
		return 0, 0, err
	}
	return rows, time.Since(start), nil
}

// labeledCount is one row of a two-column (key, count) grouped result.
type labeledCount struct {
	key   string
	count int64
}

// groupCounts runs a query whose rows carry a key column (a String or Integer)
// and an integer count column, returning the rows in the query's own order plus
// the wall-clock time. An Integer key is rendered in decimal so it can form a
// fact-line suffix (e.g. a truncated year). Used for the CASE-band, UNION, and
// by-year queries.
func groupCounts(ctx context.Context, eng *cypher.Engine, query, keyCol, countCol string, params map[string]expr.Value) ([]labeledCount, time.Duration, error) {
	start := time.Now()
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = res.Close() }()

	var out []labeledCount
	for res.Next() {
		rec := res.Record()
		key, err := cellString(rec, keyCol)
		if err != nil {
			return nil, 0, err
		}
		cnt, err := cellInt(rec, countCol)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, labeledCount{key: key, count: cnt})
	}
	if err := res.Err(); err != nil {
		return nil, 0, err
	}
	return out, time.Since(start), nil
}

// degreeDist is the decoded single row of the friend out-degree distribution
// query: the min and max degrees, the mean, the median, and the query latency.
type degreeDist struct {
	minDeg, maxDeg int64
	avg, median    float64
	latency        time.Duration
}

// degreeStats runs the friend out-degree distribution query and decodes its
// single row into a degreeDist.
func degreeStats(ctx context.Context, eng *cypher.Engine, query string) (degreeDist, error) {
	start := time.Now()
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return degreeDist{}, err
	}
	defer func() { _ = res.Close() }()

	var dist degreeDist
	var got bool
	for res.Next() {
		rec := res.Record()
		if dist.minDeg, err = cellInt(rec, "mn"); err != nil {
			return degreeDist{}, err
		}
		if dist.maxDeg, err = cellInt(rec, "mx"); err != nil {
			return degreeDist{}, err
		}
		if dist.avg, err = cellFloat(rec, "av"); err != nil {
			return degreeDist{}, err
		}
		if dist.median, err = cellFloat(rec, "med"); err != nil {
			return degreeDist{}, err
		}
		got = true
	}
	if err := res.Err(); err != nil {
		return degreeDist{}, err
	}
	if !got {
		return degreeDist{}, fmt.Errorf("degree query returned no rows")
	}
	dist.latency = time.Since(start)
	return dist, nil
}

// twoInts runs a query whose single row has two integer columns colA/colB and
// returns them plus the elapsed time.
func twoInts(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value, colA, colB string) (int64, int64, time.Duration, error) {
	start := time.Now()
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = res.Close() }()

	var a, b int64
	var got bool
	for res.Next() {
		rec := res.Record()
		if a, err = cellInt(rec, colA); err != nil {
			return 0, 0, 0, err
		}
		if b, err = cellInt(rec, colB); err != nil {
			return 0, 0, 0, err
		}
		got = true
	}
	if err := res.Err(); err != nil {
		return 0, 0, 0, err
	}
	if !got {
		return 0, 0, 0, fmt.Errorf("query returned no rows")
	}
	return a, b, time.Since(start), nil
}

// idPair runs the id()/elementId() query and returns the node's integer id and
// string element id plus the elapsed time. It verifies the Cypher contract at
// runtime — id() yields an Integer and elementId() a String — returning a typed
// error if either column has the wrong kind.
func idPair(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value) (int64, string, time.Duration, error) {
	start := time.Now()
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return 0, "", 0, err
	}
	defer func() { _ = res.Close() }()

	var id int64
	var elemID string
	var got bool
	for res.Next() {
		rec := res.Record()
		if id, err = cellInt(rec, "iid"); err != nil {
			return 0, "", 0, fmt.Errorf("id() must be an Integer: %w", err)
		}
		if elemID, err = cellString(rec, "eid"); err != nil {
			return 0, "", 0, fmt.Errorf("elementId() must be a String: %w", err)
		}
		got = true
	}
	if err := res.Err(); err != nil {
		return 0, "", 0, err
	}
	if !got {
		return 0, "", 0, fmt.Errorf("id query returned no rows")
	}
	return id, elemID, time.Since(start), nil
}

// cellInt extracts column col from rec as an int64, erroring if it is absent or
// not an [expr.IntegerValue].
func cellInt(rec map[string]any, col string) (int64, error) {
	v, ok := rec[col]
	if !ok {
		return 0, fmt.Errorf("column %q missing", col)
	}
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		return 0, fmt.Errorf("column %q is %T, want expr.IntegerValue", col, v)
	}
	return int64(iv), nil
}

// cellFloat extracts column col from rec as a float64, accepting an
// [expr.FloatValue] or an [expr.IntegerValue] (widened).
func cellFloat(rec map[string]any, col string) (float64, error) {
	v, ok := rec[col]
	if !ok {
		return 0, fmt.Errorf("column %q missing", col)
	}
	switch n := v.(type) {
	case expr.FloatValue:
		return float64(n), nil
	case expr.IntegerValue:
		return float64(int64(n)), nil
	default:
		return 0, fmt.Errorf("column %q is %T, want a number", col, v)
	}
}

// cellString extracts column col from rec as its fact-line string form: an
// [expr.StringValue] verbatim, an [expr.IntegerValue] in decimal.
func cellString(rec map[string]any, col string) (string, error) {
	v, ok := rec[col]
	if !ok {
		return "", fmt.Errorf("column %q missing", col)
	}
	switch s := v.(type) {
	case expr.StringValue:
		return string(s), nil
	case expr.IntegerValue:
		return strconv.FormatInt(int64(s), 10), nil
	default:
		return "", fmt.Errorf("column %q is %T, want String or Integer", col, v)
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
func rate(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
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

var titleAdjectives = []string{
	"Hidden", "Surprising", "Essential", "Modern", "Practical", "Complete",
	"Curious", "Quiet", "Bold", "Timeless", "Unexpected", "Everyday",
	"Radical", "Gentle", "Honest", "Lasting", "Simple", "Bright",
}

var titleNouns = []string{
	"History", "Science", "Art", "Future", "Story", "Power", "Mystery",
	"Logic", "Design", "Truth", "Rise", "Craft", "Habit", "Journey",
	"Theory", "Practice", "Life", "Method",
}

var titlePhrases = []string{
	"What Nobody Tells You", "A Beginner's Guide", "Lessons From the Field",
	"Rethinking the Basics", "Notes From a Decade", "The Long View",
	"Ten Ideas That Stuck", "Why It Matters Now", "From Theory to Practice",
	"A Field Report", "The Quiet Revolution", "How It Really Works",
	"Beyond the Obvious", "An Honest Account", "The Road Ahead",
}
