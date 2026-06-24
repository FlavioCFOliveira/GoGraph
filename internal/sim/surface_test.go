package sim

import (
	"context"
	"testing"
)

// TestCypherSurface_Scenario_Passes runs the cypher-surface scenario: a battery
// of diverse read shapes must match independently-computed oracle invariants
// throughout, including after crash/recovery.
func TestCypherSurface_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioCypherSurface)
	if !ok {
		t.Fatalf("cypher-surface scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("cypher-surface run: %v", err)
	}
	if report != nil {
		t.Fatalf("cypher-surface reported a violation (a read shape disagreed with the oracle invariant):\n%s", report)
	}
}

// TestCypherSurface_DetectsDivergence is the meta-test: after building a graph,
// corrupt the oracle's view (drop a Person from the model) and assert the battery
// flags the resulting count/sum disagreement.
func TestCypherSurface_DetectsDivergence(t *testing.T) {
	cfg := Config{Seed: 0x5C0FACE, MaxTicks: 200, Workload: surfaceWorkload(NewSeed(0x5C0FACE))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()
	if _, err := sm.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Baseline: the battery passes against the true model.
	if v := CheckCypherSurface(0, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("baseline surface battery should pass, got: %s", v[0].Message)
	}
	// Corrupt the oracle: remove one modelled Person so its count/sum invariants
	// no longer match the engine; the battery must catch it.
	for id, n := range sm.oracle.nodes {
		if hasLabel(n, "Person") {
			delete(sm.oracle.nodes, id)
			break
		}
	}
	if v := CheckCypherSurface(0, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("surface battery FAILED to detect an injected count/sum divergence")
	}
}
