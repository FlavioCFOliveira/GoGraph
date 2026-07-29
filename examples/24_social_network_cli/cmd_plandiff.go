package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// cmd_plandiff.go — the `plandiff` subcommand: a realistic exercise of the two
// count-store-gated query-reordering peepholes GoGraph ships (sprint 307), with
// explicit before/after evidence.
//
//   - The single-edge ANCHOR SWAP (#2090). In a large content graph most posts
//     carry no comments (a long-tail engagement distribution). The naive query
//     "list every commented post with its comment", written anchored on :Post as
//     `MATCH (p:Post)<-[:ON]-(c:Comment)`, makes the engine SCAN every post and
//     walk its incoming ON edges. Because ON points Comment→Post, the peephole
//     re-roots the pattern onto the far smaller :Comment population (each comment
//     is ON exactly one post) — a forward DirOut expand — examining |Comment|
//     starting rows instead of |Post|.
//   - The disjoint-component ORDERING (#2091). A comma-separated Cartesian
//     `MATCH (u:User),(c:Comment)` (e.g. sizing a moderation candidate space)
//     re-inits its inner plan once per OUTER row. The peephole drives the smaller
//     component so the inner side is re-initialised |Comment| times instead of
//     |User|.
//
// Both peepholes are gated by the exact relationship count-store: they fire only
// when every cardinality the cost model reads is exact and the reorder is proven
// order-safe. The subcommand runs each scenario on a reordering-ENABLED engine
// and a -DISABLED engine over the SAME recovered graph and surfaces, as explicit
// telemetry:
//
//   - the EXPLAIN plan-diff (the chosen operator order / expand direction differs);
//   - an exact "work" contrast read from the count-store (scanned starting rows for
//     the anchor swap; inner re-initialisations for the disjoint reorder) — the
//     db-hits-style figure the engine itself uses to make the decision; and
//   - the wall-clock cost ENABLED vs DISABLED (median of a few runs).
//
// The demonstration population is a deterministic synthetic content layer seeded
// once on top of the canonical fixture (idempotent: re-running skips re-seeding),
// so the graph carries the skew the peepholes need. It is a read-only exercise of
// the module's planner, complementing the query subcommand's write demonstration.

// plandiffConfig bundles the data directory and the scale knob of the plandiff
// subcommand.
type plandiffConfig struct {
	dir   string
	scale int // multiplies the synthetic content population (>= 1)
}

// Synthetic-content base sizes (multiplied by scale). Chosen so both peepholes
// fire with margin at scale 1 while keeping the one-shot command fast:
//   - |Post| ≫ |Comment| (long-tail engagement) makes the anchor swap re-root
//     `(p:Post)<-[:ON]-(c:Comment)` onto the small :Comment side;
//   - |User| ≫ |Comment| makes the disjoint reorder drive :Comment.
const (
	plandiffBaseUsers    = 2000
	plandiffBasePosts    = 1500
	plandiffBaseComments = 100
	// plandiffBaseMutual is how many synthetic users are wired into MUTUAL FOLLOWS
	// pairs, so `(a:User)-[:FOLLOWS]->(b:User)-[:FOLLOWS]->(a)` — the friendship
	// shape — actually matches. A one-directional ring closes no 2-cycle at all
	// (a ring of out-degree k needs k1+k2 ≡ 0 mod n), so without the back-edge the
	// bound-destination scenario would measure an empty result.
	plandiffBaseMutual = 400
	// plandiffFanout is how many EXTRA, non-mutual accounts each mutually-following
	// user follows. Without it the traversed out-degree is ~2 and a seek cannot beat a
	// two-slot scan: the first version of this scenario measured 1.04x for exactly that
	// reason. A real user follows tens of accounts of which only a few follow back, so
	// the fan-out is what makes the fixture realistic AND the access path observable.
	// The targets are drawn from users at index >= plandiffBaseMutual, which are never
	// fan-out SOURCES, so no accidental mutual pair is created and the expected mutual
	// count stays exactly the pair count.
	// 24 is a compromise, and the reason is worth stating: the seek's benefit grows
	// with the traversed degree, so a bigger fan-out demonstrates more — but the
	// scenarios run under `-race` in the short test layer, where every operation costs
	// roughly an order of magnitude more. At 48 this package took 548 s under `-race`,
	// far past the 60 s per-package budget. 24 keeps the demonstration clear (the seek
	// still wins by ~2x) while the whole subcommand runs in about a second.
	plandiffFanout = 24
	// plandiffBaseVerified is the small :Verified population for the symmetric
	// anchor swap. One high-volume account FOLLOWS plandiffBaseUsers accounts but
	// only ONE :Verified account, so the written plan (anchored on the account) walks
	// its whole out-adjacency while the mirror (anchored on :Verified) walks one
	// in-edge — the lopsidedness the cost model needs, in a shape a social product
	// really asks: "which verified accounts does this firehose account follow?"
	plandiffBaseVerified = 50
	// plandiffTriangleSeeds is how many :Core users also carry :Seed, the triangle
	// scenario's anchor. Small on purpose: triangle cost is cubic in the fan-out.
	plandiffTriangleSeeds = 32
)

