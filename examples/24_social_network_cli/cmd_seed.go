package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// seedConfig bundles the data directory with the opt-in scale and
// evidence knobs of the seed subcommand. The zero scaleConfig and
// evidence == false reproduce the example's original behaviour exactly
// (canonical fixture, single JSON fact line), so the golden tests keep
// passing byte-for-byte unless the operator asks for more.
type seedConfig struct {
	dir      string
	scale    scaleConfig
	evidence bool
}

// cmdSeed populates the data directory with the deterministic
// social-network fixture: 5 users, 8 follows, 3 posts (each with an
// author edge), 5 comments (with ON, REPLY_OF and authorship edges)
// and 7 likes. All timestamps in the fixture are hard-coded so the
// snapshot byte stream is reproducible across machines.
//
// The seed scales beyond the fixture with three opt-in flags, off by
// default so the deterministic output is unchanged:
//
//	-users N     append N extra seeded :User nodes (0 = fixture only)
//	-friends K   FOLLOWS out-degree per synthetic user (default 8)
//	-seed S      RNG seed that fixes the synthetic data shape (default 1)
//	-evidence    print "# " telemetry (seed throughput, live heap)
//
// The operation is idempotent: if at least one User already exists
// (e.g., a previous seed completed against the same data directory),
// cmdSeed returns the same JSON reply without applying the fixture a
// second time.
//
// On success cmdSeed writes a single JSON object to stdout:
//
//	{"seeded":<bool>,"status":"ok"}
//
// The seeded field is false on idempotent re-runs. With -evidence the
// JSON fact line is followed by "# "-prefixed telemetry lines.
func cmdSeed(args []string) error {
	cfg, err := parseSeedArgs(args)
	if err != nil {
		return err
	}
	return runSeedWithConfig(context.Background(), cfg, os.Stdout)
}

// parseSeedArgs parses the seed subcommand's flags into a seedConfig,
// validating the scale knobs at the boundary. A flag-parse failure or a
// missing -d is mapped to a *usageError (exit code 2); an impossible
// scale configuration is a runtime error (exit code 1).
func parseSeedArgs(args []string) (seedConfig, error) {
	cfg := seedConfig{scale: defaultScaleConfig()}
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.dir, "d", "", "data directory (required)")
	fs.IntVar(&cfg.scale.users, "users", cfg.scale.users, "number of extra seeded :User nodes to append (0 = canonical fixture only)")
	fs.IntVar(&cfg.scale.friends, "friends", cfg.scale.friends, "FOLLOWS out-degree per synthetic user")
	fs.Int64Var(&cfg.scale.seed, "seed", cfg.scale.seed, "RNG seed (fixes the synthetic data shape)")
	fs.BoolVar(&cfg.evidence, "evidence", cfg.evidence, "print \"# \" telemetry (seed throughput, live heap)")
	if perr := fs.Parse(args); perr != nil {
		return seedConfig{}, newUsageError("seed: flag parse: %v", perr)
	}
	if cfg.dir == "" {
		return seedConfig{}, newUsageError("seed: missing required flag -d <dir>")
	}
	if err := cfg.scale.validate(); err != nil {
		return seedConfig{}, err
	}
	return cfg, nil
}

// runSeed opens the data directory, applies the canonical fixture through
// the transactional API, and writes the success reply to out. It is kept
// at its original signature — no scale, no evidence — so the existing
// round-trip and unit tests drive it with a captured *bytes.Buffer and see
// byte-for-byte the same output. It delegates to runSeedWithConfig with the
// default (off) scale and evidence knobs.
func runSeed(ctx context.Context, dir string, out io.Writer) error {
	return runSeedWithConfig(ctx, seedConfig{dir: dir, scale: defaultScaleConfig()}, out)
}

