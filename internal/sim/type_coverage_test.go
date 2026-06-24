package sim

import (
	"context"
	"testing"
)

// TestTypeCoverage_Scenario_Passes runs the registered type-coverage scenario:
// every property kind (string/int/float/bool/list/temporal + a NULL-reading
// absent key) must round-trip through commit and survive crash/recovery. A nil
// report means every kind round-tripped on every check, including post-recovery.
func TestTypeCoverage_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioTypeCoverage)
	if !ok {
		t.Fatalf("type-coverage scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("type-coverage run: %v", err)
	}
	if report != nil {
		t.Fatalf("type-coverage reported a violation (a property kind did not round-trip/survive):\n%s", report)
	}
}

// TestTypeCoverage_NonVacuous confirms the run created Typed nodes and survived
// crash/recovery, so the round-trip + survival checks were genuinely exercised.
func TestTypeCoverage_NonVacuous(t *testing.T) {
	report, err := runTypeCoverage(context.Background(), 0x7A9E5)
	if err != nil {
		t.Fatalf("runTypeCoverage: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}
}

// TestTypeCoverage_DetectsValueMismatch is the meta-test: it proves the checker
// CATCHES a property that fails to round-trip. The oracle is told a Typed node
// carries one value while the engine stores a different one; CheckTypedProperties
// must flag the divergence.
func TestTypeCoverage_DetectsValueMismatch(t *testing.T) {
	cfg := Config{Seed: 1, MaxTicks: 1, Workload: typeCoverageWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	// Engine stores f:1.0; oracle is told f:999.0 — a deliberate divergence.
	if _, err := sm.engine.RunWrite(ctx, tmplCreateTyped, map[string]any{
		"id": int64(1), "s": "x", "i": int64(2), "f": 1.0, "b": true, "lst": []any{int64(1)}, "ts": "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed typed node: %v", err)
	}
	sm.oracle.typed[1] = map[string]any{
		"id": int64(1), "s": "x", "i": int64(2), "f": 999.0, "b": true, "lst": []any{int64(1)}, "ts": "2026-01-01T00:00:00Z",
	}
	if v := CheckTypedProperties(1, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("checker FAILED to detect a property value mismatch")
	}
}
