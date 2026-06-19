package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a small version of the default specification: the same
// model and code path, sized to build and probe well under the
// short-layer 60 s package budget. The topology is a pure function of
// (vertices, edges), so every fact asserted below is stable across
// machines and across seeds.
func testConfig() config {
	return config{
		vertices: 2_000,
		edges:    12_000,
		target:   50,
		probes:   200,
		seed:     1,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — the node and edge counts, the one fixed concrete route
// (its distance, hop count, and reconstructed path), the reachable
// count, and that the probe workload ran. The volatile telemetry lines
// (prefixed "# ") are ignored, as required by the examples standard for
// non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Topology counts are exact for a fixed (vertices, edges): the
	// generator chains each vertex to its successor plus shortcuts, so at
	// this scale there are no destination collisions and the edge count
	// is exactly vertices * (edges/vertices).
	if got, want := facts["graph.nodes"], int64(cfg.vertices); got != want {
		t.Errorf("graph.nodes = %d, want %d", got, want)
	}
	if got, want := facts["graph.edges"], int64(cfg.edges); got != want {
		t.Errorf("graph.edges = %d, want %d", got, want)
	}

	// The fixed concrete route 0 -> 50 is fully deterministic for this
	// topology: distance 171 over 8 hops along the chain/shortcut mix.
	if got, want := facts["route.src"], int64(srcNode); got != want {
		t.Errorf("route.src = %d, want %d", got, want)
	}
	if got, want := facts["route.dst"], int64(cfg.target); got != want {
		t.Errorf("route.dst = %d, want %d", got, want)
	}
	if got, want := facts["route.distance"], int64(171); got != want {
		t.Errorf("route.distance = %d, want %d", got, want)
	}
	if got, want := facts["route.hops"], int64(8); got != want {
		t.Errorf("route.hops = %d, want %d", got, want)
	}
	if got, want := factLine(out, "route.path"), "0 -> 1 -> 2 -> 17 -> 32 -> 47 -> 48 -> 49 -> 50"; got != want {
		t.Errorf("route.path = %q, want %q", got, want)
	}

	// The chain edges keep every vertex reachable from node 0.
	if got, want := facts["reach.from_src"], int64(cfg.vertices); got != want {
		t.Errorf("reach.from_src = %d, want %d", got, want)
	}

	// The probe workload ran the requested number of queries; with a
	// fully reachable graph every probed pair is feasible.
	if got, want := facts["probe.count"], int64(cfg.probes); got != want {
		t.Errorf("probe.count = %d, want %d", got, want)
	}
	if got, want := facts["probe.feasible"], int64(cfg.probes); got != want {
		t.Errorf("probe.feasible = %d, want %d (graph is fully reachable)", got, want)
	}

	// The test must never assert on telemetry: confirm the "# " lines are
	// present (so the evidence is actually emitted) without pinning them.
	for _, key := range []string{
		"# probe.throughput=", "# probe.latency.p50=", "# probe.latency.p95=",
		"# probe.latency.p99=", "# mem.heap_alloc=",
	} {
		if !strings.Contains(out, key) {
			t.Errorf("expected telemetry line containing %q, output:\n%s", key, out)
		}
	}
}

// TestSeedIndependentFacts confirms the central design property: the
// DIMACS 9 topology takes no seed, so changing -seed changes only the
// probe workload, never the deterministic facts. Two runs with different
// seeds must produce byte-identical fact lines, except for the config.seed
// line that legitimately echoes the seed.
func TestSeedIndependentFacts(t *testing.T) {
	cfgA := testConfig()
	cfgA.seed = 1
	cfgB := testConfig()
	cfgB.seed = 999

	var a, b bytes.Buffer
	if err := run(context.Background(), &a, cfgA); err != nil {
		t.Fatalf("run seed=1: %v", err)
	}
	if err := run(context.Background(), &b, cfgB); err != nil {
		t.Fatalf("run seed=999: %v", err)
	}

	// The config.seed fact line legitimately differs (it echoes the
	// seed); every other fact line must be identical. Compare the fact
	// lines with the seed line removed.
	fa := dropLine(factLines(a.String()), "config.seed=")
	fb := dropLine(factLines(b.String()), "config.seed=")
	if fa != fb {
		t.Errorf("facts differ between seeds (topology must be seed-independent):\n--- seed=1 ---\n%s\n--- seed=999 ---\n%s", fa, fb)
	}
}

// TestDeterministic confirms two runs with the same config produce
// identical deterministic fact lines.
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

// TestRunRejectsBadConfig confirms the boundary validation rejects an
// impossible configuration before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	cases := []config{
		{vertices: 0, edges: 10, target: 0, probes: 1, seed: 1},      // no vertices
		{vertices: 100, edges: 200, target: 100, probes: 1, seed: 1}, // target >= vertices
		{vertices: 100, edges: 200, target: 0, probes: 1, seed: 1},   // target == source
		{vertices: 100, edges: -1, target: 5, probes: 1, seed: 1},    // negative edges
		{vertices: 100, edges: 200, target: 5, probes: -1, seed: 1},  // negative probes
	}
	for i, bad := range cases {
		if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
			t.Errorf("case %d: run accepted an invalid config %+v; want error", i, bad)
		}
	}
}

// TestRunHonoursCancellation confirms run aborts promptly when the
// context is already cancelled, returning the context error.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A larger build so the cancellation lands during generation rather
	// than after it: dimacs9.Synthetic polls ctx per source vertex.
	cfg := testConfig()
	cfg.vertices = 200_000
	cfg.edges = 1_000_000
	err := run(ctx, &bytes.Buffer{}, cfg)
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// BenchmarkRun runs the full example at the small default config so
// `go test -bench` produces the evidence mechanically alongside the
// human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	for i := 0; i < b.N; i++ {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them
// as a map. Lines whose value is not an integer (e.g. route.path) are
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

// factLine returns the value of the bare fact line with the given key,
// or "" if absent. Used for non-integer facts such as route.path.
func factLine(out, key string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return v
		}
	}
	return ""
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
	return strings.Join(keep, "\n")
}

// dropLine removes any line from s that starts with prefix, joining the
// rest back together. Used to ignore the legitimately seed-dependent
// config.seed fact line when comparing across seeds.
func dropLine(s, prefix string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}
