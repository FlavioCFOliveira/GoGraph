package cypher

// label_count_asof_resolver_test.go — [lpgLabelResolver.ResolveLabelCountAsOf]
// (rmp #2773).
//
// The resolver is the seam between the operator and the substrate, and it is
// where the two gates that used to disagree meet: ResolveLabelCount declines on
// the GLOBAL history flag, ResolveLabelBitmap corrects on the PER-LABEL churn
// gate. These tests pin the new method's contract at that seam — same answer as
// the bitmap, no allocation when no correction is owed — so a regression that
// reintroduced the clone fails here as well as in the end-to-end ceiling.
//
// Race-clean; short layer. No metrics backend is installed, but these share the
// package's fixtures and do not call t.Parallel().

import "testing"

// TestResolveLabelCountAsOf_AnswersWhereTheExactCountDeclines is the seam-level
// statement of the whole finding: in the state the loaded gate reached, the
// exact-or-nothing count refuses and the bitmap that answered it needed no
// correction at all — so the number was available for nothing and was paid for
// with a clone.
func TestResolveLabelCountAsOf_AnswersWhereTheExactCountDeclines(t *testing.T) {
	eng := newSharedEntryRig(t)
	// Drain the rig's OWN history first. A freshly built graph is still churning
	// label N, and this test is about the state where the churn is somewhere else
	// — without the drain it would measure the correction path and prove the
	// opposite of what it says.
	eng.g.ReclaimNow()
	// The resolver is pinned BEFORE the write, exactly as Engine.Run pins one for
	// the duration of a statement.
	src := pinnedStatsSource(t, eng)
	if n, ok := src.ResolveLabelCount("N"); !ok {
		t.Fatalf("the fixture is still churning label N after ReclaimNow (count answered "+
			"(%d, %v)); the decline asserted below would then be the rig's own history "+
			"rather than the state this test constructs", n, ok)
	}
	records := liveHistory(t, eng.g, "asof-resolver")

	// Precondition: without the decline there is nothing here to fix, and every
	// assertion below would pass against the old code too.
	if n, ok := src.ResolveLabelCount("N"); ok {
		t.Fatalf("the snapshot-pinned exact count answered (%d, true) with %d live node-life "+
			"record(s); this test no longer covers the state it was written for", n, records)
	}

	got, ok := src.ResolveLabelCountAsOf("N")
	if !ok {
		t.Fatal("ResolveLabelCountAsOf declined; the live resolver must always answer")
	}
	// The oracle is the SCAN's own answer, not a constant: the two are the same
	// question and may never disagree.
	if want := int64(src.ResolveLabelBitmap("N").GetCardinality()); got != want {
		t.Errorf("ResolveLabelCountAsOf(N) = %d, the filtered bitmap's cardinality is %d", got, want)
	}
	if got != sharedEntryNodes {
		t.Errorf("ResolveLabelCountAsOf(N) = %d, want %d — and if the bitmap agrees with a "+
			"wrong number, both paths are wrong", got, sharedEntryNodes)
	}

	// And it is free. This is the allocation the gate measures end to end; here it
	// is measured on the one call that used to make it.
	if n := testing.AllocsPerRun(200, func() { _, _ = src.ResolveLabelCountAsOf("N") }); n != 0 {
		t.Errorf("ResolveLabelCountAsOf allocated %.2f objects while the churn was on another "+
			"label, want 0. The clone it replaced cost 12.", n)
	}
	// The control: the route it replaced still costs what the finding says it does,
	// so the comparison above is against a live number and not a remembered one.
	if n := testing.AllocsPerRun(200, func() { _ = src.ResolveLabelBitmap("N") }); n == 0 {
		t.Error("ResolveLabelBitmap now allocates nothing either, so this pair no longer " +
			"demonstrates a difference and the zero above proves nothing")
	}
}

// TestResolveLabelCountAsOf_UnknownLabelIsZero pins the boundary the operator
// relies on: an unknown label counts zero and answers ok, matching the empty
// bitmap ResolveLabelBitmap returns, so the operator never falls through to a
// bitmap resolve for a label nothing carries.
func TestResolveLabelCountAsOf_UnknownLabelIsZero(t *testing.T) {
	src := pinnedStatsSource(t, newSharedEntryRig(t))
	got, ok := src.ResolveLabelCountAsOf("NeverInternedLabel")
	if !ok || got != 0 {
		t.Fatalf("ResolveLabelCountAsOf(unknown) = (%d, %v), want (0, true)", got, ok)
	}
	if card := src.ResolveLabelBitmap("NeverInternedLabel").GetCardinality(); card != 0 {
		t.Fatalf("the bitmap for an unknown label has cardinality %d, want 0: the two "+
			"answers must agree", card)
	}
}
