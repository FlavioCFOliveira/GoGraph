package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/metrics"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestMain wraps the package tests in a goroutine-leak check: the example
// serves reg.Handler() over an httptest server and scrapes it over HTTP, so
// a leaked server or client goroutine is a defect. Run under -race.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// testConfig is a small, deterministic version of the default: the same
// code path at a scale that builds and scrapes well under the short-layer
// 60 s package budget. The shape is fixed for the seed, so every invariant
// asserted below is stable across machines.
func testConfig() config {
	return config{
		services: 200,
		callsMin: 2,
		callsMax: 6,
		seed:     1,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants: the domain counts, the write delta and round-trip
// conservation law, and — the point of the example — that every expected
// instrumented metric NAME is present in the scraped exposition. The
// volatile telemetry lines (prefixed "# ") are never asserted.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Domain facts.
	if got := facts["nodes.services"]; got != strconv.Itoa(cfg.services) {
		t.Errorf("nodes.services = %s, want %d", got, cfg.services)
	}
	edges := mustInt(t, facts, "edges.calls")
	if lo, hi := int64(cfg.services*cfg.callsMin), int64(cfg.services*cfg.callsMax); edges < lo || edges > hi {
		t.Errorf("edges.calls = %d, want within [%d,%d]", edges, lo, hi)
	}

	// The write transaction adds exactly one SERVICE node.
	if got := mustInt(t, facts, "cypher.services_before"); got != int64(cfg.services) {
		t.Errorf("cypher.services_before = %d, want %d", got, cfg.services)
	}
	if got := mustInt(t, facts, "cypher.services_after"); got != int64(cfg.services)+1 {
		t.Errorf("cypher.services_after = %d, want %d", got, cfg.services+1)
	}
	if got := mustInt(t, facts, "cypher.write_delta"); got != 1 {
		t.Errorf("cypher.write_delta = %d, want 1", got)
	}

	// Dijkstra reaches at least the source and at most every service.
	if r := mustInt(t, facts, "dijkstra.src_reached"); r < 1 || r > int64(cfg.services) {
		t.Errorf("dijkstra.src_reached = %d, want within [1,%d]", r, cfg.services)
	}

	// The CSV round-trip conserves the edge count.
	if got := mustInt(t, facts, "csv.roundtrip.edges_match"); got != 1 {
		t.Errorf("csv.roundtrip.edges_match = %d, want 1", got)
	}

	// Every expected instrumented metric must be present in the exposition.
	for _, m := range expectedMetrics {
		key := "metric.present." + m.name
		if got := facts[key]; got != "true" {
			t.Errorf("%s = %q, want %q (metric absent from the scraped exposition)", key, got, "true")
		}
	}
	if got := mustInt(t, facts, "metric.present.count"); got != int64(len(expectedMetrics)) {
		t.Errorf("metric.present.count = %d, want %d", got, len(expectedMetrics))
	}
	if got := mustInt(t, facts, "metric.expected.count"); got != int64(len(expectedMetrics)) {
		t.Errorf("metric.expected.count = %d, want %d", got, len(expectedMetrics))
	}

	// No metric may be present under an unexpected TYPE: a "# WARN
	// metric.kind.mismatch" line means the exposition and docs/metrics.md
	// disagree on counter vs histogram.
	if strings.Contains(out, "metric.kind.mismatch") {
		t.Errorf("exposition TYPE diverges from docs/metrics.md:\n%s", out)
	}
}

// TestDeterministic confirms the deterministic fact lines are reproducible:
// two runs with the same config produce identical facts (telemetry aside).
func TestDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := run(context.Background(), &a, testConfig()); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(context.Background(), &b, testConfig()); err != nil {
		t.Fatalf("run b: %v", err)
	}
	if fa, fb := factLines(a.String()), factLines(b.String()); fa != fb {
		t.Errorf("deterministic fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s", fa, fb)
	}
}

// TestRunRejectsBadConfig confirms boundary validation: a callsMax that
// exceeds the number of other services is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{services: 3, callsMin: 1, callsMax: 5, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted callsMax > services-1; want error")
	}
}

// TestRunHonoursCancellation confirms run aborts promptly on a cancelled
// context and returns the context error rather than a partial report.
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

// recordingBackend counts every metric event routed to it. The restore
// test uses it as a sentinel installed before run.
type recordingBackend struct {
	n int
}

func (r *recordingBackend) IncCounter(string, uint64)            { r.n++ }
func (r *recordingBackend) ObserveLatency(string, time.Duration) { r.n++ }

// TestRestoresBackend confirms run installs its own registry rather than
// routing to a pre-installed backend, and restores the no-op default on
// exit so no example-owned global state is left behind. A sentinel backend
// installed before run must see zero events (run used its own registry
// throughout the workload), and the backend must remain swappable afterward.
func TestRestoresBackend(t *testing.T) {
	sentinel := &recordingBackend{}
	metrics.SetBackend(sentinel)
	defer metrics.SetBackend(nil)

	if err := run(context.Background(), &bytes.Buffer{}, testConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sentinel.n != 0 {
		t.Errorf("sentinel backend saw %d events; run routed metrics to the pre-installed backend instead of its own registry", sentinel.n)
	}

	// The global backend is swappable after run: install a fresh sentinel,
	// drive one instrumented call, and confirm it is observed — proving run
	// left the surface in a clean, usable state.
	after := &recordingBackend{}
	metrics.SetBackend(after)
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	if _, err := search.Dijkstra(c, src); err != nil {
		t.Fatalf("post-run dijkstra: %v", err)
	}
	if after.n == 0 {
		t.Error("metrics surface is unusable after run: a fresh backend saw no events")
	}
}

// parseFacts extracts the deterministic "key=value" lines (everything not
// prefixed with "# ") into a map, keeping the raw string value so both
// integer and boolean facts can be asserted.
func parseFacts(t *testing.T, out string) map[string]string {
	t.Helper()
	facts := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed fact line: %q", line)
		}
		facts[k] = v
	}
	return facts
}

// mustInt returns the named fact parsed as an int64, failing the test when
// it is absent or non-integer.
func mustInt(t *testing.T, facts map[string]string, key string) int64 {
	t.Helper()
	v, ok := facts[key]
	if !ok {
		t.Fatalf("fact %q missing from output", key)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("fact %q = %q is not an integer: %v", key, v, err)
	}
	return n
}

// factLines returns only the deterministic lines of out (dropping the
// volatile "# " telemetry), joined for equality comparison.
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