// plandiff synthetic key prefixes; distinct from the fixture's natural keys
// (alice..erin, p1.., c1..) and the scale population's "u_" prefix, so the
// content layer is recognisable and its presence is a cheap idempotency sentinel.
const (
	plandiffUserPrefix     = "dp_user_"
	plandiffPostPrefix     = "dp_post_"
	plandiffCommentPrefix  = "dp_cmt_"
	plandiffVerifiedPrefix = "dp_ver_"
	// plandiffFirehoseKey is the single high-volume account whose out-adjacency the
	// symmetric-swap scenario refuses to walk.
	plandiffFirehoseKey = "dp_firehose"
)

// plandiff scenario queries. Neither carries an order-establishing operator, so
// the reorder is order-safe; the RETURN shapes are scalar (anchor) and an
// order-blind count (disjoint), both of which SuppressReorder passes.
const (
	plandiffAnchorQuery   = "MATCH (p:Post)<-[:ON]-(c:Comment) RETURN c.id AS comment, p.id AS post"
	plandiffDisjointQuery = "MATCH (u:User), (c:Comment) RETURN count(*) AS pairs"
	// plandiffMutualQuery is the bound-destination shape (#2149): the second hop's
	// destination `a` is ALREADY BOUND, so the planner emits an ExpandInto whose
	// cursor seeks `a` in b's destination-ordered neighbour run instead of walking
	// all of b's followees and discarding the misses. Mutual following is how a
	// social product detects friendship, which makes this the natural realistic
	// query for the operator.
	plandiffMutualQuery = "MATCH (a:User)-[:FOLLOWS]->(b:User)-[:FOLLOWS]->(a) RETURN count(*) AS mutuals"
	// plandiffTriangleQuery closes a 3-cycle: a follows b, b follows c, c follows a.
	// Its closing hop also seeks, but its MIDDLE hop is open, so its cost is bounded
	// by that hop's intermediate result however fast the closing hop becomes — which
	// is why it is reported separately rather than folded into the headline.
	// Anchored on :Core — the mutually-following community — rather than on all users.
	// A triangle materialises Theta(n*d^2) intermediate rows — cubic in the fan-out — so
	// anchoring it on the whole population costs SECONDS per run at a realistic degree.
	// :Seed is a small slice of the mutually-following community, which keeps the
	// one-shot command fast and still asks a meaningful question, since triangles within
	// a community are what a social product actually looks for.
	plandiffTriangleQuery = "MATCH (a:Seed)-[:FOLLOWS]->(b:User)-[:FOLLOWS]->(c:User)-[:FOLLOWS]->(a) RETURN count(*) AS triangles"
	// plandiffSymmetricSwapQuery is a single-edge pattern written in the OUT
	// direction whose cheaper anchor requires a REVERSE expand (#2150). Before the
	// swap became symmetric this was left in written order — a missed optimisation
	// that grew without bound in the firehose account's out-degree.
	plandiffSymmetricSwapQuery = "MATCH (f:Firehose)-[:FOLLOWS]->(v:Verified) RETURN v.id AS verified"
)

// cmdPlandiff is the `plandiff` entry point.
func cmdPlandiff(args []string) error {
	cfg, err := parsePlandiffArgs(args)
	if err != nil {
		return err
	}
	return runPlandiff(context.Background(), cfg, os.Stdout)
}

// parsePlandiffArgs parses the plandiff flags. A parse failure or missing -d maps
// to a *usageError (exit 2); a non-positive scale is a runtime error (exit 1).
func parsePlandiffArgs(args []string) (plandiffConfig, error) {
	cfg := plandiffConfig{scale: 1}
	fs := flag.NewFlagSet("plandiff", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.dir, "d", "", "data directory (required)")
	fs.IntVar(&cfg.scale, "scale", cfg.scale, "multiplier for the synthetic content population (>= 1)")
	if perr := fs.Parse(args); perr != nil {
		return plandiffConfig{}, newUsageError("plandiff: flag parse: %v", perr)
	}
	if cfg.dir == "" {
		return plandiffConfig{}, newUsageError("plandiff: missing required flag -d <dir>")
	}
	if cfg.scale < 1 {
		return plandiffConfig{}, fmt.Errorf("plandiff: scale must be >= 1, got %d", cfg.scale)
	}
	return cfg, nil
}

