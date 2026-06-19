package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a small version of the build graph: the same model and
// code path as the default, sized to build and analyse well under the
// short-layer 60 s package budget. The shape is deterministic for the
// fixed seed, so the invariants asserted below are stable across
// machines.
func testConfig() config {
	return config{
		modules:     800,
		layers:      8,
		depsMin:     1,
		depsMax:     4,
		pyramidBase: 1.6,
		seed:        42,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — the module count, the topological-order validity
// invariant, the longest-chain bound, and that the injected cycle is
// detected as exactly one strongly connected component. The volatile
// telemetry lines (prefixed "# ") are ignored, as required by the
// examples standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Module count is exact and independent of the RNG.
	if got := facts["nodes.modules"]; got != int64(cfg.modules) {
		t.Errorf("nodes.modules = %d, want %d", got, cfg.modules)
	}
	if got := facts["dag.layers"]; got != int64(cfg.layers) {
		t.Errorf("dag.layers = %d, want %d", got, cfg.layers)
	}

	// Every non-leaf module requests >= depsMin dependencies, so the edge
	// total is comfortably positive at this scale.
	if edges := facts["edges.dependencies"]; edges <= 0 {
		t.Errorf("edges.dependencies = %d, want > 0", edges)
	}

	// The headline correctness invariant: TopologicalSort returned a valid
	// linear extension of the DAG (every edge points forward in the order).
	if got := factBool(t, out, "topo.order_valid"); !got {
		t.Errorf("topo.order_valid = false, want true (topological order must be valid)")
	}

	// TopologicalSort omits NodeIDs with no edges, so the number of ordered
	// modules is at most the module count.
	if ordered := facts["topo.modules_ordered"]; ordered <= 0 || ordered > int64(cfg.modules) {
		t.Errorf("topo.modules_ordered = %d, want within (0,%d]", ordered, cfg.modules)
	}

	// The planted chain spans every layer, so the longest dependency chain
	// is at least cfg.layers modules and cannot exceed the module count.
	chain := facts["dag.longest_chain"]
	if chain < int64(cfg.layers) || chain > int64(cfg.modules) {
		t.Errorf("dag.longest_chain = %d, want within [%d,%d]", chain, cfg.layers, cfg.modules)
	}

	// Cycle detection: the injected back-edge closes exactly one cycle, so
	// Tarjan finds exactly one non-trivial SCC, it contains the back-edge
	// endpoints, and its size is at least the planted chain length.
	if got := factBool(t, out, "cycle.detected"); !got {
		t.Errorf("cycle.detected = false, want true (the injected cycle must be found)")
	}
	if got := facts["cycle.scc_count"]; got != 1 {
		t.Errorf("cycle.scc_count = %d, want 1 (exactly one non-trivial SCC)", got)
	}
	if size := facts["cycle.scc_size"]; size < int64(cfg.layers) || size > int64(cfg.modules) {
		t.Errorf("cycle.scc_size = %d, want within [%d,%d]", size, cfg.layers, cfg.modules)
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// fewer modules than layers (so some layer would be empty) is rejected
// before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{modules: 3, layers: 8, depsMin: 1, depsMax: 4, pyramidBase: 1.6, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with modules < layers; want error")
	}
}

// TestRunHonoursCancellation confirms generation aborts promptly when the
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

// BenchmarkRun measures the end-to-end cost of generating, ordering, and
// cycle-checking the test-sized build graph. Run it with
// `go test -bench=. ./examples/12_build_dependency` for mechanical
// evidence alongside the human-readable report.
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
// a map. Lines whose value is not an integer (e.g. the config range line
// and the boolean invariant lines) are skipped.
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

// factBool returns the boolean value of the bare fact line key=true|false,
// failing the test if the line is absent or not a boolean.
func factBool(t *testing.T, out, key string) bool {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k != key {
			continue
		}
		got, err := strconv.ParseBool(v)
		if err != nil {
			t.Fatalf("fact %q is %q, not a boolean", key, v)
		}
		return got
	}
	t.Fatalf("fact %q not found in output", key)
	return false
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
