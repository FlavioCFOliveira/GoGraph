package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a small version of the default specification: the same
// model and code path, sized to persist, snapshot and recover well under
// the short-layer 60 s package budget. The shape is deterministic for the
// fixed seed, so the invariants asserted below are stable across machines.
func testConfig() config {
	return config{
		packages: 200,
		depsMin:  2,
		depsMax:  5,
		batch:    40,
		seed:     7,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — recovered node/edge/label counts and the sampled property
// values that prove the WAL -> snapshot -> recovery round-trip. The
// volatile telemetry lines (prefixed "# ", and the per-run temp path,
// which is never printed) are ignored, as required by the examples
// standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	// The temp directory path must never reach stdout (it differs every
	// run); asserting its absence keeps the report stable.
	if strings.Contains(out, "gograph-ex04-") {
		t.Errorf("output leaked the temp directory path:\n%s", out)
	}

	facts := parseFacts(t, out)

	// Node counts are exact and independent of the RNG: one Release per
	// Package, so packages == releases and the recovered node total is 2N.
	if got := facts["nodes.packages"]; got != int64(cfg.packages) {
		t.Errorf("nodes.packages = %d, want %d", got, cfg.packages)
	}
	if got := facts["nodes.releases"]; got != int64(cfg.packages) {
		t.Errorf("nodes.releases = %d, want %d", got, cfg.packages)
	}
	if got := facts["recovered.nodes"]; got != int64(2*cfg.packages) {
		t.Errorf("recovered.nodes = %d, want %d", got, 2*cfg.packages)
	}

	// Exactly one PUBLISHED edge per package.
	if got := facts["edges.published"]; got != int64(cfg.packages) {
		t.Errorf("edges.published = %d, want %d", got, cfg.packages)
	}

	// DEPENDS_ON out-degree is in [depsMin, depsMax] per release, so the
	// total lands in the corresponding band.
	deps := facts["edges.depends_on"]
	if lo, hi := int64(cfg.packages*cfg.depsMin), int64(cfg.packages*cfg.depsMax); deps < lo || deps > hi {
		t.Errorf("edges.depends_on = %d, want within [%d,%d]", deps, lo, hi)
	}

	// The recovered edge total must equal what was committed: every
	// PUBLISHED and DEPENDS_ON edge survived the round-trip.
	if got, want := facts["recovered.edges"], facts["edges.published"]+deps; got != want {
		t.Errorf("recovered.edges = %d, want %d (published+depends_on)", got, want)
	}

	// Both labels (Package, Release) are in use after recovery.
	if got := facts["recovered.labels"]; got != 2 {
		t.Errorf("recovered.labels = %d, want 2", got)
	}

	// Recovery hit the snapshot rather than replaying from an empty base.
	if !boolFact(t, out, "recovered.snapshot_hit") {
		t.Error("recovered.snapshot_hit = false, want true")
	}

	// Sampled string and int64 property values round-tripped. The concrete
	// values are deterministic for the fixed seed; assert they are present
	// and self-consistent rather than pinning their exact text.
	for _, key := range []string{
		"recovered.sample_name", "recovered.sample_coord",
		"recovered.sample_downloads", "recovered.sample_published",
	} {
		if !lineExists(out, key) {
			t.Errorf("missing recovered fact %q", key)
		}
	}
	// The sampled coord is "<name>@<version>", so it must begin with the
	// sampled name — a cross-check that both string properties recovered
	// from the same release/package pair.
	name := stringFact(out, "recovered.sample_name")
	coord := stringFact(out, "recovered.sample_coord")
	if name == "" || !strings.HasPrefix(coord, name+"@") {
		t.Errorf("recovered coord %q does not start with name %q@", coord, name)
	}
	// Downloads is a non-negative int64.
	if dls := facts["recovered.sample_downloads"]; dls < 0 {
		t.Errorf("recovered.sample_downloads = %d, want >= 0", dls)
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// more dependencies than there are other packages is rejected before any
// work (and before any temp directory is created).
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{packages: 5, depsMin: 0, depsMax: 20, batch: 1, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with depsMax > packages-1; want error")
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

// TestDeterministic confirms the dataset shape is reproducible: two runs
// with the same config produce identical deterministic fact lines (the
// recovered counts and sampled values), independent of the per-run temp
// path and timing telemetry.
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
// a map. Lines whose value is not an integer (e.g. the config range line
// or the string-valued sampled properties) are skipped.
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

// lineExists reports whether out has any bare (non-telemetry) line
// beginning "key=".
func lineExists(out, key string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		if strings.HasPrefix(line, key+"=") {
			return true
		}
	}
	return false
}

// stringFact returns the string value of the bare fact line "key=value",
// or "" when the line is absent.
func stringFact(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// boolFact returns the boolean value of the bare fact line "key=true|false".
func boolFact(t *testing.T, out, key string) bool {
	t.Helper()
	v := stringFact(out, key)
	b, err := strconv.ParseBool(v)
	if err != nil {
		t.Fatalf("fact %q = %q, not a bool", key, v)
	}
	return b
}
