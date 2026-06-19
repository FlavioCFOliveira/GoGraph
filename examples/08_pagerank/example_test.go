package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is the small deterministic web the regression test pins. It
// is the same as defaultConfig but stated explicitly so the asserted
// facts below are anchored to a fixed shape independent of any future
// change to the binary's default.
func testConfig() config {
	return config{
		pages:   500,
		links:   4,
		attract: 1,
		seedNet: 5,
		topK:    10,
		seed:    1,
	}
}

// TestRun drives run into a buffer and asserts the deterministic facts:
// the exact node and edge counts, the top-10 page ranking, and the
// distinct-rank count. The volatile telemetry lines (prefixed "# ") — the
// PageRank scores, iteration count, timings, allocations and heap — are
// ignored, as required by the examples standard for non-deterministic
// output. The ranking is stable because the build commits edges in a
// deterministic order (never by ranging a map) and ties on rank are broken
// by ascending page id.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, testConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseIntFacts(out)
	cfg := testConfig()

	// Page count is exact; edge count is exact for a fixed seed: seedNet
	// ring edges plus links per generated page.
	if got := facts["nodes.pages"]; got != int64(cfg.pages) {
		t.Errorf("nodes.pages = %d, want %d", got, cfg.pages)
	}
	wantEdges := int64(cfg.seedNet + (cfg.pages-cfg.seedNet)*cfg.links)
	if got := facts["edges.links"]; got != wantEdges {
		t.Errorf("edges.links = %d, want %d", got, wantEdges)
	}

	// The full deterministic top-10 ranking for seed 1. The five seed-core
	// pages (0..4) accumulate the most incoming mass and head the ranking;
	// the rest are early grown pages. This is the regression baseline a
	// future change to the generator or to PageRank must preserve.
	wantRanks := []string{
		"page0000003", "page0000004", "page0000000", "page0000001",
		"page0000002", "page0000006", "page0000007", "page0000018",
		"page0000005", "page0000012",
	}
	gotRanks := strFacts(out, "rank.")
	if len(gotRanks) != cfg.topK {
		t.Fatalf("got %d rank lines, want %d", len(gotRanks), cfg.topK)
	}
	for i, want := range wantRanks {
		key := "rank." + strconv.Itoa(i+1)
		if gotRanks[key] != want {
			t.Errorf("%s = %q, want %q", key, gotRanks[key], want)
		}
	}

	// The distribution is non-uniform: a fixed, reproducible number of
	// distinct rank values for seed 1.
	if got := facts["distinct_ranks"]; got != 117 {
		t.Errorf("distinct_ranks = %d, want 117", got)
	}

	// The five highest-ranked pages must be exactly the seed-core pages
	// (0..4): in Price's model the oldest nodes win, and the core ring
	// gives them an in-degree head start. This is the invariant that proves
	// PageRank is concentrating mass on the authorities.
	core := map[string]bool{
		"page0000000": true, "page0000001": true, "page0000002": true,
		"page0000003": true, "page0000004": true,
	}
	for i := 1; i <= cfg.seedNet; i++ {
		name := gotRanks["rank."+strconv.Itoa(i)]
		if !core[name] {
			t.Errorf("rank.%d = %q, want one of the seed-core pages 0..4", i, name)
		}
	}
}

// TestDeterministic confirms the whole run is reproducible: two runs with
// the same config produce identical deterministic fact lines. This guards
// the build's edge-commit order and the ranking against the map-iteration
// non-determinism that would otherwise leak into PageRank's float
// summation order.
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

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// more out-links than there are seed-core targets is rejected before any
// work, with no output written.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{pages: 100, links: 10, attract: 1, seedNet: 4, topK: 5, seed: 1}
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, bad); err == nil {
		t.Fatal("run accepted a config with links > seedNet; want error")
	}
	if buf.Len() != 0 {
		t.Errorf("run wrote %d bytes before failing validation; want 0", buf.Len())
	}
}

// TestRunHonoursCancellation confirms the build aborts promptly when the
// context is already cancelled, returning the context error. The web is
// large enough that the build loop reaches its periodic ctx check.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := testConfig()
	cfg.pages = 200000 // big enough to enter the build loop's ctx-check
	err := run(ctx, &bytes.Buffer{}, cfg)
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// BenchmarkRun runs the full generate-snapshot-PageRank pipeline at the
// default scale, so go test -bench produces the build and ranking cost
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

// parseIntFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as
// a map. Lines whose value is not an integer (e.g. the rank.N=page... and
// config range lines) are skipped.
func parseIntFacts(out string) map[string]int64 {
	facts := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			facts[k] = n
		}
	}
	return facts
}

// strFacts extracts the deterministic string facts whose key begins with
// prefix (e.g. "rank.") into a map, ignoring telemetry lines.
func strFacts(out, prefix string) map[string]string {
	facts := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") || !strings.HasPrefix(line, prefix) {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			facts[k] = v
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