// runPlandiff opens the data directory, ensures the synthetic content layer is
// present (seeding it once), and runs both reordering scenarios against a
// reordering-ENABLED and a -DISABLED engine, writing the plan-diff and evidence
// to out.
func runPlandiff(ctx context.Context, cfg plandiffConfig, out io.Writer) (retErr error) {
	o, err := openStore(ctx, cfg.dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := o.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("plandiff: close store: %w", cerr)
		}
	}()

	seeded, err := ensurePlandiffContent(ctx, o.store, cfg)
	if err != nil {
		return fmt.Errorf("plandiff: %w", err)
	}

	// Two engines over the SAME recovered graph: one with the reordering peepholes
	// enabled (the shipped default), one with both disabled. Both recompute an
	// identical exact count-store at construction, so the only difference is
	// whether the peepholes may fire.
	onEng := cypher.NewEngineWithOptions(o.graph, cypher.EngineOptions{
		MaxResultRows: cypher.MaxResultRowsUnlimited,
	})
	offEng := cypher.NewEngineWithOptions(o.graph, cypher.EngineOptions{
		DisableAnchorSwap:  true,
		DisableJoinReorder: true,
		MaxResultRows:      cypher.MaxResultRowsUnlimited,
	})

	nUsers, err := scalarCount(ctx, onEng, "MATCH (:User) RETURN count(*) AS c")
	if err != nil {
		return fmt.Errorf("plandiff: count users: %w", err)
	}
	nPosts, err := scalarCount(ctx, onEng, "MATCH (:Post) RETURN count(*) AS c")
	if err != nil {
		return fmt.Errorf("plandiff: count posts: %w", err)
	}
	nComments, err := scalarCount(ctx, onEng, "MATCH (:Comment) RETURN count(*) AS c")
	if err != nil {
		return fmt.Errorf("plandiff: count comments: %w", err)
	}

	fmt.Fprintf(out, "# plandiff: reordering peepholes exercise (sprint 307)\n")
	writeTelemetry(out, "content.seeded_now", strconv.FormatBool(seeded))
	writeTelemetry(out, "cardinality.users", strconv.FormatInt(nUsers, 10))
	writeTelemetry(out, "cardinality.posts", strconv.FormatInt(nPosts, 10))
	writeTelemetry(out, "cardinality.comments", strconv.FormatInt(nComments, 10))

	// Scenario 1 — single-edge anchor swap (#2090). The written plan scans every
	// :Post; the swap re-roots onto the far smaller :Comment side. The "work"
	// contrast is the number of starting rows scanned: |Post| (disabled) vs
	// |Comment| (enabled).
	if err := runPlandiffScenario(ctx, out, &plandiffScenario{
		name:        "anchor-swap",
		query:       plandiffAnchorQuery,
		onEng:       onEng,
		offEng:      offEng,
		workLabel:   "scanned_start_rows",
		workEnabled: nComments,
		workWritten: nPosts,
	}); err != nil {
		return fmt.Errorf("plandiff: anchor scenario: %w", err)
	}

	// Scenario 2 — disjoint-component ordering (#2091). The Cartesian re-inits its
	// inner plan once per outer row; the swap drives the smaller side. The "work"
	// contrast is the inner re-initialisation count: |User| (disabled) vs
	// |Comment| (enabled).
	if err := runPlandiffScenario(ctx, out, &plandiffScenario{
		name:        "disjoint-reorder",
		query:       plandiffDisjointQuery,
		onEng:       onEng,
		offEng:      offEng,
		workLabel:   "inner_reinitialisations",
		workEnabled: nComments,
		workWritten: nUsers,
	}); err != nil {
		return fmt.Errorf("plandiff: disjoint scenario: %w", err)
	}

	// The traversal scenarios are gated by DIFFERENT knobs from the two reordering
	// peepholes, so they need their own engine pair. Each disables exactly ONE thing
	// against the shipped default, so a rendered diff is attributable to that knob.
	seekOffEng := cypher.NewEngineWithOptions(o.graph, cypher.EngineOptions{
		DisableExpandIntoSeek: true,
		MaxResultRows:         cypher.MaxResultRowsUnlimited,
	})

	// The degree profile explains WHY the seek engages and bounds how much it can win,
	// so it is emitted before the scenarios that depend on it.
	if err := writePlandiffDegreeProfile(ctx, out, onEng); err != nil {
		return fmt.Errorf("plandiff: %w", err)
	}

	// Scenario 3 — the bound-destination seek (#2149) on mutual following, the shape a
	// social product uses to detect friendship. The closing hop's destination is
	// already bound, so it seeks instead of walking b's whole followee list.
	if err := runPlandiffScenario(ctx, out, &plandiffScenario{
		name:     "expand-into-mutual",
		query:    plandiffMutualQuery,
		onEng:    onEng,
		offEng:   seekOffEng,
		physical: true,
		offLabel: "ExpandInto seek DISABLED (enumerate + filter)",
		onLabel:  "ExpandInto seek ENABLED",
	}); err != nil {
		return fmt.Errorf("plandiff: expand-into scenario: %w", err)
	}

	// Scenario 4 — the same access path on a TRIANGLE. Reported separately because its
	// middle hop is open, so its cost is bounded by that hop's intermediate result
	// however fast the closing hop becomes: a win here is real but cannot be as large,
	// and folding it into the headline would overstate the change.
	if err := runPlandiffScenario(ctx, out, &plandiffScenario{
		name:     "expand-into-triangle",
		query:    plandiffTriangleQuery,
		onEng:    onEng,
		offEng:   seekOffEng,
		physical: true,
		offLabel: "ExpandInto seek DISABLED (enumerate + filter)",
		onLabel:  "ExpandInto seek ENABLED",
	}); err != nil {
		return fmt.Errorf("plandiff: triangle scenario: %w", err)
	}

	// Scenario 5 — the SYMMETRIC anchor swap (#2150): a single edge written in the OUT
	// direction whose cheaper anchor needs a REVERSE expand. Before the swap became
	// symmetric this was vetoed and left in written order. The work contrast is the
	// starting rows scanned: the firehose account's whole out-adjacency (written) vs
	// the small :Verified population (mirror).
	nVerified, err := scalarCount(ctx, onEng, "MATCH (:Verified) RETURN count(*) AS c")
	if err != nil {
		return fmt.Errorf("plandiff: count verified: %w", err)
	}
	nFirehoseOut, err := scalarCount(ctx, onEng, "MATCH (:Firehose)-[r:FOLLOWS]->() RETURN count(r) AS c")
	if err != nil {
		return fmt.Errorf("plandiff: count firehose out-edges: %w", err)
	}
	if err := runPlandiffScenario(ctx, out, &plandiffScenario{
		name:        "symmetric-anchor-swap",
		query:       plandiffSymmetricSwapQuery,
		onEng:       onEng,
		offEng:      offEng,
		workLabel:   "examined_edges",
		workEnabled: nVerified,
		workWritten: nFirehoseOut,
		offLabel:    "anchor swap DISABLED",
		onLabel:     "anchor swap ENABLED (symmetric)",
	}); err != nil {
		return fmt.Errorf("plandiff: symmetric swap scenario: %w", err)
	}
	return nil
}

