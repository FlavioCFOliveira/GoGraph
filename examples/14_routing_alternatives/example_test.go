package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a smaller version of the default specification: the same
// generator and the same code path, sized to build and run all three
// queries well under the short-layer 60 s package budget. The shape is
// deterministic for the fixed seed, so the invariants asserted below are
// stable across machines.
func testConfig() config {
	return config{
		nodes:      200,
		neighbours: 6,
		scale:      1000,
		k:          3,
		seed:       42,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants: the node/edge counts are well-formed, A* returns the same
// optimal cost as Dijkstra (the admissibility guarantee), and Yen's k
// alternative costs are emitted in non-decreasing order with the cheapest
// equal to the Dijkstra optimum. The volatile telemetry lines (prefixed
// "# ", including the expansion counts and timings) are ignored, as the
// examples standard requires for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Node count is exact: every coordinate is interned.
	if got := facts["graph.nodes"]; got != int64(cfg.nodes) {
		t.Errorf("graph.nodes = %d, want %d", got, cfg.nodes)
	}

	// Each undirected k-NN/repair edge is stored as two directed arcs, so
	// the edge total is positive and bounded above by 2 * nodes * neighbours
	// plus the handful of repair arcs (repair adds at most nodes-1 merges,
	// each two arcs). The simple graph collapses symmetric duplicates, so the
	// realised total is at or below the loose upper bound.
	edges := facts["graph.edges"]
	if hi := int64(2 * cfg.nodes * (cfg.neighbours + 1)); edges <= 0 || edges > hi {
		t.Errorf("graph.edges = %d, want within (0,%d]", edges, hi)
	}

	// A* must return the SAME optimal cost as Dijkstra: this is the
	// admissibility guarantee, and it must hold for every seed. If a future
	// change made the heuristic overestimate, the two costs would diverge.
	dij := facts["dijkstra.cost"]
	astar := facts["astar.cost"]
	if dij <= 0 {
		t.Errorf("dijkstra.cost = %d, want > 0", dij)
	}
	if astar != dij {
		t.Errorf("astar.cost = %d, want == dijkstra.cost %d", astar, dij)
	}
	if got := facts["astar_cost_equals_dijkstra"]; got != boolFact(true) {
		t.Errorf("astar_cost_equals_dijkstra = %d, want true", got)
	}

	// Yen returns k alternatives, in non-decreasing cost order, the cheapest
	// equal to the Dijkstra optimum.
	if got := facts["yen.count"]; got != int64(cfg.k) {
		t.Errorf("yen.count = %d, want %d", got, cfg.k)
	}
	prev := int64(-1)
	for i := 1; i <= cfg.k; i++ {
		cost := facts["yen.cost."+strconv.Itoa(i)]
		if i == 1 && cost != dij {
			t.Errorf("yen.cost.1 = %d, want == dijkstra.cost %d", cost, dij)
		}
		if cost < prev {
			t.Errorf("yen.cost.%d = %d, not >= previous %d (must be ascending)", i, cost, prev)
		}
		prev = cost
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

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// at least as many neighbours as there are nodes is rejected before any
// work, with no output produced.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{nodes: 5, neighbours: 5, scale: 1000, k: 3, seed: 1}
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, bad); err == nil {
		t.Fatal("run accepted a config with neighbours >= nodes; want error")
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
	err := run(ctx, &bytes.Buffer{}, testConfig())
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// BenchmarkRun measures the end-to-end cost of building the default-scale
// routing graph and running all three shortest-path queries, so
// `go test -bench` produces evidence mechanically alongside the
// human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := run(ctx, &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// boolFact returns the integer parseFacts records for a boolean fact line
// (Go's %t prints "true"/"false", which parseFacts maps to 1/0 so the
// test can assert it as an integer alongside the count facts).
func boolFact(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// parseFacts extracts the deterministic "key=value" lines (everything not
// prefixed with "# ") into a map of int64s. An integer value is recorded
// as-is; the boolean "true"/"false" produced by %t is mapped to 1/0 so
// boolean facts can be asserted alongside the counts. Lines whose value
// is neither (e.g. the config.scale float line) are skipped.
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
