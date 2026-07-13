package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain doubles the package as a goroutine-leak check: the example drives
// the parallel APSP variants (FloydWarshallParallel, JohnsonAPSPParallel),
// which spawn worker goroutines internally, so a leak there would surface here.
// Run under -race to exercise the concurrent pivot kernel.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// testConfig is the small deterministic default this example ships, restated
// here so the test is self-contained. Same model and code path, sized so
// Floyd-Warshall's O(V^3) runs in microseconds — well under the short-layer
// 60 s package budget. The shape is deterministic for the fixed seed, so the
// invariants asserted below are stable across machines.
func testConfig() config {
	return defaultConfig()
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — connectivity, the three-way and parallel APSP agreement, the
// classical distance-metric relationships (r <= D <= 2r, a non-degenerate D/r
// ratio, a rich eccentricity gradient), and the bounds on the sample town's
// eccentricity. The volatile telemetry lines (prefixed "# ") are ignored, as
// required by the examples standard for output that mixes facts and telemetry.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())

	// The MST makes the network connected by construction, so every town is
	// live in the CSR snapshot and every pair is reachable.
	if got := facts["graph.nodes"]; got != int64(cfg.nodes) {
		t.Errorf("graph.nodes = %d, want %d (the graph is connected by construction)", got, cfg.nodes)
	}

	// Headline correctness oracle: the three APSP algorithms must produce
	// bit-identical distance matrices on this positive-integer graph, and each
	// parallel variant must match its serial counterpart. Any 0 here is a
	// module bug the example has surfaced.
	for _, key := range []string{"apsp_three_way_agree", "floyd_parallel_agree", "johnson_parallel_agree"} {
		if got := facts[key]; got != 1 {
			t.Errorf("%s = %d, want 1 (APSP algorithms must agree — a mismatch is a module bug)", key, got)
		}
	}

	radius := facts["metric.radius"]
	diameter := facts["metric.diameter"]
	// Buckley & Harary: in any connected graph r <= D <= 2r.
	if radius <= 0 {
		t.Errorf("metric.radius = %d, want > 0", radius)
	}
	if diameter < radius {
		t.Errorf("metric.diameter (%d) < metric.radius (%d); violates D >= r", diameter, radius)
	}
	if diameter > 2*radius {
		t.Errorf("metric.diameter (%d) > 2*radius (%d); violates D <= 2r", diameter, 2*radius)
	}
	// Non-degenerate gradient: the elongated geometric network keeps D/r
	// comfortably above 1.4 (5*D >= 7*r), so it is neither a star (D/r -> 2 with
	// everything within a couple of hops) nor a trivial blob (D/r -> 1).
	if 5*diameter < 7*radius {
		t.Errorf("D/r = %d/%d < 1.4; eccentricity gradient is too flat to be meaningful", diameter, radius)
	}

	// The eccentricity gradient must be rich: many distinct eccentricity values,
	// not a coarse handful. A geometric proximity graph yields a high-cardinality
	// gradient; require at least nodes/10 distinct values.
	if de := facts["metric.distinct_eccentricities"]; de < int64(cfg.nodes)/10 {
		t.Errorf("metric.distinct_eccentricities = %d, want >= %d (gradient must not be degenerate)", de, cfg.nodes/10)
	}

	// The sample town's eccentricity must lie within [radius, diameter], since
	// the radius is the minimum eccentricity and the diameter the maximum.
	if ecc := facts["metric.sample_eccentricity"]; ecc < radius || ecc > diameter {
		t.Errorf("metric.sample_eccentricity = %d, want within [%d, %d]", ecc, radius, diameter)
	}

	// The centre and periphery towns are real town values in [0, nodes).
	for _, key := range []string{"metric.center_town", "metric.periphery_town"} {
		if v := facts[key]; v < 0 || v >= int64(cfg.nodes) {
			t.Errorf("%s = %d, want in [0, %d)", key, v, cfg.nodes)
		}
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for more
// nearest neighbours than there are other towns cannot produce a valid k-NN
// step, so it is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := defaultConfig()
	bad.knn = bad.nodes // knn must be < nodes
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with knn >= nodes; want error")
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

// TestDeterministic confirms the network shape is reproducible: two runs with
// the same config produce identical deterministic fact lines.
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

// TestAcrossSeeds confirms the structural guarantees are robust, not a seed-1
// accident: for several seeds the network stays connected, the three-way APSP
// agreement holds, and the D/r ratio stays in the meaningful range. This is the
// invariant the geometric topology was chosen to provide for every seed.
func TestAcrossSeeds(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 7, 42, 99, 1000} {
		cfg := defaultConfig()
		cfg.seed = seed
		var buf bytes.Buffer
		if err := run(context.Background(), &buf, cfg); err != nil {
			t.Fatalf("seed %d: run: %v", seed, err)
		}
		facts := parseFacts(t, buf.String())

		if got := facts["graph.nodes"]; got != int64(cfg.nodes) {
			t.Errorf("seed %d: graph.nodes = %d, want %d (must be connected)", seed, got, cfg.nodes)
		}
		if got := facts["apsp_three_way_agree"]; got != 1 {
			t.Errorf("seed %d: apsp_three_way_agree = %d, want 1", seed, got)
		}
		radius, diameter := facts["metric.radius"], facts["metric.diameter"]
		if 5*diameter < 7*radius {
			t.Errorf("seed %d: D/r = %d/%d < 1.4; gradient too flat", seed, diameter, radius)
		}
		if diameter > 2*radius {
			t.Errorf("seed %d: diameter (%d) > 2*radius (%d); violates D <= 2r", seed, diameter, radius)
		}
	}
}

// BenchmarkRun runs the full default pipeline (generate, freeze, three APSP
// algorithms plus the two parallel variants, cross-check, metrics) so
// go test -bench produces the per-run cost mechanically alongside the report.
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
// prefixed with "# ") whose value parses as an integer, returning them as a
// map. Lines whose value is not an integer (e.g. the config.region line) are
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