// plandiffScenario captures one reordering demonstration: the query, the two
// engines, and the exact enabled-vs-written "work" figures the count-store cost
// model compares (used as the db-hits-style evidence the engine has no public
// PROFILE facility for).
type plandiffScenario struct {
	name        string
	query       string
	onEng       *cypher.Engine
	offEng      *cypher.Engine
	workLabel   string
	workEnabled int64
	workWritten int64
	// physical, when true, renders Engine.Explain (the PHYSICAL tree) instead of
	// ExplainLogical. The traversal scenarios need it: the bound-destination seek is
	// a physical ACCESS PATH, surfaced as the operator's plan detail
	// ("Expand [ExpandInto seek]"), and the logical tree cannot show it because the
	// logical Expand is the same node either way.
	physical bool
	// offLabel and onLabel name what each arm has turned off or on, so the rendered
	// diff says which knob produced it rather than leaving the reader to guess. They
	// default to the reordering wording the two original scenarios shipped with, whose
	// exact text cmd_plandiff_test.go pins.
	offLabel string
	onLabel  string
}

// plandiffTimingRuns is how many times each scenario query is timed on each
// engine; the reported wall-clock is the median, which is robust to a single
// scheduling outlier while keeping the one-shot command fast.
// 3 rather than 5: the median of three still discards a single scheduling outlier,
// and every extra run is multiplied by two arms and five scenarios — and again by the
// `-race` factor when the acceptance test drives this in the short layer.
const plandiffTimingRuns = 3

