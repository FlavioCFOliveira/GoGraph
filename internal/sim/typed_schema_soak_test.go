//go:build soak

package sim

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// typedSchemaSoakSeeds is how many derived seeds the soak sweep drives, and
// typedSchemaSoakTicks the per-seed tick budget.
//
// The sweep exists because every non-vacuity gate in
// [TypedSchemaEvidence.Finish] is a claim about what a run REACHES, and a claim
// verified on one seed is a claim about one seed. #2491 shipped a fixture that
// was unbuildable for 120 of 200 seeds and passed only because the catalogue
// seed was lucky; the short layer's single-seed run has that exposure for every
// gate whose precondition depends on a draw — which of the three durable shapes
// an accept arm picks, which declared kind a mismatch is constructed from, and
// which pin target an epoch lands on.
//
// The coverage sweep and the forced crash remove that exposure by construction
// for the fifteen cells and for the post-recovery clauses; the sweep is what
// proves the removal actually holds across seeds rather than only in the
// argument.
const (
	typedSchemaSoakSeeds = 24
	typedSchemaSoakTicks = 600
)

// TestTypedSchema_SoakSeedSweep drives the scenario across many derived seeds at
// a larger budget and requires every run to be clean.
//
// A violation here names the seed, and because the scenario is deterministic the
// seed replays the failure exactly: `go run ./cmd/sim -scenario=typed-schema
// <seed>`.
func TestTypedSchema_SoakSeedSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < typedSchemaSoakSeeds; i++ {
		seed := typedSchemaDefaultSeed ^ (uint64(i+1) * 0x9E37_79B9_7F4A_7C15)
		cfg := DefaultTypedSchemaConfig(seed)
		cfg.MaxTicks = typedSchemaSoakTicks

		ev, report, err := RunTypedSchema(context.Background(), cfg)
		if err != nil {
			t.Fatalf("seed %d: run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %d reported a violation:\n%s", seed, report)
		}
		// The pure-store PIN is the scenario's one measurement of behaviour it does
		// not endorse, so the sweep says out loud, per seed, that the resurrection
		// is still what happens. If a future change fixes the fsync/validate
		// ordering, the run itself fails first (the pin's clause is unconditional);
		// this line is what makes the state of the finding legible in the soak log
		// without reading the source.
		if ev.PureStoreResurrected == 0 {
			t.Fatalf("seed %d: the pure-store arm no longer observes the resurrection, yet the run "+
				"passed: %s", seed, ev.String())
		}
		// The gates already ran inside RunTypedSchema; logging what each seed
		// exercised is what makes a future budget change that quietly stops
		// reaching a phase visible in the soak log rather than invisible.
		t.Logf("seed %d: %s", seed, ev.ReproducibleSummary())
	}
}

// TestTypedSchema_SoakDeterminismSweep re-runs each seed and requires the
// reproducible evidence to match.
//
// It is a separate sweep from the run above because the two claims are different:
// that one asserts the invariants hold, this one asserts the harness is a pure
// function of the seed. A harness that drifts makes every failure it ever reports
// unreplayable, so the claim is worth its own sweep — and worth making across
// seeds, since a digest can be stable for one seed and not for the draw sequence
// another seed takes.
func TestTypedSchema_SoakDeterminismSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < typedSchemaSoakSeeds; i++ {
		seed := typedSchemaDefaultSeed ^ (uint64(i+1) * 0x51ED_2701_C0FF_EE01)
		cfg := DefaultTypedSchemaConfig(seed)
		cfg.MaxTicks = typedSchemaSoakTicks / 2

		first, report, err := RunTypedSchema(context.Background(), cfg)
		if err != nil || report != nil {
			t.Fatalf("seed %d: first run err=%v report=%v", seed, err, report)
		}
		second, report, err := RunTypedSchema(context.Background(), cfg)
		if err != nil || report != nil {
			t.Fatalf("seed %d: second run err=%v report=%v", seed, err, report)
		}
		if first.ReproducibleSummary() != second.ReproducibleSummary() {
			t.Fatalf("seed %d is NOT reproducible:\nfirst:  %s\nsecond: %s",
				seed, first.ReproducibleSummary(), second.ReproducibleSummary())
		}
	}
}

// TestTypedSchema_SoakForcedCrashOnlySweep drives every seed with the crash
// SCHEDULE disabled, so the only recovery in each run is the forced one.
//
// It is the sweep that proves the forced-crash arm is not merely a fallback the
// catalogue seed never takes: with the schedule off, every seed must still reach
// the pin and the post-recovery witness read, and every seed must report exactly
// one crash. A run that reported two would mean the schedule was silently
// re-enabled by [TypedSchemaConfig.normalise], which is precisely the defaulting
// that function documents itself as NOT doing.
func TestTypedSchema_SoakForcedCrashOnlySweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < typedSchemaSoakSeeds; i++ {
		seed := typedSchemaDefaultSeed ^ (uint64(i+1) * 0xD1B5_4A32_D192_ED03)
		cfg := DefaultTypedSchemaConfig(seed)
		cfg.MaxTicks = typedSchemaSoakTicks / 2
		cfg.Crash = CrashConfig{}

		ev, report, err := RunTypedSchema(context.Background(), cfg)
		if err != nil {
			t.Fatalf("seed %d: run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %d reported a violation:\n%s", seed, report)
		}
		if ev.Crashes != 1 || ev.ForcedCrashes != 1 {
			t.Fatalf("seed %d: crashes=%d forced=%d, want exactly 1 of each with the schedule off: %s",
				seed, ev.Crashes, ev.ForcedCrashes, ev.String())
		}
		if ev.PinNoValidatorAccepted == 0 || ev.WitnessReadsAfterRecovery == 0 {
			t.Fatalf("seed %d: the forced crash did not supply the recovery coverage: %s",
				seed, ev.String())
		}
	}
}
