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
	"sort"
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
// The oracle needs no fixture: bracket an EMPTY window. Whatever the INSTRUMENT
// allocates between its two snapshots is then the only thing of its own the
// delta can contain, and the correct answer is exactly zero objects and exactly
// zero bytes — not "small", not "within a band".
//
// # Why the assertion is scoped to non-runtime stacks (rmp #2666)
//
// It used to require the whole window to read zero, and the whole window is
// PROCESS-GLOBAL. An empty window contains no work of ours, but it is not empty
// of the Go runtime: the scheduler and the collector allocate on their own
// schedule, on stacks that never enter this package. Measured on the reference
// host (Apple M4, 10 cores, darwin/arm64, go1.27.0), running this test ALONE
// under -race, which is what `make ci` runs:
//
//	round 0: an EMPTY window attributed 1 objects / 2048 bytes
//	  [0] 100.00%  1 objs  2048 B
//	          runtime.allocm
//	          runtime.newm
//	          runtime.startm
//	          runtime.wakep
//	          runtime.schedule
//
// runtime.allocm is the scheduler creating an OS thread; runtime.gcBgMarkWorker
// was reported in the same position during the rmp #2665 work, from a run of
// ~20 packages in parallel (that one is second-hand — this session measured
// runtime.allocm). Neither is the instrument, and neither is anything the
// instrument can prevent — so a gate that failed on them was reporting the
// machine, not the subject. That is the same defect this file exists to catch,
// in the file that catches it.
//
// The correction is the one #2652 itself applied: measure the SUBJECT. The
// window's records carry STACKS, and a stack is exactly what scopes a
// process-global counter to a component. Every allocation the instrument can
// make — readMemProfile's record buffer, its maps, their growth — is made from
// a function in this package, so it carries an audit352_test frame; the
// runtime's background work carries none. The assertion is therefore still
// EXACTLY ZERO, and it is zero for the thing being asserted about:
//
//	no attributed site inside an empty window may have any frame outside
//	package runtime.
//
// The runtime's own residue is reported, never asserted on. It is a property of
// the host at that instant, and the honest thing to do with it is print it.
//
// Several rounds run because the residue was position-dependent: the storage
// grows on the first call and the runtime forces one profiled allocation per P
// after each MemProfileRate change, so a single round could pass by luck.
//
// # Falsification (measured, rmp #2666)
//
// The #2652 defect was reintroduced — readMemProfile made to allocate fresh
// storage on every call — and the scoped assertion went red on the first round,
// naming the phantom:
//
//	round 0: an EMPTY window attributed 1 objects / 188416 bytes to stacks
//	  OUTSIDE package runtime ([bench/audit352_test.readMemProfile])
//
// so narrowing the subject to this package's own frames did not narrow away the
// defect the gate exists for.
func TestAllocInstrumentDoesNotEnterItsOwnWindow(t *testing.T) {
	for round := 0; round < 4; round++ {
		at := exerciseAttributed(t, 1, func() {})
		objs, bytes, who := nonRuntimeResidue(&at)
		if objs != 0 || bytes != 0 {
			t.Errorf("round %d: an EMPTY window attributed %d objects / %d bytes to stacks "+
				"OUTSIDE package runtime (%v); the instrument allocated inside its own "+
				"bracket, so every share it reports is inflated by that amount:\n%s",
				round, objs, bytes, who, topSites(&at, 4, 5))
		}
		if objs == 0 && bytes == 0 && (at.totalObjects != 0 || at.totalBytes != 0) {
			// Not a failure: see the doc comment. Printed so a residue that ever
			// became large is visible. It is printed only when the scoped
			// assertion above held, so this line can never describe OUR residue
			// as the runtime's.
			t.Logf("round %d: the Go runtime allocated %d objects / %d bytes inside the "+
				"empty window (MemStats delta %d mallocs / %d bytes). None of it is "+
				"attributed to this package, so the instrument is clean:\n%s",
				round, at.totalObjects, at.totalBytes, at.windowMallocs, at.windowBytes,
				topSites(&at, 2, 5))
		}
	}

	// FALSIFICATION: the same bracket around a KNOWN workload must report it,
	// AND must report it through the very view the assertion above reads. A zero
	// residue proves a clean instrument only if a dirty one would have shown up
	// in that view.
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
	seenObjs, seenBytes, seenWho := nonRuntimeResidue(&at)
	if seenObjs < objs {
		t.Fatalf("the non-runtime view saw only %d of the %d known allocations (%d bytes, "+
			"frames %v): the scoped assertion above cannot see an allocation made from "+
			"this package, so its zero is vacuous",
			seenObjs, objs, seenBytes, seenWho)
	}
	at.assertDescribesWindow(t, "known workload")
	t.Logf("empty window: 0 objs / 0 B attributed outside package runtime; known window: "+
		"%d objs / %d B (%d objs / %d B of it outside package runtime) against a %d-byte "+
		"TotalAlloc delta (ratio %.4f)",
		at.totalObjects, at.totalBytes, seenObjs, seenBytes, at.windowBytes,
		float64(at.totalBytes)/float64(at.windowBytes))
}

// nonRuntimeResidue sums the objects and bytes of every attributed site whose
// stack carries at least one frame OUTSIDE package runtime, and names the
// distinct leaf frames it found.
//
// It is the scoping primitive behind TestAllocInstrumentDoesNotEnterItsOwnWindow:
// the memory profile is process-global, and a stack is what turns a
// process-global counter into a per-component one. An allocation made by this
// package always carries one of this package's frames (readMemProfile's own
// make, its maps, the closure a caller passed to exerciseAttributed); an
// allocation made by the runtime's scheduler or collector on its own account
// carries none.
func nonRuntimeResidue(at *attribution) (objects, bytes int64, frames []string) {
	seen := map[string]bool{}
	for _, s := range at.sites {
		leaf := leafAfterRuntime(s.frames)
		if leaf == "" {
			continue // entirely inside package runtime: not ours
		}
		objects += s.objects
		bytes += s.bytes
		if !seen[leaf] {
			seen[leaf] = true
			frames = append(frames, shortFn(leaf))
		}
	}
	sort.Strings(frames)
	return objects, bytes, frames
}
