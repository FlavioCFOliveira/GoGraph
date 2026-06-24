package sim

import (
	"context"
	"testing"
)

// TestEdgeProperties_Scenario_Passes runs the edge-properties scenario: every
// KNOWS edge's since/weight must round-trip and survive crash/recovery.
func TestEdgeProperties_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioEdgeProperties)
	if !ok {
		t.Fatalf("edge-properties scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("edge-properties run: %v", err)
	}
	if report != nil {
		t.Fatalf("edge-properties reported a violation:\n%s", report)
	}
}

// TestEdgeProperties_NonVacuous confirms KNOWS-with-properties edges were created
// and survived crash/recovery, so the round-trip + survival checks ran.
func TestEdgeProperties_NonVacuous(t *testing.T) {
	sc := edgePropertiesScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	report, err := runEdgeProperties(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("runEdgeProperties: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}
	// Re-run a short loop to inspect modelled edges.
	sm2, _ := New(cfg)
	t.Cleanup(func() { _ = sm2.Close() })
	for i := 0; i < 300; i++ {
		op := (&EdgePropsWriter{counter: int64(i)}).NextOp(sm2.seed, sm2.oracle)
		committed := sm2.execute(context.Background(), op)
		sm2.applyToOracle(op, committed)
	}
	if len(sm2.oracle.KnowsEdgesByName()) == 0 {
		t.Fatalf("vacuous: no KNOWS-with-properties edge was modelled")
	}
}

// TestEdgeProperties_DetectsMismatch is the meta-test: a modelled edge property
// that disagrees with the engine must be flagged.
func TestEdgeProperties_DetectsMismatch(t *testing.T) {
	cfg := Config{Seed: 1, MaxTicks: 1, Workload: edgePropertiesWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()
	if _, err := sm.engine.RunWrite(ctx, tmplCreatePerson, map[string]any{"name": "A", "age": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := sm.engine.RunWrite(ctx, tmplCreatePerson, map[string]any{"name": "B", "age": int64(1)}); err != nil {
		t.Fatal(err)
	}
	sm.oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": "A", "age": int64(1)})
	sm.oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": "B", "age": int64(1)})
	if _, err := sm.engine.RunWrite(ctx, tmplCreateKnowsProps, map[string]any{"a": "A", "b": "B", "since": "2026-01-01", "weight": 1.0}); err != nil {
		t.Fatal(err)
	}
	// Model a DIFFERENT weight than the engine stored.
	sm.oracle.ApplyCreate(tmplCreateKnowsProps, map[string]any{"a": "A", "b": "B", "since": "2026-01-01", "weight": 999.0})
	if v := CheckEdgeProperties(1, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("checker FAILED to detect an edge-property mismatch")
	}
}
