package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a small version of the default specification: same model
// and code path, sized to build and query well under the short-layer 60 s
// package budget. The shape is deterministic for the fixed seed, so the
// invariants asserted below are stable across machines.
func testConfig() config {
	return config{
		users:    60,
		knowsMin: 3,
		knowsMax: 6,
		minAge:   30,
		top:      5,
		batch:    3,
		maxHops:  10,
		seed:     42,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — node count, edge-count bounds, that the read battery agrees
// with the materialised graph, that the engine's shortestPath length matches
// the independent search oracle, and that every write's effect is exactly as
// specified (node/edge deltas, MERGE idempotency, SET/REMOVE read-backs,
// DELETE / DETACH DELETE removals, and the aggregate end state). The volatile
// telemetry lines (prefixed "# ") are ignored, as required by the examples
// standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Node count is exact and independent of the RNG.
	if got := facts["nodes.users"]; got != int64(cfg.users) {
		t.Errorf("nodes.users = %d, want %d", got, cfg.users)
	}

	// KNOWS out-degree is in [knowsMin, knowsMax] per user, so the total
	// lands in the corresponding band.
	knows := facts["edges.knows"]
	if lo, hi := int64(cfg.users*cfg.knowsMin), int64(cfg.users*cfg.knowsMax); knows < lo || knows > hi {
		t.Errorf("edges.knows = %d, want within [%d,%d]", knows, lo, hi)
	}

	// The relationship-pattern count must equal the edge total reported
	// during the build.
	if got := facts["q.knows_count"]; got != knows {
		t.Errorf("q.knows_count = %d, want %d", got, knows)
	}

	// The label-scan projection is LIMIT cfg.top, and there are more than
	// cfg.top users, so it returns exactly cfg.top rows.
	if rows := facts["q.oldest_users.rows"]; rows != int64(cfg.top) {
		t.Errorf("q.oldest_users.rows = %d, want %d", rows, cfg.top)
	}

	// The anchored relationship sample matches exactly one row: the anchor
	// user's name is unique, and the build guarantees it has at least
	// knowsMin (> 0) acquaintances.
	if rows := facts["q.knows_sample.rows"]; rows != 1 {
		t.Errorf("q.knows_sample.rows = %d, want 1", rows)
	}

	// The WHERE-filter count is non-negative and cannot exceed the user
	// total; with the chosen age range and threshold it is comfortably > 0.
	if older := facts["q.older_than"]; older <= 0 || older > int64(cfg.users) {
		t.Errorf("q.older_than = %d, want within (0,%d]", older, cfg.users)
	}

	// Traversal cross-check: the engine's shortestPath length must equal the
	// independent search.BiBFS oracle (the headline correctness invariant),
	// and every allShortestPaths row must be a shortest path. sp.len and
	// asp.count are deterministic for the fixed test seed.
	if m := facts["sp.len_matches_bibfs"]; m != 1 {
		t.Errorf("sp.len_matches_bibfs = %d, want 1 (engine vs BiBFS oracle disagree — module bug)", m)
	}
	if l := facts["sp.len"]; l != 3 {
		t.Errorf("sp.len = %d, want 3 (seed-dependent shortest-path length)", l)
	}
	if m := facts["asp.all_min_length"]; m != 1 {
		t.Errorf("asp.all_min_length = %d, want 1 (allShortestPaths returned a non-shortest path)", m)
	}
	if c := facts["asp.count"]; c != 15 {
		t.Errorf("asp.count = %d, want 15 (seed-dependent shortest-path multiplicity)", c)
	}

	// Write battery. Each mutation's effect is a fixed fact for the fresh graph.
	writeWants := map[string]int64{
		"create.node_delta":         2, // multi-pattern CREATE: two new users
		"create.edge_delta":         2, // ... and two new KNOWS edges
		"unwind.created":            int64(cfg.batch),
		"merge.created":             1, // first MERGE takes ON CREATE
		"merge.created_second_pass": 0, // second MERGE creates nothing (idempotent)
		"merge.matched_second_pass": 1, // ... and takes ON MATCH
		"set.updated":               1, // SET read-back matched the written value
		"remove.cleared":            1, // REMOVE left the property null
		"delete.rel_removed":        1, // DELETE removed exactly one relationship
		"delete.nodes_kept":         1, // ... and no nodes
		"detach.node_removed":       1, // DETACH DELETE removed one node
		"detach.rel_removed":        1, // ... and its remaining edge
	}
	for k, want := range writeWants {
		if got := facts[k]; got != want {
			t.Errorf("%s = %d, want %d", k, got, want)
		}
	}

	// Aggregate end state: the net user count grows by two (CREATE) + batch
	// (UNWIND) + one (MERGE) − one (DETACH DELETE); the net KNOWS count returns
	// to the build total, since the writes add two edges and later remove two.
	if got, want := facts["users.final"], int64(cfg.users+cfg.batch+2); got != want {
		t.Errorf("users.final = %d, want %d", got, want)
	}
	if got := facts["knows.final"]; got != knows {
		t.Errorf("knows.final = %d, want %d (build total)", got, knows)
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// more acquaintances than there are other users is rejected before any
// work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{users: 5, knowsMin: 0, knowsMax: 20, minAge: 30, top: 5, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with knowsMax > users-1; want error")
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
// with the same config produce identical deterministic fact lines. Because
// each run builds a fresh graph, the CREATE delta is independent of the
// previous run.
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

// BenchmarkRun runs the whole example (build + query battery + CREATE) at
// the test scale so `go test -bench` produces the timing evidence
// mechanically alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as
// a map. Lines whose value is not an integer (e.g. the config range lines)
// are skipped.
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
// volatile "# " telemetry), joined back into a single string for equality
// comparison.
func factLines(out string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}
