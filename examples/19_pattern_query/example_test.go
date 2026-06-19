package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a small version of the model: same generator and code
// path, sized to build and query well under the short-layer 60 s package
// budget. The shape is deterministic for the fixed seed, so the invariants
// asserted below are stable across machines.
func testConfig() config {
	return config{
		nodes:        2000,
		outDegreeMin: 2,
		outDegreeMax: 6,
		deprecatedN:  8,
		seed:         1,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — node counts, edge-count bounds, the pattern-query
// cardinalities, and the read-back property facts. The volatile telemetry
// lines (prefixed "# ") are ignored, as required by the examples standard
// for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Node counts are exact and independent of the RNG: deprecated is the
	// count of indices divisible by deprecatedN in [0, nodes).
	wantDeprecated := int64((cfg.nodes-1)/cfg.deprecatedN + 1)
	if got := facts["nodes.packages"]; got != int64(cfg.nodes) {
		t.Errorf("nodes.packages = %d, want %d", got, cfg.nodes)
	}
	if got := facts["nodes.deprecated"]; got != wantDeprecated {
		t.Errorf("nodes.deprecated = %d, want %d", got, wantDeprecated)
	}

	// Out-degree is in [0, outDegreeMax] per package (clamped to the index
	// for the earliest nodes), so the edge total lands at or below the cap.
	edges := facts["edges.depends_on"]
	if hi := int64(cfg.nodes * cfg.outDegreeMax); edges <= 0 || edges > hi {
		t.Errorf("edges.depends_on = %d, want within (0,%d]", edges, hi)
	}

	// The label-scan query over (:Package) must equal the node count, and
	// the (:Package:Deprecated) intersection must equal the deprecated count.
	checks := []struct {
		col  string
		want int64
	}{
		{"q.all_packages", int64(cfg.nodes)},
		{"q.deprecated", wantDeprecated},
	}
	for _, c := range checks {
		if got := facts[c.col]; got != c.want {
			t.Errorf("%s = %d, want %d", c.col, got, c.want)
		}
	}

	// The property-predicate queries select a categorical subset, so the
	// counts are strictly between zero and the total at this scale.
	for _, col := range []string{"q.ecosystem_go", "q.license_mit"} {
		if got := facts[col]; got <= 0 || got >= int64(cfg.nodes) {
			t.Errorf("%s = %d, want within (0,%d)", col, got, cfg.nodes)
		}
	}

	// The one-hop expansion from deprecated packages reaches a non-empty,
	// bounded set of direct dependencies.
	if deps := facts["q.deprecated_deps"]; deps <= 0 || deps >= int64(cfg.nodes) {
		t.Errorf("q.deprecated_deps = %d, want within (0,%d)", deps, cfg.nodes)
	}

	// Exactly readBackTop read-back rows are printed, each in the
	// "id:... downloads:... license:..." shape.
	if got := facts["readback.rows"]; got != int64(readBackTop) {
		t.Errorf("readback.rows = %d, want %d", got, readBackTop)
	}
	for i := 0; i < readBackTop; i++ {
		mustContainPrefix(t, out, "readback."+strconv.Itoa(i)+"=id:")
	}
}

// TestReadBackOrder confirms the read-back is a total order by downloads
// DESC: the deterministic facts the example was upgraded to surface — for
// a fixed -seed the top-by-downloads matched packages, with their
// downloads and license, are reproducible and correctly ordered.
func TestReadBackOrder(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, testConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	var prev int64 = 1<<63 - 1
	for i := 0; i < readBackTop; i++ {
		line := factValue(t, out, "readback."+strconv.Itoa(i))
		// line looks like "id:<hex> downloads:<n> license:<lic>".
		dl := fieldValue(line, "downloads:")
		n, err := strconv.ParseInt(dl, 10, 64)
		if err != nil {
			t.Fatalf("readback.%d downloads %q not an int: %v", i, dl, err)
		}
		if n > prev {
			t.Errorf("readback not sorted by downloads DESC: row %d has %d > previous %d", i, n, prev)
		}
		prev = n
		if id := fieldValue(line, "id:"); id == "" {
			t.Errorf("readback.%d has empty id", i)
		}
		if lic := fieldValue(line, "license:"); lic == "" {
			t.Errorf("readback.%d has empty license", i)
		}
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

// TestRunRejectsBadConfig confirms the boundary validation rejects an
// impossible configuration (an inverted out-degree range) before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{nodes: 100, outDegreeMin: 6, outDegreeMax: 2, deprecatedN: 8, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with outDegreeMin > outDegreeMax; want error")
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

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as
// a map. Lines whose value is not an integer (e.g. config ranges and
// read-back rows) are skipped.
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

// factValue returns the value of the bare fact line "key=value".
func factValue(t *testing.T, out, key string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return v
		}
	}
	t.Fatalf("fact line %q not found", key)
	return ""
}

// fieldValue returns the token following marker in a space-separated
// "field:value" line, or "" if the marker is absent.
func fieldValue(line, marker string) string {
	for _, field := range strings.Fields(line) {
		if strings.HasPrefix(field, marker) {
			return strings.TrimPrefix(field, marker)
		}
	}
	return ""
}

// mustContainPrefix fails the test unless out has a line starting with
// prefix.
func mustContainPrefix(t *testing.T, out, prefix string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return
		}
	}
	t.Errorf("output missing a line with prefix %q", prefix)
}
