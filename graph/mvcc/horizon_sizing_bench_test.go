package mvcc

// horizon_sizing_bench_test.go — rmp #2315: size the reclamation horizon from
// measurement, not from an argument about cache sizes.
//
// # Why a replica, and why it is validated
//
// [horizonSlots] is a compile-time constant backing a fixed array, so the real
// [Horizon] exists at exactly one size. Measuring 256/1024/4096 would therefore
// require changing production code BEFORE the decision the measurement is meant to
// inform — precisely backwards.
//
// So this measures a replica whose enter and oldest are algorithms over the same
// 128-byte-strided layout, parameterised by slot count. A replica is only worth
// anything if it behaves like the original, so [TestSizingReplica_MatchesRealHorizon]
// pins that: the replica at 64 slots must agree with the real Horizon on the
// registration cliff and the watermark, and [BenchmarkHorizonReal64] measures the real
// type at 64 so the replica's own 64-slot number can be compared against it. Treat a
// divergence there as invalidating every larger figure in the table.
//
// # The replica now measures a SUPERSEDED algorithm, deliberately
//
// rmp #2292 replaced the real [Horizon.Oldest] with an occupancy-summary scan that
// visits only occupied slots, so it is O(active readers) rather than O(slots). The
// replica below still implements the O(slots) form, on purpose: its whole job is to
// reproduce the cost curve the #2315 sizing table records, and rewriting it would
// silently invalidate that table's provenance.
//
// The behavioural control still holds — the two agree on the watermark and the
// registration cliff, which is all it ever asserted — but the replica's `oldest`
// TIMINGS no longer describe production. For the current cost see
// [BenchmarkHorizonOldestByOccupancy] in horizon_occupancy_test.go.

import (
	"strconv"
	"sync/atomic"
	"testing"
)

// sizedSlot mirrors horizonSlot, including the padding, because the stride is the
// whole point: the scan's cost is slots × 128 bytes of distinct cache lines.
type sizedSlot struct {
	ts atomic.Uint64
	_  [cacheLine - 8]byte
}

// sizedHorizon mirrors [Horizon] with a runtime slot count. The count must be a
// power of two, as the rotating probe masks rather than divides.
type sizedHorizon struct {
	slots []sizedSlot
	mask  uint64
	next  atomic.Uint64
	_     [cacheLine - 8]byte
	unreg atomic.Int64
	_     [cacheLine - 8]byte
}

func newSizedHorizon(slots int) *sizedHorizon {
	if slots <= 0 || slots&(slots-1) != 0 {
		panic("sizing replica: slot count must be a power of two")
	}
	return &sizedHorizon{slots: make([]sizedSlot, slots), mask: uint64(slots - 1)}
}

func (h *sizedHorizon) enter(startTS uint64) int {
	start := int(h.next.Add(1) & h.mask)
	want := startTS + 1
	n := len(h.slots)
	for i := 0; i < n; i++ {
		slot := (start + i) & int(h.mask)
		if h.slots[slot].ts.CompareAndSwap(0, want) {
			return slot
		}
	}
	h.unreg.Add(1)
	return unregistered
}

func (h *sizedHorizon) leave(slot int) {
	if slot == unregistered {
		h.unreg.Add(-1)
		return
	}
	h.slots[slot].ts.Store(0)
}

func (h *sizedHorizon) oldest(fallback uint64) uint64 {
	if h.unreg.Load() != 0 {
		return 0
	}
	oldest := fallback
	found := false
	for i := range h.slots {
		v := h.slots[i].ts.Load()
		if v == 0 {
			continue
		}
		ts := v - 1
		if !found || ts < oldest {
			oldest, found = ts, true
		}
	}
	if h.unreg.Load() != 0 {
		return 0
	}
	return oldest
}

// sizingCounts are the capacities the task asks for.
var sizingCounts = []int{64, 256, 1024, 4096}

