package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestRunAcyclic drives run on the deterministic default (a layered-DAG
// freight network with negative rebate lanes but no negative cycle) and
// asserts both the fixed-seed facts and the structural invariants. The
// headline invariant is bellman_ford_matches_johnson=true: the library's
// Bellman-Ford distances equal Johnson's reweighted all-pairs row (and the
// textbook reference) for every node. The volatile "# " telemetry is
// ignored, as the examples standard requires.
func TestRunAcyclic(t *testing.T) {
	out := mustRun(t, defaultConfig())
	facts := parseFacts(t, out)

	// Fixed-seed facts (seed=1). These pin the exact data shape and result.
	wantExact := map[string]string{
		"config.arbitrage":             "false",
		"graph.nodes":                  "81", // depot plus five tiers of sixteen hubs
		"graph.src":                    "0",
		"neg_edges":                    "91",
		"neg_cycle_detected":           "0",
		"dijkstra_rejects_negative":    "true",
		"bellman_ford_matches_johnson": "true",
		"markets.count":                "16",
		"markets.dist_min":             "-175",
		"markets.dist_max":             "44",
		"markets.dist_sum":             "-1532",
		"cheapest_market":              "68",
		"cheapest_cost":                "-175",
		"cheapest_hops":                "5", // every depot->market path is exactly layers-1 hops
	}
	for k, want := range wantExact {
		if got := facts[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// The routing problem is genuinely a negative-weight one: rebate lanes
	// exist and the cheapest route has a NET-NEGATIVE cost. Dijkstra could
	// not have produced this (it refuses the negative edges), which the
	// dijkstra_rejects_negative fact above confirms.
	if factInt(t, facts, "neg_edges") <= 0 {
		t.Error("neg_edges must be > 0 for a negative-weight instance")
	}
	if factInt(t, facts, "markets.dist_min") >= 0 {
		t.Error("markets.dist_min must be < 0: rebate chaining should yield a net-negative route")
	}

	// Internal consistency: the cheapest cost equals the reported minimum,
	// and the aggregate sum equals the sum of the individual market lines.
	if facts["cheapest_cost"] != facts["markets.dist_min"] {
		t.Errorf("cheapest_cost %q != markets.dist_min %q", facts["cheapest_cost"], facts["markets.dist_min"])
	}
	var perMarketSum int64
	markets := 0
	for k, v := range facts {
		if strings.HasPrefix(k, "market.") {
			perMarketSum += mustAtoi64(t, v)
			markets++
		}
	}
	if markets != int(factInt(t, facts, "markets.count")) {
		t.Errorf("counted %d market.* lines, markets.count=%d", markets, factInt(t, facts, "markets.count"))
	}
	if perMarketSum != factInt(t, facts, "markets.dist_sum") {
		t.Errorf("sum of market.* lines = %d, markets.dist_sum = %d", perMarketSum, factInt(t, facts, "markets.dist_sum"))
	}

	// The reconstructed route starts at the depot, ends at the cheapest
	// market, and has exactly cheapest_hops+1 nodes.
	path := strings.Split(facts["cheapest_path"], ">")
	if got, want := len(path), int(factInt(t, facts, "cheapest_hops"))+1; got != want {
		t.Errorf("cheapest_path has %d nodes, want %d", got, want)
	}
	if path[0] != "0" {
		t.Errorf("cheapest_path starts at %q, want depot 0", path[0])
	}
	if last := path[len(path)-1]; last != facts["cheapest_market"] {
		t.Errorf("cheapest_path ends at %q, want cheapest_market %q", last, facts["cheapest_market"])
	}
}

// TestArbitrageDetectsCycle drives run with -arbitrage and asserts that the
// injected negative cycle is detected by Bellman-Ford, Johnson, and the
// textbook reference alike, and that no distance facts are emitted (the
// shortest-path answer is undefined in the presence of a negative cycle).
func TestArbitrageDetectsCycle(t *testing.T) {
	cfg := defaultConfig()
	cfg.arbitrage = true
	out := mustRun(t, cfg)
	facts := parseFacts(t, out)

	wantExact := map[string]string{
		"config.arbitrage":             "true",
		"graph.edges":                  "253", // acyclic 252 + one injected back-edge
		"neg_edges":                    "92",  // acyclic 91 + the negative back-edge
		"neg_cycle_detected":           "1",
		"bellman_ford_detects":         "true",
		"johnson_detects":              "true",
		"reference_detects":            "true",
		"bellman_ford_matches_johnson": "true",
	}
	for k, want := range wantExact {
		if got := facts[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// With a negative cycle the distances are undefined, so no per-market
	// distance facts must be emitted.
	if strings.Contains(out, "\nmarket.0=") || strings.Contains(out, "cheapest_market=") {
		t.Error("arbitrage instance emitted distance facts; distances are undefined under a negative cycle")
	}
}

// TestDeterministic confirms the data shape is reproducible: two runs with
// the same config produce identical deterministic fact lines.
func TestDeterministic(t *testing.T) {
	a := factLines(mustRun(t, defaultConfig()))
	b := factLines(mustRun(t, defaultConfig()))
	if a != b {
		t.Errorf("deterministic fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

// TestRunRejectsBadConfig confirms boundary validation: a fanout larger
// than the tier width cannot pick enough distinct targets and is rejected
// before any work, with no output produced.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := defaultConfig()
	bad.fanout = bad.width + 1
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, bad); err == nil {
		t.Fatal("run accepted a config with fanout > width; want error")
	}
	if buf.Len() != 0 {
		t.Errorf("run wrote %d bytes before failing validation; want none", buf.Len())
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

// BenchmarkRun measures the end-to-end cost of building the default-scale
// freight network, solving single-source Bellman-Ford, and cross-checking
// it against Johnson APSP and the textbook reference, so `go test -bench`
// produces evidence mechanically alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := defaultConfig()
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := run(ctx, &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// mustRun drives run into a buffer and fails the test on error.
func mustRun(t *testing.T, cfg config) string {
	t.Helper()
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	return buf.String()
}

// parseFacts extracts the deterministic "key=value" lines (everything not
// prefixed with "# ") into a map of strings. Values are kept as text so
// integer, float, boolean, and path facts can all be asserted uniformly.
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
// volatile "# " telemetry), joined for equality comparison.
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

// factInt returns the named fact parsed as an int64, failing the test if it
// is missing or non-numeric.
func factInt(t *testing.T, facts map[string]string, key string) int64 {
	t.Helper()
	v, ok := facts[key]
	if !ok {
		t.Fatalf("missing fact %q", key)
	}
	return mustAtoi64(t, v)
}

// mustAtoi64 parses v as an int64 or fails the test.
func mustAtoi64(t *testing.T, v string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("value %q is not an int64: %v", v, err)
	}
	return n
}
