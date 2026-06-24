package sim

import (
	"context"
	"testing"
)

// TestConstraintEnforce_Scenario_Passes runs the registered constraint-enforce
// scenario: the engine must reject every duplicate-name CREATE under the UNIQUE
// constraint exactly as the oracle predicts, hold engine/oracle parity, and keep
// enforcing across crash/recovery. A clean (nil) report means every accept/reject
// decision matched and the constraint survived every recovery.
func TestConstraintEnforce_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioConstraintEnforce)
	if !ok {
		t.Fatalf("constraint-enforce scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("constraint-enforce run: %v", err)
	}
	if report != nil {
		t.Fatalf("constraint-enforce reported a violation (enforcement gap):\n%s", report)
	}
}

// TestConstraintEnforce_NonVacuous asserts the run genuinely exercised both arms
// — at least one duplicate was rejected (the constraint actually bit) and a
// non-trivial set of unique names committed — and that crash/recovery happened so
// the survives-recovery property was tested.
func TestConstraintEnforce_NonVacuous(t *testing.T) {
	sc := constraintEnforceScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()
	if err := sm.engineRunDDL(ctx, constraintEnforceDDL); err != nil {
		t.Fatalf("create constraint: %v", err)
	}
	sm.Oracle().SetUniqueOnName(true)

	report, err := sm.runConstraintLoop(ctx)
	if err != nil {
		t.Fatalf("runConstraintLoop: %v", err)
	}
	if report != nil {
		t.Fatalf("enforcement gap:\n%s", report)
	}
	// rejectedWrites accumulates every CREATE the engine refused — under this
	// scenario those are exactly the UNIQUE-violating duplicates.
	if sm.RejectedWrites() == 0 {
		t.Fatalf("vacuous run: no duplicate was ever rejected; the UNIQUE constraint never bit")
	}
	if sm.Oracle().NodeCount() == 0 {
		t.Fatalf("expected unique-name creates to populate the graph")
	}
	if sm.CrashCount() == 0 {
		t.Fatalf("expected crash/recovery cycles to test the survives-recovery property")
	}
	t.Logf("constraint-enforce: rejectedDuplicates=%d uniqueNodes=%d crashes=%d",
		sm.RejectedWrites(), sm.Oracle().NodeCount(), sm.CrashCount())
}

// TestConstraintEnforce_DetectsEnforcementGap is the meta-test required by the
// AC: it proves the harness CATCHES an enforcement failure. The oracle is told a
// UNIQUE constraint is active, but NO constraint is created in the engine — so
// the engine happily commits a duplicate the oracle predicts it must reject. The
// per-op comparison must flag that disagreement as an ACID_CONSISTENCY violation.
func TestConstraintEnforce_DetectsEnforcementGap(t *testing.T) {
	cfg := Config{Seed: 0xC047A157, MaxTicks: 200, Workload: WriteHeavyWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	// Model an active UNIQUE constraint WITHOUT creating it in the engine: the
	// engine is now (deliberately) not enforcing what the oracle expects.
	sm.Oracle().SetUniqueOnName(true)

	report, err := sm.runConstraintLoop(ctx)
	if err != nil {
		t.Fatalf("runConstraintLoop: %v", err)
	}
	if report == nil {
		t.Fatal("harness FAILED to detect the enforcement gap: a duplicate CREATE the engine committed (no constraint) was not flagged")
	}
	if len(report.Violations) == 0 || report.Violations[0].Kind != ViolationACIDConsistency {
		t.Fatalf("expected an ACID_CONSISTENCY enforcement-gap violation, got: %+v", report.Violations)
	}
}
