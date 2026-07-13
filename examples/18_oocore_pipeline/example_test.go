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

	// Bulk-loader equivalence oracle. The store/bulk high-throughput path
	// ingested the SAME seeded edge stream into a second csrfile; that file must
	// be byte-identical to the CSV -> CSR csrfile, and the bulk-built CSR must
	// have the same vertex and edge set. A regression in any of these three is a
	// correctness bug in store/bulk, not in the example.
	if got := facts["bulk.order"]; got != facts["csr.order"] {
		t.Errorf("bulk.order (%d) != csr.order (%d): the bulk loader built a different vertex set",
			got, facts["csr.order"])
	}
	if got := facts["bulk.size"]; got != facts["csr.size"] {
		t.Errorf("bulk.size (%d) != csr.size (%d): the bulk loader built a different edge set",
			got, facts["csr.size"])
	}
	if got := facts["bulk.identical"]; got != 1 {
		t.Errorf("bulk.identical = %d, want 1: the bulk csrfile is NOT byte-identical to the "+
			"CSV-path csrfile (module bug in store/bulk)", got)
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

// TestBulkParallelIdentical drives the pipeline with the bulk loader's parallel
// build engaged. That path builds the CSR through a different construction (a
// counting sort straight from the buffered edge stream, not the per-edge
// adjacency the CSV path uses), so its byte-identity to the CSV-path csrfile is
// an independent oracle: it must hold regardless of which build the loader
// chose. The deterministic facts are otherwise unchanged, because the parallel
// build is byte-for-byte identical to the sequential one by contract.
func TestBulkParallelIdentical(t *testing.T) {
	cfg := defaultConfig()
	cfg.bulkParallel = true

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())

	if got := facts["bulk.identical"]; got != 1 {
		t.Errorf("bulk.identical = %d, want 1 under the parallel build "+
			"(module bug in store/bulk's parallel path)", got)
	}
	if facts["bulk.order"] != facts["csr.order"] {
		t.Errorf("bulk.order (%d) != csr.order (%d) under the parallel build",
			facts["bulk.order"], facts["csr.order"])
	}
	if facts["bulk.size"] != facts["csr.size"] {
		t.Errorf("bulk.size (%d) != csr.size (%d) under the parallel build",
			facts["bulk.size"], facts["csr.size"])
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
