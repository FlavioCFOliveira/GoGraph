//go:build soak

package sim

// count_store_soak_test.go — the soak-layer arms of the count-store oracle
// scenario (rmp #2494).
//
// Three claims live here rather than in the short layer, each for its own
// reason:
//
//   - The `Cells()` BOUNDEDNESS claim needs |E| to have actually grown. The
//     design says the store's footprint is a function of schema cardinality and
//     never of |V| or |E| (docs/count-store-design.md §2.3); a short-budget run
//     reaches a few dozen edges, which is the same order as the ceiling itself,
//     so the claim is not yet distinguishable from "both numbers are small".
//     [TestCountStore_SoakCellsBoundedIndependentlyOfEdges] drives |E| into the
//     hundreds and asserts the ceiling still holds — and asserts the RATIO, so a
//     regression that made the footprint creep with the data is visible as a
//     number rather than only as a threshold breach.
//   - The seed SWEEP. Every non-vacuity gate in [CountStoreEvidence.Finish] is a
//     claim about what a run REACHES, and a claim verified on one seed is a claim
//     about one seed. This scenario has already been bitten once: the first
//     version of the negative-cell fixture reused the shared `Vip` label, and its
//     `T(Vip,KNOWS,Vip)` cell was negative at every live observation for the
//     catalogue seed and not for the next one, so the heal clause fired as
//     vacuous only when a second seed was tried. The dedicated `Neg` label
//     removed the exposure BY CONSTRUCTION; the sweep is what proves the removal
//     holds rather than only sounds right.
//   - The FORCED-CRASH-ONLY arm, which proves the constructed recovery is not a
//     fallback the seeded schedule always pre-empts.

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// The soak budgets. countStoreSoakTicks is large enough that the modelled edge
// set reaches several hundred — an order of magnitude above the combinatorial
// cell ceiling — which is what makes the boundedness claim measurable.
const (
	countStoreSoakSeeds = 16
	countStoreSoakTicks = 1500
	// countStoreSoakMinEdges is the |E| floor the boundedness arm requires. It is
	// enforced by the run itself through [CountStoreConfig.MinEdgesForBoundClaim],
	// so a budget change that stops growing the graph fails the run rather than
	// passing a vacuous claim.
	countStoreSoakMinEdges = 200
)

// TestCountStore_SoakCellsBoundedIndependentlyOfEdges is the boundedness
// assertion the acceptance criteria place in soak.
//
// It asserts three separate things, because they fail for different reasons:
// that the footprint never exceeded the combinatorial ceiling of the observed
// vocabulary; that |E| grew far past that ceiling, so the first assertion is
// about a graph big enough for the claim to mean something; and that the run
// reported the |E| floor it was given rather than silently falling short of it.
func TestCountStore_SoakCellsBoundedIndependentlyOfEdges(t *testing.T) {
	defer goleak.VerifyNone(t)

	cfg := DefaultCountStoreConfig(countStoreDefaultSeed)
	cfg.MaxTicks = countStoreSoakTicks
	cfg.MinEdgesForBoundClaim = countStoreSoakMinEdges

	ev, report, err := RunCountStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("reported a violation:\n%s", report)
	}
	if ev.MaxCells > ev.MaxBound {
		t.Fatalf("Cells() reached %d, above the ceiling %d implied by the observed vocabulary",
			ev.MaxCells, ev.MaxBound)
	}
	if ev.MaxEdges < countStoreSoakMinEdges {
		t.Fatalf("the run modelled only %d edge(s); the boundedness claim needs at least %d to be "+
			"distinguishable from \"both numbers are small\"", ev.MaxEdges, countStoreSoakMinEdges)
	}
	// The claim in its strongest legible form: the footprint is a small multiple of
	// the schema's combinatorial size and a small FRACTION of the data's size.
	if int64(ev.MaxCells)*4 > ev.MaxEdges {
		t.Fatalf("Cells()=%d is not small relative to |E|=%d (ceiling %d): the footprint appears to "+
			"track the data rather than the schema", ev.MaxCells, ev.MaxEdges, ev.MaxBound)
	}
	t.Logf("boundedness: Cells() max %d, ceiling %d, |E| max %d (Cells() is %.1f%% of |E|); %s",
		ev.MaxCells, ev.MaxBound, ev.MaxEdges,
		100*float64(ev.MaxCells)/float64(ev.MaxEdges), ev.String())
}

