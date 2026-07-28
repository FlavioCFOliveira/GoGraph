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
// After the battery it exercises the planner statistics (tasks #2097–#2102):
// it calls Engine.RefreshStatistics once and prints the EXPLAIN plan of a label
// scan, a property equality, and a property range, each operator annotated with
// its estimated row count and provenance (exact / heuristic / stats-with-error),
// then reports the stats-health telemetry — the tracked-(label, property)-pair
// count, the refresh latency, and the estimate-vs-actual accuracy of both an
// exact label scan and the approximate range. These estimates are display-only:
// they annotate EXPLAIN but never change which plan runs.
//
// Next main runs the columnar-execution exercise (task #2121): three analytic
// queries — a GROUP BY aggregation, a filter-over-traversal, and a disconnected
// equi-join — that each engage one of the engine's columnar (chunk-at-a-time)
// physical operators, measured against a result-identical row-mode baseline so the
// de-boxing allocation win each delivers is observable. It runs on its own bounded
// working set (never the full -users scale), so it is driven from main after run
// rather than inside run: it must stay cheap and identical however large -users is.
// See [columnarExercise].
//
// Finally main runs the intra-query parallelism exercise (task #2122): over a
// bounded user population large enough to cross the parallel-scan threshold, it runs
// a whole-graph min/max aggregate on a parallel-configured engine and on a serial one
// and reports the speedup (verifying the two results are bit-identical), then contrasts
// an O(1) count(*) pushdown against the O(N) scan it replaces. Like the columnar
// exercise it runs on its own fixed-scale working set and is driven from main. The
// parallel win is idle-core-bound and reported as single-tenant telemetry, never
// claimed to hold under concurrent load. See [parallelismExercise].
//
// # Model
//
//	(:USER    {id, name, country})              // id is a 24-char hex string
//	(:ARTICLE {id, title})                      // id is a 24-char hex string
//	(:USER)-[:FRIEND {since}]->(:USER)          // friendsMin..friendsMax per user
//	(:USER)-[:LIKE   {when}]->(:ARTICLE)        // 0..likesMax per user
//
// FRIEND is modelled as a directed out-edge: every user is given a
// random out-degree in [friendsMin, friendsMax] to distinct other
// users (no self-loops, no duplicate targets). LIKE is a directed
// out-edge to between zero and likesMax distinct articles. Each user also
// carries a low-cardinality categorical country, derived deterministically
// from the user index (never from the RNG), as the group-by key of the
// columnar-aggregation exercise.
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
	"strings"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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

	// propUserCountry is a low-cardinality categorical dimension every user
	// carries: the group-by key of the columnar-aggregation exercise (see
	// columnarExercise). It is derived deterministically from the user index,
	// never from the RNG, so it leaves every seed-pinned fact untouched.
	propUserCountry = "country"
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

	ctx := context.Background()
	if err := run(ctx, os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
	// The columnar exercise runs on its own bounded working set (never the full
	// -users scale — see columnarExercise), so it is driven separately from the
	// main battery rather than from run: it must stay cheap and identical however
	// large -users is, and the regression test drives it directly.
	if err := columnarExercise(ctx, cfg, os.Stdout); err != nil {
		log.Fatal(err)
	}
	// The parallelism exercise likewise runs on its own fixed-scale working set
	// (never the -users scale — see parallelismExercise), large enough to cross the
	// parallel-scan threshold, so it too is driven from main rather than from run.
	if err := parallelismExercise(ctx, cfg, os.Stdout); err != nil {
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
	if err := statisticsExercise(ctx, eng, cfg, w); err != nil {
		return fmt.Errorf("statistics: %w", err)
	}
	return nil
}

// buildStats reports the realised shape of a build (the random degrees
// mean the edge totals are not known until the graph is materialised)
// plus the wall-clock cost and a sample user to anchor traversal
// queries.
type buildStats struct {
	sampleUser  string   // id of an arbitrary, fixed user for FoF queries
	sampleIDs   []string // first unwindBatch user ids, for the UNWIND batch read
	users       int
	articles    int
	friendEdges int
	likeEdges   int
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

	// Users. Each user also carries a low-cardinality categorical country,
	// derived deterministically from the user index (never from rng), so it adds
	// a realistic group-by key for the columnar-aggregation exercise WITHOUT
	// perturbing the seeded RNG stream — every other seed-pinned fact (friend
	// degrees, likes, dates) is unchanged. See columnarExercise.
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
		if err := g.SetNodeProperty(id, propUserCountry, lpg.StringValue(countries[i%len(countries)])); err != nil {
			return buildStats{}, fmt.Errorf("SetNodeProperty country %s: %w", id, err)
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

// ─────────────────────────────────────────────────────────────────────────────
// Planner statistics & cardinality estimates (rmp #2120)
// ─────────────────────────────────────────────────────────────────────────────

// statisticsExercise builds the planner statistics over the assembled graph and
// then exercises the display-only cardinality estimates the engine surfaces in
// EXPLAIN (tasks #2097–#2102). It is the observability half of the statistics
// feature: it calls [cypher.Engine.RefreshStatistics] once — the single,
// caller-driven rebuild — then prints, for three representative predicate shapes,
// the EXPLAIN plan annotated with each operator's estimated row count and its
// provenance:
//
//   - a label scan          → the EXACT live count (estExact, from the label index);
//   - an equality predicate → the 1/NDV heuristic over a high-NDV column
//     (estHeuristic), or an MCV-exact count when the literal is a tracked heavy hitter;
//   - a range predicate     → the equi-depth histogram estimate (estStats) with its
//     certified absolute selectivity error δ = 1/B + Δ/N.
//
// It closes with the stats-health telemetry the feature is meant to expose: the
// tracked-(label, property)-pair count, the refresh latency, and — for both the
// exact label scan and the approximate range — the estimate-vs-actual accuracy,
// comparing each annotated estimate to the query's real result count.
//
// Every line is volatile telemetry (prefixed "# "): the EXPLAIN rendering and the
// estimate numbers are diagnostic observations of the planner's internal model,
// not deterministic data-shape facts, so the regression test reads them by name
// rather than pinning them into the fact-line set. The queries are over node
// labels and node properties only, so the plans and estimates are identical in
// both relationship-encoding modes.
func statisticsExercise(ctx context.Context, eng *cypher.Engine, _ config, w io.Writer) error {
	// Build the statistics off the (already-assembled, read-only) graph. This is
	// the sole explicit rebuild: one consistent scan over every (label, property).
	start := time.Now()
	if err := eng.RefreshStatistics(ctx); err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	refreshLatency := time.Since(start)

	fmt.Fprintln(w, "# --- planner statistics & cardinality estimates (#2120) ---")
	fmt.Fprintf(w, "# stats.tracked_pairs=%d\n", eng.StatsTrackedPairs())
	fmt.Fprintf(w, "# stats.refresh.latency=%s\n", refreshLatency.Round(time.Microsecond))

	// Representative queries, all with inline literals so their annotated plans
	// match the proven estimate path. A name that the word lists never produce
	// makes the equality estimate the honest 1/NDV distribution-average heuristic;
	// the mid-alphabet range bound "M" matches every user whose name sorts before it.
	const (
		labelScanQ  = "MATCH (u:USER) RETURN u"
		labelCountQ = "MATCH (u:USER) RETURN count(u) AS c"
		equalityQ   = "MATCH (u:USER) WHERE u.name = '~~~ no such user' RETURN u"
		rangeQ      = "MATCH (u:USER) WHERE u.name < 'M' RETURN u"
		rangeCountQ = "MATCH (u:USER) WHERE u.name < 'M' RETURN count(*) AS c"
	)

	// Print each annotated EXPLAIN plan (every line prefixed "# ").
	if err := explainAnnotated(w, eng, "label_scan", labelScanQ); err != nil {
		return err
	}
	if err := explainAnnotated(w, eng, "equality_1_over_ndv", equalityQ); err != nil {
		return err
	}
	if err := explainAnnotated(w, eng, "range_histogram", rangeQ); err != nil {
		return err
	}

	// Estimate-vs-actual accuracy.
	// (a) The label scan is estExact: its estimate must equal the real count.
	labelEst, err := estRowsFor(eng, labelScanQ, "NodeByLabelScan")
	if err != nil {
		return err
	}
	labelActual, _, err := scalarCount(ctx, eng, labelCountQ, nil)
	if err != nil {
		return fmt.Errorf("label actual: %w", err)
	}
	fmt.Fprintf(w, "# stats.label.est_rows=%d\n", labelEst)
	fmt.Fprintf(w, "# stats.label.actual_rows=%d\n", labelActual)

	// (b) The range is estStats: an approximate histogram estimate whose absolute
	// row error the equi-depth guarantee bounds. Report both and their gap.
	rangeEst, err := estRowsFor(eng, rangeQ, "Selection")
	if err != nil {
		return err
	}
	rangeActual, _, err := scalarCount(ctx, eng, rangeCountQ, nil)
	if err != nil {
		return fmt.Errorf("range actual: %w", err)
	}
	fmt.Fprintf(w, "# stats.range.est_rows=%d\n", rangeEst)
	fmt.Fprintf(w, "# stats.range.actual_rows=%d\n", rangeActual)
	fmt.Fprintf(w, "# stats.range.abs_row_error=%d\n", absInt64(rangeEst-rangeActual))
	return nil
}

// explainAnnotated prints the LOGICAL plan for query, one "# "-prefixed line per
// plan line, under a named header. Every emitted line is telemetry, so it never
// enters the deterministic fact-line set the tests pin.
//
// It uses ExplainLogical rather than Explain because this exercise is about the
// planner's CARDINALITY ESTIMATES: those annotations belong to logical nodes and
// have no counterpart on a built operator, so the physical rendering does not
// carry them. Engine.Explain is the surface for what actually executes.
func explainAnnotated(w io.Writer, eng *cypher.Engine, name, query string) error {
	plan, err := eng.ExplainLogical(query, nil)
	if err != nil {
		return fmt.Errorf("explain %s: %w", name, err)
	}
	fmt.Fprintf(w, "# stats.explain.%s:\n", name)
	for _, line := range strings.Split(strings.TrimRight(plan, "\n"), "\n") {
		fmt.Fprintf(w, "#   %s\n", line)
	}
	return nil
}

// estRowsFor returns the estimated row count annotated on the first plan operator
// line containing op (e.g. "NodeByLabelScan" or "Selection"). It errors when the
// operator line is absent or carries no estimate annotation (an estFallback line
// is rendered without one).
//
// The estimates live on the LOGICAL plan, so this reads ExplainLogical; see
// explainAnnotated.
func estRowsFor(eng *cypher.Engine, query, op string) (int64, error) {
	plan, err := eng.ExplainLogical(query, nil)
	if err != nil {
		return 0, fmt.Errorf("explain %s: %w", op, err)
	}
	line := planLineContaining(plan, op)
	if line == "" {
		return 0, fmt.Errorf("EXPLAIN has no %q operator line:\n%s", op, plan)
	}
	n, ok := parseEstRows(line)
	if !ok {
		return 0, fmt.Errorf("%q line carries no estimate annotation: %q", op, line)
	}
	return n, nil
}

// planLineContaining returns the first line of plan that contains substr, or "".
func planLineContaining(plan, substr string) string {
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

// parseEstRows extracts the integer row estimate from an EXPLAIN annotation of the
// form "(est. rows=N, …)" (exact) or "(est. rows~N, …)" (approximate). ok is false
// when the line carries no such annotation (for example an estFallback operator,
// which is deliberately rendered without one).
func parseEstRows(line string) (int64, bool) {
	const marker = "est. rows"
	i := strings.Index(line, marker)
	if i < 0 {
		return 0, false
	}
	rest := line[i+len(marker):]
	if rest == "" {
		return 0, false
	}
	rest = rest[1:] // skip the '=' (exact) or '~' (approximate) separator
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(rest[:j], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// absInt64 returns the absolute value of n.
func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// Columnar execution exercise (rmp #2121)
// ─────────────────────────────────────────────────────────────────────────────

// Columnar working-set bounds. The columnar exercise deliberately runs each
// columnar query alongside a byte-identical row-mode twin whose allocation cost
// grows with the rows it touches. Those twins are heavy on purpose (they are the
// baseline the columnar path beats), so the exercise caps its working set to a
// bounded sub-graph — never the full -users scale — keeping the A/B fast and its
// allocation figures reproducible at any -users. The columnar operators are the
// SAME ones the main battery drives at full scale; only their allocation
// behaviour is measured here, where a controlled row-mode baseline is affordable.
const (
	colScaleUsers    = 1500
	colScaleArticles = 200
)

// colProfile is one measured query execution: the allocation count and bytes it
// charged, the number of result rows it produced, and how many columnar-filter
// batches it drove (0 unless the columnar Expand+Filter chain engaged).
type colProfile struct {
	mallocs uint64
	bytes   uint64
	rows    int
	batches uint64
}

// colBackend is the metrics sink the columnar exercise installs for the duration
// of its measurements. It records only the columnar-filter batch counter the exec
// package increments once per columnar FillChunk batch, so the exercise can prove
// the columnar Expand+Filter chain actually engaged (batch counter > 0) rather
// than silently falling back to the boxed row path. The name mirrors the exec
// package's unexported constant; a rename there would make this read zero, which
// the "engaged" assertion in the regression test would catch.
type colBackend struct{ filterBatches atomic.Uint64 }

func (c *colBackend) IncCounter(name string, delta uint64) {
	if name == "cypher.exec.columnar_filter.batch" {
		c.filterBatches.Add(delta)
	}
}
func (c *colBackend) ObserveLatency(string, time.Duration) {}

// columnarExercise builds a bounded social sub-graph and drives three
// representative analytic queries that each engage one of the engine's columnar
// (chunk-at-a-time) physical operators, measuring the allocation win each delivers
// over the byte-identical row-at-a-time baseline (rmp #2121):
//
//   - a GROUP BY aggregation (RETURN u.country, count(*)) — engages the columnar
//     aggregation (cypher/exec/agg_column_kernel.go), which hashes the grouping key
//     UNBOXED and boxes it only once per distinct group (#2049/#2104);
//   - a filter-over-traversal (MATCH (u)-[r]->(p) WHERE p.id >= k RETURN p.id) —
//     engages the columnar Expand + ColumnarFilter chain (#2106), which reads the
//     far node's id and evaluates the predicate over the traversal output unboxed;
//   - a disconnected equi-join (MATCH (a:USER),(b:USER) WHERE a.name = b.name …) —
//     engages the columnar hash join (#2105), whose build side retains rows into a
//     column-major buffer instead of a per-row snapshot.
//
// For each it emits the allocation profile of the columnar execution and of a
// baseline that is result-identical but forced onto the row path: the aggregation
// and filter baselines wrap the property in coalesce() (value-equivalent, but it
// disqualifies the columnar pre-projection / predicate), and the join baseline runs
// the same query on an engine with the hash join disabled (the O(V²) nested-loop
// plan). Each pair is asserted to return the identical result before its allocation
// delta is reported, so a divergence fails loudly rather than flattering the win.
//
// Every line is volatile telemetry (prefixed "# "): the allocation figures vary per
// run and machine, so the regression test reads them by name and asserts the
// direction of the win (and, for the filter, the batch-counter engagement proof),
// never a pinned value.
func columnarExercise(ctx context.Context, cfg config, w io.Writer) error {
	// Bounded working set (see colScaleUsers). Degrees are kept modest and clamped
	// so the sub-graph is always valid however small -users/-articles are; the seed
	// and relationship-encoding mode follow the run so the sub-graph is deterministic.
	ccfg := config{
		users:    min(cfg.users, colScaleUsers),
		articles: min(cfg.articles, colScaleArticles),
		seed:     cfg.seed,
		relTypes: cfg.relTypes,
	}
	ccfg.friendsMax = min(8, ccfg.users-1)
	ccfg.friendsMin = min(5, ccfg.friendsMax)
	ccfg.likesMax = min(10, ccfg.articles)
	if err := ccfg.validate(); err != nil {
		return fmt.Errorf("working-set config: %w", err)
	}

	cg := lpg.New[string, float64](adjlist.Config{Directed: true, Weightless: true})
	if _, err := build(ctx, cg, ccfg, io.Discard); err != nil {
		return fmt.Errorf("working-set build: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cg.AdjList().Compact(ctx)

	eng := cypher.NewEngine(cg)
	engNL := cypher.NewEngineWithOptions(cg, cypher.EngineOptions{DisableHashJoin: true})

	be := &colBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	fmt.Fprintln(w, "# --- columnar execution exercise (#2121) ---")
	fmt.Fprintf(w, "# columnar.scale.users=%d\n", ccfg.users)
	fmt.Fprintf(w, "# columnar.scale.articles=%d\n", ccfg.articles)

	if err := columnarAggExercise(ctx, eng, be, w); err != nil {
		return err
	}
	if err := columnarFilterExercise(ctx, eng, be, w); err != nil {
		return err
	}
	return columnarJoinExercise(ctx, eng, engNL, be, w)
}

// columnarAggExercise drives the GROUP BY aggregation and its coalesce() row-mode
// baseline, verifying they group identically before reporting the allocation delta.
func columnarAggExercise(ctx context.Context, eng *cypher.Engine, be *colBackend, w io.Writer) error {
	const (
		colQ = "MATCH (u:USER) RETURN u.country AS country, count(*) AS members ORDER BY country"
		rowQ = "MATCH (u:USER) RETURN coalesce(u.country) AS country, count(*) AS members ORDER BY country"
	)
	colGroups, _, err := groupCounts(ctx, eng, colQ, "country", "members", nil)
	if err != nil {
		return fmt.Errorf("agg columnar: %w", err)
	}
	rowGroups, _, err := groupCounts(ctx, eng, rowQ, "country", "members", nil)
	if err != nil {
		return fmt.Errorf("agg row: %w", err)
	}
	if !sameGroups(colGroups, rowGroups) {
		return fmt.Errorf("agg columnar/row results differ: %v vs %v", colGroups, rowGroups)
	}
	var total int64
	for _, g := range colGroups {
		total += g.count
	}

	colP, err := colMeasure(ctx, eng, be, colQ, nil)
	if err != nil {
		return fmt.Errorf("agg columnar measure: %w", err)
	}
	rowP, err := colMeasure(ctx, eng, be, rowQ, nil)
	if err != nil {
		return fmt.Errorf("agg row measure: %w", err)
	}

	fmt.Fprintf(w, "# columnar.agg.groups=%d\n", len(colGroups))
	fmt.Fprintf(w, "# columnar.agg.members_total=%d\n", total)
	fmt.Fprintf(w, "# columnar.agg.col_mallocs=%d\n", colP.mallocs)
	fmt.Fprintf(w, "# columnar.agg.row_mallocs=%d\n", rowP.mallocs)
	fmt.Fprintf(w, "# columnar.agg.col_bytes=%d\n", colP.bytes)
	fmt.Fprintf(w, "# columnar.agg.row_bytes=%d\n", rowP.bytes)
	fmt.Fprintf(w, "# columnar.agg.malloc_ratio=%.2f\n", safeDiv(float64(rowP.mallocs), float64(colP.mallocs)))
	return nil
}

// columnarFilterExercise drives the bare-pattern filter-over-traversal and its
// coalesce() row-mode baseline. The bare pattern (no labels/types) is required: a
// label or type on either endpoint inserts a Selection that breaks the
// scan→Expand→ColumnarFilter chain, so the columnar path would silently not engage.
func columnarFilterExercise(ctx context.Context, eng *cypher.Engine, be *colBackend, w io.Writer) error {
	const (
		colQ = "MATCH (u)-[r]->(p) WHERE p.id >= '8' RETURN p.id AS id"
		rowQ = "MATCH (u)-[r]->(p) WHERE coalesce(p.id) >= '8' RETURN coalesce(p.id) AS id"
	)
	colRows, err := colDrainCount(ctx, eng, colQ, nil)
	if err != nil {
		return fmt.Errorf("filter columnar: %w", err)
	}
	rowRows, err := colDrainCount(ctx, eng, rowQ, nil)
	if err != nil {
		return fmt.Errorf("filter row: %w", err)
	}
	if colRows != rowRows {
		return fmt.Errorf("filter columnar/row row counts differ: %d vs %d", colRows, rowRows)
	}

	colP, err := colMeasure(ctx, eng, be, colQ, nil)
	if err != nil {
		return fmt.Errorf("filter columnar measure: %w", err)
	}
	rowP, err := colMeasure(ctx, eng, be, rowQ, nil)
	if err != nil {
		return fmt.Errorf("filter row measure: %w", err)
	}

	fmt.Fprintf(w, "# columnar.filter.rows=%d\n", colP.rows)
	fmt.Fprintf(w, "# columnar.filter.col_batches=%d\n", colP.batches)
	fmt.Fprintf(w, "# columnar.filter.row_batches=%d\n", rowP.batches)
	fmt.Fprintf(w, "# columnar.filter.col_mallocs=%d\n", colP.mallocs)
	fmt.Fprintf(w, "# columnar.filter.row_mallocs=%d\n", rowP.mallocs)
	fmt.Fprintf(w, "# columnar.filter.col_bytes=%d\n", colP.bytes)
	fmt.Fprintf(w, "# columnar.filter.row_bytes=%d\n", rowP.bytes)
	fmt.Fprintf(w, "# columnar.filter.malloc_ratio=%.2f\n", safeDiv(float64(rowP.mallocs), float64(colP.mallocs)))
	return nil
}

// columnarJoinExercise drives the disconnected equi-join under the default engine
// (columnar hash join) and under a hash-join-disabled engine (the O(V²) nested-loop
// baseline), verifying both count the same pairs before reporting the delta.
//
// The nested-loop baseline is O(V²), so it is executed EXACTLY ONCE — via
// [colCountMeasure] with no warm-up, which reads the single count value and the
// allocation profile from the same run — keeping the bounded-working-set exercise
// affordable under the race detector. The columnar side is O(V), so it is warmed
// (excluding its one-off plan compilation) before its clean measurement.
func columnarJoinExercise(ctx context.Context, eng, engNL *cypher.Engine, be *colBackend, w io.Writer) error {
	const q = "MATCH (a:USER),(b:USER) WHERE a.name = b.name AND a.id < b.id RETURN count(*) AS c"

	colPairs, colP, err := colCountMeasure(ctx, eng, be, q, true)
	if err != nil {
		return fmt.Errorf("join columnar: %w", err)
	}
	nlPairs, nlP, err := colCountMeasure(ctx, engNL, be, q, false)
	if err != nil {
		return fmt.Errorf("join nested-loop: %w", err)
	}
	if colPairs != nlPairs {
		return fmt.Errorf("join columnar/nested-loop pair counts differ: %d vs %d", colPairs, nlPairs)
	}

	fmt.Fprintf(w, "# columnar.hashjoin.pairs=%d\n", colPairs)
	fmt.Fprintf(w, "# columnar.hashjoin.col_mallocs=%d\n", colP.mallocs)
	fmt.Fprintf(w, "# columnar.hashjoin.nested_mallocs=%d\n", nlP.mallocs)
	fmt.Fprintf(w, "# columnar.hashjoin.col_bytes=%d\n", colP.bytes)
	fmt.Fprintf(w, "# columnar.hashjoin.nested_bytes=%d\n", nlP.bytes)
	fmt.Fprintf(w, "# columnar.hashjoin.malloc_ratio=%.2f\n", safeDiv(float64(nlP.mallocs), float64(colP.mallocs)))
	return nil
}

// colCountMeasure runs a single-row count query and returns the count value plus a
// one-execution allocation profile. When warm is true it first runs the query once
// to populate the plan cache (so the measured run excludes plan compilation); when
// false it measures the very first run — used for the O(V²) nested-loop baseline,
// whose execution cost dwarfs its one-off compilation, so it need never run twice.
func colCountMeasure(ctx context.Context, eng *cypher.Engine, be *colBackend, query string, warm bool) (int64, colProfile, error) {
	if warm {
		if _, _, err := scalarCount(ctx, eng, query, nil); err != nil {
			return 0, colProfile{}, err
		}
	}
	be.filterBatches.Store(0)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	val, _, err := scalarCount(ctx, eng, query, nil)
	runtime.ReadMemStats(&m1)
	if err != nil {
		return 0, colProfile{}, err
	}
	return val, colProfile{
		mallocs: m1.Mallocs - m0.Mallocs,
		bytes:   m1.TotalAlloc - m0.TotalAlloc,
		rows:    1,
		batches: be.filterBatches.Load(),
	}, nil
}

// colMeasure warms query on eng (populating the plan cache so the measured run
// excludes one-off parse/plan allocations), then measures a single clean execution:
// the allocation count and bytes it charges (via monotonic, GC-independent
// [runtime.MemStats] counters), the rows it drains, and the columnar-filter batches
// it drove. The result is drained through Next alone — never Record — so the figure
// reflects the OPERATOR's execution allocations (where the columnar de-box lives),
// not the per-row map materialisation a caller would add identically to both paths.
func colMeasure(ctx context.Context, eng *cypher.Engine, be *colBackend, query string, params map[string]expr.Value) (colProfile, error) {
	if _, err := colDrainCount(ctx, eng, query, params); err != nil { // warm the plan cache
		return colProfile{}, err
	}
	be.filterBatches.Store(0)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	rows, err := colDrainCount(ctx, eng, query, params)
	runtime.ReadMemStats(&m1)
	if err != nil {
		return colProfile{}, err
	}
	return colProfile{
		mallocs: m1.Mallocs - m0.Mallocs,
		bytes:   m1.TotalAlloc - m0.TotalAlloc,
		rows:    rows,
		batches: be.filterBatches.Load(),
	}, nil
}

// colDrainCount runs query and drains it to completion via Next, returning the row
// count. It reads no cell (no Record/ValueAt), so the only allocations charged are
// the engine's own execution allocations — the surface the columnar operators cut.
func colDrainCount(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value) (int, error) {
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	return rows, nil
}

// sameGroups reports whether two grouped-count result sets are the identical
// multiset of (key, count) rows, order-independent.
func sameGroups(a, b []labeledCount) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int64, len(a))
	for _, r := range a {
		m[r.key] += r.count
	}
	for _, r := range b {
		m[r.key] -= r.count
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
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
// Intra-query parallelism exercise (rmp #2122)
// ─────────────────────────────────────────────────────────────────────────────

// Parallelism working-set bounds. The parallel min/max aggregate (#2111) engages
// only above [cypher.EngineOptions.ParallelScanThreshold] live nodes, so this
// exercise builds a user population large enough to cross it and to give the
// morsel-parallel reduce several [exec.DefaultMorselSize] morsels of work to spread
// across cores — but small enough that the whole exercise (build + timed queries)
// stays a few seconds under -race in the short test layer. Unlike run's -users
// scale it is a CONSTANT: intra-query parallelism only matters at scale, so the
// exercise pins its own working set rather than tracking -users (mirroring
// columnarExercise, which likewise runs on a fixed bounded sub-graph).
const (
	parScaleUsers = 60_000

	// parScanThreshold is the ParallelScanThreshold the exercise lowers its parallel
	// engine to. The morsel-parallel path engages strictly above it (#1672), so a
	// value far below parScaleUsers guarantees engagement at this scale. The example
	// lowers it through EngineOptions — an engine CONFIG knob, not a module change —
	// because the shipped default ([cypher.DefaultParallelScanThreshold] = 50000)
	// sits just below this working set; a production engine keeps the default.
	parScanThreshold = 1024

	// reputationRange bounds the synthetic per-user reputation score to
	// [0, reputationRange). It is the numeric property the whole-graph min/max scans.
	reputationRange = 100_000

	// propReputation is the numeric per-user score the parallel min/max aggregate
	// runs over.
	propReputation = "reputation"

	// parReps is the number of timed repetitions per query; the best (smallest)
	// wall-clock is reported, which damps scheduler noise without pinning a
	// machine-specific number. Kept small so the exercise's contribution to the
	// short-layer package budget stays a few seconds under -race.
	parReps = 3
)

// parallelismExercise builds a bounded user population and exercises the engine's
// automatic intra-query parallelism over whole-graph node aggregates (rmp #2122):
//
//   - a bare-scan min/max over a numeric property (MATCH (n) RETURN min/max(n.reputation))
//     engages the morsel-parallel aggregate (#2111) above ParallelScanThreshold: the
//     per-node property read and Compare are split across up to GOMAXPROCS workers,
//     each carrying its partial extremum with the scan position that breaks a tie, so
//     the parallel reduce is BIT-IDENTICAL to the serial first-seen result. The
//     exercise runs the SAME query on a parallel-configured engine and on a
//     DisableParallelScan serial engine, verifies the two values are identical
//     (failing loud otherwise), and reports both wall-clocks and the speedup.
//   - a count(*) over every node engages the O(1) count pushdown (#2113): the serial
//     AllNodesCountScan reads the maintained live-node counter directly instead of
//     walking the graph. The exercise contrasts its wall-clock against an O(N) full
//     scan count (a trivially-true WHERE keeps the scan non-bare, so the pushdown
//     declines and the whole population is walked) over the same node count, making
//     the O(1) nature visible, and verifies both counts equal the population.
//
// Honest telemetry: intra-query parallelism is a LATENCY win that pays only by
// consuming otherwise-idle cores — speedup ≈ min(workers, idle cores) × efficiency.
// The figures here are the single-tenant, idle-box case (one query at a time). Under
// concurrent multi-client load the shared worker governor correctly throttles each
// query toward the serial path (the budget==1 short-circuit runs the reduce inline),
// so the win narrows toward parity. This exercise does not model that regime and does
// not claim the speedup holds under saturation.
//
// Every line is volatile telemetry (prefixed "# "): the wall-clocks and speedups vary
// per run and machine, so the regression test reads them by name and asserts the
// deterministic invariants — result identity and the exact population count — rather
// than pinning a timing or gating on the win.
func parallelismExercise(ctx context.Context, cfg config, w io.Writer) error {
	g, err := buildPopulation(ctx, cfg.seed, parScaleUsers)
	if err != nil {
		return fmt.Errorf("population build: %w", err)
	}

	// Two engines over the SAME immutable population: the parallel path engages above
	// parScanThreshold, the serial path is forced off. Sharing g keeps the scan order
	// identical, so the parallel and serial extrema are bit-identical, not merely
	// value-equal on a tie.
	engPar := cypher.NewEngineWithOptions(g, cypher.EngineOptions{ParallelScanThreshold: parScanThreshold})
	engSer := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableParallelScan: true})

	fmt.Fprintln(w, "# --- intra-query parallelism exercise (#2122) ---")
	fmt.Fprintf(w, "# parallel.scale.nodes=%d\n", parScaleUsers)
	fmt.Fprintf(w, "# parallel.scale.threshold=%d\n", parScanThreshold)
	fmt.Fprintf(w, "# parallel.gomaxprocs=%d\n", runtime.GOMAXPROCS(0))

	if err := parallelAggExercise(ctx, engPar, engSer, "min", w); err != nil {
		return err
	}
	if err := parallelAggExercise(ctx, engPar, engSer, "max", w); err != nil {
		return err
	}
	return parallelCountExercise(ctx, engSer, parScaleUsers, w)
}

// buildPopulation materialises n users into a fresh weightless graph, each carrying a
// realistic name and a numeric reputation score in [0, reputationRange), drawn from a
// seed-derived RNG so the population — and therefore the min/max extrema and the count
// — is reproducible for a given seed. It builds NO edges: the parallel aggregate and
// the count pushdown operate over the whole-graph NODE scan and never traverse
// relationships, so a pure account population is the faithful working set for this
// exercise (edge traversal is exercised by the main battery and columnarExercise).
func buildPopulation(ctx context.Context, seed int64, n int) (*lpg.Graph[string, float64], error) {
	//nolint:gosec // G404: a seeded math/rand fixes the reputation distribution for a
	// given -seed; crypto/rand would defeat the reproducibility this benchmark needs.
	rng := rand.New(rand.NewSource(seed))
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Weightless: true})
	for i := 0; i < n; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		k := "u" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			return nil, fmt.Errorf("AddNode %s: %w", k, err)
		}
		if err := g.SetNodeLabel(k, labelUser); err != nil {
			return nil, fmt.Errorf("SetNodeLabel %s: %w", k, err)
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue(realisticName(rng))); err != nil {
			return nil, fmt.Errorf("SetNodeProperty name %s: %w", k, err)
		}
		if err := g.SetNodeProperty(k, propReputation, lpg.Int64Value(int64(rng.Intn(reputationRange)))); err != nil {
			return nil, fmt.Errorf("SetNodeProperty reputation %s: %w", k, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Right-size the node backing arrays before the read-only query phase, mirroring
	// run (this is a build-then-query workload).
	g.AdjList().Compact(ctx)
	return g, nil
}

// parallelAggExercise runs a bare-scan aggregate (agg is "min" or "max") over the
// reputation property on both the parallel and the serial engine, verifies the two
// results are identical before reporting (the parallel reduce must be bit-identical to
// serial), and emits both wall-clocks and the speedup as telemetry.
func parallelAggExercise(ctx context.Context, engPar, engSer *cypher.Engine, agg string, w io.Writer) error {
	q := fmt.Sprintf("MATCH (n) RETURN %s(n.%s) AS c", agg, propReputation)

	parVal, parDur, err := bestScalar(ctx, engPar, q)
	if err != nil {
		return fmt.Errorf("%s parallel: %w", agg, err)
	}
	serVal, serDur, err := bestScalar(ctx, engSer, q)
	if err != nil {
		return fmt.Errorf("%s serial: %w", agg, err)
	}
	if parVal != serVal {
		return fmt.Errorf("%s parallel/serial results differ: %d vs %d (the parallel reduce must be bit-identical to serial)", agg, parVal, serVal)
	}

	fmt.Fprintf(w, "# parallel.%s.value=%d\n", agg, parVal)
	fmt.Fprintf(w, "# parallel.%s.parallel_elapsed=%s\n", agg, parDur.Round(time.Microsecond))
	fmt.Fprintf(w, "# parallel.%s.serial_elapsed=%s\n", agg, serDur.Round(time.Microsecond))
	fmt.Fprintf(w, "# parallel.%s.speedup=%.2f\n", agg, safeDiv(float64(serDur), float64(parDur)))
	return nil
}

// parallelCountExercise contrasts the O(1) count pushdown against the O(N) full scan
// it replaces, both on the serial (DisableParallelScan) engine so the O(1) path is the
// AllNodesCountScan. It verifies the O(1) count, the O(N) scan count, and the known
// population all agree before reporting, making the O(1) nature visible as a wall-clock
// that does not scale with the node count.
func parallelCountExercise(ctx context.Context, engSer *cypher.Engine, nodes int, w io.Writer) error {
	const (
		o1Q = "MATCH (n) RETURN count(*) AS c"
		// A trivially-true predicate over every node (reputation is always >= 0) keeps
		// the scan non-bare, so the O(1) pushdown declines and the whole population is
		// walked — the O(N) baseline the direct read replaces.
		scanQ = "MATCH (n) WHERE n.reputation >= 0 RETURN count(*) AS c"
	)
	o1Val, o1Dur, err := bestScalar(ctx, engSer, o1Q)
	if err != nil {
		return fmt.Errorf("count O(1): %w", err)
	}
	scanVal, scanDur, err := bestScalar(ctx, engSer, scanQ)
	if err != nil {
		return fmt.Errorf("count O(N) scan: %w", err)
	}
	if o1Val != scanVal || o1Val != int64(nodes) {
		return fmt.Errorf("count mismatch: o1=%d scan=%d population=%d (all three must agree)", o1Val, scanVal, nodes)
	}

	fmt.Fprintf(w, "# parallel.count.nodes=%d\n", nodes)
	fmt.Fprintf(w, "# parallel.count.value=%d\n", o1Val)
	fmt.Fprintf(w, "# parallel.count.o1_elapsed=%s\n", o1Dur)
	fmt.Fprintf(w, "# parallel.count.scan_elapsed=%s\n", scanDur.Round(time.Microsecond))
	fmt.Fprintf(w, "# parallel.count.speedup=%.1f\n", safeDiv(float64(scanDur), float64(o1Dur)))
	return nil
}

// bestScalar warms query on eng (populating the plan cache so the timed runs exclude
// one-off compilation) then runs it parReps times, returning the single-row integer
// value and the best (smallest) wall-clock. Best-of damps scheduler noise without
// pinning a machine-specific number. Every result is fully drained and closed, so the
// parallel worker pool joins before return (the package's goleak TestMain enforces it).
func bestScalar(ctx context.Context, eng *cypher.Engine, query string) (int64, time.Duration, error) {
	if _, _, err := scalarCount(ctx, eng, query, nil); err != nil { // warm the plan cache
		return 0, 0, err
	}
	var (
		best time.Duration
		val  int64
	)
	for i := 0; i < parReps; i++ {
		v, d, err := scalarCount(ctx, eng, query, nil)
		if err != nil {
			return 0, 0, err
		}
		val = v
		if i == 0 || d < best {
			best = d
		}
	}
	return val, best, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Realistic-data word lists. Fixed so the dataset is reproducible.
// ─────────────────────────────────────────────────────────────────────────────

// countries is the small fixed set the per-user country dimension cycles
// through (see build). Its low cardinality is deliberate: it makes each group
// hold many users, so the columnar aggregation's win — hashing the grouping key
// UNBOXED and boxing it only once per distinct group (#2049) — is visible in the
// allocation profile the columnar exercise reports. Kept immutable after init.
var countries = []string{
	"Portugal", "Spain", "France", "Germany", "Italy", "Netherlands",
	"Ireland", "Poland", "Sweden", "Norway", "Denmark", "Finland",
}

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
