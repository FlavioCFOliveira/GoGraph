package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestRun drives the full out-of-core pipeline at the small deterministic
// default and asserts only the deterministic facts — the CSV/CSR counts, the
// BFS reachable count, and the PageRank top-k node ids — all of which are
// reproducible for the fixed default seed. The volatile telemetry lines
// (prefixed "# ", and the temp-directory path, which run never prints) are
// ignored, as required by the examples standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, defaultConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)
	cfg := defaultConfig()

	// The CSV stage ingested every generated edge, the CSR snapshot has one
	// vertex per page, and its edge count matches the CSV count exactly — the
	// pipeline neither dropped nor duplicated an edge crossing the
	// CSV -> CSR -> csrfile boundaries.
	if got := facts["csr.order"]; got != int64(cfg.nodes) {
		t.Errorf("csr.order = %d, want %d", got, cfg.nodes)
	}
	if facts["csv.edges"] != facts["csr.size"] {
		t.Errorf("csv.edges (%d) != csr.size (%d): the pipeline lost or duplicated edges",
			facts["csv.edges"], facts["csr.size"])
	}

	// The exact realised edge count is deterministic for the default seed.
	// Pinning it catches any change to the generator's RNG draw order.
	if got := facts["csr.size"]; got != 17921 {
		t.Errorf("csr.size = %d, want 17921 (deterministic for the default seed)", got)
	}

	// The BFS is seeded from the captured NodeID of the portal page and
	// reaches a large but bounded fraction of the graph (~54% by the
	// navigation-link design). Both numbers are deterministic for the default
	// seed.
	if got := facts["bfs.seed_node"]; got != 1280 {
		t.Errorf("bfs.seed_node = %d, want 1280 (deterministic for the default seed)", got)
	}
	if got := facts["bfs.reachable"]; got != 2146 {
		t.Errorf("bfs.reachable = %d, want 2146 (deterministic for the default seed)", got)
	}
	// Sanity bound independent of the exact value: reachability is positive
	// and strictly below the node count (the graph is not fully reachable).
	if reach := facts["bfs.reachable"]; reach <= 0 || reach >= int64(cfg.nodes) {
		t.Errorf("bfs.reachable = %d, want within (0,%d)", reach, cfg.nodes)
	}

	// The PageRank top-k node ids separate the authorities from the bulk by a
	// wide margin, so they are stable for the default seed. The full top-10 is
	// pinned in descending-rank, ascending-id-tie-broken order.
	wantTop := []int64{3, 80, 28, 182, 105, 207, 130, 128, 32, 150}
	for i, want := range wantTop {
		col := "pagerank.top" + strconv.Itoa(i)
		if got := facts[col]; got != want {
			t.Errorf("%s = %d, want %d (deterministic for the default seed)", col, got, want)
		}
	}
}

// TestDeterministic confirms the dataset shape is reproducible: two runs with
// the same config produce identical deterministic fact lines.
func TestDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := run(context.Background(), &a, defaultConfig()); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(context.Background(), &b, defaultConfig()); err != nil {
		t.Fatalf("run b: %v", err)
	}
	if factLines(a.String()) != factLines(b.String()) {
		t.Errorf("deterministic fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s",
			factLines(a.String()), factLines(b.String()))
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for more
// out-links than there are pages is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{nodes: 4, outDegree: 8, attractive: 1, navFrac: 0.5, topK: 4, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with out-degree >= nodes; want error")
	}
}

// TestRunHonoursCancellation confirms the pipeline aborts promptly when the
// context is already cancelled, returning the context error.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, &bytes.Buffer{}, defaultConfig())
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as a
// map. Lines whose value is not an integer (e.g. the nav_frac config line) are
// skipped.
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

// factLines returns only the deterministic lines of out (dropping the volatile
// "# " telemetry), joined back into a single string for equality comparison.
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

// BenchmarkRun runs the full pipeline at the default config so go test -bench
// produces the per-stage evidence mechanically alongside the human-readable
// report. Output is discarded; the benchmark exercises the whole
// CSV -> CSR -> csrfile -> mmap -> {BFS, PageRank} path.
func BenchmarkRun(b *testing.B) {
	cfg := defaultConfig()
	for b.Loop() {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}
