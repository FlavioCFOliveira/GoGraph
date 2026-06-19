package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a small, deterministic version of the backbone: the same
// model and code path as the default, sized to build and analyse well
// under the short-layer 60 s package budget. The shape is deterministic
// for the fixed seed, so the invariants asserted below are stable across
// machines.
func testConfig() config {
	return config{
		clusters:    4,
		clusterSize: 6,
		chords:      6,
		seed:        42,
	}
}

// TestRun drives run into a buffer and asserts the deterministic
// invariants the transit-stub topology guarantees for every seed and
// scale: exactly one off-spine bridge with its two articulation-point
// endpoints, a max-flow equal to the narrowest interior boundary
// (spineLinksNarrow links of capSpine), a two-link min-cut, and the
// max-flow == min-cut conservation law. The volatile telemetry lines
// (prefixed "# ") are ignored, as the examples standard requires for
// non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, testConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())
	cfg := testConfig()

	// Site count is exact: K spine clusters plus one stub cluster.
	wantSites := int64((cfg.clusters + 1) * cfg.clusterSize)
	if got := facts["nodes.sites"]; got != wantSites {
		t.Errorf("nodes.sites = %d, want %d", got, wantSites)
	}

	// Structural single points of failure: the single off-spine bridge and
	// its two endpoints. The dense, Hamiltonian-cycle clusters contribute
	// no internal articulation point or bridge, so the count is exact.
	if got := facts["spof.articulation_points"]; got != 2 {
		t.Errorf("spof.articulation_points = %d, want 2", got)
	}
	if got := facts["spof.bridges"]; got != 1 {
		t.Errorf("spof.bridges = %d, want 1", got)
	}

	// Throughput: the global source-to-sink min-cut is the narrowest
	// interior spine boundary — spineLinksNarrow links of capSpine each —
	// strictly cheaper than either terminal's incident capacity and not the
	// off-spine bridge.
	wantFlow := int64(spineLinksNarrow * capSpine)
	if got := facts["flow.max_value"]; got != wantFlow {
		t.Errorf("flow.max_value = %d, want %d", got, wantFlow)
	}
	if got := facts["flow.min_cut_size"]; got != int64(spineLinksNarrow) {
		t.Errorf("flow.min_cut_size = %d, want %d", got, spineLinksNarrow)
	}
	if got := facts["flow.min_cut_capacity"]; got != wantFlow {
		t.Errorf("flow.min_cut_capacity = %d, want %d", got, wantFlow)
	}

	// The max-flow min-cut theorem: the saturated-link capacity equals the
	// maximum flow. run already errors if this is violated; assert the fact
	// line records it as true.
	if got := facts["flow.maxflow_eq_mincut"]; got != 1 {
		t.Errorf("flow.maxflow_eq_mincut = %d (bool), want true", got)
	}
}

// TestScaleInvariants confirms the topology guarantees hold across seeds
// and scales, not just the pinned default — the deterministic acceptance
// gate the topology is designed to pass for every seed.
func TestScaleInvariants(t *testing.T) {
	cases := []config{
		{clusters: 2, clusterSize: 4, chords: 0, seed: 1},
		{clusters: 3, clusterSize: 5, chords: 5, seed: 7},
		{clusters: 6, clusterSize: 10, chords: 10, seed: 99},
		{clusters: 5, clusterSize: 8, chords: 8, seed: 12345},
	}
	for _, cfg := range cases {
		t.Run(strconv.FormatInt(cfg.seed, 10), func(t *testing.T) {
			var buf bytes.Buffer
			if err := run(context.Background(), &buf, cfg); err != nil {
				t.Fatalf("run: %v", err)
			}
			facts := parseFacts(t, buf.String())

			if got := facts["spof.articulation_points"]; got < 1 {
				t.Errorf("spof.articulation_points = %d, want >= 1", got)
			}
			if got := facts["spof.bridges"]; got < 1 {
				t.Errorf("spof.bridges = %d, want >= 1", got)
			}
			// The min-cut is the narrowest interior boundary regardless of
			// scale: spineLinksNarrow saturated links of capSpine.
			if got := facts["flow.max_value"]; got != int64(spineLinksNarrow*capSpine) {
				t.Errorf("flow.max_value = %d, want %d", got, spineLinksNarrow*capSpine)
			}
			if got := facts["flow.min_cut_size"]; got != int64(spineLinksNarrow) {
				t.Errorf("flow.min_cut_size = %d, want %d", got, spineLinksNarrow)
			}
			// The conservation law must hold for every seed.
			if facts["flow.min_cut_capacity"] != facts["flow.max_value"] {
				t.Errorf("min_cut_capacity %d != max_value %d (max-flow min-cut violated)",
					facts["flow.min_cut_capacity"], facts["flow.max_value"])
			}
			if got := facts["flow.maxflow_eq_mincut"]; got != 1 {
				t.Errorf("flow.maxflow_eq_mincut = %d (bool), want true", got)
			}
		})
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: a spine with
// fewer than two clusters (no interior boundary) is rejected before any
// work, and so is a cluster too small to be 2-vertex-connected, and a
// negative chord count.
func TestRunRejectsBadConfig(t *testing.T) {
	cases := []config{
		{clusters: 1, clusterSize: 8, chords: 4, seed: 1},  // no interior boundary
		{clusters: 4, clusterSize: 2, chords: 0, seed: 1},  // cluster too small
		{clusters: 4, clusterSize: 6, chords: -1, seed: 1}, // negative chords
	}
	for _, bad := range cases {
		if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
			t.Errorf("run accepted invalid config %+v; want error", bad)
		}
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

// TestDeterministic confirms the topology is reproducible: two runs with
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

// BenchmarkRun exercises the full generate-analyse pipeline at a moderate
// scale so `go test -bench` produces the structural-analysis and max-flow
// evidence mechanically alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := config{clusters: 20, clusterSize: 16, chords: 16, seed: 1}
	b.ReportAllocs()
	for b.Loop() {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=value" lines (everything not
// prefixed with "# "). Integer values are stored directly; the boolean
// "maxflow_eq_mincut" is normalised to 1 (true) or 0 (false) so a single
// integer map covers every asserted fact. Lines whose value is neither an
// integer nor a boolean are skipped.
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
		switch v {
		case "true":
			facts[k] = 1
		case "false":
			facts[k] = 0
		default:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				facts[k] = n
			}
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
