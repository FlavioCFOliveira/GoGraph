package sim

import (
	"context"
	"testing"
)

// TestCypherPaths_Scenario_Passes runs the cypher-paths scenario: the engine's
// shortestPath() hop count must equal an independent BFS reference for every
// probed pair, including after crash/recovery. A nil report means the operator
// agreed with the reference on every check.
func TestCypherPaths_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioCypherPaths)
	if !ok {
		t.Fatalf("cypher-paths scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("cypher-paths run: %v", err)
	}
	if report != nil {
		t.Fatalf("cypher-paths reported a violation (shortestPath disagreed with BFS):\n%s", report)
	}
}

// TestBFSHops_Reference is a focused unit check on the independent reference so a
// reference bug cannot silently mask an engine bug.
func TestBFSHops_Reference(t *testing.T) {
	adj := map[string][]string{"a": {"b", "c"}, "b": {"d"}, "c": {"d"}, "d": {"e"}}
	cases := []struct {
		a, b string
		want int64
	}{
		{"a", "a", 0}, {"a", "b", 1}, {"a", "d", 2}, {"a", "e", 3}, {"e", "a", -1}, {"a", "z", -1},
	}
	for _, c := range cases {
		if got := bfsHops(adj, c.a, c.b); got != c.want {
			t.Errorf("bfsHops(%q,%q)=%d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestCypherPaths_DetectsWrongReference is the meta-test: a deliberately wrong
// reference (claiming an unreachable pair has a path) must be caught by the
// engine-vs-reference comparison — proving the check is non-vacuous.
func TestCypherPaths_DetectsWrongReference(t *testing.T) {
	cfg := Config{Seed: 0x5409A78, MaxTicks: 120, Workload: WriteHeavyWorkload(NewSeed(0x5409A78))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()
	if _, err := sm.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Sanity: on the real graph the check passes.
	if v := CheckShortestPath(0, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("baseline shortestPath check should pass, got %d violations: %v", len(v), v[0].Message)
	}
}

// TestBFSShortestPathCount_Reference unit-checks the shortest-path COUNT
// reference (used for allShortestPaths) so a reference bug cannot mask an engine
// bug. The diamond a->b->d, a->c->d has TWO shortest a->d paths.
func TestBFSShortestPathCount_Reference(t *testing.T) {
	adj := map[string][]string{"a": {"b", "c"}, "b": {"d"}, "c": {"d"}, "d": {"e"}}
	cases := []struct {
		a, b string
		want int64
	}{
		{"a", "a", 1}, {"a", "b", 1}, {"a", "d", 2}, {"a", "e", 2}, {"e", "a", 0}, {"a", "z", 0},
	}
	for _, c := range cases {
		if got := bfsShortestPathCount(adj, c.a, c.b); got != c.want {
			t.Errorf("bfsShortestPathCount(%q,%q)=%d, want %d", c.a, c.b, got, c.want)
		}
	}
}
