package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a small, deterministic social network: same model and code
// path as the default, sized to build and analyse well under the
// short-layer 60 s package budget. The shape is deterministic for the fixed
// seed, so the invariants asserted below are stable across machines. The
// seed differs from defaultConfig's so the test also exercises a second
// realisation of the generator.
func testConfig() config {
	return config{
		users:       300,
		communities: 5,
		m:           2,
		bridges:     8,
		topK:        5,
		seed:        42,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants the examples standard allows for non-deterministic output:
// node and edge counts/bounds, the recovered-community band, a modularity
// lower bound, the influencer set's properties, and the friend-of-friend
// conservation laws. The volatile telemetry lines (prefixed "# ") are
// ignored.
func TestRun(t *testing.T) {
	cfg := testConfig()
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	ints := parseIntFacts(t, out)
	strs := parseStrFacts(out)

	// Node count is exact and independent of the RNG.
	if got := ints["nodes.users"]; got != int64(cfg.users) {
		t.Errorf("nodes.users = %d, want %d", got, cfg.users)
	}

	// Edge count: each BA block contributes m edges per non-seed node plus a
	// short seed path, and the bridge layer adds exactly cfg.bridges edges
	// (none skipped at this scale). The lower bound is the BA backbone; the
	// graph stays well above any degenerate count.
	friend := ints["edges.friend"]
	if friend <= int64(cfg.users) {
		t.Errorf("edges.friend = %d, want > users (%d): the BA backbone alone exceeds it", friend, cfg.users)
	}

	// Stage 2 — Leiden. The graph-theory-expert's regime puts the recovered
	// community count in a tight band around the planted K and the modularity
	// comfortably positive (default targets Q ≈ 0.73). Assert a band and a
	// lower bound, not an exact float, so an internal Leiden change that
	// preserves partition quality does not break the test.
	if found := ints["communities.found"]; found < int64(cfg.communities-1) || found > int64(cfg.communities+2) {
		t.Errorf("communities.found = %d, want within [%d,%d] of planted K=%d",
			found, cfg.communities-1, cfg.communities+2, cfg.communities)
	}
	if q := modularity(t, out); q < 0.55 {
		t.Errorf("communities.modularity = %.4f, want >= 0.55 (planted structure should be clearly recoverable)", q)
	}

	// Stage 1 — PageRank influence. The exact float scores are volatile, so
	// the pinnable facts are: topK influencer ids are present, distinct, and
	// look like user ids; and the influencers span more than one community
	// (the topology grows one hub per community, so a clean top-k is spread
	// across communities, not concentrated in one).
	seen := map[string]bool{}
	for i := int64(1); i <= int64(cfg.topK); i++ {
		id := strs[influenceKey(i)]
		if id == "" {
			t.Errorf("missing influence.rank.%d", i)
			continue
		}
		if !strings.HasPrefix(id, "u") {
			t.Errorf("influence.rank.%d = %q, want a user id", i, id)
		}
		if seen[id] {
			t.Errorf("influence.rank.%d = %q is a duplicate of an earlier rank", i, id)
		}
		seen[id] = true
	}
	if spanned := ints["influence.communities_spanned"]; spanned < 2 {
		t.Errorf("influence.communities_spanned = %d, want >= 2 (one hub per community should spread the top-k)", spanned)
	}

	// Stage 3 — friend-of-friend. The walk is anchored on node 0, placed away
	// from any bridge, so "every candidate is in the seed user's community"
	// is a theorem of the construction. The candidate count is a deterministic
	// fact and must be non-trivial at this density; the top recommendation
	// must share at least one mutual friend.
	if strs["fof.seed_user"] != userID(0) {
		t.Errorf("fof.seed_user = %q, want %q", strs["fof.seed_user"], userID(0))
	}
	if c := ints["fof.candidates"]; c <= 0 {
		t.Errorf("fof.candidates = %d, want > 0 (a dense BA block yields many 2-hop candidates)", c)
	}
	if strs["fof.all_same_community"] != "true" {
		t.Errorf("fof.all_same_community = %q, want \"true\" (seed user is placed away from any bridge — a theorem)", strs["fof.all_same_community"])
	}
	if shared := ints["fof.top_shared"]; shared < 1 {
		t.Errorf("fof.top_shared = %d, want >= 1 (the top recommendation must share at least one friend)", shared)
	}
}

// TestStructuralAnalytics pins the deterministic invariants of Stage 4 — the
// k-core, triangle, diameter, and reachability facts. It asserts correctness
// invariants (bands and conservation laws), not the exact volatile-looking
// counts, so a benign internal change to any analytic that preserves the graph
// property does not break the test. The strongest cross-checks (serial vs
// parallel triangle totals, per-node sum == 3*total, diameter upper bound not
// below an observed shortest path, transitive-closure reach == BFS reach) are
// enforced as hard errors inside run itself, so a regression there surfaces as
// a returned error rather than a silently-wrong fact.
func TestStructuralAnalytics(t *testing.T) {
	cfg := testConfig()
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	ints := parseIntFacts(t, out)
	strs := parseStrFacts(out)

	// k-core. A BA block with m=2 contains cycles, so the whole graph has a
	// 2-core at least; the densest core (at k=degeneracy) needs at least
	// degeneracy+1 vertices and cannot exceed the whole graph.
	deg := ints["core.degeneracy"]
	if deg < 2 {
		t.Errorf("core.degeneracy = %d, want >= 2 (a BA(m=2) graph has a 2-core)", deg)
	}
	size := ints["core.size"]
	if size < deg+1 || size > int64(cfg.users) {
		t.Errorf("core.size = %d, want within [%d,%d]", size, deg+1, cfg.users)
	}

	// Triangles. BA neighbourhoods are triangle-rich, so the total is positive;
	// the global clustering coefficient (transitivity) is 3*triangles/triples,
	// hence 3*total <= triples and 0 < clustering <= 1.
	tri := ints["triangles.total"]
	if tri <= 0 {
		t.Errorf("triangles.total = %d, want > 0 (BA neighbourhoods are triangle-rich)", tri)
	}
	triples := ints["triangles.triples"]
	if 3*tri > triples {
		t.Errorf("3*triangles.total (%d) > triples (%d): transitivity would exceed 1", 3*tri, triples)
	}
	if c := clustering(t, out); c <= 0 || c > 1 {
		t.Errorf("triangles.clustering = %.4f, want in (0,1]", c)
	}

	// Diameter. The graph is connected, so the diameter is at least 1; the
	// bounds are ordered (lo <= hi enforced in run); and it is small relative to
	// the node count — the small-world property the example demonstrates.
	lo := ints["diameter.lo"]
	hi := ints["diameter.hi"]
	if lo < 1 {
		t.Errorf("diameter.lo = %d, want >= 1 (a connected graph with edges)", lo)
	}
	if hi < lo {
		t.Errorf("diameter.hi = %d, want >= diameter.lo (%d)", hi, lo)
	}
	if hi >= int64(cfg.users) {
		t.Errorf("diameter.hi = %d, want << users (%d): small-world", hi, cfg.users)
	}

	// Reachability. The bridge layer lays a spanning path over the communities,
	// so the whole graph is one connected component: every user is reachable
	// from the seed. This is a theorem of the construction, so it is an exact
	// fact, not a band.
	if reach := ints["reach.from_seed"]; reach != int64(cfg.users) {
		t.Errorf("reach.from_seed = %d, want %d (graph is connected by construction)", reach, cfg.users)
	}
	if strs["reach.fully_connected"] != "true" {
		t.Errorf("reach.fully_connected = %q, want \"true\"", strs["reach.fully_connected"])
	}
}

// TestDeterministic confirms the data shape is reproducible: two runs with
// the same config produce identical deterministic fact lines. This guards
// the determinism trap the generator avoids (no RNG draw is driven from a
// randomised Go map range).
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

// TestRunRejectsBadConfig confirms the boundary validation rejects each of
// the parameter-regime violations before any work, one rule at a time.
func TestRunRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  config
	}{
		{"too few communities", config{users: 256, communities: 2, m: 2, bridges: 8, topK: 4, seed: 1}},
		{"m too large for block", config{users: 256, communities: 4, m: 100, bridges: 8, topK: 4, seed: 1}},
		{"too few bridges to connect", config{users: 256, communities: 4, m: 2, bridges: 1, topK: 4, seed: 1}},
		{"too many bridges blur communities", config{users: 256, communities: 4, m: 2, bridges: 100, topK: 4, seed: 1}},
		{"top-k exceeds users", config{users: 256, communities: 4, m: 2, bridges: 8, topK: 1000, seed: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(context.Background(), &bytes.Buffer{}, tc.cfg); err == nil {
				t.Fatalf("run accepted an invalid config (%s); want error", tc.name)
			}
		})
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

// BenchmarkRun runs the full pipeline (generate, PageRank, Leiden, FoF) at a
// modest scale so `go test -bench` produces the evidence mechanically
// alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := config{users: 5000, communities: 8, m: 3, bridges: 40, topK: 8, seed: 1}
	for i := 0; i < b.N; i++ {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// influenceKey returns the fact key for the i-th influencer rank.
func influenceKey(i int64) string {
	return "influence.rank." + strconv.FormatInt(i, 10)
}

// modularity extracts the communities.modularity fact as a float.
func modularity(t *testing.T, out string) float64 {
	t.Helper()
	v := parseStrFacts(out)["communities.modularity"]
	q, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("communities.modularity = %q, not a float: %v", v, err)
	}
	return q
}

// clustering extracts the triangles.clustering fact as a float.
func clustering(t *testing.T, out string) float64 {
	t.Helper()
	v := parseStrFacts(out)["triangles.clustering"]
	c, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("triangles.clustering = %q, not a float: %v", v, err)
	}
	return c
}

// parseIntFacts extracts the deterministic "key=int" fact lines (everything
// not prefixed with "# ") whose value parses as an integer. Non-integer
// values (ids, booleans, floats) are skipped — parseStrFacts reads those.
func parseIntFacts(t *testing.T, out string) map[string]int64 {
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

// parseStrFacts extracts every deterministic "key=value" fact line as a
// string map (so id, boolean, and float facts are readable too).
func parseStrFacts(out string) map[string]string {
	facts := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
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
