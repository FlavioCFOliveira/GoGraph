package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// testConfig is the small deterministic network the regression test pins.
// It is the package default: a few thousand junctions, which builds and
// queries well under the short-layer 60 s package budget. The shape is
// deterministic for the fixed seed, so the facts asserted below are stable
// across machines, OS and architecture.
func testConfig() config {
	return defaultConfig()
}

// TestRun drives run into a buffer and asserts only the deterministic
// facts: the network shape, full reachability via the backbone, and the
// exact shortest distances to the fixed target junctions. The route is
// checked by its invariants (it starts at the source, ends at the target,
// every hop is a real road, and the hop weights sum to the reported
// distance) rather than its exact node sequence, because the predecessor
// chosen on an equal-distance tie depends on the priority queue's internal
// order. The volatile telemetry lines (prefixed "# ") are ignored, as the
// examples standard requires for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Network shape is exact and independent of the RNG.
	if got := facts["nodes.junctions"]; got != int64(cfg.nodes) {
		t.Errorf("nodes.junctions = %d, want %d", got, cfg.nodes)
	}

	// The backbone guarantees every junction is reachable from the source.
	if got := facts["query.reachable"]; got != int64(cfg.nodes) {
		t.Errorf("query.reachable = %d, want %d (backbone must connect all)", got, cfg.nodes)
	}

	// The shortest distances to the fixed targets are deterministic for the
	// default seed: they are tie-independent, so they are pinned exactly.
	for col, want := range map[string]int64{
		"dist.to_1250": 811,
		"dist.to_2500": 3945,
		"dist.to_4999": 3388,
	} {
		if got := facts[col]; got != want {
			t.Errorf("%s = %d, want %d", col, got, want)
		}
	}

	// The printed route's invariants must hold against the materialised
	// graph, regardless of which equal-distance predecessor was chosen.
	assertRouteInvariants(t, out, cfg)
}

// assertRouteInvariants rebuilds the network from cfg and verifies the
// route printed by run: it must begin at the source junction, end at the
// route target, traverse only real roads, and have hop weights that sum to
// the reported shortest distance.
func assertRouteInvariants(t *testing.T, out string, cfg config) {
	t.Helper()
	targets := targetJunctions(cfg.nodes)
	routeTarget := targets[len(targets)-1]

	routeLine := factValue(out, "route.to_"+strconv.Itoa(routeTarget))
	if routeLine == "" {
		t.Fatalf("no route line for target %d in output", routeTarget)
	}
	hops := parseRoute(t, routeLine)
	if len(hops) < 2 {
		t.Fatalf("route has %d junctions, want at least 2", len(hops))
	}
	if hops[0] != sourceJunction {
		t.Errorf("route starts at %d, want source %d", hops[0], sourceJunction)
	}
	if hops[len(hops)-1] != routeTarget {
		t.Errorf("route ends at %d, want target %d", hops[len(hops)-1], routeTarget)
	}

	// Rebuild the identical network and verify each consecutive pair is a
	// real road; sum the road weights along the route.
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	r := autoRadius(cfg) * cfg.radius
	if _, err := build(context.Background(), a, cfg, r); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	var total int64
	for i := 0; i+1 < len(hops); i++ {
		w, ok := roadWeight(c, mapper, hops[i], hops[i+1])
		if !ok {
			t.Fatalf("route hop %d->%d is not a real road", hops[i], hops[i+1])
		}
		total += w
	}

	// Re-derive the shortest distance to the target and confirm the route
	// realises it exactly.
	src, _ := mapper.Lookup(sourceJunction)
	d, err := search.Dijkstra(c, src)
	if err != nil {
		t.Fatalf("dijkstra: %v", err)
	}
	tid, _ := mapper.Lookup(routeTarget)
	dist, reach := d.Distance(tid)
	if !reach {
		t.Fatalf("target %d unexpectedly unreachable", routeTarget)
	}
	if total != int64(dist) {
		t.Errorf("route weight sum = %d, want shortest distance %d", total, dist)
	}
}

// roadWeight returns the weight of the directed road from the junction with
// user id src to the junction with user id dst, and whether such a road
// exists, by scanning the source's CSR adjacency.
func roadWeight(c *csr.CSR[int64], mapper *graph.Mapper[int], src, dst int) (int64, bool) {
	sid, ok := mapper.Lookup(src)
	if !ok {
		return 0, false
	}
	did, ok := mapper.Lookup(dst)
	if !ok {
		return 0, false
	}
	for nb, w := range c.NeighboursByID(sid) {
		if nb == did {
			return w, true
		}
	}
	return 0, false
}

// TestDeterministic confirms the data shape is reproducible: two runs with
// the same config produce identical deterministic fact lines (including the
// exact route, which is stable for a fixed graph build).
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

// TestRunRejectsBadConfig confirms the boundary validation rejects an
// impossible configuration before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	for name, bad := range map[string]config{
		"too few nodes": {nodes: 1, span: 100, radius: 1, seed: 1},
		"zero span":     {nodes: 100, span: 0, radius: 1, seed: 1},
		"span overflow": {nodes: 100, span: maxSpan + 1, radius: 1, seed: 1},
		"zero radius":   {nodes: 100, span: 100, radius: 0, seed: 1},
	} {
		if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
			t.Errorf("%s: run accepted an invalid config; want error", name)
		}
	}
}

// BenchmarkRun measures the whole build-freeze-query cycle at the default
// config, so `go test -bench` produces the evidence mechanically alongside
// the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	b.ReportAllocs()
	for b.Loop() {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as a
// map. Lines whose value is not an integer (e.g. the route line) are
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

// factValue returns the value of the bare fact line with the given key, or
// "" if absent. Telemetry ("# ") lines are ignored.
func factValue(out, key string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && k == key {
			return v
		}
	}
	return ""
}

// parseRoute parses an "A -> B -> C" route line into its junction ids.
func parseRoute(t *testing.T, s string) []int {
	t.Helper()
	fields := strings.Split(s, " -> ")
	ids := make([]int, len(fields))
	for i, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			t.Fatalf("malformed route junction %q: %v", f, err)
		}
		ids[i] = n
	}
	return ids
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
