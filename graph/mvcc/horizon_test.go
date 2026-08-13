package mvcc

// horizon_test.go — the reclamation watermark must never be newer than the
// oldest active reader (rmp #2282).
//
// Layer: short.
//
// That one property is the whole contract, and violating it is silent data
// loss: a reclaimer frees a version a live reader then fails to find. The
// concurrency test below is written specifically to catch the way an earlier
// draft of Horizon broke it — by letting two readers share a slot, so the first
// to leave released the watermark on the second's behalf.

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestHorizon_EmptyReturnsFallback(t *testing.T) {
	var h Horizon
	if got := h.Oldest(42); got != 42 {
		t.Fatalf("Oldest with no reader = %d, want the fallback 42", got)
	}
	if h.Active() != 0 || h.Unregistered() != 0 {
		t.Fatalf("a fresh Horizon reports %d active and %d unregistered, want 0 and 0",
			h.Active(), h.Unregistered())
	}
}

func TestHorizon_HoldsBackToTheOldestReader(t *testing.T) {
	var h Horizon
	a := h.Enter(10)
	b := h.Enter(20)
	if got := h.Oldest(99); got != 10 {
		t.Fatalf("Oldest = %d, want 10 — the older of two active readers", got)
	}
	// The YOUNGER leaving must not release the watermark.
	h.Leave(b)
	if got := h.Oldest(99); got != 10 {
		t.Fatalf("after the younger reader left, Oldest = %d, want 10", got)
	}
	h.Leave(a)
	if got := h.Oldest(99); got != 99 {
		t.Fatalf("with no reader left, Oldest = %d, want the fallback 99", got)
	}
}

// TestHorizon_ZeroStartTimestampIsNotFree pins the encoding: a reader that
// starts at timestamp 0 — legitimate before anything has committed — must not
// read as an empty slot.
func TestHorizon_ZeroStartTimestampIsNotFree(t *testing.T) {
	var h Horizon
	s := h.Enter(0)
	if got := h.Oldest(50); got != 0 {
		t.Fatalf("Oldest = %d with a reader at timestamp 0, want 0", got)
	}
	if h.Active() != 1 {
		t.Fatalf("Active = %d, want 1: a reader at timestamp 0 is invisible", h.Active())
	}
	h.Leave(s)
	if got := h.Oldest(50); got != 50 {
		t.Fatalf("Oldest = %d after that reader left, want the fallback", got)
	}
}

// TestHorizon_ExhaustionSuspendsReclamation pins the degradation. More
// concurrent readers than slots must stop reclamation, not corrupt it.
func TestHorizon_ExhaustionSuspendsReclamation(t *testing.T) {
	var h Horizon
	slots := make([]int, 0, horizonSlots+4)
	for i := 0; i < horizonSlots; i++ {
		slots = append(slots, h.Enter(uint64(100+i)))
	}
	if h.Unregistered() != 0 {
		t.Fatalf("%d readers were unregistered while slots remained", h.Unregistered())
	}
	over := h.Enter(500)
	if over != unregistered {
		t.Fatalf("the %dth reader got slot %d, want unregistered: slots must be EXCLUSIVE, "+
			"because a shared slot lets the first leaver release the watermark on the "+
			"other's behalf", horizonSlots+1, over)
	}
	if got := h.Oldest(9999); got != 0 {
		t.Fatalf("Oldest = %d with an unregistered reader active, want 0 — reclaim nothing", got)
	}
	h.Leave(over)
	if got := h.Oldest(9999); got != 100 {
		t.Fatalf("Oldest = %d after the unregistered reader left, want 100", got)
	}
	for _, s := range slots {
		h.Leave(s)
	}
	if got := h.Oldest(9999); got != 9999 {
		t.Fatalf("Oldest = %d with every reader gone, want the fallback", got)
	}
}

// TestHorizon_NeverNewerThanAnyActiveReader is the property test, and the one
// that catches the shared-slot bug an earlier draft of Horizon had.
//
// THE ORACLE IS SELF-CHECKED BY EACH READER, deliberately. A first version had a
// sampler goroutine compare the watermark against a shared "live readers" set,
// and it reported 1413 violations against a correct Horizon — because a reader
// joined that set BEFORE calling Enter, so the sampler counted readers the
// watermark could not yet know about. Snapshotting "who is registered right
// now" from another goroutine is not possible without serialising the very
// concurrency under test.
//
// A reader checking BETWEEN its own Enter and Leave needs no snapshot: it is
// registered for the whole of that interval by construction, so the watermark
// must not exceed its start timestamp at any point inside it. That is exactly
// the contract, and it is what a reclaimer relies on.
//
// Run under -race.
func TestHorizon_NeverNewerThanAnyActiveReader(t *testing.T) {
	var h Horizon
	const readers = 24
	const iters = 500

	var clock atomic.Uint64
	clock.Store(1000)
	var violations, worst atomic.Int64
	var wg sync.WaitGroup

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				ts := clock.Add(1)
				slot := h.Enter(ts)
				// Registered from here until Leave. The watermark may never be
				// newer than this reader's start timestamp during the interval.
				for k := 0; k < 8; k++ {
					if wm := h.Oldest(clock.Load()); wm > ts {
						violations.Add(1)
						if d := int64(wm - ts); d > worst.Load() {
							worst.Store(d)
						}
					}
				}
				h.Leave(slot)
			}
		}()
	}
	wg.Wait()

	if n := violations.Load(); n != 0 {
		t.Fatalf("the watermark was newer than a REGISTERED reader %d times (worst overshoot "+
			"%d ticks): a reclaimer would have freed a version that reader could still reach",
			n, worst.Load())
	}
	if h.Active() != 0 || h.Unregistered() != 0 {
		t.Fatalf("after every reader left, %d slots and %d unregistered remain: Enter and "+
			"Leave are not balanced", h.Active(), h.Unregistered())
	}
}