// TestCountStore_SoakSeedSweep drives the scenario across many derived seeds at a
// larger budget and requires every run to be clean.
//
// A violation here names the seed, and because the scenario is deterministic the
// seed replays the failure exactly: `go run ./cmd/sim -scenario=count-store
// <seed>`.
func TestCountStore_SoakSeedSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < countStoreSoakSeeds; i++ {
		seed := countStoreDefaultSeed ^ (uint64(i+1) * 0x9E37_79B9_7F4A_7C15)
		cfg := DefaultCountStoreConfig(seed)
		cfg.MaxTicks = countStoreSoakTicks / 2

		ev, report, err := RunCountStore(context.Background(), cfg)
		if err != nil {
			t.Fatalf("seed %d: run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %d reported a violation:\n%s", seed, report)
		}
		// The two heal counters are the scenario's core claim, and they are the ones
		// whose precondition used to depend on a draw. Asserting them per seed is
		// what makes a regression in the CONSTRUCTION visible here rather than only
		// on whichever seed happens to expose it.
		if ev.HealedFromDirty == 0 || ev.HealedNegative == 0 {
			t.Fatalf("seed %d: the reopen's heal was never witnessed (dirty=%d negative=%d): %s",
				seed, ev.HealedFromDirty, ev.HealedNegative, ev.String())
		}
		t.Logf("seed %d: %s", seed, ev.ReproducibleSummary())
	}
}

// TestCountStore_SoakDeterminismSweep re-runs each seed and requires the
// reproducible evidence to match.
//
// It is a separate sweep from the run above because the two claims are different:
// that one asserts the invariants hold, this one asserts the harness is a pure
// function of the seed. A digest can be stable for one seed and not for the draw
// sequence another seed takes.
func TestCountStore_SoakDeterminismSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < countStoreSoakSeeds; i++ {
		seed := countStoreDefaultSeed ^ (uint64(i+1) * 0x51ED_2701_C0FF_EE01)
		cfg := DefaultCountStoreConfig(seed)
		cfg.MaxTicks = countStoreSoakTicks / 4

		first, report, err := RunCountStore(context.Background(), cfg)
		if err != nil || report != nil {
			t.Fatalf("seed %d: first run err=%v report=%v", seed, err, report)
		}
		second, report, err := RunCountStore(context.Background(), cfg)
		if err != nil || report != nil {
			t.Fatalf("seed %d: second run err=%v report=%v", seed, err, report)
		}
		if first.ReproducibleSummary() != second.ReproducibleSummary() {
			t.Fatalf("seed %d is NOT reproducible:\nfirst:  %s\nsecond: %s",
				seed, first.ReproducibleSummary(), second.ReproducibleSummary())
		}
	}
}

// TestCountStore_SoakForcedCrashOnlySweep drives every seed with the crash
// SCHEDULE disabled, so the only recovery in each run is the constructed one.
//
// It proves the forced crash carries the recovered-phase clauses on its own: with
// the schedule off, every seed must still reach a recovered observation and both
// heal counters, and every seed must report exactly one crash. A run reporting
// two would mean [CountStoreConfig.normalise] had silently re-enabled the
// schedule — precisely the defaulting that function documents itself as not
// doing.
func TestCountStore_SoakForcedCrashOnlySweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < countStoreSoakSeeds; i++ {
		seed := countStoreDefaultSeed ^ (uint64(i+1) * 0xD1B5_4A32_D192_ED03)
		cfg := DefaultCountStoreConfig(seed)
		cfg.MaxTicks = countStoreSoakTicks / 4
		cfg.Crash = CrashConfig{}

		ev, report, err := RunCountStore(context.Background(), cfg)
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
		if ev.RecoveredChecks == 0 || ev.HealedFromDirty == 0 || ev.HealedNegative == 0 {
			t.Fatalf("seed %d: the forced crash did not supply the recovered-phase coverage: %s",
				seed, ev.String())
		}
	}
}