// runPlandiffScenario renders the EXPLAIN plan-diff (enabled vs disabled), the
// exact work contrast, and the median wall-clock for one scenario.
//
// It reads ExplainLogical rather than Explain because this scenario demonstrates
// two PLANNER peepholes — the single-edge anchor swap and the disjoint-component
// reorder — and the diff turns on the logical rendering's pattern annotations
// ("(c)-[:ON]->(p)") and variable-qualified labels ("[c:Comment]"). A built
// Expand carries neither: it holds the direction it will walk, not the pattern
// text that named it. Engine.Explain is the surface for what physically executes;
// this one is for what the planner decided.
func runPlandiffScenario(ctx context.Context, out io.Writer, s *plandiffScenario) error {
	explain := func(eng *cypher.Engine) (string, error) {
		if s.physical {
			return eng.Explain(s.query, nil)
		}
		return eng.ExplainLogical(s.query, nil)
	}
	onPlan, err := explain(s.onEng)
	if err != nil {
		return fmt.Errorf("explain (enabled): %w", err)
	}
	offPlan, err := explain(s.offEng)
	if err != nil {
		return fmt.Errorf("explain (disabled): %w", err)
	}

	offLabel, onLabel := s.offLabel, s.onLabel
	if offLabel == "" {
		offLabel = "reordering DISABLED"
	}
	if onLabel == "" {
		onLabel = "reordering ENABLED"
	}
	fmt.Fprintf(out, "\n## scenario: %s\n", s.name)
	fmt.Fprintf(out, "query: %s\n", s.query)
	fmt.Fprintf(out, "--- EXPLAIN (%s) ---\n%s", offLabel, offPlan)
	fmt.Fprintf(out, "--- EXPLAIN (%s) ---\n%s", onLabel, onPlan)

	reordered := onPlan != offPlan
	writeTelemetry(out, s.name+".reordered", strconv.FormatBool(reordered))
	if s.workLabel != "" {
		writeTelemetry(out, s.name+"."+s.workLabel+".disabled", strconv.FormatInt(s.workWritten, 10))
		writeTelemetry(out, s.name+"."+s.workLabel+".enabled", strconv.FormatInt(s.workEnabled, 10))
		if s.workEnabled > 0 {
			writeTelemetry(out, s.name+"."+s.workLabel+".ratio",
				fmt.Sprintf("%.1fx", float64(s.workWritten)/float64(s.workEnabled)))
		}
	}

	offCost, err := measurePlandiffRun(ctx, s.offEng, s.query)
	if err != nil {
		return fmt.Errorf("time (disabled): %w", err)
	}
	onCost, err := measurePlandiffRun(ctx, s.onEng, s.query)
	if err != nil {
		return fmt.Errorf("time (enabled): %w", err)
	}
	if onCost.rows != offCost.rows {
		// A plan change that alters the answer is a defect, not an optimisation, so the
		// example refuses to report a speedup it cannot stand behind.
		return fmt.Errorf("%s: the two plans disagree on the result: enabled returned %d rows, "+
			"disabled returned %d — a reordering or access-path change must be result-identical",
			s.name, onCost.rows, offCost.rows)
	}
	writeTelemetry(out, s.name+".rows", strconv.FormatInt(onCost.rows, 10))
	writeTelemetry(out, s.name+".elapsed.disabled", offCost.elapsed.Round(time.Microsecond).String())
	writeTelemetry(out, s.name+".elapsed.enabled", onCost.elapsed.Round(time.Microsecond).String())
	if onCost.elapsed > 0 {
		writeTelemetry(out, s.name+".speedup", fmt.Sprintf("%.2fx", float64(offCost.elapsed)/float64(onCost.elapsed)))
	}
	// Allocation evidence, from runtime.MemStats around one drained run. Reported for
	// BOTH arms because a time win paid for with allocations is not a win, and because
	// the seek is expected to leave allocations UNCHANGED — it removes CPU work, not
	// row construction, which #2206 had already removed.
	writeTelemetry(out, s.name+".allocs.disabled", strconv.FormatUint(offCost.mallocs, 10))
	writeTelemetry(out, s.name+".allocs.enabled", strconv.FormatUint(onCost.mallocs, 10))
	writeTelemetry(out, s.name+".bytes.disabled", strconv.FormatUint(offCost.bytes, 10))
	writeTelemetry(out, s.name+".bytes.enabled", strconv.FormatUint(onCost.bytes, 10))
	return nil
}

// plandiffCost is one measured run: the answer size plus the wall-clock and
// allocation cost of producing it.
type plandiffCost struct {
	rows    int64
	elapsed time.Duration
	mallocs uint64
	bytes   uint64
}

