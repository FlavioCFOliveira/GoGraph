package main

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a laptop-sized version of the full specification: same
// model and code path, small enough to build and query well under the
// short-layer 60 s package budget. The shape is deterministic for the
// fixed seed, so the invariants asserted below are stable across
// machines.
func testConfig() config {
	return config{
		users:      2000,
		articles:   200,
		friendsMin: 5,
		friendsMax: 8,
		likesMax:   10,
		seed:       42,
		relTypes:   true,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — node counts, edge-count bounds, and that every query in
// the battery returned a self-consistent answer. The volatile telemetry
// lines (prefixed "# ") are ignored, as required by the examples
// standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, testConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)
	cfg := testConfig()

	// Node counts are exact and independent of the RNG.
	if got := facts["nodes.users"]; got != int64(cfg.users) {
		t.Errorf("nodes.users = %d, want %d", got, cfg.users)
	}
	if got := facts["nodes.articles"]; got != int64(cfg.articles) {
		t.Errorf("nodes.articles = %d, want %d", got, cfg.articles)
	}

	// FRIEND out-degree is in [friendsMin, friendsMax] per user, so the
	// total lands in the corresponding band.
	friend := facts["edges.friend"]
	if lo, hi := int64(cfg.users*cfg.friendsMin), int64(cfg.users*cfg.friendsMax); friend < lo || friend > hi {
		t.Errorf("edges.friend = %d, want within [%d,%d]", friend, lo, hi)
	}

	// LIKE out-degree is in [0, likesMax] per user.
	like := facts["edges.like"]
	if hi := int64(cfg.users * cfg.likesMax); like < 0 || like > hi {
		t.Errorf("edges.like = %d, want within [0,%d]", like, hi)
	}

	// The query battery must agree with the materialised graph: the
	// label-scan counts equal the node counts, and the relationship
	// counts equal the edge totals reported during the build.
	checks := []struct {
		col  string
		want int64
	}{
		{"q.count_users", int64(cfg.users)},
		{"q.count_articles", int64(cfg.articles)},
		{"q.count_friend", friend},
		{"q.count_like", like},
	}
	for _, c := range checks {
		if got := facts[c.col]; got != c.want {
			t.Errorf("%s = %d, want %d", c.col, got, c.want)
		}
	}

	// Friend-of-friend reach is non-negative and cannot exceed the
	// number of users; with the chosen degrees it is comfortably > 0.
	if fof := facts["q.fof_reach"]; fof <= 0 || fof > int64(cfg.users) {
		t.Errorf("q.fof_reach = %d, want within (0,%d]", fof, cfg.users)
	}

	// The trending query is LIMIT 10, so it returns exactly 10 rows
	// whenever at least 10 distinct articles were liked (true at this
	// scale).
	if rows := facts["q.top_articles.rows"]; rows != 10 {
		t.Errorf("q.top_articles.rows = %d, want 10", rows)
	}
}

// TestRunCompact confirms the implicit-type mode (relTypes=false) is
// functionally equivalent: it stores no per-edge labels, yet the
// endpoint-inferred relationship queries return the same counts as the
// explicit-type mode for the same seed.
func TestRunCompact(t *testing.T) {
	explicit := testConfig()
	compact := testConfig()
	compact.relTypes = false

	var eb, cb bytes.Buffer
	if err := run(context.Background(), &eb, explicit); err != nil {
		t.Fatalf("run explicit: %v", err)
	}
	if err := run(context.Background(), &cb, compact); err != nil {
		t.Fatalf("run compact: %v", err)
	}
	ef := parseFacts(t, eb.String())
	cf := parseFacts(t, cb.String())

	// Same dataset shape and same query answers, regardless of how the
	// relationship kind is encoded.
	for _, k := range []string{
		"nodes.users", "nodes.articles", "edges.friend", "edges.like",
		"q.count_users", "q.count_articles", "q.count_friend", "q.count_like",
		"q.fof_reach", "q.top_articles.rows",
	} {
		if ef[k] != cf[k] {
			t.Errorf("%s: explicit=%d compact=%d (must be equal)", k, ef[k], cf[k])
		}
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// more friends than there are other users is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{users: 10, articles: 5, friendsMin: 0, friendsMax: 20, likesMax: 0, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with friendsMax > users-1; want error")
	}
}

// TestRunHonoursCancellation confirms the build aborts promptly when the
// context is already cancelled, returning the context error.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, &bytes.Buffer{}, testConfig())
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// TestDeterministic confirms the dataset shape is reproducible: two runs
// with the same config produce identical deterministic fact lines.
func TestDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := run(context.Background(), &a, testConfig()); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(context.Background(), &b, testConfig()); err != nil {
		t.Fatalf("run b: %v", err)
	}
	if factLines(a.String()) != factLines(b.String()) {
		t.Errorf("deterministic fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s",
			factLines(a.String()), factLines(b.String()))
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them
// as a map. Lines whose value is not an integer (e.g. the config range
// lines) are skipped.
func parseFacts(t *testing.T, out string) map[string]int64 {
	t.Helper()
	facts := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed fact line: %q", line)
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			facts[k] = n
		}
	}
	return facts
}

// factLines returns only the deterministic lines of out (dropping the
// volatile "# " telemetry), joined back into a single string for
// equality comparison.
func factLines(out string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		keep = append(keep, line)
	}
	return fmt.Sprint(strings.Join(keep, "\n"))
}
