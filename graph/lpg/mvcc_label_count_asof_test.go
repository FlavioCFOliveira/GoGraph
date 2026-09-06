package lpg

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// mvcc_label_count_asof_test.go — [Graph.LabelCountAsOf] (rmp #2773).
//
// The method exists because the two gates that govern the same question
// disagreed in scope: [Graph.LabelCountExact] declines on the GLOBAL
// "any history live" flag, while the bitmap route its caller then took gates on
// the PER-LABEL churn counter, and so returned an UNCORRECTED clone. Every test
// here is built around that asymmetry, and each one asserts the precondition it
// depends on rather than assuming it: a state that is not actually churning
// would make all of them pass for the wrong reason.

// labelCountAsOfGraph seeds `seeded` nodes carrying label "P" plus `spare`
// unlabelled ones, drains all history, and returns the graph with P's id. A
// drained substrate is the precondition for every test below: with history
// already live the states they construct would not be distinguishable.
func labelCountAsOfGraph(t *testing.T, seeded, spare int) (*Graph[string, float64], LabelID) {
	t.Helper()
	g, lid := newLabelCountProbeGraph(t, seeded, spare)
	if n, ok := g.LabelCountExact(lid, nil); !ok || n != int64(seeded) {
		t.Fatalf("precondition: a freshly drained graph answered (%d, %v), want (%d, true)",
			n, ok, seeded)
	}
	return g, lid
}

// labelOther commits one node under a label the counted reads never name, so the
// write raises the GLOBAL history flag while leaving the per-label churn gate for
// "P" untouched. That is exactly the state the read-path allocation gate hit
// under load (rmp #2773).
func labelOther(t *testing.T, g *Graph[string, float64], key string) {
	t.Helper()
	if err := g.AddNode(key); err != nil {
		t.Fatalf("AddNode(%s): %v", key, err)
	}
	if err := g.ApplyVersionedCtx(context.Background(), func(tx WriteTx) error {
		return g.Writer(tx).SetNodeLabel(key, "Q")
	}); err != nil {
		t.Fatalf("label %s as Q: %v", key, err)
	}
}

// TestLabelCountAsOf_AgreesWithTheFilteredBitmapInEveryState is the correctness
// oracle: whatever the churn, the number must be the one the SCAN would produce,
// because those two answers are the same query asked twice.
//
// The states are not decoration. State 2 is the one the fix changes — the global
// flag up, the per-label gate clear — and states 3 and 4 are the ones it must NOT
// change, where a correction is genuinely owed and the clone is not waste.
func TestLabelCountAsOf_AgreesWithTheFilteredBitmapInEveryState(t *testing.T) {
	const seeded = 12
	g, lid := labelCountAsOfGraph(t, seeded, 8)

	check := func(t *testing.T, what string, s *Snapshot, want int64) {
		t.Helper()
		scan := int64(g.LabelBitmapAsOf(lid, s).GetCardinality())
		if scan != want {
			t.Fatalf("%s: the SCAN control answered %d, want %d — the fixture is not in the "+
				"state this case describes, so the comparison below proves nothing",
				what, scan, want)
		}
		if got := g.LabelCountAsOf(lid, s); got != scan {
			t.Errorf("%s: LabelCountAsOf = %d, the filtered bitmap's cardinality is %d. The "+
				"count and the scan must never disagree: they are the same question.",
				what, got, scan)
		}
	}

	// 1. Drained: no history at all.
	quiet := g.BeginRead()
	check(t, "drained", quiet, seeded)
	g.EndRead(quiet)

	// 2. Global flag up, per-label gate clear. The reader is registered FIRST so
	// the record it caps cannot be reclaimed, which makes the state deterministic
	// rather than a race against the vacuum.
	other := g.BeginRead()
	labelOther(t, g, "other-1")
	if _, ok := g.LabelCountExact(lid, other); ok {
		t.Fatal("LabelCountExact no longer declines for a snapshot with unrelated history " +
			"live, so this case no longer exercises the asymmetry it was written for")
	}
	if g.churnLive(oneLabel(lid)) {
		t.Fatal("the per-label churn gate is UP for P after a write to Q, so this case is " +
			"the same as case 3 and the fix it covers is untested")
	}
	check(t, "unrelated history live", other, seeded)
	g.EndRead(other)

	// 3. Same label, correction genuinely owed: the snapshot predates a write that
	// adds a member, so the raw count is wrong for it by exactly one.
	pinned := g.BeginRead()
	if err := labelOne(g, seeded); err != nil {
		t.Fatalf("label one more P: %v", err)
	}
	if !g.churnLive(oneLabel(lid)) {
		t.Fatal("the per-label churn gate is DOWN for P after labelling a P node, so the " +
			"correction path is not reached and this case tests nothing")
	}
	if raw := int64(g.nodeIdx.Count(uint32(lid))); raw != seeded+1 {
		t.Fatalf("the raw index says %d, want %d: the write did not land, so a count that "+
			"read the raw index would be indistinguishable from the correct answer", raw, seeded+1)
	}
	check(t, "same-label history live, snapshot predates the write", pinned, seeded)

	// 4. And the present-time reader on that same churning graph sees the new one.
	check(t, "present time, same-label history live", nil, seeded+1)
	g.EndRead(pinned)
}