// measurePlandiffRun times q on eng and, separately, samples its allocation cost.
//
// The two are measured in DIFFERENT runs on purpose: reading runtime.MemStats forces
// a stop-the-world, so folding it into the timed run would inflate the very number the
// scenario reports. The timing is the median of plandiffTimingRuns; the allocation
// sample is one further run bracketed by MemStats reads after a GC, so the delta is
// this query's own and not a leftover from the previous scenario.
func measurePlandiffRun(ctx context.Context, eng *cypher.Engine, q string) (plandiffCost, error) {
	elapsed, err := medianRunDuration(ctx, eng, q)
	if err != nil {
		return plandiffCost{}, err
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rows, err := drainRowCount(ctx, eng, q)
	if err != nil {
		return plandiffCost{}, err
	}
	runtime.ReadMemStats(&after)
	return plandiffCost{
		rows:    rows,
		elapsed: elapsed,
		mallocs: after.Mallocs - before.Mallocs,
		bytes:   after.TotalAlloc - before.TotalAlloc,
	}, nil
}

// drainRowCount runs q, counts its rows and closes the result.
func drainRowCount(ctx context.Context, eng *cypher.Engine, q string) (int64, error) {
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		return 0, err
	}
	var n int64
	for res.Next() {
		n++
	}
	if e := res.Err(); e != nil {
		_ = res.Close()
		return 0, e
	}
	if err := res.Close(); err != nil {
		return 0, err
	}
	return n, nil
}

// writePlandiffDegreeProfile reports the FOLLOWS out-degree distribution of the
// vertices the traversal scenarios walk.
//
// This is the indicator that explains WHY the bound-destination seek engages and how
// much it can win: the seek replaces a Θ(d) walk of a vertex's neighbour run with an
// O(log d + r) probe, so its benefit is a function of the degrees actually traversed.
// A reader seeing only a speedup cannot tell whether it came from the access path or
// from the fixture; the degree profile is what makes that attributable.
func writePlandiffDegreeProfile(ctx context.Context, out io.Writer, eng *cypher.Engine) error {
	// Each degree metric must GROUP BY the source before aggregating. Omitting the
	// `WITH u, count(r)` grouping aggregates GLOBALLY and reports the total edge count
	// as if it were a maximum degree — which the first version of this profile did,
	// printing 806 for a graph whose busiest :User followed far fewer accounts.
	for _, m := range []struct{ key, query string }{
		{"degree.user_follows.max",
			"MATCH (u:User)-[r:FOLLOWS]->() WITH u, count(r) AS d RETURN max(d) AS c"},
		{"degree.user_follows.min",
			"MATCH (u:User)-[r:FOLLOWS]->() WITH u, count(r) AS d RETURN min(d) AS c"},
		{"degree.user_follows.mean",
			"MATCH (u:User)-[r:FOLLOWS]->() WITH u, count(r) AS d RETURN toInteger(avg(d)) AS c"},
		{"degree.follows.edges", "MATCH ()-[r:FOLLOWS]->() RETURN count(r) AS c"},
		{"degree.follows.sources", "MATCH (u)-[:FOLLOWS]->() RETURN count(DISTINCT u) AS c"},
		{"degree.firehose.outdegree", "MATCH (:Firehose)-[r:FOLLOWS]->() RETURN count(r) AS c"},
		{"degree.verified.indegree", "MATCH ()-[r:FOLLOWS]->(:Verified) RETURN count(r) AS c"},
	} {
		v, err := scalarCount(ctx, eng, m.query)
		if err != nil {
			return fmt.Errorf("degree profile %s: %w", m.key, err)
		}
		writeTelemetry(out, m.key, strconv.FormatInt(v, 10))
	}
	return nil
}

// medianRunDuration runs q on eng plandiffTimingRuns times, fully draining and
// closing each Result, and returns the median wall-clock duration.
func medianRunDuration(ctx context.Context, eng *cypher.Engine, q string) (time.Duration, error) {
	durs := make([]time.Duration, 0, plandiffTimingRuns)
	for i := 0; i < plandiffTimingRuns; i++ {
		start := time.Now()
		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			return 0, err
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if e := res.Err(); e != nil {
			_ = res.Close()
			return 0, e
		}
		if err := res.Close(); err != nil {
			return 0, err
		}
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	return durs[len(durs)/2], nil
}

// scalarCount runs a single-row count(*) query and returns the count as int64.
func scalarCount(ctx context.Context, eng *cypher.Engine, q string) (int64, error) {
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var got int64
	for res.Next() {
		rec := res.Record()
		n, perr := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(rec["c"])), 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("parse count %q: %w", fmt.Sprint(rec["c"]), perr)
		}
		got = n
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	return got, nil
}

