package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is the small deterministic default this example ships, restated
// here so the test is self-contained. Same model and code path, sized so the
// O(V*E) Brandes pass runs in microseconds — well under the short-layer 60 s
// package budget. The shape is deterministic for the fixed seed, so the
// invariants asserted below are stable across machines.
func testConfig() config {
	return defaultConfig()
}

// bridgeIDs returns the node values of the C bridge nodes for cfg — the
// dedicated cut vertices the betweenness ranking must surface.
func bridgeIDs(cfg config) map[int64]bool {
	out := make(map[int64]bool, cfg.communities)
	for c := 0; c < cfg.communities; c++ {
		out[int64(cfg.bridgeID(c))] = true
	}
	return out
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — reachability, the cut-vertex betweenness ranking, the
// PageRank/betweenness disagreement, and the bounds on the shortest-path
// distance. The volatile telemetry lines (prefixed "# ") are ignored, as
// required by the examples standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// The graph is connected by construction, so BFS reaches every node.
	wantNodes := int64(cfg.communities*cfg.nodesPerCommunity + cfg.communities)
	if got := facts["nodes.total"]; got != wantNodes {
		t.Errorf("nodes.total = %d, want %d", got, wantNodes)
	}
	if got := facts["bfs.reachable"]; got != wantNodes {
		t.Errorf("bfs.reachable = %d, want %d (graph is connected by construction)", got, wantNodes)
	}
	// The bridge ring gives the graph a large diameter, so the source's
	// eccentricity is comfortably greater than a single community's.
	if ecc := facts["bfs.eccentricity"]; ecc <= 1 {
		t.Errorf("bfs.eccentricity = %d, want > 1 (the bridge ring stretches the diameter)", ecc)
	}

	// Headline invariant: the C bridge nodes are exactly the top-C
	// betweenness nodes. They are the articulation points, so every
	// inter-community shortest path is forced through one of them.
	bridges := bridgeIDs(cfg)
	for rank := 1; rank <= cfg.communities; rank++ {
		key := "betweenness.top" + strconv.Itoa(rank)
		got, ok := facts[key]
		if !ok {
			t.Fatalf("missing fact %s", key)
		}
		if !bridges[got] {
			t.Errorf("%s = %d, want a bridge node (one of %v)", key, got, sortedKeys(bridges))
		}
	}

	// The two centralities must disagree: a bridge is low-degree by design,
	// so no bridge appears in the PageRank top-k — the BA hubs dominate that.
	for rank := 1; rank <= cfg.topK; rank++ {
		key := "pagerank.top" + strconv.Itoa(rank)
		got, ok := facts[key]
		if !ok {
			t.Fatalf("missing fact %s", key)
		}
		if bridges[got] {
			t.Errorf("%s = %d is a bridge node; bridges must rank LOW in PageRank, not high", key, got)
		}
	}

	// PageRank's power iteration must converge within the default budget.
	if iters := facts["pagerank.iterations"]; iters <= 0 || iters >= 100 {
		t.Errorf("pagerank.iterations = %d, want within (0,100)", iters)
	}

	// The weighted shortest-path distance to the far-community anchor is
	// strictly positive, and — because every edge weight is at least
	// weightMin — it is at least weightMin per hop along the chosen path.
	dist := facts["dijkstra.dist_to_farthest"]
	hops := facts["dijkstra.hops_to_farthest"]
	if dist <= 0 {
		t.Errorf("dijkstra.dist_to_farthest = %d, want > 0", dist)
	}
	if hops <= 0 {
		t.Errorf("dijkstra.hops_to_farthest = %d, want > 0", hops)
	}
	if dist < hops*cfg.weightMin {
		t.Errorf("dijkstra.dist_to_farthest = %d, want >= hops*weightMin = %d", dist, hops*cfg.weightMin)
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: a Barabási–Albert
// attachment parameter that is not smaller than the community size cannot
// produce a valid community, so it is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := defaultConfig()
	bad.baAttach = bad.nodesPerCommunity // m must be < nodes per community
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with baAttach >= nodesPerCommunity; want error")
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

// TestDeterministic confirms the graph shape is reproducible: two runs with
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

// TestBridgesTopBetweennessAcrossSeeds confirms the cut-vertex invariant is
// robust, not a seed-1 accident: for several seeds, the top-C betweenness
// nodes are exactly the bridge set and no bridge ever enters the PageRank
// top-k. This is the structural guarantee the topology was chosen to provide.
func TestBridgesTopBetweennessAcrossSeeds(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 7, 42, 99, 1000} {
		cfg := defaultConfig()
		cfg.seed = seed
		var buf bytes.Buffer
		if err := run(context.Background(), &buf, cfg); err != nil {
			t.Fatalf("seed %d: run: %v", seed, err)
		}
		facts := parseFacts(t, buf.String())
		bridges := bridgeIDs(cfg)

		top := make(map[int64]bool, cfg.communities)
		for rank := 1; rank <= cfg.communities; rank++ {
			top[facts["betweenness.top"+strconv.Itoa(rank)]] = true
		}
		if !equalSets(top, bridges) {
			t.Errorf("seed %d: top-%d betweenness = %v, want the bridge set %v",
				seed, cfg.communities, sortedKeys(top), sortedKeys(bridges))
		}
		for rank := 1; rank <= cfg.topK; rank++ {
			if got := facts["pagerank.top"+strconv.Itoa(rank)]; bridges[got] {
				t.Errorf("seed %d: pagerank.top%d = %d is a bridge; bridges must rank low in PageRank", seed, rank, got)
			}
		}
	}
}

// BenchmarkRun runs the full default pipeline (generate, freeze, four
// algorithms) so go test -bench produces the per-run cost mechanically
// alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// equalSets reports whether two int64 sets contain the same keys.
func equalSets(a, b map[int64]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// sortedKeys returns the keys of an int64 set in ascending order, for stable
// error messages.
func sortedKeys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as a
// map. Lines whose value is not an integer (e.g. the config range line) are
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