// TestLabelCountAsOf_AllocatesNothingWhenNoCorrectionIsOwed is the regression gate
// for the defect itself: the bitmap clone.
//
// Both arms are load-bearing and they target the two disjuncts of the guard
// separately. The first is [Graph.LabelCountExact]'s own post-sample, the second
// is the per-label gate this method added; deleting either one makes exactly one
// arm allocate.
func TestLabelCountAsOf_AllocatesNothingWhenNoCorrectionIsOwed(t *testing.T) {
	const seeded = 12
	g, lid := labelCountAsOfGraph(t, seeded, 8)

	t.Run("no history at all", func(t *testing.T) {
		s := g.BeginRead()
		defer g.EndRead(s)
		if n := testing.AllocsPerRun(200, func() { _ = g.LabelCountAsOf(lid, s) }); n != 0 {
			t.Errorf("LabelCountAsOf allocated %.2f objects on a drained graph, want 0", n)
		}
	})

	t.Run("present time, same-label history live", func(t *testing.T) {
		// The FIRST disjunct on its own: a present-time reader needs no filtering
		// at all while no index add is in flight, however much committed history
		// is live on this very label. Deleting that arm sends this state through
		// the clone for a number the raw index already holds.
		if err := labelOne(g, seeded); err != nil {
			t.Fatalf("label one more P: %v", err)
		}
		if !g.churnLive(oneLabel(lid)) {
			t.Fatal("the per-label churn gate is DOWN for P after labelling a P node, so " +
				"this arm is the drained case again and the disjunct it targets is untested")
		}
		if n := testing.AllocsPerRun(200, func() { _ = g.LabelCountAsOf(lid, nil) }); n != 0 {
			t.Errorf("present-time LabelCountAsOf allocated %.2f objects with same-label "+
				"history live, want 0", n)
		}
		if got := g.LabelCountAsOf(lid, nil); got != seeded+1 {
			t.Errorf("present-time LabelCountAsOf = %d, want %d", got, seeded+1)
		}
	})

	t.Run("unrelated history live", func(t *testing.T) {
		var s *Snapshot
		// The arm above left history live on P, which would make this one measure
		// the correction path instead. Drain it — with s registered the drain
		// cannot free anything newer than s, so s is taken after the drain.
		g.ReclaimNow()
		if g.churnLive(oneLabel(lid)) {
			t.Fatal("P is still churning after ReclaimNow, so this arm cannot isolate the " +
				"per-label disjunct it targets")
		}
		s = g.BeginRead()
		defer g.EndRead(s)
		labelOther(t, g, "other-2")
		// The precondition: the exact-or-nothing count must DECLINE here, or this
		// arm measures the drained case again under a different name.
		if _, ok := g.LabelCountExact(lid, s); ok {
			t.Fatal("LabelCountExact still answers with unrelated history live, so this arm " +
				"no longer covers the state the clone was paid in")
		}
		if n := testing.AllocsPerRun(200, func() { _ = g.LabelCountAsOf(lid, s) }); n != 0 {
			t.Errorf("LabelCountAsOf allocated %.2f objects while the global history flag was "+
				"up and the per-label churn gate for P was clear, want 0. That is the whole "+
				"of rmp #2773: the bitmap is cloned and then returned uncorrected.", n)
		}
	})
}

