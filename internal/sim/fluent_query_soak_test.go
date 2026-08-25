//go:build soak

package sim

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// fluentQuerySoakSeeds is how many derived seeds the soak sweep drives, and
// fluentQuerySoakTicks the per-seed tick budget.
//
// The sweep exists because every non-vacuity gate in [FluentQueryProbes.Finish]
// is a claim about what a run REACHES, and a claim verified on one seed is a
// claim about one seed. #2491 shipped a fixture that was unbuildable for 120 of
// 200 seeds and passed only because the catalogue seed was lucky; the short
// layer's single-seed run has exactly that exposure for the gates that depend on
// the workload's draws (a crash landing inside the budget, a churn victim being
// available, an age-bearing Person existing at a battery).
const (
	fluentQuerySoakSeeds = 24
	fluentQuerySoakTicks = 500
)

// TestFluentQuery_SoakSeedSweep drives the scenario across many derived seeds at
// a larger budget and requires every run to be clean.
//
// A violation here names the seed, and because the scenario is deterministic the
// seed replays the failure exactly: `go run ./cmd/sim -scenario=fluent-query
// <seed>`.
func TestFluentQuery_SoakSeedSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < fluentQuerySoakSeeds; i++ {
		seed := fluentQueryDefaultSeed ^ (uint64(i+1) * 0x9E37_79B9_7F4A_7C15)
		cfg := DefaultFluentQueryConfig(seed)
		cfg.MaxTicks = fluentQuerySoakTicks

		ev, report, err := RunFluentQuery(context.Background(), cfg)
		if err != nil {
			t.Fatalf("seed %d: run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %d reported a violation:\n%s", seed, report)
		}
		// The claim in docs/dst-feature-coverage.md that the raw and live-filtered
		// CSR builds never differ ON THE LIVE GRAPH is a claim about EVERY battery,
		// so it is asserted per seed rather than read off the last battery's pair.
		// A non-zero value is not a defect — it would mean the live graph started
		// producing ghost arcs, which would make the invariance clause non-vacuous
		// there too — so it is reported loudly instead of failed.
		if ev.CSRGenerationsDiffered != 0 {
			t.Logf("seed %d: NOTE the two CSR generations differed at %d of %d batteries; the live "+
				"graph now produces ghost arcs, so docs/dst-feature-coverage.md's claim that it does "+
				"not needs revisiting", seed, ev.CSRGenerationsDiffered, ev.Batteries)
		}
		// The gates already ran inside RunFluentQuery; logging what each seed
		// exercised is what makes a future budget change that quietly stops
		// reaching a phase visible in the soak log rather than invisible.
		t.Logf("seed %d: %s", seed, ev.ReproducibleSummary())
	}
}

// TestFluentQuery_SoakDeterminismSweep re-runs each seed and requires the
// reproducible evidence to match.
//
// It is a separate sweep from the run above because the two claims are
// different: that one asserts the invariants hold, this one asserts the harness
// is a pure function of the seed. A harness that drifts makes every failure it
// ever reports unreplayable, so the claim is worth its own sweep — and worth
// making across seeds, since a digest can be stable for one seed and not for the
// draw sequence another seed takes.
func TestFluentQuery_SoakDeterminismSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < fluentQuerySoakSeeds; i++ {
		seed := fluentQueryDefaultSeed ^ (uint64(i+1) * 0x51ED_2701_C0FF_EE01)
		cfg := DefaultFluentQueryConfig(seed)
		cfg.MaxTicks = fluentQuerySoakTicks / 2

		first, report, err := RunFluentQuery(context.Background(), cfg)
		if err != nil || report != nil {
			t.Fatalf("seed %d: first run err=%v report=%v", seed, err, report)
		}
		second, report, err := RunFluentQuery(context.Background(), cfg)
		if err != nil || report != nil {
			t.Fatalf("seed %d: second run err=%v report=%v", seed, err, report)
		}
		if first.ReproducibleSummary() != second.ReproducibleSummary() {
			t.Fatalf("seed %d is NOT reproducible:\nfirst:  %s\nsecond: %s",
				seed, first.ReproducibleSummary(), second.ReproducibleSummary())
		}
	}
}
