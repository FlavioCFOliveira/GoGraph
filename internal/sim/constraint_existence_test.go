package sim

import (
	"context"
	"testing"
)

// TestConstraintExistence_Scenario_Passes runs the registered
// constraint-existence scenario (#1754): the engine must reject every
// email-omitting CREATE under the NOT NULL constraint exactly as expected, hold
// the per-op accept/reject parity, and keep enforcing across crash/recovery. A
// clean (nil) report means every decision matched and the constraint survived
// every recovery.
func TestConstraintExistence_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioConstraintExistence)
	if !ok {
		t.Fatalf("constraint-existence scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("constraint-existence run: %v", err)
	}
	if report != nil {
		t.Fatalf("constraint-existence reported a violation (enforcement gap):\n%s", report)
	}
}

// TestConstraintExistence_NonVacuous asserts the run genuinely exercised both
// arms — at least one omit was rejected (the constraint actually bit) and the
// email-bearing creates populated the graph — and that crash/recovery happened so
// the survives-recovery property was tested.
func TestConstraintExistence_NonVacuous(t *testing.T) {
	sc := constraintExistenceScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()
	if err := sm.engineRunDDL(ctx, constraintExistenceDDL); err != nil {
		t.Fatalf("create constraint: %v", err)
	}
	sm.Oracle().SetExistenceOnEmail(true)

	report, err := sm.runExistenceLoop(ctx)
	if err != nil {
		t.Fatalf("runExistenceLoop: %v", err)
	}
	if report != nil {
		t.Fatalf("enforcement gap:\n%s", report)
	}
	if sm.RejectedWrites() == 0 {
		t.Fatalf("vacuous run: no omit was ever rejected; the NOT NULL constraint never bit")
	}
	if sm.CrashCount() == 0 {
		t.Fatalf("expected crash/recovery cycles to test the survives-recovery property")
	}
	t.Logf("constraint-existence: rejectedOmits=%d crashes=%d", sm.RejectedWrites(), sm.CrashCount())
}

// TestConstraintExistence_DetectsEnforcementGap is the meta-test: it proves the
// harness CATCHES an existence-enforcement failure. The loop runs WITHOUT creating
// the NOT NULL constraint in the engine, so the engine happily commits an
// email-omitting CREATE the loop expects it to reject. The per-op comparison must
// flag that disagreement as an ACID_CONSISTENCY violation.
func TestConstraintExistence_DetectsEnforcementGap(t *testing.T) {
	sc := constraintExistenceScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	// Disable crashes for this meta-test: it only needs the first omitting CREATE
	// to surface the gap, deterministically.
	cfg.Crash = CrashConfig{}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	// Model an active NOT NULL constraint in the ORACLE without creating it in the
	// engine: the engine is now (deliberately) not enforcing what the oracle
	// expects, so it commits an email-omitting CREATE the oracle predicts rejected.
	sm.Oracle().SetExistenceOnEmail(true)

	report, err := sm.runExistenceLoop(ctx)
	if err != nil {
		t.Fatalf("runExistenceLoop: %v", err)
	}
	if report == nil {
		t.Fatal("harness FAILED to detect the existence-enforcement gap: an email-omitting CREATE the engine committed (no constraint) was not flagged")
	}
	if len(report.Violations) == 0 || report.Violations[0].Kind != ViolationACIDConsistency {
		t.Fatalf("expected an ACID_CONSISTENCY enforcement-gap violation, got: %+v", report.Violations)
	}
}
