package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
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
)

// plandiff synthetic key prefixes; distinct from the fixture's natural keys
// (alice..erin, p1.., c1..) and the scale population's "u_" prefix, so the
// content layer is recognisable and its presence is a cheap idempotency sentinel.
const (
	plandiffUserPrefix    = "dp_user_"
	plandiffPostPrefix    = "dp_post_"
	plandiffCommentPrefix = "dp_cmt_"
)

// plandiff scenario queries. Neither carries an order-establishing operator, so
// the reorder is order-safe; the RETURN shapes are scalar (anchor) and an
// order-blind count (disjoint), both of which SuppressReorder passes.
const (
	plandiffAnchorQuery   = "MATCH (p:Post)<-[:ON]-(c:Comment) RETURN c.id AS comment, p.id AS post"
	plandiffDisjointQuery = "MATCH (u:User), (c:Comment) RETURN count(*) AS pairs"
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
}

// plandiffTimingRuns is how many times each scenario query is timed on each
// engine; the reported wall-clock is the median, which is robust to a single
// scheduling outlier while keeping the one-shot command fast.
const plandiffTimingRuns = 5

// runPlandiffScenario renders the EXPLAIN plan-diff (enabled vs disabled), the
// exact work contrast, and the median wall-clock for one scenario.
func runPlandiffScenario(ctx context.Context, out io.Writer, s *plandiffScenario) error {
	onPlan, err := s.onEng.Explain(s.query, nil)
	if err != nil {
		return fmt.Errorf("explain (enabled): %w", err)
	}
	offPlan, err := s.offEng.Explain(s.query, nil)
	if err != nil {
		return fmt.Errorf("explain (disabled): %w", err)
	}

	fmt.Fprintf(out, "\n## scenario: %s\n", s.name)
	fmt.Fprintf(out, "query: %s\n", s.query)
	fmt.Fprintf(out, "--- EXPLAIN (reordering DISABLED) ---\n%s", offPlan)
	fmt.Fprintf(out, "--- EXPLAIN (reordering ENABLED) ---\n%s", onPlan)

	reordered := onPlan != offPlan
	writeTelemetry(out, s.name+".reordered", strconv.FormatBool(reordered))
	writeTelemetry(out, s.name+"."+s.workLabel+".disabled", strconv.FormatInt(s.workWritten, 10))
	writeTelemetry(out, s.name+"."+s.workLabel+".enabled", strconv.FormatInt(s.workEnabled, 10))
	if s.workEnabled > 0 {
		writeTelemetry(out, s.name+"."+s.workLabel+".ratio",
			fmt.Sprintf("%.1fx", float64(s.workWritten)/float64(s.workEnabled)))
	}

	offDur, err := medianRunDuration(ctx, s.offEng, s.query)
	if err != nil {
		return fmt.Errorf("time (disabled): %w", err)
	}
	onDur, err := medianRunDuration(ctx, s.onEng, s.query)
	if err != nil {
		return fmt.Errorf("time (enabled): %w", err)
	}
	writeTelemetry(out, s.name+".elapsed.disabled", offDur.Round(time.Microsecond).String())
	writeTelemetry(out, s.name+".elapsed.enabled", onDur.Round(time.Microsecond).String())
	if onDur > 0 {
		writeTelemetry(out, s.name+".speedup", fmt.Sprintf("%.2fx", float64(offDur)/float64(onDur)))
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
		return false, nil // already seeded
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
	return true, nil
}

// seedPlandiffContent adds the synthetic content layer to tx: `users` :User
// nodes, `posts` :Post nodes, and `comments` :Comment nodes each with a single
// :ON edge to a distinct post (comments < posts, so most posts stay uncommented —
// the long-tail engagement shape the anchor swap exploits). Only the nodes and
// edges the two scenarios read are created, keeping the count-store cardinalities
// clean and the seed fast.
func seedPlandiffContent(ctx context.Context, tx *txn.Tx[string, float64], users, posts, comments int) error {
	for i := 0; i < users; i++ {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		key := plandiffUserPrefix + strconv.Itoa(i)
		if err := addPlandiffNode(tx, key, labelUser); err != nil {
			return err
		}
	}
	for i := 0; i < posts; i++ {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		key := plandiffPostPrefix + strconv.Itoa(i)
		if err := addPlandiffNode(tx, key, labelPost); err != nil {
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
