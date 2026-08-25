package sim

// generation_swap_wide_test.go — rmp #2491: the wide-fleet arms of the
// generation-swap scenario.
//
// generation_swap_test.go runs the scenario at 2, 8, 32 and 64 readers, which
// is the range in which a width-dependent failure would first show on a
// developer's machine. This file pushes the fleet to the concurrency levels
// CLAUDE.md publishes (64, 256, 1024 goroutines) and repeats the run across a
// 64-seed sweep, so many plan geometries are exercised as well as many widths.
// 64 appears in both files deliberately: it is the seam, and a divergence
// between the two at the shared width would be a harness defect worth seeing.
//
// # Why this is SHORT layer, and what it used to claim
//
// These arms were originally gated behind `//go:build soak || nightly` with the
// justification that "at 1024 readers the reader fleet alone performs on the
// order of a million Acquire/Release pairs per seed, which is minutes of work
// under the race detector". Measurement refuted that by three orders of
// magnitude and the gate was removed (rmp #2491 validation pass, darwin/arm64,
// 10 logical cores, under -race):
//
//	one run at 1024 readers          : 10,247 acquisitions in 37 ms
//	one run at  256 readers          :  3,223 acquisitions in 23 ms
//	TestGenerationSwap_WideFleets    : 0.28-0.29 s (3 widths x 5 seeds)
//	TestGenerationSwap_SeedSweep     : 0.17-0.18 s (64 seeds)
//	both together                    : 0.46 s (two runs, before and after
//	                                   the promotion)
//
// The reason the estimate was so far out is worth recording, because it is a
// property of the scenario rather than of the machine: a reader performs
// MinOpsPerReader acquisitions and then stops as soon as the publisher is
// done, and the publisher does not pace itself. So MaxOpsPerReader (2048) is
// never approached, and per-reader work FALLS as the fleet widens — about 199
// acquisitions each at 8 readers, 13 at 256, 10 at 1024. Total work grows
// sublinearly with the width. Making the publisher pace against the readers
// would change that, and is tracked as separate work rather than done here.
//
// So these arms are not here for cost reasons at all. They are here for
// FLEET-WIDTH and GEOMETRY diversity, and the rationale is worth stating
// precisely rather than by slogan:
//
//   - WIDTH. The refcount CEILING clause is parameterised on the reader
//     count, so each width evaluates it against a different bound. Note the
//     direction honestly: at 1024 readers the bound is LOOSER, not tighter, so
//     the wide arms do not sharpen that clause. What they add is contention
//     DEPTH — a defect that only manifests when many goroutines are preempted
//     inside Acquire's retry window is only reachable here. That is the stated
//     rationale, not a measured detection rate; see the sensitivity note in
//     generation_swap.go for what the (inherited) measurements do and do not
//     license.
//   - GEOMETRY. The drain-timeout arm's POSITION in the publish sequence is
//     drawn from the master seed, so only a seed sweep varies it. Measured on
//     the 64-seed sweep: 31 distinct positions.
//
// At 0.46 s combined they sit comfortably inside the short layer's per-package
// budget (docs/test-layers.md).

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestGenerationSwap_WideFleets drives the published concurrency levels. The
// refcount CEILING clause is the one that gains most here: it is parameterised
// on the reader count, so each width evaluates it against a different bound
// (see the file header for why that adds contention depth rather than a
// tighter bound). The per-run context deadline is a HANG DETECTOR, not a
// budget: the measured cost of the whole test is ~0.29 s.
func TestGenerationSwap_WideFleets(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, readers := range []int{64, 256, 1024} {
		for _, seed := range genSwapTestSeeds {
			cfg := DefaultGenerationSwapConfig(seed)
			cfg.Readers = readers
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			ev, err := RunGenerationSwap(ctx, cfg)
			cancel()
			if err != nil {
				t.Fatalf("readers=%d seed=%#x: %v", readers, seed, err)
			}
			v := append(checkGenerationSwap(ev), checkGenerationSwapNonVacuity(ev)...)
			if len(v) != 0 {
				t.Fatalf("readers=%d seed=%#x reported:\n%s\nevidence: %s",
					readers, seed, genSwapViolationsText(v), ev)
			}
			t.Logf("readers=%d %s", readers, ev)
		}
	}
}

// TestGenerationSwap_SeedSweep repeats the default fleet across a wide
// seed sweep, so many plan geometries — generation counts, node counts, and
// above all the POSITION of the drain-timeout arm within the publish
// sequence — are exercised. The position matters: the arm leaves its
// predecessor permanently un-drained, and whether the next unbounded drain
// then behaves depends on where in the sequence that happens.
func TestGenerationSwap_SeedSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	const sweep = 64
	seenTimeoutAt := make(map[int]struct{}, sweep)
	for i := 0; i < sweep; i++ {
		seed := uint64(i)*0x9E3779B97F4A7C15 + generationSwapDefaultSeed
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		ev, err := RunGenerationSwap(ctx, DefaultGenerationSwapConfig(seed))
		cancel()
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		v := append(checkGenerationSwap(ev), checkGenerationSwapNonVacuity(ev)...)
		if len(v) != 0 {
			t.Fatalf("seed %#x reported:\n%s\nevidence: %s", seed, genSwapViolationsText(v), ev)
		}
		seenTimeoutAt[ev.DrainTimeoutAt] = struct{}{}
	}
	// Non-vacuity for the sweep itself: a sweep that placed the drain-timeout
	// arm at the same sequence every time would have run 64 copies of one
	// geometry.
	if len(seenTimeoutAt) < 8 {
		t.Fatalf("the %d-seed sweep placed the drain-timeout arm at only %d distinct sequences; the "+
			"sweep is not varying the geometry", sweep, len(seenTimeoutAt))
	}
	t.Logf("%d seeds, drain-timeout arm placed at %d distinct sequences", sweep, len(seenTimeoutAt))
}
