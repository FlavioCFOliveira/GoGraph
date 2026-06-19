package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a small version of the default instance: same model and
// code path, sized to solve well under the short-layer 60 s package
// budget. The shape is deterministic for the fixed seed, so the
// invariants asserted below are byte-stable across machines. Like the
// default it sits in the binding regime (the maximum feasible matching
// falls strictly short of the worker ceiling).
func testConfig() config {
	return config{
		workers:     60,
		tasks:       80,
		skills:      5,
		noiseFrac:   0.3,
		feasiblePct: 0.15,
		seed:        42,
	}
}

// TestRun drives run into a buffer and asserts the deterministic
// invariants: the exact counts, the Hungarian optimal total cost, and the
// Hopcroft-Karp maximum matching size — the two scalars that are unique
// for a fixed seed even though the specific assignment is not. The
// volatile telemetry lines (prefixed "# ") are ignored, as required by
// the examples standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())

	// Golden invariants, discovered by running the config once and pinned
	// thereafter. The optimal cost is a sum of integer cost cells (exact
	// in float64), and the matching size is the unique maximum
	// cardinality, so both are safe to assert with ==.
	exact := []struct {
		col  string
		want int64
	}{
		{"feasible.pairs", 723},
		{"hungarian.optimal_cost", 25679},
		{"matching.size", 52},
		{"matching.ceiling", int64(cfg.workers)},
		{"feasibility.binding", 1},
	}
	for _, c := range exact {
		if got := facts[c.col]; got != c.want {
			t.Errorf("%s = %d, want %d", c.col, got, c.want)
		}
	}

	// Structural sanity, independent of the golden constants: the feasible
	// edge set is non-empty and below the full matrix, and the matching is
	// non-trivial yet strictly below the ceiling (the binding regime the
	// example is built to show). This catches a future generator change
	// that stays self-consistent but drifts out of the intended regime.
	ceiling := int64(cfg.workers)
	if cfg.tasks < cfg.workers {
		ceiling = int64(cfg.tasks)
	}
	if fp := facts["feasible.pairs"]; fp <= 0 || fp >= int64(cfg.workers*cfg.tasks) {
		t.Errorf("feasible.pairs = %d, want within (0,%d)", fp, cfg.workers*cfg.tasks)
	}
	if sz := facts["matching.size"]; sz <= 0 || sz >= ceiling {
		t.Errorf("matching.size = %d, want within (0,%d) (binding regime)", sz, ceiling)
	}
	if bind := facts["feasibility.binding"]; bind != 1 {
		t.Errorf("feasibility.binding = %d, want 1 (matching below ceiling)", bind)
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// more workers than tasks is rejected before any work (Hungarian requires
// at least as many columns as rows).
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{workers: 100, tasks: 80, skills: 4, noiseFrac: 0.3, feasiblePct: 0.2, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with workers > tasks; want error")
	}
}

// TestRunHonoursCancellation confirms run aborts promptly when the context
// is already cancelled, returning the context error.
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

// TestDeterministic confirms the instance shape is reproducible: two runs
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
// prefixed with "# ") whose value parses as an integer, returning them as
// a map. Lines whose value is not an integer (e.g. the feasible_pct line)
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

// BenchmarkRun exercises the full generate-solve-match pipeline at the
// small test scale so `go test -bench` produces the timing evidence
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