// TestLabelCountAsOf_TheInvertedOrderReportsAWriteFromInsideTheWindow is the gate
// on [Graph.LabelCountAsOf]'s ORDER: the cardinality is read first and the gate
// second, and swapping them is an Isolation break.
//
// # Why the order is the claim, and why it needs its own seam
//
// The two interleavings differ only in what a write landing mid-call can do:
//
//	sound (count -> gate): the write lands before the count, so its hold was
//	  already raised before it touched the index; the gate read AFTER the count
//	  sees that hold and the correction path answers. The raw count is discarded.
//
//	inverted (gate -> count): the gate reads clear, THEN the write raises its hold
//	  and touches the index, THEN the cardinality read picks up the contaminated
//	  number — and it is returned as the snapshot's exact answer.
//
// rmp #2773 established by mutation that this cannot be tested from
// [Graph.labelCountGateProbe]'s position. That seam fires before BOTH reads, so a
// write it drives is visible to the gate wherever the gate sits, and an inverted
// implementation passes every test in this package. So this test drives
// [Graph.labelCountAsOfWindowProbe], which fires BETWEEN the two reads, and is
// therefore the only place from which the difference is observable at all.
//
// It is driven and not raced, for the reason recorded on
// [TestLabelCountExact_DeclinesWhenHistoryGoesLiveDuringTheCall]: the window is a
// few nanoseconds between two atomic loads, and a concurrent oracle was MEASURED
// not to pin it.
func TestLabelCountAsOf_TheInvertedOrderReportsAWriteFromInsideTheWindow(t *testing.T) {
	const seeded = 2 * labelCountProbeBatch
	g, lid := labelCountAsOfGraph(t, seeded, 8)

	snap := g.BeginRead()
	if got := g.LabelCountAsOf(lid, snap); got != seeded {
		t.Fatalf("precondition: a drained graph answered %d, want %d", got, seeded)
	}

	fired := 0
	g.labelCountAsOfWindowProbe = func() {
		if fired > 0 {
			return // once: the nested reads below must not re-enter
		}
		fired++
		// The write raises its per-label hold BEFORE it touches the index, which
		// is the whole basis of the soundness argument. It lands entirely inside
		// the window, after the cardinality read and before the gate read.
		if err := labelOne(g, seeded); err != nil {
			t.Errorf("seam write: %v", err)
		}
	}
	got := g.LabelCountAsOf(lid, snap)
	g.labelCountAsOfWindowProbe = nil

	if fired == 0 {
		t.Fatal("the seam never fired, so no write landed inside the call: this test did not " +
			"enter the window it exists to close")
	}
	// The oracle is differential as well as absolute: the raw index really did
	// move, so an implementation that read it after the gate has a DIFFERENT
	// number available to return, and returning the right one is a choice.
	if raw := int64(g.nodeIdx.Count(uint32(lid))); raw != seeded+1 {
		t.Fatalf("the seam write did not reach the index (raw count %d, want %d): the two "+
			"orders would return the same number here and this test cannot fail",
			raw, seeded+1)
	}
	if got != seeded {
		// The message states the OBSERVATION first and the diagnosis second,
		// because this assertion is also reached by other defects in the same
		// function (a broken correction path returns 0 here, not seeded+1) and a
		// message that named only one cause would misdirect the reader.
		t.Fatalf("LabelCountAsOf returned %d for a snapshot pinned when the count was %d, "+
			"after a write landed between the cardinality read and the gate read. If %d is "+
			"%d, the gate was consulted BEFORE the cardinality and the count it guards was "+
			"taken after it — the inverted order, which reports a commit newer than the "+
			"snapshot (rmp #2688, rmp #2773). Any other value means the correction path "+
			"itself is wrong.",
			got, seeded, got, seeded+1)
	}

	// The pessimism is CONFINED to the window: once the reader that pinned the
	// horizon is gone and the history it held drains, the zero-alloc branch
	// resumes rather than the gate latching. Releasing snap FIRST is load-bearing:
	// reclamation cannot pass a live reader, so ReclaimNow under an open snap
	// frees nothing and the arm below would measure the correction path again.
	g.EndRead(snap)
	g.ReclaimNow()
	after := g.BeginRead()
	defer g.EndRead(after)
	if n := g.LabelCountAsOf(lid, after); n != seeded+1 {
		t.Fatalf("after draining, LabelCountAsOf answered %d, want %d: the gate latched",
			n, seeded+1)
	}
	if n := testing.AllocsPerRun(100, func() { _ = g.LabelCountAsOf(lid, after) }); n != 0 {
		t.Errorf("after draining, LabelCountAsOf allocated %.2f objects, want 0", n)
	}
}

// TestLabelCountAsOf_PinnedSnapshotIgnoresLaterCommits is the isolation property
// stated on the number rather than on the bitmap: commits that land after the
// snapshot must not move the count, however many of them there are.
func TestLabelCountAsOf_PinnedSnapshotIgnoresLaterCommits(t *testing.T) {
	const seeded = 8
	g, lid := labelCountAsOfGraph(t, seeded, 8)

	snap := g.BeginRead()
	defer g.EndRead(snap)

	for i := 0; i < 4; i++ {
		if err := g.AddNode(fmt.Sprintf("late%d", i)); err != nil {
			t.Fatalf("AddNode(late%d): %v", i, err)
		}
		if err := g.ApplyVersionedCtx(context.Background(), func(tx WriteTx) error {
			return g.Writer(tx).SetNodeLabel(fmt.Sprintf("late%d", i), "P")
		}); err != nil {
			t.Fatalf("label late%d: %v", i, err)
		}
	}
	if raw := int64(g.nodeIdx.Count(uint32(lid))); raw != seeded+4 {
		t.Fatalf("the raw index says %d, want %d: the four commits did not land", raw, seeded+4)
	}
	if got := g.LabelCountAsOf(lid, snap); got != seeded {
		t.Errorf("LabelCountAsOf = %d for a snapshot pinned at %d, after 4 later commits. A "+
			"count that observes commits newer than its snapshot is an Isolation break.",
			got, seeded)
	}
	if got := g.LabelCountAsOf(lid, nil); got != seeded+4 {
		t.Errorf("present-time LabelCountAsOf = %d, want %d", got, seeded+4)
	}
}

// TestLabelCountAsOf_UnknownLabelIsZero pins the boundary the resolver above it
// relies on: a label nothing ever carried counts zero rather than panicking or
// interning an id on a read path.
func TestLabelCountAsOf_UnknownLabelIsZero(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if _, ok := g.Registry().Lookup("NeverInterned"); ok {
		t.Fatal("the fixture already interned the label this test needs to be absent")
	}
	if got := g.LabelCountAsOf(LabelID(4242), nil); got != 0 {
		t.Errorf("LabelCountAsOf on an id no node carries = %d, want 0", got)
	}
}
