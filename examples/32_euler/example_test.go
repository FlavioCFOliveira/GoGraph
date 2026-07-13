package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestRun drives the default (Eulerian) configuration and asserts the tour
// invariants for both graph orientations: every street is used exactly once,
// the trail length is streets+1, and — on the all-even / balanced network —
// the tour is a closed circuit.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, defaultConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	mustContain(t, out, "config.broken=false")
	for _, kind := range []string{"undirected", "directed"} {
		mustContain(t, out, kind+".trail_len_is_streets_plus_1=true")
		mustContain(t, out, kind+".each_street_once=true")
		mustContain(t, out, kind+".is_circuit=true")
	}
}

// TestBrokenHasNoEulerian closes two vertex-disjoint streets and asserts the
// module reports no Eulerian trail for either orientation. Closing a single
// street would instead leave an Eulerian path, so the two-closure case is the
// one that must fail.
func TestBrokenHasNoEulerian(t *testing.T) {
	cfg := defaultConfig()
	cfg.broken = true

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run(broken): %v", err)
	}
	out := buf.String()

	mustContain(t, out, "config.broken=true")
	mustContain(t, out, "undirected.no_eulerian=true")
	mustContain(t, out, "directed.no_eulerian=true")
}

// TestDeterministic verifies two runs of the same config produce byte-identical
// fact lines (the "# " telemetry, which varies, is stripped first).
func TestDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := run(context.Background(), &a, defaultConfig()); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(context.Background(), &b, defaultConfig()); err != nil {
		t.Fatalf("run b: %v", err)
	}
	if factLines(a.String()) != factLines(b.String()) {
		t.Errorf("fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s", factLines(a.String()), factLines(b.String()))
	}
}

// TestRunRejectsBadConfig checks the validation boundary rejects impossible
// shapes.
func TestRunRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*config)
	}{
		{"too few nodes", func(c *config) { c.nodes = 3 }},
		{"loop-min below 3", func(c *config) { c.loopMin = 2 }},
		{"loop-max below min", func(c *config) { c.loopMin, c.loopMax = 6, 5 }},
		{"loop-max above nodes", func(c *config) { c.loopMax = c.nodes + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig()
			tc.mut(&cfg)
			if err := run(context.Background(), &bytes.Buffer{}, cfg); err == nil {
				t.Errorf("expected an error for %s, got nil", tc.name)
			}
		})
	}
}

// TestRunHonoursCancellation checks a cancelled context stops the generator
// without a panic.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := defaultConfig()
	cfg.nodes = 100000
	cfg.loops = 20000
	// A cancelled context may or may not surface as an error depending on how
	// far the generator got; the contract is simply that it must not panic.
	_ = run(ctx, &bytes.Buffer{}, cfg)
}

// factLines returns only the deterministic fact lines: every line that is not
// blank and does not start with the "# " telemetry prefix.
func factLines(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// mustContain fails the test when out has no line exactly equal to want.
func mustContain(t *testing.T, out, want string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return
		}
	}
	t.Errorf("output missing fact line %q\n--- facts ---\n%s", want, factLines(out))
}
