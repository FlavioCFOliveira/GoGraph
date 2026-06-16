package sim

import (
	"testing"
)

// TestRegistry_RejectsDuplicateNames verifies the registry refuses two
// scenarios that share a name rather than silently shadowing one.
func TestRegistry_RejectsDuplicateNames(t *testing.T) {
	t.Parallel()
	_, err := NewRegistry(
		Scenario{Name: "dup", Mode: ModeDeterministic},
		Scenario{Name: "dup", Mode: ModeDeterministic},
	)
	if err == nil {
		t.Fatal("expected an error for a duplicate scenario name")
	}
}

// TestRegistry_RejectsEmptyName verifies an unnamed scenario is rejected.
func TestRegistry_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	if _, err := NewRegistry(Scenario{Mode: ModeDeterministic}); err == nil {
		t.Fatal("expected an error for an empty scenario name")
	}
}

// TestRegistry_ListsAndLooksUpSorted verifies Names is sorted and Lookup
// resolves by name.
func TestRegistry_ListsAndLooksUpSorted(t *testing.T) {
	t.Parallel()
	r, err := NewRegistry(
		Scenario{Name: "zebra", Mode: ModeDeterministic},
		Scenario{Name: "alpha", Mode: ModeDeterministic},
		Scenario{Name: "mike", Mode: ModeDeterministic},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := r.Names(); len(got) != 3 || got[0] != "alpha" || got[1] != "mike" || got[2] != "zebra" {
		t.Fatalf("Names not sorted: %v", got)
	}
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}
	sc, ok := r.Lookup("mike")
	if !ok || sc.Name != "mike" {
		t.Fatalf("Lookup(mike) = %v, %v", sc, ok)
	}
	if _, ok := r.Lookup("absent"); ok {
		t.Fatal("Lookup(absent) should be false")
	}
}

// TestExecMode_Reproducible verifies only the deterministic mode is flagged as
// trace-replayable.
func TestExecMode_Reproducible(t *testing.T) {
	t.Parallel()
	if !ModeDeterministic.Reproducible() {
		t.Fatal("deterministic mode must be reproducible")
	}
	for _, m := range []ExecMode{ModeConcurrent, ModeLiveness, ModeBulkVsOnline} {
		if m.Reproducible() {
			t.Fatalf("mode %s must not be reproducible", m)
		}
	}
}

// TestScenario_RunDeterministicClean runs a tiny deterministic scenario and
// asserts it passes (nil report).
func TestScenario_RunDeterministicClean(t *testing.T) {
	t.Parallel()
	sc := Scenario{
		Name:        "tiny",
		Mode:        ModeDeterministic,
		DefaultSeed: 7,
		MaxTicks:    200,
		Workload:    DefaultWorkload,
	}
	report, err := sc.Run(t.Context(), sc.resolveSeed(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report != nil {
		t.Fatalf("expected clean run, got report:\n%s", report)
	}
}