// TestHorizon_StaleLeaveIsDetected pins [Horizon.StaleLeaves] on BOTH controls:
// a balanced Enter/Leave must report zero, and a slot released twice must report
// the breach.
//
// The counter exists because the damage of a double release is not to itself — it
// is that the SECOND clear lands on whichever reader has since claimed that slot,
// removing it from the watermark silently. Proving the detector on a sound arm as
// well as a broken one is what stops it being a counter that can only ever read
// zero (rmp #2420).
func TestHorizon_StaleLeaveIsDetected(t *testing.T) {
	t.Parallel()

	t.Run("balanced enter and leave report nothing", func(t *testing.T) {
		var h Horizon
		for i := 0; i < 64; i++ {
			slot := h.Enter(uint64(i + 1))
			h.Leave(slot)
		}
		if n := h.StaleLeaves(); n != 0 {
			t.Fatalf("StaleLeaves = %d after balanced use, want 0", n)
		}
	})

	t.Run("a slot released twice is reported", func(t *testing.T) {
		var h Horizon
		slot := h.Enter(7)
		h.Leave(slot)
		if n := h.StaleLeaves(); n != 0 {
			t.Fatalf("StaleLeaves = %d after the FIRST release, want 0", n)
		}
		h.Leave(slot)
		if n := h.StaleLeaves(); n != 1 {
			t.Fatalf("StaleLeaves = %d after a double release, want 1", n)
		}
	})

	t.Run("a slot number nobody claimed is reported", func(t *testing.T) {
		var h Horizon
		h.Leave(300)
		if n := h.StaleLeaves(); n != 1 {
			t.Fatalf("StaleLeaves = %d after releasing an unclaimed slot, want 1", n)
		}
	})
}

// TestHorizon_SlotStateReportsTheOccupantsInstant pins [Horizon.SlotState], which
// is how a reader verifies it is still represented in the watermark for as long as
// it is reading.
//
// The three states it must distinguish are exactly the three the watermark scan
// distinguishes: published (the instant, occupied), claimed-but-not-published
// (zero, occupied — hold everything), and released (not occupied).
func TestHorizon_SlotStateReportsTheOccupantsInstant(t *testing.T) {
	t.Parallel()

	var h Horizon
	slot := h.EnterHolding()
	if ts, occ := h.SlotState(slot); ts != 0 || !occ {
		t.Fatalf("claimed-but-unpublished slot reads (%d, %v), want (0, true)", ts, occ)
	}
	h.Publish(slot, 4242)
	if ts, occ := h.SlotState(slot); ts != 4242 || !occ {
		t.Fatalf("published slot reads (%d, %v), want (4242, true)", ts, occ)
	}
	h.Leave(slot)
	if _, occ := h.SlotState(slot); occ {
		t.Fatal("a released slot still reads as occupied")
	}
	if ts, occ := h.SlotState(-1); ts != 0 || occ {
		t.Fatalf("an unregistered slot reads (%d, %v), want (0, false)", ts, occ)
	}
}

// TestHorizon_OldestNeverExceedsItsFallback pins the ceiling property that
// protects a reader which arrives WHILE a scan is in progress (rmp #2420).
//
// The scan reads each occupancy word once, so a reader that claims a slot in a word
// already passed is invisible to it — claiming the bit before reading the clock
// cannot help, because nothing looks at that bit again. What covers such a reader is
// that the caller sampled the fallback BEFORE the scan: every reader appearing
// afterwards begins at or after the frontier of that moment, so a watermark capped
// at the fallback is below every one of their start instants.
//
// This test therefore asserts the cap directly. It FAILS against the previous
// implementation, which discarded the fallback as soon as any reader was found and
// returned 105 here — a watermark above the fallback, and above the start instant of
// any reader born in between.
func TestHorizon_OldestNeverExceedsItsFallback(t *testing.T) {
	t.Parallel()

	t.Run("a reader newer than the fallback does not raise the watermark", func(t *testing.T) {
		var h Horizon
		// A reader born AFTER the caller sampled its fallback: its start instant is
		// above the fallback, which is exactly the case the old code mishandled.
		slot := h.Enter(105)
		if got := h.Oldest(100); got != 100 {
			t.Fatalf("Oldest(100) with one reader at 105 = %d, want 100: a watermark above "+
				"the fallback frees versions that a reader born during the scan still needs", got)
		}
		h.Leave(slot)
	})

	t.Run("a reader older than the fallback still lowers the watermark", func(t *testing.T) {
		var h Horizon
		slot := h.Enter(10)
		if got := h.Oldest(100); got != 10 {
			t.Fatalf("Oldest(100) with one reader at 10 = %d, want 10: the cap must not stop "+
				"an older reader holding the watermark back", got)
		}
		h.Leave(slot)
	})

	t.Run("the oldest of several readers wins, still capped", func(t *testing.T) {
		var h Horizon
		a, b, c := h.Enter(40), h.Enter(70), h.Enter(900)
		if got := h.Oldest(100); got != 40 {
			t.Fatalf("Oldest(100) with readers at 40/70/900 = %d, want 40", got)
		}
		h.Leave(a)
		if got := h.Oldest(100); got != 70 {
			t.Fatalf("after the oldest left: Oldest(100) = %d, want 70", got)
		}
		h.Leave(b)
		if got := h.Oldest(100); got != 100 {
			t.Fatalf("with only a reader at 900 left: Oldest(100) = %d, want the fallback 100", got)
		}
		h.Leave(c)
	})
}
