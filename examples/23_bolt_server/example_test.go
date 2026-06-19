package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain wraps every test in this package with go.uber.org/goleak so the
// Bolt-server round-trip doubles as a goroutine-leak check: it fails the
// package if the server's per-connection goroutines, the serve goroutine, or
// the neo4j-go-driver's connection-pool goroutines outlive a clean teardown.
// Run the package under -race to also exercise the concurrent session load
// for data races:
//
//	go test -race ./examples/23_bolt_server/...
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// testConfig is a laptop-sized version of the default: the same model and code
// path, small enough to seed, serve, and query well under the short-layer 60 s
// package budget. The shape is deterministic for the fixed seed, so the
// invariants asserted below are stable across machines.
func testConfig() config {
	return config{
		nodes:    500,
		knowsMin: 4,
		knowsMax: 6,
		queries:  400,
		sessions: 4,
		seed:     42,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — the seeded node count, the fixed query's result over that data,
// and that every fired query succeeded. The volatile telemetry lines (prefixed
// "# ", e.g. throughput and latency percentiles) are never asserted, as
// required by the examples standard for non-deterministic network output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// The seeded node count is exact and independent of the RNG.
	if got := facts["nodes.person"]; got != int64(cfg.nodes) {
		t.Errorf("nodes.person = %d, want %d", got, cfg.nodes)
	}

	// KNOWS out-degree is in [knowsMin, knowsMax] per person, so the total
	// lands in the corresponding band.
	knows := facts["edges.knows"]
	if lo, hi := int64(cfg.nodes*cfg.knowsMin), int64(cfg.nodes*cfg.knowsMax); knows < lo || knows > hi {
		t.Errorf("edges.knows = %d, want within [%d,%d]", knows, lo, hi)
	}

	// The fixed query MATCH (n:Person) RETURN count(n) must, over the seeded
	// data, return exactly the node count — read back over the Bolt wire.
	if got := facts["q.count_person"]; got != int64(cfg.nodes) {
		t.Errorf("q.count_person = %d, want %d", got, cfg.nodes)
	}

	// Every fired query must have succeeded and returned the expected count.
	if got := facts["queries.ok"]; got != int64(cfg.queries) {
		t.Errorf("queries.ok = %d, want %d", got, cfg.queries)
	}

	// Sanity: the clean-shutdown telemetry line is present (it is "# "-prefixed
	// so parseFacts drops it; check the raw output instead).
	if !strings.Contains(out, "# server shut down cleanly") {
		t.Errorf("missing clean-shutdown line in output:\n%s", out)
	}
}

// TestDeterministic confirms the dataset shape and the queried result are
// reproducible: two runs with the same config produce identical deterministic
// fact lines. The volatile "# " telemetry (throughput, latency, heap) is
// dropped before comparison.
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

// TestRunRejectsBadConfig confirms the boundary validation: asking for more
// acquaintances than there are other people is rejected before any work
// (before the server ever starts).
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{nodes: 10, knowsMin: 0, knowsMax: 20, queries: 5, sessions: 1, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with knowsMax > nodes-1; want error")
	}
}

// TestRunHonoursCancellation confirms the run aborts when the context is
// already cancelled, returning the context error and leaving no goroutine
// behind (the TestMain goleak guard enforces the latter).
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

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as a
// map. Lines whose value is not an integer (e.g. the config range line) are
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
