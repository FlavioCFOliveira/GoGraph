package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestRun drives run into a buffer at the deterministic default config and
// asserts only the deterministic invariants — recovered counts, the
// bit-exact float64 weight verdict, and a sampled value for each typed
// property. The volatile telemetry lines (prefixed "# ") — durations,
// on-disk bytes, heap — are never asserted, and the temp directory path is
// never printed, as required by the examples standard for non-deterministic
// output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, defaultConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())

	// Recovered shape is exact for the fixed seed.
	wantInts := map[string]string{
		"config.nodes":               "256",
		"config.seed":                "1",
		"recovered.nodes":            "256",
		"recovered.edges":            "1139",
		"recovered.label_records":    "1395",
		"recovered.property_records": "4185", // 256*3 node props + 1139*3 edge props
		"recovered.schema_version":   "v2",   // non-string graph: no mapper.bin
		"weights.verified":           "1139", // every recovered edge checked
		"weights.bit_exact":          "true", // float64 weights survived bit-for-bit
	}
	for k, want := range wantInts {
		if got := facts[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// Recovered edges and verified weights must be the same population:
	// every committed edge is checked, and every check passed bit-for-bit.
	if facts["weights.verified"] != facts["recovered.edges"] {
		t.Errorf("weights.verified (%s) != recovered.edges (%s): not every edge was verified",
			facts["weights.verified"], facts["recovered.edges"])
	}

	// One sampled value per typed property — string, int64, float64 (as raw
	// IEEE-754 bits), and bool. These are fixed by the seed and prove each
	// property type round-tripped through the snapshot.
	wantSamples := map[string]string{
		"sample.node_name":          "Northmoor",          // string
		"sample.node_zone":          "8",                  // int64
		"sample.edge_distance_bits": "0x40629773ae3dcb2a", // float64 (bits)
		"sample.edge_toll":          "false",              // bool
	}
	for k, want := range wantSamples {
		if got := facts[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// TestRunBitExactScaled confirms the bit-exact weight round-trip holds at a
// different seed and a larger fan-out — the property is a contract of the
// codec, not an artefact of one dataset. It asserts the verdict and that
// every recovered edge was verified, but pins no seed-specific counts.
func TestRunBitExactScaled(t *testing.T) {
	cfg := config{nodes: 512, fanoutMin: 2, fanoutMax: 10, hubFrac: 0.2, seed: 99}
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())

	if facts["weights.bit_exact"] != "true" {
		t.Errorf("weights.bit_exact = %q, want true", facts["weights.bit_exact"])
	}
	if facts["weights.verified"] != facts["recovered.edges"] {
		t.Errorf("weights.verified (%s) != recovered.edges (%s)",
			facts["weights.verified"], facts["recovered.edges"])
	}
	if facts["recovered.schema_version"] != "v2" {
		t.Errorf("recovered.schema_version = %q, want v2", facts["recovered.schema_version"])
	}
}

// TestDeterministic confirms the dataset shape is reproducible: two runs
// with the same config produce identical deterministic fact lines.
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
// out-edges than there are other nodes is rejected before any work, and the
// error names the offending dimension.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{nodes: 8, fanoutMin: 0, fanoutMax: 20, hubFrac: 0.1, seed: 1}
	err := run(context.Background(), &bytes.Buffer{}, bad)
	if err == nil {
		t.Fatal("run accepted a config with fanoutMax > nodes-1; want error")
	}
	if !strings.Contains(err.Error(), "fanoutMax") {
		t.Fatalf("run error = %v, want it to mention fanoutMax", err)
	}
}

// TestRunHonoursCancellation confirms the build aborts promptly when the
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

// BenchmarkRun benchmarks the full build + snapshot + recovery + verify
// round-trip at a chosen scale, so `go test -bench=Run` produces the
// persistence evidence mechanically alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := config{nodes: 2000, fanoutMin: 3, fanoutMax: 8, hubFrac: 0.1, seed: 1}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=value" lines (everything not
// prefixed with "# ") into a map of raw string values, so the test can pin
// both integer counts and string/bool/hex sample values. The volatile
// telemetry lines are ignored.
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
