package lpg

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// newLabelsCountProbeGraph is [newLabelCountProbeGraph] for the CONJUNCTION
// count: every seeded node carries BOTH labels, so the raw intersection is
// exactly seeded, and the spare nodes carry neither.
//
// Two labels is a floor, not a choice: [label.Index.IntersectCardinality]
// reports (0, false) for fewer than two, so a one-label fixture would send
// [Graph.LabelsCountExact] down its `!ok` return and never reach the window
// under test at all.
func newLabelsCountProbeGraph(t *testing.T, seeded, spare int) (*Graph[string, float64], []LabelID) {
	t.Helper()
	ctx := context.Background()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < seeded+spare; i++ {
		if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatalf("AddNode(n%d): %v", i, err)
		}
	}
	if err := g.ApplyVersionedCtx(ctx, func(tx WriteTx) error {
		w := g.Writer(tx)
		for i := 0; i < seeded; i++ {
			for _, name := range []string{"P", "Q"} {
				if err := w.SetNodeLabel(fmt.Sprintf("n%d", i), name); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed labels: %v", err)
	}
	g.ReclaimNow()
	return g, []LabelID{g.reg.Intern("P"), g.reg.Intern("Q")}
}

// labelOneBoth commits one node's two labels in its own transaction, so the node
// enters BOTH raw bitmaps and the conjunction really moves.
func labelOneBoth(g *Graph[string, float64], idx int) error {
	return g.ApplyVersionedCtx(context.Background(), func(tx WriteTx) error {
		w := g.Writer(tx)
		for _, name := range []string{"P", "Q"} {
			if err := w.SetNodeLabel(fmt.Sprintf("n%d", idx), name); err != nil {
				return err
			}
		}
		return nil
	})
}

// rawIntersect reports the UNCORRECTED conjunction cardinality straight from the
// live bitmaps — the number [Graph.LabelsCountExact]'s zero-alloc branch would
// hand back if its second gate sample were missing.
func rawIntersect(t *testing.T, g *Graph[string, float64], lids []LabelID) int64 {
	t.Helper()
	n, ok := g.nodeIdx.IntersectCardinality(uint32(lids[0]), uint32(lids[1]))
	if !ok {
		t.Fatalf("raw IntersectCardinality declined for %d labels: the fixture cannot drive the "+
			"window under test", len(lids))
	}
	return int64(n)
}

// TestLabelsCountExact_CorrectsWhenHistoryGoesLiveDuringTheCall is the rmp #2775
// regression gate on [Graph.LabelsCountExact]'s SECOND GATE SAMPLE.
//
// # The defect
//
// The function was UNDRIVEN. rmp #2775 measured it: no test in the repository
// installed [Graph.labelCountGateProbe] and called LabelsCountExact, so deleting
// its second gate sample outright left `go test ./graph/lpg/ ./cypher/
// ./cypher/exec/` at exit 0. The sample's own godoc cited rmp #2688 and rmp
// #2326 as the reasons it exists; nothing checked that it still did.
//
// With the sample deleted, a write landing between the gate reading clear and
// the AndCardinality that follows it returns the PRESENT-TIME intersection to a
// snapshot reader. That is the rmp #2326 defect exactly: the count evaluates no
// per-row predicate, so nothing downstream can correct it.
//
// # Why the oracle is the VALUE and not a flag
//
// [Graph.LabelsCountExact] CORRECTS rather than declines — refusing the
// conjunction whenever any label history is live made the intersection
// optimisation never engage at all (rmp #2326) — so ok stays true through the
// window and carries no information. The number is the whole answer, and the
// seam fires from BEFORE the cardinality precisely so the raw intersection is
// contaminated and the correction has something to undo.
func TestLabelsCountExact_CorrectsWhenHistoryGoesLiveDuringTheCall(t *testing.T) {
	const seeded = 2 * labelCountProbeBatch
	g, lids := newLabelsCountProbeGraph(t, seeded, 8)

	snap := g.BeginRead()
	defer g.EndRead(snap)

	// Precondition: on a drained graph the conjunction is answered exactly, so a
	// wrong number below is a CHANGE of behaviour and not the standing answer.
	if n, ok := g.LabelsCountExact(lids, snap); !ok || n != seeded {
		t.Fatalf("precondition: a drained graph answered (%d, %v), want (%d, true); the window "+
			"under test is unreachable and this test cannot fail", n, ok, seeded)
	}

	fired := 0
	g.labelCountGateProbe = func() {
		if fired > 0 {
			return // once: the nested reads below must not re-enter
		}
		fired++
		if err := labelOneBoth(g, seeded); err != nil {
			t.Errorf("seam write: %v", err)
		}
	}
	n, ok := g.LabelsCountExact(lids, snap)
	g.labelCountGateProbe = nil

	if fired == 0 {
		t.Fatal("the seam never fired, so no write landed inside the call: this test did not " +
			"enter the window it exists to close")
	}
	// The seam fires ABOVE the cardinality, so the raw intersection the zero-alloc
	// branch reads is contaminated. Proving that here is what makes the assertion
	// below falsifiable: if the write never reached both bitmaps, the corrected
	// and uncorrected answers coincide and nothing could fail.
	if raw := rawIntersect(t, g, lids); raw != seeded+1 {
		t.Fatalf("the seam write did not reach the raw intersection (got %d, want %d): the "+
			"corrected and uncorrected answers coincide and this test cannot fail",
			raw, seeded+1)
	}
	if !ok {
		t.Fatalf("LabelsCountExact DECLINED (returning %d) for a conjunction it can answer. "+
			"This function corrects rather than declines: refusing whenever any label history "+
			"is live made the intersection optimisation never engage at all (rmp #2326), which "+
			"TestLabelIntersect_Rapid detects as a vacuous property.", n)
	}
	if n != seeded {
		t.Fatalf("LabelsCountExact returned %d for a snapshot pinned when the conjunction was "+
			"%d, after a write landed inside the call and BEFORE the cardinality read. %d is "+
			"the PRESENT-TIME intersection handed to a snapshot reader: the gate is not "+
			"re-sampled after the cardinality, so roaring's AndCardinality answer over the LIVE "+
			"bitmaps is returned uncorrected. Nothing downstream can repair it — the count "+
			"evaluates no per-row label predicate (rmp #2326, rmp #2688).",
			n, seeded, n)
	}
}

// TestLabelsCountExact_TheInvertedOrderReportsAWriteFromInsideTheWindow is the
// rmp #2775 regression gate on [Graph.LabelsCountExact]'s ORDER: the cardinality
// is read first and the second gate sample second, and swapping them is an
// Isolation break.
//
// # Why this needs a seam of its own
//
// [Graph.labelCountGateProbe] fires BEFORE the cardinality, and from there the
// order is unobservable: a write driven there lands before both reads, so the
// gate sees its hold whichever side of the cardinality it sits on, the
// correction runs either way, and both orders return the same right answer.
// rmp #2775 measured that — swapping the two reads left the whole suite at
// exit 0. So this test drives [Graph.labelCountWindowProbe], which fires BETWEEN
// them and is the only position from which the difference exists at all:
//
//	sound (count -> gate): the write lands after the cardinality, so the raw
//	  intersection is the snapshot's own; the gate then sees the hold and the
//	  correction returns that same number.
//	inverted (gate -> count): the gate reads clear, THEN the write raises its
//	  hold and enters both bitmaps, THEN AndCardinality picks the contaminated
//	  number up — and it is returned as the snapshot's answer.
//
// It is driven and not raced for the reason recorded on
// [TestLabelCountExact_DeclinesWhenHistoryGoesLiveDuringTheCall]: the window is a
// few nanoseconds between two atomic loads, and a concurrent oracle was MEASURED
// not to pin it.
func TestLabelsCountExact_TheInvertedOrderReportsAWriteFromInsideTheWindow(t *testing.T) {
	const seeded = 2 * labelCountProbeBatch
	g, lids := newLabelsCountProbeGraph(t, seeded, 8)

	snap := g.BeginRead()
	defer g.EndRead(snap)

	if n, ok := g.LabelsCountExact(lids, snap); !ok || n != seeded {
		t.Fatalf("precondition: a drained graph answered (%d, %v), want (%d, true); the window "+
			"under test is unreachable and this test cannot fail", n, ok, seeded)
	}

	fired := 0
	g.labelCountWindowProbe = func() {
		if fired > 0 {
			return // once: the nested reads below must not re-enter
		}
		fired++
		// The write raises its per-label hold BEFORE it touches either bitmap,
		// which is the whole basis of the soundness argument. It lands entirely
		// inside the window, after the cardinality read and before the gate read.
		if err := labelOneBoth(g, seeded); err != nil {
			t.Errorf("seam write: %v", err)
		}
	}
	n, ok := g.LabelsCountExact(lids, snap)
	g.labelCountWindowProbe = nil

	if fired == 0 {
		t.Fatal("the seam never fired, so no write landed inside the call: this test did not " +
			"enter the window it exists to close")
	}
	// Differential: the raw intersection really did move, so an implementation
	// that reads it AFTER the gate has a different number available to return.
	if raw := rawIntersect(t, g, lids); raw != seeded+1 {
		t.Fatalf("the seam write did not reach the raw intersection (got %d, want %d): the two "+
			"orders would return the same number here and this test cannot fail",
			raw, seeded+1)
	}
	if !ok {
		t.Fatalf("LabelsCountExact DECLINED (returning %d) for a conjunction it can answer; it "+
			"corrects rather than declines (rmp #2326)", n)
	}
	if n != seeded {
		// The message states the OBSERVATION first and the diagnosis second: this
		// assertion is also reached by a broken correction path, and a message
		// naming only one cause would misdirect the reader.
		t.Fatalf("LabelsCountExact returned %d for a snapshot pinned when the conjunction was "+
			"%d, after a write landed between the cardinality read and the gate read. %d is the "+
			"PRESENT-TIME intersection, and only the INVERTED order can return it: the gate was "+
			"consulted BEFORE the cardinality, so it read clear and the intersection it guards "+
			"was taken after the write — reporting a commit newer than the snapshot (rmp "+
			"#2775). Any other value means the correction path itself is wrong.",
			n, seeded, seeded+1)
	}
}

// TestLabelCountBound_TheInvertedOrderReportsAWriteFromInsideTheWindow is the
// rmp #2775 regression gate on [Graph.LabelCountBound]'s ORDER: the cardinality
// is read FIRST and the gate second, and swapping them is an Isolation break.
//
// # The defect
//
// [Graph.LabelCountBound]'s godoc rests on that order — it is the stated reason
// the function never had [Graph.LabelCountExact]'s rmp #2688 defect — and until
// rmp #2775 the function had NO seam at all, so the claim was untestable rather
// than merely untested. Inverting the two reads left `go test ./graph/lpg/
// ./cypher/ ./cypher/exec/` at exit 0.
//
// Inverted, a gate reading clear lets the write raise its hold, touch the index,
// and have the contaminated present-time count returned as the snapshot's EXACT
// answer. exact is the contract this function makes about a number it otherwise
// only bounds, so the flag is the oracle here.
//
// # What this gate is worth, honestly
//
// rmp #2775 also measured the blast radius, and it is narrower than the claim:
// no production caller reads exact. [lpgLabelResolver.ResolveLabelCountBound]'s
// only non-test consumer discards it (`n, _ :=`, cypher/api.go), and the
// statistics path rejected the bound outright as the wrong instrument (rmp
// #2771). The inverted order's error in n is also an OVER-count, which is the
// safe direction for an upper bound and is re-checked against the materialised
// cardinality anyway.
//
// The seam is not free either, and rmp #2775 measured it rather than waving it
// through: +0.337 ns/op on the bound's cheapest path (9.410n ± 2% against 9.073n
// ± 1%, +3.58%, p=0.004, n=6, interleaved, Apple M4, host NOT idle at loadavg
// ~2.4), with B/op and allocs/op unchanged at 0. It was added regardless because
// LabelCountBound is EXPORTED — the claim binds callers outside this repository —
// and because the function is called once per plan build, where a third of a
// nanosecond is ~1e-4 of the plan it screens. See [Graph.LabelCountBound] for the
// full weighing.
func TestLabelCountBound_TheInvertedOrderReportsAWriteFromInsideTheWindow(t *testing.T) {
	const seeded = 2 * labelCountProbeBatch
	g, lid := newLabelCountProbeGraph(t, seeded, 8)

	snap := g.BeginRead()
	defer g.EndRead(snap)

	// Precondition: on a drained graph the bound is EXACT and equal to the count,
	// so a decline below is a CHANGE of behaviour and not the standing answer.
	if n, exact := g.LabelCountBound(lid, snap); !exact || n != seeded {
		t.Fatalf("precondition: a drained graph answered (%d, %v), want (%d, true); the window "+
			"under test is unreachable and this test cannot fail", n, exact, seeded)
	}

	fired := 0
	g.labelCountWindowProbe = func() {
		if fired > 0 {
			return // once: the nested reads below must not re-enter
		}
		fired++
		if err := labelOne(g, seeded); err != nil {
			t.Errorf("seam write: %v", err)
		}
	}
	n, exact := g.LabelCountBound(lid, snap)
	g.labelCountWindowProbe = nil

	if fired == 0 {
		t.Fatal("the seam never fired, so no write landed inside the call: this test did not " +
			"enter the window it exists to close")
	}
	// Differential: the raw index really did move, so an implementation that reads
	// it AFTER the gate has a different number available to return.
	if raw := int64(g.nodeIdx.Count(uint32(lid))); raw != seeded+1 {
		t.Fatalf("the seam write did not reach the index (raw count %d, want %d): the two orders "+
			"would return the same number here and this test cannot fail", raw, seeded+1)
	}
	if exact {
		t.Fatalf("LabelCountBound returned (%d, EXACT) for a snapshot pinned when the count was "+
			"%d, after a write landed between the cardinality read and the gate read. The gate "+
			"was therefore consulted BEFORE the cardinality: it read clear, the write then "+
			"raised its hold and touched the index, and the count that followed is "+
			"present-time — handed back as the snapshot's EXACT answer. exact may be true ONLY "+
			"when no filtering is needed at all, and a write held live by this pinned snapshot "+
			"means it is (rmp #2775).", n, seeded)
	}
	// The loose arm must still be a BOUND. Declining to be exact is not licence to
	// answer below the snapshot's own count.
	if n < seeded {
		t.Fatalf("LabelCountBound returned %d, BELOW the snapshot's own count of %d: the "+
			"correction arm is no longer an upper bound and the planner screens it feeds "+
			"would decline a scan that has rows", n, seeded)
	}
}
