package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"gograph/graph/lpg"
	"gograph/store/txn"
)

// cmdSeed populates the data directory with the deterministic
// social-network fixture: 5 users, 8 follows, 3 posts (each with an
// author edge), 5 comments (with ON, REPLY_OF and authorship edges)
// and 7 likes. All timestamps in the fixture are hard-coded so the
// snapshot byte stream is reproducible across machines.
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
// The seeded field is false on idempotent re-runs.
func cmdSeed(args []string) error {
	dir, _, err := parseDataDir("seed", args)
	if err != nil {
		return err
	}
	return runSeed(context.Background(), dir, os.Stdout)
}

// runSeed opens the data directory, applies the fixture through the
// transactional API, and writes the success reply to out. The body is
// factored out from cmdSeed so the round-trip test in T9 can drive it
// with a captured *bytes.Buffer instead of os.Stdout.
//
// Writes go through the direct txn.Tx API rather than Engine.RunInTx:
// the current Cypher engine cannot express multi-edge CREATE patterns
// nor MATCH+CREATE-relationship statements through the WAL-backed
// planner, so the simplest and most idiomatic path for bulk loads is
// the same one used by examples/04_persistence. The query subcommand
// remains the demonstration of Cypher writes for individual statements.
func runSeed(ctx context.Context, dir string, out io.Writer) (retErr error) {
	o, err := openStore(ctx, dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := o.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("seed: close store: %w", cerr)
		}
	}()

	seeded, err := seedFixture(o.store)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	return writeJSONObject(out, map[string]any{
		"status": "ok",
		"seeded": seeded,
	})
}

// seedFixture inserts the canonical social-network fixture through one
// transaction on store. It returns true when the fixture was applied
// and false when the graph already contained at least one User node
// (idempotent re-invocation).
//
// The function is exported (package-internal) so the round-trip test
// in T9 can construct a store directly and reuse the same fixture.
func seedFixture(store *txn.Store[string, float64]) (bool, error) {
	g := store.Graph()
	if hasAnyUser(g) {
		return false, nil
	}

	tx := store.Begin()
	if err := applyFixture(tx); err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
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
