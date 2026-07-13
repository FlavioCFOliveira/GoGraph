package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is the small deterministic default this example ships, restated
// here so the test is self-contained. The shape is a pure function of the fixed
// seed, and the link costs are exact integers (rounded Euclidean distances), so
// every fact asserted below is byte-stable across machines and Go versions
// (the math/rand stream is covered by the Go 1 compatibility guarantee).
func testConfig() config { return defaultConfig() }

// TestRun drives run into a buffer at the connected default and asserts the
// deterministic MST facts exactly, plus the spanning-tree shape invariant. The
// volatile telemetry lines (prefixed "# ") are ignored, as the examples
// standard requires. Because run() embeds the Prim-vs-Kruskal correctness
// oracle, a returned error here already means the two algorithms disagreed.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, testConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// The default is one connected backbone over every site.
	wantSites := int64(testConfig().regions * testConfig().sitesPerRegion)
	assertFact(t, facts, "sites.total", wantSites)
	assertFact(t, facts, "links.total", 336)
	assertFact(t, facts, "graph.component_count", 1)

	// Exact optimal-backbone facts for seed 1 (Prim == Kruskal).
	assertFact(t, facts, "mst.total_weight", 437920)
	assertFact(t, facts, "mst.edge_count", 119)
	assertFact(t, facts, "mst.min_link_cost", 249)
	assertFact(t, facts, "mst.max_link_cost", 53647)

	// A spanning tree over V sites in K components has exactly V-K edges.
	if got := facts["mst.edge_count"]; got != facts["sites.total"]-facts["graph.component_count"] {
		t.Errorf("mst.edge_count = %d, want sites.total - component_count = %d",
			got, facts["sites.total"]-facts["graph.component_count"])
	}
	if facts["mst.min_link_cost"] > facts["mst.max_link_cost"] {
		t.Errorf("mst.min_link_cost %d > mst.max_link_cost %d",
			facts["mst.min_link_cost"], facts["mst.max_link_cost"])
	}
	mustContain(t, out, "config.interconnect=true")
}

// TestForest exercises the spanning-FOREST path: with the regions left
// disjoint, WCC finds one component per region and the minimum spanning forest
// has exactly V-K edges. The oracle inside run() still cross-checks Prim
// (summed over the three component trees) against Kruskal's whole-graph forest.
func TestForest(t *testing.T) {
	cfg := testConfig()
	cfg.interconnect = false
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())

	assertFact(t, facts, "graph.component_count", int64(cfg.regions))
	assertFact(t, facts, "sites.total", int64(cfg.regions*cfg.sitesPerRegion))
	assertFact(t, facts, "mst.edge_count", 117) // 120 sites - 3 components
	assertFact(t, facts, "mst.total_weight", 339175)
	if got := facts["mst.edge_count"]; got != facts["sites.total"]-facts["graph.component_count"] {
		t.Errorf("forest edge count = %d, want V-K = %d", got, facts["sites.total"]-facts["graph.component_count"])
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

// TestOracleAcrossSeeds runs the full pipeline for several seeds in both the
// connected and forest modes. Every run drives the embedded Prim-vs-Kruskal
// oracle, so a bug in either algorithm on any of these shapes surfaces as a
// returned error — the invariant is not a seed-1 accident.
func TestOracleAcrossSeeds(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 7, 42, 99, 1000} {
		for _, interconnect := range []bool{true, false} {
			cfg := testConfig()
			cfg.seed = seed
			cfg.interconnect = interconnect
			var buf bytes.Buffer
			if err := run(context.Background(), &buf, cfg); err != nil {
				t.Fatalf("seed %d interconnect=%t: run: %v", seed, interconnect, err)
			}
			facts := parseFacts(t, buf.String())
			// Spanning-forest shape must hold for every shape.
			if got := facts["mst.edge_count"]; got != facts["sites.total"]-facts["graph.component_count"] {
				t.Errorf("seed %d interconnect=%t: mst.edge_count = %d, want V-K = %d",
					seed, interconnect, got, facts["sites.total"]-facts["graph.component_count"])
			}
			if interconnect && facts["graph.component_count"] != 1 {
				t.Errorf("seed %d: interconnected graph has %d components, want 1",
					seed, facts["graph.component_count"])
			}
			if !interconnect && facts["graph.component_count"] != int64(cfg.regions) {
				t.Errorf("seed %d: disjoint graph has %d components, want %d",
					seed, facts["graph.component_count"], cfg.regions)
			}
		}
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: a region of a
// single site cannot carry an edge, so it is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := testConfig()
	bad.sitesPerRegion = 1
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted sitesPerRegion=1; want error")
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

// BenchmarkRun runs the full default pipeline (generate, freeze, Kruskal, Prim,
// oracle) so go test -bench produces the per-run cost mechanically alongside
// the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// ── test helpers ────────────────────────────────────────────────────────────

// assertFact fails the test unless fact key equals want.
func assertFact(t *testing.T, facts map[string]int64, key string, want int64) {
	t.Helper()
	got, ok := facts[key]
	if !ok {
		t.Fatalf("missing fact %q", key)
	}
	if got != want {
		t.Errorf("%s = %d, want %d", key, got, want)
	}
}

// mustContain fails the test unless out contains want.
func mustContain(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("output missing %q", want)
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as a
// map. Lines whose value is not an integer (e.g. config.interconnect) are
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
