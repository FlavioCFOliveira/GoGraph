//go:build soak

package cypher_test

// delete_scaling_soak_test.go — the WALL-CLOCK half of the rmp #2400 / #2418
// delete-scaling gates.
//
// These two tests assert exactly what the short-layer gates in
// delete_scaling_test.go asserted until 2026-08-19: that the last of six
// seed-and-wipe cycles does not take more than maxCycleRatio times the first,
// measured in wall-clock time. Nothing about the assertion changed — only
// where it runs.
//
// It moved because wall clock is not a load-invariant instrument and the short
// layer is not a quiet machine. Under `make ci` the same flat engine produced
// per-cycle wall times of 637 ms … 2.376 s … 2.074 s (note the DIP) and a
// 3.25x ratio, and a deliberately ramped load reproduced 12.96x — against a
// pre-fix defect signal of 5.2x. The full measurement table is in the header
// of delete_scaling_test.go, together with the reasoning that keeps the
// REGRESSION property in the short layer on a CPU-time instrument instead.
//
// What these tests add over their short-layer counterparts is the user-visible
// property. CPU time is the mechanism of the defect; wall time is what a caller
// actually waits. On the soak layer's quiet machine wall time measures that
// honestly, so the two layers are complementary rather than duplicative.
//
// Neither test calls t.Parallel(): a wall-clock assertion needs the quiet
// machine that a parallel sibling would take away.

import "testing"

// TestDeleteWallTimeDoesNotDegradeAcrossCycles is the wall-clock form of the
// rmp #2400 gate, on a quiet machine.
func TestDeleteWallTimeDoesNotDegradeAcrossCycles(t *testing.T) {
	got := deleteCycles(t, 20_000, 5_000, 6, false, false)
	wall := wallOf(got)
	r := ratio(wall)
	if r > maxCycleRatio {
		t.Fatalf("DELETE wipe time grew %.2fx from the first cycle to the last (limit %.1fx); per-cycle %v",
			r, maxCycleRatio, wall)
	}
	t.Logf("DELETE per-cycle %v (last/first %.2fx)", wall, r)
}

// TestDetachDeleteWallTimeDoesNotDegradeAcrossCycles is the wall-clock form of
// the rmp #2418 gate, on a quiet machine.
func TestDetachDeleteWallTimeDoesNotDegradeAcrossCycles(t *testing.T) {
	got := deleteCycles(t, 5_000, 1_000, 6, true, false)
	wall := wallOf(got)
	r := ratio(wall)
	if r > maxCycleRatio {
		t.Fatalf("DETACH DELETE wipe time grew %.2fx from the first cycle to the last (limit %.1fx); per-cycle %v",
			r, maxCycleRatio, wall)
	}
	t.Logf("DETACH DELETE per-cycle %v (last/first %.2fx)", wall, r)
}