// ensurePlandiffContent seeds the synthetic content layer once, in a single
// transaction, and returns whether it seeded now (false when the layer was
// already present — the sentinel dp_post_0 node exists). It requires the base
// fixture to be present so the graph is a faithful superset of the shipped
// example.
func ensurePlandiffContent(ctx context.Context, store *txn.Store[string, float64], cfg plandiffConfig) (bool, error) {
	g := store.Graph()
	if g.HasNodeLabel(plandiffPostPrefix+"0", labelPost) {
		// The content layer is present. The mutual-FOLLOWS and firehose layers were
		// added later (#2154), so check them SEPARATELY: a data directory seeded by an
		// earlier build has the posts but not these, and gating everything on one
		// sentinel would leave the two new scenarios measuring an empty graph.
		return false, ensurePlandiffTraversalShapes(ctx, store, cfg)
	}
	if !hasAnyUser(g) {
		return false, fmt.Errorf("base fixture missing; run `seed -d %s` first", cfg.dir)
	}

	users := plandiffBaseUsers * cfg.scale
	posts := plandiffBasePosts * cfg.scale
	comments := plandiffBaseComments * cfg.scale

	tx := store.Begin()
	if err := seedPlandiffContent(ctx, tx, users, posts, comments); err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, ensurePlandiffTraversalShapes(ctx, store, cfg)
}

// seedPlandiffContent adds the synthetic content layer to tx: `users` :User
// nodes, `posts` :Post nodes, and `comments` :Comment nodes each with a single
// :ON edge to a distinct post (comments < posts, so most posts stay uncommented —
// the long-tail engagement shape the anchor swap exploits). Only the nodes and
// edges the two reordering scenarios read are created, keeping the count-store
// cardinalities clean and the seed fast; the traversal shapes are added separately
// by [seedPlandiffTraversalShapes].
func seedPlandiffContent(ctx context.Context, tx *txn.Tx[string, float64], users, posts, comments int) error {
	for i := 0; i < users; i++ {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := addPlandiffNode(tx, plandiffUserPrefix+strconv.Itoa(i), labelUser); err != nil {
			return err
		}
	}
	for i := 0; i < posts; i++ {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := addPlandiffNode(tx, plandiffPostPrefix+strconv.Itoa(i), labelPost); err != nil {
			return err
		}
	}
	for i := 0; i < comments; i++ {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		key := plandiffCommentPrefix + strconv.Itoa(i)
		if err := addPlandiffNode(tx, key, labelComment); err != nil {
			return err
		}
		// Each synthetic comment is ON a distinct synthetic post (i < comments <
		// posts), matching the fixture's (:Comment)-[:ON]->(:Post) direction.
		if err := addLabelledEdge(tx, key, plandiffPostPrefix+strconv.Itoa(i), relOn); err != nil {
			return err
		}
	}
	return nil
}

// ensurePlandiffTraversalShapes adds, once, the two shapes the traversal scenarios
// need on top of the content layer: MUTUAL :FOLLOWS pairs among the synthetic users
// (so the bound-destination pattern matches at all) and the firehose/:Verified skew
// (so the symmetric anchor swap has a genuinely cheaper reverse anchor).
//
// It carries its own sentinel — the firehose node — because these layers postdate the
// content layer, so a data directory seeded by an earlier build already satisfies the
// content sentinel and would otherwise never receive them.
func ensurePlandiffTraversalShapes(ctx context.Context, store *txn.Store[string, float64], cfg plandiffConfig) error {
	if store.Graph().HasNodeLabel(plandiffFirehoseKey, labelFirehose) {
		return nil // already present
	}
	tx := store.Begin()
	if err := seedPlandiffTraversalShapes(ctx, tx, cfg); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit traversal shapes: %w", err)
	}
	return nil
}

