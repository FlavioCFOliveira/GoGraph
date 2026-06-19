package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a small version of the round-trip: same model and code
// path as the default, small enough to build and serialise well under
// the short-layer 60 s package budget. The shape is deterministic for
// the fixed seed, so the invariants asserted below are stable across
// machines.
func testConfig() config {
	return config{
		nodes:        500,
		edgesPerNode: 5,
		maxWeight:    50,
		seed:         42,
		sampleLines:  0,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — node and edge counts, the parse-back counts, and the
// weight round-trip checksum. The volatile telemetry lines (prefixed
// "# ") are ignored, as required by the examples standard for
// non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())

	// The graph has exactly cfg.nodes pages. Page i links to
	// min(edgesPerNode, i) distinct earlier pages, so the edge total is a
	// closed form independent of the RNG draw (the seed decides which
	// targets are chosen, not how many).
	if got := facts["nodes"]; got != int64(cfg.nodes) {
		t.Errorf("nodes = %d, want %d", got, cfg.nodes)
	}
	wantEdges := expectedEdges(cfg)
	if got := facts["edges"]; got != wantEdges {
		t.Errorf("edges = %d, want %d", got, wantEdges)
	}

	// The parse-back must agree with the source graph edge-for-edge: the
	// generated graph is simple and directed, so the GraphML reader
	// re-materialises every node and edge.
	if got := facts["graphml.parsed_nodes"]; got != int64(cfg.nodes) {
		t.Errorf("graphml.parsed_nodes = %d, want %d", got, cfg.nodes)
	}
	if got := facts["graphml.parsed_edges"]; got != facts["edges"] {
		t.Errorf("graphml.parsed_edges = %d, want %d (= edges)", got, facts["edges"])
	}
	if got := facts["dot.written_edges"]; got != facts["edges"] {
		t.Errorf("dot.written_edges = %d, want %d (= edges)", got, facts["edges"])
	}

	// The weight round-trip invariant: the sum of edge weights survives
	// the serialise/parse trip exactly, and roundtrip.ok asserts all three
	// conservation laws (nodes, edges, weight sum) at once.
	if got := facts["roundtrip.weight_sum"]; got != facts["weight.sum"] {
		t.Errorf("roundtrip.weight_sum = %d, want %d (= weight.sum)", got, facts["weight.sum"])
	}
	if got := facts["roundtrip.ok"]; got != 1 {
		t.Errorf("roundtrip.ok = %d, want 1 (round-trip must be exact)", got)
	}

	// The weight sum is positive: every edge carries a weight in
	// [1, maxWeight], so the sum lands in the corresponding band.
	sum := facts["weight.sum"]
	if lo, hi := wantEdges, wantEdges*int64(cfg.maxWeight); sum < lo || sum > hi {
		t.Errorf("weight.sum = %d, want within [%d,%d]", sum, lo, hi)
	}
}

// expectedEdges returns the exact edge count the generator produces for
// cfg: page i links to min(edgesPerNode, i) distinct earlier pages, so
// the total is the sum of that over every page. Only the choice of
// targets depends on the seed, not the count, so this is an exact
// deterministic check.
func expectedEdges(cfg config) int64 {
	var total int64
	for i := range cfg.nodes {
		total += int64(min(cfg.edgesPerNode, i))
	}
	return total
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

// TestRunRejectsBadConfig confirms the boundary validation: an impossible
// configuration is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	cases := []config{
		{nodes: 0, edgesPerNode: 1, maxWeight: 1, seed: 1},                  // nodes <= 0
		{nodes: 5, edgesPerNode: -1, maxWeight: 1, seed: 1},                 // edges < 0
		{nodes: 5, edgesPerNode: 1, maxWeight: 0, seed: 1},                  // max-weight < 1
		{nodes: 5, edgesPerNode: 1, maxWeight: 1, seed: 1, sampleLines: -1}, // sample < 0
	}
	for i, bad := range cases {
		if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
			t.Errorf("case %d: run accepted an invalid config %+v; want error", i, bad)
		}
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

// BenchmarkRun measures the whole generate → serialise → parse →
// re-serialise round-trip at the test scale, so `go test -bench` produces
// the interchange evidence mechanically alongside the human-readable
// report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	for b.Loop() {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as
// a map. Lines whose value is not an integer are skipped.
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