// TestSizingReplica_MatchesRealHorizon is the control that makes the table below
// worth reading. The replica at 64 slots must reproduce the real Horizon's observable
// behaviour: every reader up to capacity registers and the watermark tracks the
// oldest start timestamp; one reader past capacity is unregistered and the watermark
// collapses to zero; both recover when the readers leave.
func TestSizingReplica_MatchesRealHorizon(t *testing.T) {
	var real Horizon
	rep := newSizedHorizon(horizonSlots)

	realSlots := make([]int, 0, horizonSlots+1)
	repSlots := make([]int, 0, horizonSlots+1)
	// Start timestamps 10, 11, 12, … so the oldest is unambiguous.
	for i := 0; i < horizonSlots; i++ {
		realSlots = append(realSlots, real.Enter(uint64(10+i)))
		repSlots = append(repSlots, rep.enter(uint64(10+i)))
	}
	if got, want := real.Oldest(999), uint64(10); got != want {
		t.Fatalf("real Horizon at capacity: Oldest = %d, want %d", got, want)
	}
	if got, want := rep.oldest(999), uint64(10); got != want {
		t.Fatalf("replica at capacity: oldest = %d, want %d — the replica does not "+
			"track the real Horizon, so every figure measured with it is void", got, want)
	}

	// One past capacity: unregistered, watermark collapses.
	realOver := real.Enter(500)
	repOver := rep.enter(500)
	if realOver != unregistered || repOver != unregistered {
		t.Fatalf("one reader past capacity got a slot: real=%d replica=%d", realOver, repOver)
	}
	if got := real.Oldest(999); got != 0 {
		t.Fatalf("real Horizon with an unregistered reader: Oldest = %d, want 0", got)
	}
	if got := rep.oldest(999); got != 0 {
		t.Fatalf("replica with an unregistered reader: oldest = %d, want 0 — the replica "+
			"does not reproduce the suspension cliff", got)
	}

	// Recovery.
	real.Leave(realOver)
	rep.leave(repOver)
	for i := range realSlots {
		real.Leave(realSlots[i])
		rep.leave(repSlots[i])
	}
	if got := real.Oldest(777); got != 777 {
		t.Fatalf("real Horizon after drain: Oldest = %d, want the fallback 777", got)
	}
	if got := rep.oldest(777); got != 777 {
		t.Fatalf("replica after drain: oldest = %d, want the fallback 777", got)
	}
}

// BenchmarkHorizonReal64 measures the REAL type, so the replica's 64-slot figure has
// something to be checked against.
func BenchmarkHorizonReal64(b *testing.B) {
	b.Run("oldest/near-empty", func(b *testing.B) {
		var h Horizon
		s := h.Enter(42)
		defer h.Leave(s)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sink = h.Oldest(1 << 40)
		}
	})
	b.Run("oldest/near-full", func(b *testing.B) {
		var h Horizon
		slots := make([]int, 0, horizonSlots-1)
		for i := 0; i < horizonSlots-1; i++ {
			slots = append(slots, h.Enter(uint64(10+i)))
		}
		defer func() {
			for _, s := range slots {
				h.Leave(s)
			}
		}()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sink = h.Oldest(1 << 40)
		}
	})
	b.Run("enter-leave/near-empty", func(b *testing.B) {
		var h Horizon
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			h.Leave(h.Enter(uint64(i)))
		}
	})
	b.Run("enter-leave/near-full", func(b *testing.B) {
		var h Horizon
		slots := make([]int, 0, horizonSlots-1)
		for i := 0; i < horizonSlots-1; i++ {
			slots = append(slots, h.Enter(uint64(10+i)))
		}
		defer func() {
			for _, s := range slots {
				h.Leave(s)
			}
		}()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			h.Leave(h.Enter(uint64(i)))
		}
	})
}

// sink defeats dead-code elimination on the Oldest benchmarks, whose result is
// otherwise unused.
var sink uint64

// BenchmarkHorizonSizing measures the replica across [sizingCounts] in both regimes
// the task names: near-empty, which is the common case, and near-full, which is the
// case the extra slots would exist for.
//
// The two operations scale differently, which is why they are measured separately:
//   - oldest is an unconditional scan of EVERY slot, so it is O(slots) always;
//   - enter is a rotating probe, O(1) while slots are plentiful and O(slots) only
//     when nearly full.
func BenchmarkHorizonSizing(b *testing.B) {
	for _, n := range sizingCounts {
		name := strconv.Itoa(n)

		b.Run("oldest/near-empty/slots="+name, func(b *testing.B) {
			h := newSizedHorizon(n)
			s := h.enter(42)
			defer h.leave(s)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink = h.oldest(1 << 40)
			}
		})

		b.Run("oldest/near-full/slots="+name, func(b *testing.B) {
			h := newSizedHorizon(n)
			for i := 0; i < n-1; i++ {
				h.enter(uint64(10 + i))
			}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink = h.oldest(1 << 40)
			}
		})

		b.Run("enter-leave/near-empty/slots="+name, func(b *testing.B) {
			h := newSizedHorizon(n)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				h.leave(h.enter(uint64(i)))
			}
		})

		b.Run("enter-leave/near-full/slots="+name, func(b *testing.B) {
			h := newSizedHorizon(n)
			for i := 0; i < n-1; i++ {
				h.enter(uint64(10 + i))
			}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				h.leave(h.enter(uint64(i)))
			}
		})
	}
}