// seedPlandiffTraversalShapes writes both shapes into tx.
//
// MUTUAL FOLLOWS. Synthetic users 0..mutual-1 are paired (2i, 2i+1) with an edge in
// BOTH directions, plus a forward-only chain edge so the triangle pattern has a third
// hop to close. Pairing rather than a ring is deliberate: it makes the expected mutual
// count exactly the pair count in each direction, so the scenario's own output is
// checkable by hand rather than only against itself.
//
// FIREHOSE. One :Firehose account FOLLOWS every synthetic user — a large
// out-adjacency — plus exactly ONE of the small :Verified population. So
// `(f:Firehose)-[:FOLLOWS]->(v:Verified)` anchored as written walks the whole
// out-adjacency to find one edge, while anchored on :Verified it walks one in-edge.
func seedPlandiffTraversalShapes(ctx context.Context, tx *txn.Tx[string, float64], cfg plandiffConfig) error {
	users := plandiffBaseUsers * cfg.scale
	mutual := plandiffBaseMutual * cfg.scale
	if mutual > users {
		mutual = users - (users % 2)
	}
	verified := plandiffBaseVerified * cfg.scale

	// A RING FAN-OUT over every synthetic user: user i follows i+1 … i+plandiffFanout
	// (modulo the population). Two properties matter.
	//
	// First, EVERY user has a followee list, so the closing hop of a bound-destination
	// pattern has a neighbour run worth seeking in. An earlier version fanned out only
	// the mutually-following users, which left the far endpoints at out-degree ZERO —
	// the closing hop then walked an empty range and the scenario measured 1.06x,
	// reporting a real operator as if it did nothing.
	//
	// Second, the ring creates NO accidental mutual pair, which keeps the mutual count
	// attributable to the back-edges below: i→j means j−i ∈ [1, fanout] (mod n), and
	// j→i would need i−j in the same range, impossible while 2·fanout < n.
	for i := 0; i < users; i++ {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for k := 1; k <= plandiffFanout; k++ {
			t := plandiffUserPrefix + strconv.Itoa((i+k)%users)
			if err := addLabelledEdge(tx, plandiffUserPrefix+strconv.Itoa(i), t, relFollows); err != nil {
				return err
			}
		}
	}

	// MUTUAL pairs. The ring already gives 2i → 2i+1, so only the BACK edge is added;
	// that makes the pair mutual without creating a parallel edge. The mutual edge
	// therefore sits inside a run of plandiffFanout+1 slots rather than at its head, so
	// a seek that wrongly returned only a run's first slot would not pass by luck.
	//
	// TRIANGLES. 2i → 2i+1 and 2i+1 → 2i+2 are both ring edges, so adding 2i+2 → 2i
	// closes a 3-cycle. The ring alone closes none (a cycle needs the hop offsets to sum
	// to 0 modulo n, and 3·fanout < n), so every triangle the scenario counts comes from
	// these edges.
	for i := 0; i < mutual; i++ {
		if err := tx.SetNodeLabel(plandiffUserPrefix+strconv.Itoa(i), labelCore); err != nil {
			return fmt.Errorf("label core %d: %w", i, err)
		}
		// :Seed is the triangle scenario's anchor — deliberately tiny, because a triangle's
		// cost is cubic in the fan-out.
		if i < plandiffTriangleSeeds {
			if err := tx.SetNodeLabel(plandiffUserPrefix+strconv.Itoa(i), labelSeed); err != nil {
				return fmt.Errorf("label seed %d: %w", i, err)
			}
		}
	}
	for i := 0; i+2 < mutual; i += 2 {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := addLabelledEdge(tx, plandiffUserPrefix+strconv.Itoa(i+1),
			plandiffUserPrefix+strconv.Itoa(i), relFollows); err != nil {
			return err
		}
		if err := addLabelledEdge(tx, plandiffUserPrefix+strconv.Itoa(i+2),
			plandiffUserPrefix+strconv.Itoa(i), relFollows); err != nil {
			return err
		}
	}

	// FIREHOSE. One :Firehose account follows every synthetic user — a large
	// out-adjacency — plus exactly ONE of the small :Verified population. So
	// `(f:Firehose)-[:FOLLOWS]->(v:Verified)` anchored as written walks the whole
	// out-adjacency to find one edge, while anchored on :Verified it walks one in-edge.
	// That asymmetry is what makes the reverse anchor genuinely cheaper.
	for i := 0; i < verified; i++ {
		key := plandiffVerifiedPrefix + strconv.Itoa(i)
		if err := addPlandiffNode(tx, key, labelVerified); err != nil {
			return err
		}
	}
	if err := addPlandiffNode(tx, plandiffFirehoseKey, labelFirehose); err != nil {
		return err
	}
	for i := 0; i < users; i++ {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := addLabelledEdge(tx, plandiffFirehoseKey, plandiffUserPrefix+strconv.Itoa(i), relFollows); err != nil {
			return err
		}
	}
	return addLabelledEdge(tx, plandiffFirehoseKey, plandiffVerifiedPrefix+"0", relFollows)
}

// addPlandiffNode adds one labelled synthetic node carrying an "id" property (the
// only property the scenarios project), keeping the synthetic layer minimal.
func addPlandiffNode(tx *txn.Tx[string, float64], key, label string) error {
	if err := tx.AddNode(key); err != nil {
		return fmt.Errorf("add node %s: %w", key, err)
	}
	if err := tx.SetNodeLabel(key, label); err != nil {
		return fmt.Errorf("label %s: %w", key, err)
	}
	if err := tx.SetNodeProperty(key, "id", lpg.StringValue(key)); err != nil {
		return fmt.Errorf("id %s: %w", key, err)
	}
	return nil
}
