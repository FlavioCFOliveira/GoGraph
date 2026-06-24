package sim

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestDefaultRegistry_ListsAllScenarios verifies the catalogue registers exactly
// the ten named scenarios.
func TestDefaultRegistry_ListsAllScenarios(t *testing.T) {
	t.Parallel()
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	want := []string{
		ScenarioBadActors, ScenarioBulkVsOnline, ScenarioCrashStorm, ScenarioLongRunning,
		ScenarioOverload, ScenarioReadHeavy, ScenarioSchemaChaos, ScenarioSearch, ScenarioSearchCrash,
		ScenarioWriteHeavy,
	}
	if r.Len() != len(want) {
		t.Fatalf("registry has %d scenarios, want %d: %v", r.Len(), len(want), r.Names())
	}
	for _, name := range want {
		if _, ok := r.Lookup(name); !ok {
			t.Fatalf("scenario %q missing from catalogue", name)
		}
	}
}

// TestCatalogue_SmokeSubsetRunsClean is the short-layer catalogue smoke: it
// runs a cheap representative subset of the catalogue (a deterministic
// read-heavy and write-heavy scenario, each at a small tick budget) and asserts
// each passes (nil report). It keeps the scenario-run path wired on every PR;
// the full eight-scenario sweep is in the soak lane below.
func TestCatalogue_SmokeSubsetRunsClean(t *testing.T) {
	defer goleak.VerifyNone(t)

	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}

	for _, name := range []string{ScenarioReadHeavy, ScenarioWriteHeavy} {
		sc, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("scenario %q missing from catalogue", name)
		}
		sc.MaxTicks = 200
		t.Run(sc.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			report, err := sc.Run(ctx, sc.DefaultSeed)
			if err != nil {
				t.Fatalf("scenario %q run error: %v", sc.Name, err)
			}
			if report != nil {
				t.Fatalf("scenario %q reported a violation:\n%s", sc.Name, report)
			}
		})
	}
}

// TestCatalogue_EachScenarioRunsClean runs every catalogue scenario (the
// long-running one with a short-layer-sized budget) and asserts each passes
// (nil report) — clean or with only expected errors, which the scenario's own
// classification already folds into "clean". goleak guards the concurrent ones.
//
// Gated to the soak layer: running all eight scenarios end-to-end (several of
// them concurrent or DDL-churning) is minutes-long under -race. The short-layer
// TestCatalogue_SmokeSubsetRunsClean covers the scenario-run path on every PR.
func TestCatalogue_EachScenarioRunsClean(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}

	for _, sc := range r.Scenarios() {
		sc := sc
		// The long-running scenario's full budget is a soak workload; here run a
		// short, representative slice so the catalogue stays under the short ceiling.
		if sc.Name == ScenarioLongRunning {
			sc.MaxTicks = 2000
		}
		t.Run(sc.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			report, err := sc.Run(ctx, sc.DefaultSeed)
			if err != nil {
				t.Fatalf("scenario %q run error: %v", sc.Name, err)
			}
			if report != nil {
				t.Fatalf("scenario %q reported a violation:\n%s", sc.Name, report)
			}
		})
	}
}
