package mvcc

// horizon_occupancy_test.go — rmp #2292: the occupancy summary that makes
// [Horizon.Oldest] cost O(active readers) instead of O(capacity).
//
// Layer: short.
//
// The change moved the authority for "is this slot in use" from the slot's timestamp
// to a separate bit, and made [Horizon.Leave] clear the bit while LEAVING THE TIMESTAMP
// BEHIND. That is safe (see the residue argument on [Horizon.Enter]) but it creates a
// failure mode the previous design could not have: a stale timestamp that outlives its
// reader. Every test here exists because it would fail if the bit and the timestamp
// disagreed in one specific way.

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestHorizon_ResidueOfAReleasedSlotIsNotAReader is the direct regression for the new
// failure mode: a released slot keeps its timestamp, and nothing may treat that as a
// live reader.
//
// If the residue were counted, the watermark would be pinned at the first timestamp
// the graph ever used and reclamation would never advance again — an unbounded memory
// leak that no existing test distinguishes from correct conservatism, because holding
// back too much is otherwise always the safe direction.
func TestHorizon_ResidueOfAReleasedSlotIsNotAReader(t *testing.T) {
	var h Horizon
	s := h.Enter(10)
	h.Leave(s)

	if got := h.Oldest(9999); got != 9999 {
		t.Fatalf("Oldest = %d after the only reader left, want the fallback 9999: the "+
			"released slot's timestamp is being read as a live reader, which pins the "+
			"watermark forever", got)
	}
	if got := h.Active(); got != 0 {
		t.Fatalf("Active = %d after the only reader left, want 0: Active is counting "+
			"timestamps rather than occupancy bits", got)
	}
}

// TestHorizon_ReusedSlotReportsTheNewReader pins that a re-claimed slot answers for its
// CURRENT occupant. The residue is older by construction, so a stale read would hold
// back too much rather than too little — safe, but a permanent watermark stall.
func TestHorizon_ReusedSlotReportsTheNewReader(t *testing.T) {
	var h Horizon
	first := h.Enter(10)
	h.Leave(first)

	// Re-claim until the same slot comes round again, so this is genuinely a reuse
	// rather than a fresh slot that happens to work.
	var held []int
	reused := -1
	for i := 0; i < horizonSlots; i++ {
		s := h.Enter(uint64(1000 + i))
		if s == first {
			reused = s
			break
		}
		held = append(held, s)
	}
	if reused < 0 {
		t.Fatal("the probe never returned to the first slot, so this test did not " +
			"exercise reuse at all")
	}
	for _, s := range held {
		h.Leave(s)
	}
	if got := h.Oldest(9999); got < 1000 {
		t.Fatalf("Oldest = %d with only the reused slot's reader active, want >= 1000: "+
			"the slot is still answering with the RESIDUE of its previous occupant, so "+
			"the watermark can never advance past the graph's first reader", got)
	}
	h.Leave(reused)
	if got := h.Oldest(9999); got != 9999 {
		t.Fatalf("Oldest = %d fully drained, want the fallback", got)
	}
}

// TestHorizon_ChurnDoesNotAccumulateActiveSlots drives every slot through claim and
// release many times over. Occupancy must return to zero exactly, and the watermark
// must return to the fallback — the two observable ways a leaked bit would show.
func TestHorizon_ChurnDoesNotAccumulateActiveSlots(t *testing.T) {
	var h Horizon
	for round := 0; round < 4; round++ {
		slots := make([]int, 0, horizonSlots)
		for i := 0; i < horizonSlots; i++ {
			s := h.Enter(uint64(round*horizonSlots + i + 1))
			if s == unregistered {
				t.Fatalf("round %d: reader %d failed to register, so a previous round "+
					"leaked an occupancy bit", round, i)
			}
			slots = append(slots, s)
		}
		if got := h.Active(); got != horizonSlots {
			t.Fatalf("round %d: Active = %d at capacity, want %d", round, got, horizonSlots)
		}
		for _, s := range slots {
			h.Leave(s)
		}
		if got := h.Active(); got != 0 {
			t.Fatalf("round %d: Active = %d after draining, want 0", round, got)
		}
		if got := h.Oldest(777); got != 777 {
			t.Fatalf("round %d: Oldest = %d after draining, want the fallback 777", round, got)
		}
	}
}

// TestHorizon_ConcurrentChurnKeepsOccupancyExact is the concurrent half. A bitmap
// claimed with a compare-and-swap loop can lose an update if the loop is written
// wrong, and the symptom is either two readers on one slot — which is the unsound
// direction the file header warns about — or a bit that is never released.
//
// Run under -race.
func TestHorizon_ConcurrentChurnKeepsOccupancyExact(t *testing.T) {
	var h Horizon
	const workers = 32
	const iters = 400

	var clock atomic.Uint64
	clock.Store(1)
	// owner records which worker believes it holds each slot, so a slot handed to two
	// workers at once is detected rather than merely making the watermark odd.
	var owner [horizonSlots]atomic.Int64
	for i := range owner {
		owner[i].Store(-1)
	}
	var doubleBooked atomic.Int64
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				s := h.Enter(clock.Add(1))
				if s == unregistered {
					continue
				}
				if !owner[s].CompareAndSwap(-1, int64(w)) {
					doubleBooked.Add(1)
				}
				owner[s].Store(-1)
				h.Leave(s)
			}
		}(w)
	}
	wg.Wait()

	if n := doubleBooked.Load(); n != 0 {
		t.Fatalf("a slot was handed to two workers at once %d times: slots must be "+
			"EXCLUSIVE, because a shared slot lets the first leaver release the "+
			"watermark on the other's behalf", n)
	}
	if got := h.Active(); got != 0 {
		t.Fatalf("Active = %d after every worker finished, want 0: an occupancy bit "+
			"was leaked, and once every bit leaks the horizon is permanently full", got)
	}
	if got := h.Unregistered(); got != 0 {
		t.Fatalf("Unregistered = %d after every worker finished, want 0", got)
	}
	if got := h.Oldest(1 << 40); got != 1<<40 {
		t.Fatalf("Oldest = %d with no reader left, want the fallback", got)
	}
}

// BenchmarkHorizonOldestByOccupancy is the measurement the change exists for. Before
// the occupancy summary, `Oldest` cost the same at one reader as at capacity, because
// it scanned all 1024 slot cache lines either way.
func BenchmarkHorizonOldestByOccupancy(b *testing.B) {
	for _, active := range []int{0, 1, 8, 64, horizonSlots} {
		b.Run("active="+itoa(active), func(b *testing.B) {
			var h Horizon
			slots := make([]int, 0, active)
			for i := 0; i < active; i++ {
				slots = append(slots, h.Enter(uint64(10+i)))
			}
			b.ReportAllocs()
			b.ResetTimer()
			var sink uint64
			for i := 0; i < b.N; i++ {
				sink += h.Oldest(1 << 40)
			}
			b.StopTimer()
			_ = sink
			for _, s := range slots {
				h.Leave(s)
			}
		})
	}
}

// itoa avoids pulling strconv in for one call site in a benchmark label.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
