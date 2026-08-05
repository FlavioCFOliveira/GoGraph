package mvcc

// horizon_capacity_test.go — rmp #2315: the reclamation horizon's capacity is
// pinned, in both directions, at a stated number.
//
// Layer: short.

import "testing"

// wantHorizonCapacity is the capacity this test enforces, written as a LITERAL and
// deliberately not as [horizonSlots].
//
// Asserting against the constant would make the test tautological: shrinking
// horizonSlots would shrink the expectation with it and the test would still pass,
// which is exactly the silent regression AC 3 asks to prevent. The number is stated
// here so a change to capacity has to change this file too, and reviewing that change
// means reading why the number is what it is.
//
// If you are raising capacity deliberately, update this constant and the measured
// table in docs/benchmarks/mvcc-horizon-sizing-2026-08-05.md together.
const wantHorizonCapacity = 1024

// TestHorizon_CapacityIsPinned is the cheap half: the compiled capacity is the number
// the design chose.
func TestHorizon_CapacityIsPinned(t *testing.T) {
	if horizonSlots != wantHorizonCapacity {
		t.Fatalf("horizon capacity is %d, want %d. Capacity is a MEASURED decision, not a "+
			"free parameter: see the rationale on horizonSlots and the table in "+
			"docs/benchmarks/mvcc-horizon-sizing-2026-08-05.md. Lowering it moves the "+
			"reclamation-suspension cliff closer, and past that cliff version memory has no "+
			"bound.", horizonSlots, wantHorizonCapacity)
	}
	if horizonSlots&(horizonSlots-1) != 0 {
		t.Fatalf("horizon capacity %d is not a power of two, so Enter's mask is wrong", horizonSlots)
	}
}

// TestHorizon_ExhaustionCliffAtCapacity is the behavioural half, and it asserts the
// three things AC 3 names: at capacity every reader is registered and the watermark
// advances; one past capacity a reader is unregistered and the watermark collapses to
// zero; after the readers leave, both recover.
//
// It exercises the REAL capacity rather than a scaled-down stand-in, so it fails if the
// capacity regresses in either direction — too few slots and the cliff arrives early,
// too many and the "one past capacity" reader still gets a slot.
func TestHorizon_ExhaustionCliffAtCapacity(t *testing.T) {
	var h Horizon

	// Fill exactly to capacity. Start timestamps 10, 11, 12, … so the oldest is
	// unambiguous and a wrong watermark is visible rather than plausible.
	slots := make([]int, 0, wantHorizonCapacity)
	for i := 0; i < wantHorizonCapacity; i++ {
		s := h.Enter(uint64(10 + i))
		if s == unregistered {
			t.Fatalf("reader %d of %d failed to register: capacity is smaller than the "+
				"design states, so reclamation suspends earlier than intended",
				i+1, wantHorizonCapacity)
		}
		slots = append(slots, s)
	}
	if got := h.Unregistered(); got != 0 {
		t.Fatalf("at capacity: UnregisteredSnapshots = %d, want 0", got)
	}
	if got, want := h.Oldest(1<<40), uint64(10); got != want {
		t.Fatalf("at capacity: Oldest = %d, want %d — the watermark must still advance "+
			"with every slot taken, or reclamation is suspended at capacity rather than "+
			"past it", got, want)
	}

	// One past capacity: the cliff.
	over := h.Enter(500)
	if over != unregistered {
		t.Fatalf("reader %d got slot %d: capacity is LARGER than the design states, so the "+
			"measured table and the memory cost in the horizonSlots rationale are both wrong",
			wantHorizonCapacity+1, over)
	}
	if got := h.Unregistered(); got == 0 {
		t.Fatal("past capacity: UnregisteredSnapshots = 0, want non-zero — an unregistered " +
			"reader that is not observable is the failure this metric exists to prevent")
	}
	if got := h.Oldest(1 << 40); got != 0 {
		t.Fatalf("past capacity: Oldest = %d, want 0. An unregistered reader's start "+
			"timestamp is not represented in the slots, so any non-zero watermark could "+
			"free versions that reader still needs", got)
	}

	// Recovery, in both quantities.
	h.Leave(over)
	if got := h.Unregistered(); got != 0 {
		t.Fatalf("after the unregistered reader left: UnregisteredSnapshots = %d, want 0", got)
	}
	if got, want := h.Oldest(1<<40), uint64(10); got != want {
		t.Fatalf("after the unregistered reader left: Oldest = %d, want %d — reclamation "+
			"must resume as soon as the last unregistered reader is gone", got, want)
	}
	for _, s := range slots {
		h.Leave(s)
	}
	if got, want := h.Oldest(777), uint64(777); got != want {
		t.Fatalf("fully drained: Oldest = %d, want the fallback %d", got, want)
	}
}