// runSeedWithConfig is the full seed entry point: it applies the canonical
// fixture and, when cfg.scale is enabled, layers the seeded synthetic
// population on top, all in a single transaction. With cfg.evidence set it
// follows the JSON fact line with "# "-prefixed telemetry.
//
// Writes go through the direct txn.Tx API rather than Engine.RunInTx:
// the current Cypher engine cannot express multi-edge CREATE patterns
// nor MATCH+CREATE-relationship statements through the WAL-backed
// planner, so the simplest and most idiomatic path for bulk loads is
// the same one used by examples/04_persistence. The query subcommand
// remains the demonstration of Cypher writes for individual statements.
func runSeedWithConfig(ctx context.Context, cfg seedConfig, out io.Writer) (retErr error) {
	o, err := openStore(ctx, cfg.dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := o.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("seed: close store: %w", cerr)
		}
	}()

	var base runtime.MemStats
	if cfg.evidence {
		base = readMem()
	}

	seeded, scaled, err := seedFixtureScaled(ctx, o.store, cfg.scale)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if err := writeJSONObject(out, map[string]any{
		"status": "ok",
		"seeded": seeded,
	}); err != nil {
		return err
	}
	if cfg.evidence {
		writeSeedTelemetry(out, base, scaled)
	}
	return nil
}

// writeSeedTelemetry emits the volatile seed evidence as "# " lines after
// the deterministic JSON fact line: synthetic-population throughput
// (nodes/s, edges/s), the realised synthetic counts, and the live Go heap
// after the build. The counts are reported as telemetry too because they
// depend on the opt-in scale knobs rather than the pinned fixture, so they
// must never enter the golden output. base is the heap snapshot taken
// before the build so the heap line can be read either absolute or as
// growth.
func writeSeedTelemetry(out io.Writer, base runtime.MemStats, scaled scaleStats) {
	after := readMem()
	writeTelemetry(out, "scale.users", fmt.Sprintf("%d", scaled.users))
	writeTelemetry(out, "scale.follows", fmt.Sprintf("%d", scaled.follows))
	writeTelemetry(out, "seed.elapsed", scaled.elapsed.Round(time.Microsecond).String())
	writeTelemetry(out, "seed.node_rate", fmt.Sprintf("%.0f nodes/s", rate(scaled.users, scaled.elapsed)))
	writeTelemetry(out, "seed.edge_rate", fmt.Sprintf("%.0f edges/s", rate(scaled.follows, scaled.elapsed)))
	writeTelemetry(out, "mem.heap_alloc", humanBytes(after.HeapAlloc))
	writeTelemetry(out, "mem.heap_growth", humanBytes(after.HeapAlloc-base.HeapAlloc))
}

// seedFixture inserts the canonical social-network fixture through one
// transaction on store. It returns true when the fixture was applied
// and false when the graph already contained at least one User node
// (idempotent re-invocation).
//
// The function is exported (package-internal) so the round-trip test
// in T9 can construct a store directly and reuse the same fixture. It is
// the canonical-fixture-only path; seedFixtureScaled is the superset that
// also layers the opt-in synthetic population.
func seedFixture(store *txn.Store[string, float64]) (bool, error) {
	seeded, _, err := seedFixtureScaled(context.Background(), store, scaleConfig{})
	return seeded, err
}

