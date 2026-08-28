package audit352_test

// allocattrib_selfcheck_test.go — the instrument's own oracle.
//
// The bracketed MemProfile differencing in allocattrib_test.go is only sound if
// runtime.MemProfile has actually flushed the window's allocations into its
// active cycle by the time the closing snapshot is taken. The runtime documents
// the profile as possibly "up to two garbage collection cycles old" and provides
// no explicit flush, so the number of collections the snapshot forces is a
// correctness parameter, not a detail.
//
// This file measures it against a counter that does NOT lag:
// runtime.MemStats.Mallocs is incremented at allocation time. If the profile
// window's total alloc_objects does not agree with the Mallocs delta over the
// same window, the attribution is lagging and every share derived from it is
// wrong — including, and especially, the "before" arm of an A/B, whose tail would
// be charged to the "after" arm.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
)

// TestAllocAttributionAgreesWithMallocs runs the same window four times —
// decorated, decorated, legacy, legacy — and reports both the profile total and
// the Mallocs delta for each.
//
// Two properties must hold:
//   - same-vs-same: the two decorated windows must agree with each other, and the
//     two legacy windows with each other. A window whose total depends on its
//     POSITION is a lagging instrument.
//   - profile vs Mallocs: the profile total must track the Mallocs delta.
func TestAllocAttributionAgreesWithMallocs(t *testing.T) {
	const n = 1000
	eng := sortShapeEngine(t, n)

	// Warm both arms outside every window.
	for _, d := range []bool{true, false} {
		restore := sortseam.SetKeyDecorationDisabled(d)
		drainCounting(t, eng, sortShapeQuery)
		restore()
	}

	type obs struct {
		arm       string
		profObjs  int64
		mallocs   uint64
		rowLess   int64
		decorated int64
	}
	out := make([]obs, 0, 4)
	for _, step := range []struct {
		arm      string
		disabled bool
	}{{"decorated", false}, {"decorated", false}, {"legacy", true}, {"legacy", true}} {
		restore := sortseam.SetKeyDecorationDisabled(step.disabled)
		at := exerciseAttributed(t, 1, func() { drainCounting(t, eng, sortShapeQuery) })
		restore()

		rl, _ := at.cum(frameSortLegacy)
		dec, _ := at.cum(frameSortDecorated)
		out = append(out, obs{step.arm, at.totalObjects, at.windowMallocs, rl, dec})
	}

	for i, o := range out {
		t.Logf("#%d arm=%-9s profile_objs=%-9d mallocs=%-9d ratio=%.4f  rowLess=%-8d sortDecorated=%d",
			i, o.arm, o.profObjs, o.mallocs, float64(o.profObjs)/float64(o.mallocs), o.rowLess, o.decorated)
	}

	// The Mallocs bracket includes the two ReadMemStats stop-the-worlds and any
	// runtime background allocation, so exact equality is not the claim. A
	// factor-of-two disagreement, or a same-vs-same pair that differs by more
	// than a few per cent, is.
	for _, pair := range [][2]int{{0, 1}, {2, 3}} {
		a, b := out[pair[0]], out[pair[1]]
		dev := relDev(a.profObjs, b.profObjs)
		t.Logf("same-vs-same %s: profile totals %d vs %d, deviation %.2f%%", a.arm, a.profObjs, b.profObjs, dev)
		if dev > 10 {
			t.Errorf("INSTRUMENT LAGS: two identical %s windows reported %d and %d alloc objects "+
				"(%.2f%% apart). The profile is not flushed by the snapshot, so a window's tail "+
				"is being charged to the NEXT window.", a.arm, a.profObjs, b.profObjs, dev)
		}
		devM := relDev(int64(a.mallocs), int64(b.mallocs))
		if devM > 10 {
			t.Errorf("two identical %s windows allocated %d and %d times (%.2f%% apart): the "+
				"workload itself is not repeatable", a.arm, a.mallocs, b.mallocs, devM)
		}
	}
	for i, o := range out {
		r := float64(o.profObjs) / float64(o.mallocs)
		if r < 0.8 || r > 1.25 {
			t.Errorf("#%d arm=%s: profile total %d disagrees with Mallocs delta %d (ratio %.4f); "+
				"the attribution does not describe this window", i, o.arm, o.profObjs, o.mallocs, r)
		}
	}
}

func relDev(a, b int64) float64 {
	if a == 0 && b == 0 {
		return 0
	}
	m := float64(a+b) / 2
	d := float64(a - b)
	if d < 0 {
		d = -d
	}
	return 100 * d / m
}

// TestAllocInstrumentDoesNotEnterItsOwnWindow is the regression gate for the
// defect that made every cell of the #2652 A/B sweep fail: the instrument's own
// snapshot storage landed inside the window it was measuring, so the profile
// total disagreed with the TotalAlloc delta by 17x at the smallest cell and by
// 264x inside a full package run.
//
// The oracle is the strongest one available and needs no fixture: bracket an
// EMPTY window. Whatever the instrument allocates between its two snapshots is
// then the ONLY thing the delta can contain, so the correct answer is exactly
// zero objects and exactly zero bytes — not "small", not "within a band".
//
// It is deliberately not expressed as a ratio. assertDescribesWindow's band is a
// tolerance for a real window; here there is nothing to tolerate, and a ratio
// oracle would divide by a zero TotalAlloc delta.
//
// Several rounds run because the residue was position-dependent: the storage
// grows on the first call and the runtime forces one profiled allocation per P
// after each MemProfileRate change, so a single round could pass by luck.
func TestAllocInstrumentDoesNotEnterItsOwnWindow(t *testing.T) {
	for round := 0; round < 4; round++ {
		at := exerciseAttributed(t, 1, func() {})
		if at.totalObjects != 0 || at.totalBytes != 0 {
			t.Errorf("round %d: an EMPTY window attributed %d objects / %d bytes; the "+
				"instrument allocated inside its own bracket, so every share it reports "+
				"is inflated by that amount:\n%s",
				round, at.totalObjects, at.totalBytes, topSites(&at, 4, 5))
		}
		if at.windowMallocs != 0 || at.windowBytes != 0 {
			t.Errorf("round %d: an EMPTY window's MemStats deltas are %d mallocs / %d bytes, "+
				"want 0 each; something allocated between the two ReadMemStats calls",
				round, at.windowMallocs, at.windowBytes)
		}
	}

	// FALSIFICATION: the same bracket around a KNOWN workload must report it, so
	// the zero above is evidence of a clean instrument rather than of an
	// instrument that has stopped recording anything at all.
	const objs, size = 100, 1024
	var sink [][]byte
	at := exerciseAttributed(t, 1, func() {
		sink = make([][]byte, 0, objs)
		for i := 0; i < objs; i++ {
			sink = append(sink, make([]byte, size))
		}
	})
	_ = sink
	if at.totalObjects < objs {
		t.Fatalf("a window of %d known %d-byte allocations attributed only %d objects: "+
			"the instrument records nothing, so the zero-residue assertion above is vacuous",
			objs, size, at.totalObjects)
	}
	at.assertDescribesWindow(t, "known workload")
	t.Logf("empty window: 0 objs / 0 B; known window: %d objs / %d B against a %d-byte "+
		"TotalAlloc delta (ratio %.4f)",
		at.totalObjects, at.totalBytes, at.windowBytes,
		float64(at.totalBytes)/float64(at.windowBytes))
}
