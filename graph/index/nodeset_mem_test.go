package index

import (
	"runtime"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
)

// nodeset_mem_test.go — the resident-heap regression guard the perf audit
// (2026-06-18) recommended for the small-set tier (sprint 206, #1584/#1585).
//
// It measures LIVE heap bytes per entry for a high-cardinality index shape
// (1M distinct keys, one node each) two ways: the OLD per-key
// *roaring64.Bitmap representation (~286 B/entry, the audited baseline) and
// the NEW 16-byte packed NodeSet representation (target ~56 B/entry). The probe
// pins the populated structure across two GC cycles so HeapAlloc reflects
// only retained memory, then divides by the entry count.
//
// These tests are NOT parallel: they read process-global runtime.MemStats.
// The numbers are coarse (resident heap is noisy), so the assertions use wide
// margins — they guard against an order-of-magnitude regression, not against
// single-byte drift.

const memProbeEntries = 1_000_000

// liveHeapBytes returns HeapAlloc after two GC cycles, with keep referenced so
// the populated structure cannot be collected before the measurement.
func liveHeapBytes(keep any) uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	runtime.KeepAlive(keep)
	return ms.HeapAlloc
}

// bytesPerEntry measures the retained heap delta of building want via build,
// divided by memProbeEntries. base is captured before build runs.
func bytesPerEntry(t *testing.T, build func() any) float64 {
	t.Helper()
	// Settle the heap, capture the baseline, build, then measure.
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	obj := build()
	after := liveHeapBytes(obj)

	delta := float64(after) - float64(before.HeapAlloc)
	if delta < 0 {
		delta = 0
	}
	return delta / float64(memProbeEntries)
}

// oldBitmapBytesPerEntry builds the audited baseline shape — 1M singleton keys
// each held as a distinct *roaring64.Bitmap — and returns its retained bytes
// per entry. This is the ~286 B/entry representation the small-set tier
// replaces; both the baseline test and the NodeSet regression guard measure it
// in-process so the guard is self-calibrating against the host's allocator.
func oldBitmapBytesPerEntry(t *testing.T) float64 {
	t.Helper()
	return bytesPerEntry(t, func() any {
		m := make(map[uint64]*roaring64.Bitmap, memProbeEntries)
		for i := uint64(0); i < memProbeEntries; i++ {
			bm := roaring64.New()
			bm.Add(i)
			m[i] = bm
		}
		return m
	})
}

// newNodeSetBytesPerEntry builds 1M singleton keys held by value as NodeSets
// and returns its retained bytes per entry.
func newNodeSetBytesPerEntry(t *testing.T) float64 {
	t.Helper()
	return bytesPerEntry(t, func() any {
		m := make(map[uint64]NodeSet, memProbeEntries)
		for i := uint64(0); i < memProbeEntries; i++ {
			var s NodeSet
			s.Add(i)
			m[i] = s
		}
		return m
	})
}

// TestMem_OldBitmapBaseline records the audited baseline (~286 B/entry) as the
// control, asserting only that it is large so a future reader (and the NodeSet
// guard below) can compare against it.
func TestMem_OldBitmapBaseline(t *testing.T) {
	perEntry := oldBitmapBytesPerEntry(t)
	t.Logf("OLD *roaring64.Bitmap per singleton entry: %.1f B", perEntry)
	if perEntry < 100 {
		t.Fatalf("baseline unexpectedly small (%.1f B/entry) — control invalid", perEntry)
	}
}

// TestMem_NewNodeSetSingleton is the regression guard for the 16-byte packed
// NodeSet ({ptr unsafe.Pointer; meta uint64}, #1596). A singleton key costs no
// separate heap object and the value itself is two machine words, so a
// high-cardinality index of 1M singletons retains ~56 B/entry — about 5.1x
// lighter than the ~286 B/entry per-key *roaring64.Bitmap baseline, and ~2.4x
// lighter than the 48-byte safe-union predecessor's ~134 B/entry (#1584/#1585).
//
// The guard's job is to catch a re-introduction of the per-singleton roaring
// object (which would push the cost back toward the baseline) OR a struct-bloat
// regression that loses the 16-byte packing, NOT to police single-byte drift on
// a noisy resident-heap measurement. It therefore checks both an absolute
// ceiling above the observed ~56 B and a RATIO against the same-process bitmap
// baseline: either a re-added per-key roaring object or a re-widened value
// collapses the ratio and trips this test.
func TestMem_NewNodeSetSingleton(t *testing.T) {
	const (
		// absCeiling sits above the measured ~56 B (noise margin) yet below the
		// ~134 B of the 48-byte predecessor, so losing the 16-byte packing (or
		// re-adding a per-key roaring object) breaches it.
		absCeiling = 90.0
		// maxFractionOfBaseline requires the packed NodeSet to retain at most
		// ~30% of the per-key-bitmap baseline. Observed ~56/286 ≈ 0.20, so this
		// passes with headroom while still catching a regression toward the
		// 48-byte (~0.47) or bitmap cost.
		maxFractionOfBaseline = 0.30
	)
	baseline := oldBitmapBytesPerEntry(t)
	perEntry := newNodeSetBytesPerEntry(t)
	t.Logf("NEW NodeSet per singleton entry: %.1f B (baseline %.1f B, ratio %.2f)",
		perEntry, baseline, perEntry/baseline)

	if perEntry > absCeiling {
		t.Fatalf("NodeSet singleton entry = %.1f B/entry, want <= %.0f "+
			"(16-byte packing target ~56 B); the 2-word packing may have been "+
			"lost or a per-singleton roaring object re-introduced", perEntry, absCeiling)
	}
	if baseline > 0 && perEntry > maxFractionOfBaseline*baseline {
		t.Fatalf("NodeSet singleton entry = %.1f B/entry is %.0f%% of the "+
			"%.1f B per-key-bitmap baseline, want <= %.0f%%; the small-set "+
			"tier regressed toward per-key roaring objects",
			perEntry, 100*perEntry/baseline, baseline, 100*maxFractionOfBaseline)
	}
}
