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
