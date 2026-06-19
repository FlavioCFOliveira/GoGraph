package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is the small planted-partition default the regression suite
// pins: four communities of twenty-five nodes, well above the SBM
// detectability threshold. The shape is deterministic for the fixed seed,
// so the invariants asserted below are stable across machines and the
// build/detect cost is negligible against the short-layer 60 s budget.
func testConfig() config {
	return config{
		communities:   4,
		communitySize: 25,
		pIn:           0.55,
		pOut:          0.01,
		seed:          1,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants: the node total is exact, the planted-edge total lands in the
// expected band, Leiden recovers a community count close to K, and the
// modularity clears a conservative floor. The volatile telemetry lines
// (prefixed "# ") are ignored, as required by the examples standard for
// non-deterministic output. Leiden is randomised internally by contract,
// so the modularity is asserted as a lower bound rather than an exact
// float (empirically the default seed yields Q ≈ 0.69; the floor sits well
// below that and below the planted-partition expectation of ≈0.70).
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Node total is exact and independent of the RNG.
	if got, want := facts["nodes"], int64(cfg.communities*cfg.communitySize); got != want {
		t.Errorf("nodes = %d, want %d", got, want)
	}

	// Edge count is random but bounded: every intra-community pair is offered
	// an edge with probability pIn and every inter-community pair with pOut,
	// so the realised total lands strictly between zero and the number of
	// unordered pairs. At the default densities it is comfortably positive.
	nodes := cfg.communities * cfg.communitySize
	maxPairs := int64(nodes * (nodes - 1) / 2)
	if edges := facts["edges"]; edges <= 0 || edges > maxPairs {
		t.Errorf("edges = %d, want within (0,%d]", edges, maxPairs)
	}

	// Leiden recovers the K planted blocks. At ≈3× above the detectability
	// threshold this is the overwhelmingly likely outcome; a ±1 band absorbs
	// rare boundary-node reassignment without masking a real regression.
	if found, k := facts["communities_found"], int64(cfg.communities); found < k-1 || found > k+1 {
		t.Errorf("communities_found = %d, want within [%d,%d]", found, k-1, k+1)
	}

	// Modularity floor. The report prints Q to two decimals as the fact line
	// "modularity=0.NN"; assert it clears a conservative lower bound. parseFacts
	// keeps only integer-valued facts, so read the modularity line directly.
	q := parseModularity(t, out)
	if q < 0.55 {
		t.Errorf("modularity = %.4f, want >= 0.55", q)
	}
}

// TestDeterministic confirms the run is reproducible: two runs with the
// same config produce identical deterministic fact lines (including the
// rounded modularity). Leiden's output is deterministic for a fixed input,
// so the whole fact block is byte-stable run to run.
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

// TestRunRejectsBadConfig confirms the boundary validation rejects
// configurations that cannot produce a recoverable planted partition,
// before any work: a pIn at or below pOut (no planted structure) and a pIn
// too low to keep a block connected (it would fragment and inflate the
// recovered community count).
func TestRunRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  config
	}{
		{"p_in <= p_out", config{communities: 4, communitySize: 25, pIn: 0.1, pOut: 0.2, seed: 1}},
		{"p_in below connectivity floor", config{communities: 4, communitySize: 25, pIn: 0.05, pOut: 0.01, seed: 1}},
		{"too few communities", config{communities: 1, communitySize: 25, pIn: 0.55, pOut: 0.01, seed: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(context.Background(), &bytes.Buffer{}, tc.cfg); err == nil {
				t.Fatalf("run accepted an invalid config (%s); want error", tc.name)
			}
		})
	}
}

// TestRunHonoursCancellation confirms the run aborts promptly when the
// context is already cancelled, returning a context error. The cancellation
// is observed during the generator's pair enumeration.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A larger graph guarantees the build loop reaches a ctx check before
	// finishing, so the cancelled context is observed deterministically.
	cfg := config{communities: 8, communitySize: 500, pIn: 0.06, pOut: 0.0008, seed: 1}
	err := run(ctx, &bytes.Buffer{}, cfg)
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// BenchmarkRun measures end-to-end build + Leiden detection at a scale
// where the detection cost is observable, so `go test -bench` produces the
// timing evidence mechanically alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := config{communities: 8, communitySize: 500, pIn: 0.06, pOut: 0.0008, seed: 1}
	var sink bytes.Buffer
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink.Reset()
		if err := run(context.Background(), &sink, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as a
// map. Lines whose value is not an integer (e.g. the float config lines or
// the modularity line) are skipped.
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

// parseModularity extracts the rounded modularity from the deterministic
// "modularity=0.NN" fact line.
func parseModularity(t *testing.T, out string) float64 {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if v, ok := strings.CutPrefix(line, "modularity="); ok {
			q, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("malformed modularity line %q: %v", line, err)
			}
			return q
		}
	}
	t.Fatal("no modularity fact line in output")
	return 0
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