// seedFixtureScaled inserts the canonical fixture and, when scale.enabled(),
// the seeded synthetic population on top of it — all in one transaction so
// the whole seed is atomic and durable as a unit. It returns whether the
// fixture was applied (false on an idempotent re-invocation, when at least
// one fixture User already exists) and the realised synthetic-population
// stats (zero value when scale is disabled or the seed was a no-op).
//
// The transaction commits exactly once, so the WAL records one durable seed
// regardless of how large the synthetic population is; a crash mid-build
// leaves the data directory recoverable to the pre-seed state.
func seedFixtureScaled(ctx context.Context, store *txn.Store[string, float64], scale scaleConfig) (bool, scaleStats, error) {
	g := store.Graph()
	if hasAnyUser(g) {
		return false, scaleStats{}, nil
	}

	tx := store.Begin()
	if err := applyFixture(tx); err != nil {
		_ = tx.Rollback()
		return false, scaleStats{}, err
	}
	var scaled scaleStats
	if scale.enabled() {
		var err error
		if scaled, err = appendSyntheticPopulation(ctx, tx, scale); err != nil {
			_ = tx.Rollback()
			return false, scaleStats{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, scaleStats{}, fmt.Errorf("commit: %w", err)
	}
	return true, scaled, nil
}

// hasAnyUser reports whether the in-memory graph already carries at
// least one node labelled User. Used to short-circuit re-invocations
// of seed without scanning the WAL again.
func hasAnyUser(g *lpg.Graph[string, float64]) bool {
	for _, name := range fixtureUsers {
		if g.HasNodeLabel(name.username, labelUser) {
			return true
		}
	}
	return false
}

// fixtureUser describes a single :User node in the fixture.
type fixtureUser struct {
	username    string
	displayName string
	createdAt   string
}

// fixturePost describes a single :Post node in the fixture together
// with its author.
type fixturePost struct {
	id        string
	text      string
	createdAt string
	author    string
}

// fixtureComment describes a single :Comment node in the fixture,
// including its post (ON), optional parent comment (REPLY_OF) and
// author (AUTHORED).
type fixtureComment struct {
	id        string
	text      string
	createdAt string
	author    string
	post      string
	replyTo   string // empty when the comment is a top-level reply
}

// fixtureLike describes a single :LIKED edge.
type fixtureLike struct {
	from string // user
	to   string // post id or comment id
	kind string // "Post" or "Comment", used to pick the right SetEdgeLabel sink
}

// applyFixture issues every AddNode / SetNodeLabel / SetNodeProperty /
// AddEdge / SetEdgeLabel call required by the fixture against tx. The
// caller is responsible for committing or rolling back tx.
func applyFixture(tx *txn.Tx[string, float64]) error {
	for _, u := range fixtureUsers {
		if err := addUser(tx, u); err != nil {
			return err
		}
	}
	for _, e := range fixtureFollows {
		if err := addLabelledEdge(tx, e.from, e.to, relFollows); err != nil {
			return err
		}
	}
	for _, p := range fixturePosts {
		if err := addPost(tx, p); err != nil {
			return err
		}
	}
	for _, c := range fixtureComments {
		if err := addComment(tx, c); err != nil {
			return err
		}
	}
	for _, l := range fixtureLikes {
		if err := addLabelledEdge(tx, l.from, l.to, relLiked); err != nil {
			return err
		}
	}
	return nil
}

// addUser adds a :User node with username, display_name and created_at
// to tx. Properties carry strings; timestamps are encoded as RFC 3339
// in the fixture so they survive the property codec verbatim.
func addUser(tx *txn.Tx[string, float64], u fixtureUser) error {
	if err := tx.AddNode(u.username); err != nil {
		return fmt.Errorf("user %s: add node: %w", u.username, err)
	}
	if err := tx.SetNodeLabel(u.username, labelUser); err != nil {
		return fmt.Errorf("user %s: label: %w", u.username, err)
	}
	if err := tx.SetNodeProperty(u.username, "username", lpg.StringValue(u.username)); err != nil {
		return fmt.Errorf("user %s: username: %w", u.username, err)
	}
	if err := tx.SetNodeProperty(u.username, "display_name", lpg.StringValue(u.displayName)); err != nil {
		return fmt.Errorf("user %s: display_name: %w", u.username, err)
	}
	if err := tx.SetNodeProperty(u.username, "created_at", lpg.StringValue(u.createdAt)); err != nil {
		return fmt.Errorf("user %s: created_at: %w", u.username, err)
	}
	return nil
}

// addPost adds a :Post node and the :AUTHORED edge from its author.
func addPost(tx *txn.Tx[string, float64], p fixturePost) error {
	if err := tx.AddNode(p.id); err != nil {
		return fmt.Errorf("post %s: add node: %w", p.id, err)
	}
	if err := tx.SetNodeLabel(p.id, labelPost); err != nil {
		return fmt.Errorf("post %s: label: %w", p.id, err)
	}
	if err := tx.SetNodeProperty(p.id, "id", lpg.StringValue(p.id)); err != nil {
		return fmt.Errorf("post %s: id: %w", p.id, err)
	}
	if err := tx.SetNodeProperty(p.id, "text", lpg.StringValue(p.text)); err != nil {
		return fmt.Errorf("post %s: text: %w", p.id, err)
	}
	if err := tx.SetNodeProperty(p.id, "created_at", lpg.StringValue(p.createdAt)); err != nil {
		return fmt.Errorf("post %s: created_at: %w", p.id, err)
	}
	return addLabelledEdge(tx, p.author, p.id, relAuthored)
}

// addComment adds a :Comment node and the :ON edge to its post, the
// :AUTHORED edge from its author, and (when set) the :REPLY_OF edge to
// the parent comment.
func addComment(tx *txn.Tx[string, float64], c fixtureComment) error {
	if err := tx.AddNode(c.id); err != nil {
		return fmt.Errorf("comment %s: add node: %w", c.id, err)
	}
	if err := tx.SetNodeLabel(c.id, labelComment); err != nil {
		return fmt.Errorf("comment %s: label: %w", c.id, err)
	}
	if err := tx.SetNodeProperty(c.id, "id", lpg.StringValue(c.id)); err != nil {
		return fmt.Errorf("comment %s: id: %w", c.id, err)
	}
	if err := tx.SetNodeProperty(c.id, "text", lpg.StringValue(c.text)); err != nil {
		return fmt.Errorf("comment %s: text: %w", c.id, err)
	}
	if err := tx.SetNodeProperty(c.id, "created_at", lpg.StringValue(c.createdAt)); err != nil {
		return fmt.Errorf("comment %s: created_at: %w", c.id, err)
	}
	if err := addLabelledEdge(tx, c.author, c.id, relAuthored); err != nil {
		return err
	}
	if err := addLabelledEdge(tx, c.id, c.post, relOn); err != nil {
		return err
	}
	if c.replyTo != "" {
		if err := addLabelledEdge(tx, c.id, c.replyTo, relReplyOf); err != nil {
			return err
		}
	}
	return nil
}

// addLabelledEdge inserts a directed edge from src to dst with weight 1
// and attaches the supplied relationship type as a label so the edge is
// visible to Cypher patterns like [:FOLLOWS] / [:LIKED] / [:ON].
func addLabelledEdge(tx *txn.Tx[string, float64], src, dst, label string) error {
	if err := tx.AddEdge(src, dst, 1); err != nil {
		return fmt.Errorf("edge %s -[%s]-> %s: %w", src, label, dst, err)
	}
	if err := tx.SetEdgeLabel(src, dst, label); err != nil {
		return fmt.Errorf("edge %s -[%s]-> %s: label: %w", src, label, dst, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture data — fixed values so the byte stream is reproducible.
// ─────────────────────────────────────────────────────────────────────────────

var fixtureUsers = []fixtureUser{
	{"alice", "Alice", "2026-01-01T00:00:00Z"},
	{"bob", "Bob", "2026-01-02T00:00:00Z"},
	{"carol", "Carol", "2026-01-03T00:00:00Z"},
	{"dave", "Dave", "2026-01-04T00:00:00Z"},
	{"erin", "Erin", "2026-01-05T00:00:00Z"},
}

// fixtureFollows: 8 directed edges chosen so each user follows at
// least one other and the closure is non-trivial.
var fixtureFollows = []struct{ from, to string }{
	{"alice", "bob"},
	{"alice", "carol"},
	{"bob", "carol"},
	{"bob", "dave"},
	{"carol", "dave"},
	{"carol", "erin"},
	{"dave", "erin"},
	{"erin", "alice"},
}

var fixturePosts = []fixturePost{
	{"p1", "Hello, GoGraph!", "2026-02-01T10:00:00Z", "alice"},
	{"p2", "Cypher is fun.", "2026-02-02T11:00:00Z", "bob"},
	{"p3", "ACID via WAL.", "2026-02-03T12:00:00Z", "carol"},
}

// fixtureComments: 5 comments, 3 top-level and 2 replies (c4 ⇒ c1,
// c5 ⇒ c4). REPLY_OF and ON coexist: a reply is still "on" its
// post so a single MATCH on (:Comment)-[:ON]->(:Post) sees the full
// thread.
var fixtureComments = []fixtureComment{
	{"c1", "Welcome!", "2026-03-01T09:00:00Z", "bob", "p1", ""},
	{"c2", "Same here.", "2026-03-01T10:00:00Z", "carol", "p1", ""},
	{"c3", "Nice post.", "2026-03-02T11:00:00Z", "alice", "p2", ""},
	{"c4", "Thanks!", "2026-03-01T11:00:00Z", "alice", "p1", "c1"},
	{"c5", "Agreed.", "2026-03-01T12:00:00Z", "bob", "p1", "c4"},
}

// fixtureLikes: 7 likes — 5 on posts, 2 on comments — covering all
// users at least once on the read side.
var fixtureLikes = []fixtureLike{
	{"alice", "p2", "Post"},
	{"bob", "p1", "Post"},
	{"carol", "p1", "Post"},
	{"dave", "p1", "Post"},
	{"erin", "p2", "Post"},
	{"alice", "c1", "Comment"},
	{"bob", "c2", "Comment"},
}
