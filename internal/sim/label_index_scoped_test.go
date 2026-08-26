package sim

// label_index_scoped_test.go — the short-layer gate for the scoped/range label
// index scenario (rmp #2496).
//
// The structure follows the standing shape of this package: the scenario runs
// clean; the evidence is asserted as NUMBERS, so "it passed" is a separate
// question from "it exercised something"; every non-vacuity gate is shown to be
// WIRED by driving a configuration that cannot satisfy it; and every clause is
// shown to be able to FAIL under a perturbation that reproduces the output the
// real defect would produce, with the unperturbed control required to stay
// silent first.
//
// Four tests here are unusual and deliberate:
//
//   - [TestLabelIndexScoped_ModelIsIndependent] tests the ORACLE. The whole
//     scenario is an argument from a naive model, and a model nobody has checked
//     is an assumption with extra steps. It pins the model's own answers on
//     hand-computed adjacency, overlap and inverted cases — the exact cases the
//     index is then held to.
//   - [TestLabelIndexScoped_RelBoundsMatchTheirNames] tests the FIXTURE. Every
//     relationship is asserted to have the geometry its name claims, because a
//     mislabelled "adjacent" that quietly overlapped would delete the coverage
//     the sweep exists for while every clause still passed.
//   - [TestLabelIndexScoped_GuardClassifierIsExact] tests the INSTRUMENT. The
//     corruption arm distinguishes six refusal branches by the wording of their
//     messages, and a classifier that binned two of them together would credit
//     one guard's answer to another. It includes an ambiguous message and
//     requires it to come back unclassified rather than approximated.
//   - [TestLabelIndexScoped_SmallSetMaxMirrorMatchesSource] parses the library's
//     unexported `smallSetMax` out of its source. Both the tier arm's widths and
//     both halves of the dense-small pin are positioned RELATIVE to it, so a
//     silent drift would put the treatment and the control on the same side of
//     the threshold and make the pin vacuous while it still passed.
//
// Two of the scenario's clauses are TRIPWIRES rather than detectors on their
// production path, and [liCheckCorruption]'s doc comment says which and why:
// `corrupt-restore` cannot fire while `Deserialize` parses into a fresh map and
// swaps at the end, and `corrupt-refusal` on the raw family detects the CRC
// CHECK rather than a CRC collision. Their LOGIC is still proved fireable here,
// by [liPerturbHideRestore] and [liPerturbSkipDamage] respectively — which is
// the point of separating "the clause can reject this evidence" from "the
// production path can produce that evidence".
//
// NOTHING in this file calls t.Parallel(). Every test here uses goleak, and the
// two are structurally incompatible: Go's testing package parks a parallel test's
// parent in `testing.tRunner.func1`, which goleak does not ignore, so a test that
// did both could never pass.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// liClauseNames returns the clause name of every violation, in order, with the
// scenario prefix and the angle brackets stripped.
func liClauseNames(vs []Violation) []string {
	out := make([]string, 0, len(vs))
	const prefix = "label-index-scoped:"
	for _, v := range vs {
		name := v.Op
		if len(name) > 2 && name[0] == '<' {
			name = name[1 : len(name)-1]
		}
		name = strings.TrimPrefix(name, prefix)
		out = append(out, name)
	}
	return out
}

// liHasClause reports whether the named clause fired.
func liHasClause(vs []Violation, clause string) bool {
	for _, got := range liClauseNames(vs) {
		if got == clause {
			return true
		}
	}
	return false
}

// liUniqueClauses returns the sorted set of clause names that fired.
func liUniqueClauses(vs []Violation) []string {
	seen := map[string]struct{}{}
	for _, c := range liClauseNames(vs) {
		seen[c] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// liAdjudicate re-runs both check families over the evidence, so a test reads the
// COMPLETE clause list rather than the report's bounded sample.
func liAdjudicate(ev *LabelIndexScopedEvidence) []Violation {
	return append(checkLabelIndexScoped(ev), checkLabelIndexScopedNonVacuity(ev)...)
}

// liRunPerturbed drives one run with the given perturbation and returns the
// evidence and the full violation list.
func liRunPerturbed(t *testing.T, seed uint64, p liPerturb) (*LabelIndexScopedEvidence, []Violation) {
	t.Helper()
	cfg := DefaultLabelIndexScopedConfig(seed)
	cfg.Perturb = p
	ev, _, err := RunLabelIndexScoped(context.Background(), cfg)
	if err != nil {
		t.Fatalf("perturbation %s: run error: %v", p, err)
	}
	return ev, liAdjudicate(ev)
}

// TestLabelIndexScoped_ScenarioPasses runs the registered scenario at its
// catalogue seed and requires a clean report.
func TestLabelIndexScoped_ScenarioPasses(t *testing.T) {
	defer goleak.VerifyNone(t)
	ev, report, err := RunLabelIndexScoped(context.Background(),
		DefaultLabelIndexScopedConfig(labelIndexScopedDefaultSeed))
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("the scenario reported %d violation(s):\n%s\n\nevidence:\n%s",
			len(report.Violations), report.String(), ev.String())
	}
	t.Logf("evidence:\n%s", ev.String())
}

// TestLabelIndexScoped_ScenarioIsRegistered pins the catalogue entry's mode and
// the presence of a run override. A ModeDeterministic scenario without one
// dispatches to runDeterministic, which drives an engine this scenario does not
// have.
func TestLabelIndexScoped_ScenarioIsRegistered(t *testing.T) {
	defer goleak.VerifyNone(t)
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := r.Lookup(ScenarioLabelIndexScoped)
	if !ok {
		t.Fatalf("%q is not in the default registry", ScenarioLabelIndexScoped)
	}
	if sc.Mode != ModeDeterministic {
		t.Fatalf("mode is %v, want %v", sc.Mode, ModeDeterministic)
	}
	if sc.run == nil {
		t.Fatal("the scenario has no run override; it would dispatch to runDeterministic, which " +
			"drives a Cypher engine this scenario does not build")
	}
	if sc.DefaultSeed != labelIndexScopedDefaultSeed {
		t.Fatalf("default seed is %#x, want %#x", sc.DefaultSeed, labelIndexScopedDefaultSeed)
	}
}

// TestLabelIndexScoped_EvidenceIsSubstantive asserts what the run REACHED, as
// numbers. A scenario that passes because it did nothing is the failure mode
// these assertions exist for, and they are separate from the non-vacuity gates
// so that a gate going soft cannot hide behind the clauses passing.
func TestLabelIndexScoped_EvidenceIsSubstantive(t *testing.T) {
	defer goleak.VerifyNone(t)
	ev, _, err := RunLabelIndexScoped(context.Background(),
		DefaultLabelIndexScopedConfig(labelIndexScopedDefaultSeed))
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	wantCells := int(liRelCount) * int(liDirCount)
	if len(ev.Cells) != wantCells {
		t.Fatalf("the sweep produced %d (relationship, direction) cells, want %d",
			len(ev.Cells), wantCells)
	}
	for i := range ev.Cells {
		c := &ev.Cells[i]
		if c.Drives < ev.Epochs {
			t.Errorf("cell %s/%s was driven %d times across %d epochs; every cell is swept twice "+
				"per epoch", c.Rel, c.Dir, c.Drives, ev.Epochs)
		}
		if c.IndexDelta != c.ModelDelta {
			t.Errorf("cell %s/%s moved the index by %+d and the model by %+d",
				c.Rel, c.Dir, c.IndexDelta, c.ModelDelta)
		}
	}
	// The two degenerate relationships must move NOTHING, in either direction.
	// If they ever moved members the sweep would be quietly testing something
	// other than an empty interval.
	for i := range ev.Cells {
		c := &ev.Cells[i]
		if (c.Rel == liRelInverted || c.Rel == liRelEmptyOffByOne) && c.ModelDelta != 0 {
			t.Errorf("the %s relationship moved the model by %+d through %s; an interval that names "+
				"no ids must move nothing", c.Rel, c.ModelDelta, c.Dir)
		}
	}

	if ev.RangeOps < liFloorRangeOps || ev.PeakMembers < liFloorMembers {
		t.Errorf("the sweep drove %d range ops peaking at %d members, floors %d and %d",
			ev.RangeOps, ev.PeakMembers, liFloorRangeOps, liFloorMembers)
	}
	if ev.Mismatches != 0 || ev.UnionMismatches != 0 || ev.RTContentMismatch != 0 {
		t.Errorf("mismatches: membership %d, union %d, round-trip content %d; want none",
			ev.Mismatches, ev.UnionMismatches, ev.RTContentMismatch)
	}
	if ev.RoundTrips != 2 {
		t.Errorf("the round-trip arm ran %d cycles, want 2 (the fixpoint claim needs both)",
			ev.RoundTrips)
	}
	if ev.TierChecks != len(liTierWidths()) || ev.TierMismatch != 0 {
		t.Errorf("tier identity: %d checks, %d mismatches; want %d and 0",
			ev.TierChecks, ev.TierMismatch, len(liTierWidths()))
	}
	if len(ev.Scopes) != 3*len(liScopeOps()) {
		t.Errorf("the scope arm produced %d rows, want %d", len(ev.Scopes), 3*len(liScopeOps()))
	}

	// The corruption arm: the control, every raw region, every re-stamped trial
	// and every truncation, all against a populated receiver.
	raw, restamp, trunc, control := 0, 0, 0, 0
	for i := range ev.Corrupt {
		c := &ev.Corrupt[i]
		if !c.TargetWasPopulated {
			t.Errorf("corruption trial %s/%s used an empty receiver", c.Family, c.Region)
		}
		switch {
		case c.Region == liCleanControlRegion:
			control++
		case c.Family == liFamilyRaw:
			raw++
			if c.Guard != liGuardCRC {
				t.Errorf("the raw flip of %s was answered by %q, want the CRC; the checksum covers "+
					"every byte of the body and is the only detector a bad sector can reach",
					c.Region, c.Guard)
			}
		case c.Family == liFamilyRestamp:
			restamp++
		default:
			trunc++
		}
	}
	if control != 1 || raw != len(liRegionOrder()) || restamp != liRestampTrials ||
		trunc < liTruncateTrialsMin {
		t.Errorf("corruption coverage: %d control, %d raw, %d restamp, %d truncate; want 1, %d, %d, >=%d",
			control, raw, restamp, trunc, len(liRegionOrder()), liRestampTrials, liTruncateTrialsMin)
	}

	// The boundary CONTRACT (#2607) and the two remaining pins, asserted as the
	// numbers the file header quotes.
	if ev.Boundary.AddGot != liBoundarySpan+1 || ev.Boundary.AddNaive != liBoundarySpan+1 {
		t.Errorf("boundary: AddRange over [max-%d, max] yielded %d, naive %d; the closed interval "+
			"must be honoured at math.MaxUint64",
			liBoundarySpan, ev.Boundary.AddGot, ev.Boundary.AddNaive)
	}
	if ev.Boundary.AddBelowGot != ev.Boundary.AddBelowNaive {
		t.Errorf("the boundary control yielded %d, want %d — behaviour at the boundary must be "+
			"attributable to the final id", ev.Boundary.AddBelowGot, ev.Boundary.AddBelowNaive)
	}
	if ev.Boundary.RemoveGot != ev.Boundary.RemoveNaive {
		t.Errorf("boundary: RemoveRange over [max-3, max] left %d of %d ids on the bitmap tier, "+
			"want %d", ev.Boundary.RemoveGot, ev.Boundary.RemoveBefore, ev.Boundary.RemoveNaive)
	}
	if ev.Boundary.RemoveInlineGot != ev.Boundary.RemoveGot ||
		ev.Boundary.RemoveInlineBefore != ev.Boundary.RemoveBefore {
		t.Errorf("boundary: the inline tier went %d -> %d and the bitmap tier %d -> %d; the same "+
			"operation over the same membership cannot depend on the tier",
			ev.Boundary.RemoveInlineBefore, ev.Boundary.RemoveInlineGot,
			ev.Boundary.RemoveBefore, ev.Boundary.RemoveGot)
	}
	if int(ev.Phantom.AfterLabelCount) != liPhantomLabels ||
		ev.Phantom.AfterBytes <= ev.Phantom.EmptyBytes || ev.Phantom.QueryVisible {
		t.Errorf("phantom: %d labels declared in %d bytes (empty index %d), query-visible=%v",
			ev.Phantom.AfterLabelCount, ev.Phantom.AfterBytes, ev.Phantom.EmptyBytes,
			ev.Phantom.QueryVisible)
	}
	d := &ev.DenseSmall
	if d.Stable || !d.CtrlStable || d.Second != d.Third || d.Second <= d.First {
		t.Errorf("dense-small: width %d went %d -> %d -> %d (stable=%v); control width %d went "+
			"%d -> %d (stable=%v). The pin records an unstable first cycle that converges, against "+
			"a stable control", d.Width, d.First, d.Second, d.Third, d.Stable, d.CtrlWidth,
			d.CtrlFirst, d.CtrlSecond, d.CtrlStable)
	}
	same, differ := 0, 0
	for i := range ev.RangeTier {
		if ev.RangeTier[i].Equal {
			same++
		} else {
			differ++
		}
	}
	if same == 0 || differ == 0 {
		t.Errorf("the Add-versus-AddRange measurement found %d identical and %d differing widths; "+
			"it must bracket the crossover", same, differ)
	}
}

// TestLabelIndexScoped_Deterministic requires two runs of one seed to produce
// identical evidence, and two DIFFERENT seeds to produce different evidence.
//
// The second half matters as much as the first: a digest that never moves is
// stable for the wrong reason, and would make the first half pass on a harness
// that measured nothing.
func TestLabelIndexScoped_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)
	run := func(seed uint64) *LabelIndexScopedEvidence {
		ev, _, err := RunLabelIndexScoped(context.Background(), DefaultLabelIndexScopedConfig(seed))
		if err != nil {
			t.Fatalf("seed %#x: run error: %v", seed, err)
		}
		return ev
	}
	a, b := run(labelIndexScopedDefaultSeed), run(labelIndexScopedDefaultSeed)
	if a.Digest != b.Digest {
		t.Fatalf("two runs of seed %#x produced digests %#016x and %#016x",
			labelIndexScopedDefaultSeed, a.Digest, b.Digest)
	}
	if a.ReproducibleSummary() != b.ReproducibleSummary() {
		t.Fatalf("two runs of seed %#x produced different summaries:\n%s\n%s",
			labelIndexScopedDefaultSeed, a.ReproducibleSummary(), b.ReproducibleSummary())
	}
	other := run(labelIndexScopedDefaultSeed ^ 0x5EED)
	if other.Digest == a.Digest {
		t.Fatalf("two DIFFERENT seeds produced the same digest %#016x; the digest is not a "+
			"function of what the run measured", a.Digest)
	}
}

// liPerturbTargets maps each perturbation to the clause or gate it must fire.
// Other clauses may co-fire — several perturbations damage a shared fixture —
// and the test records the full set; what it requires is that the NAMED target
// is among them and that the unperturbed control fires nothing at all.
func liPerturbTargets() map[liPerturb]string {
	return map[liPerturb]string{
		liPerturbDropScanTail:       "range-model",
		liPerturbBumpCount:          "range-model",
		liPerturbFlipHas:            "range-model",
		liPerturbDropUnionMember:    "union-model",
		liPerturbRoundTripContent:   "roundtrip-content",
		liPerturbRoundTripBytes:     "roundtrip-fixpoint",
		liPerturbUnstableSerialize:  "serialize-stable",
		liPerturbTierDivergence:     "tier-identity",
		liPerturbSkipDamage:         "corrupt-refusal",
		liPerturbHideRestore:        "corrupt-restore",
		liPerturbBreakCleanControl:  "corrupt-clean",
		liPerturbWrongGuard:         "corrupt-guard",
		liPerturbScopeSwap:          "scope-constructor",
		liPerturbScopeRouting:       "scope-routing",
		liPerturbBoundaryWraps:      "boundary-pin",
		liPerturbPhantomGone:        "phantom-pin",
		liPerturbEntryFloor:         "entry-floor",
		liPerturbDenseSmallStable:   "dense-small-pin",
		liPerturbEmptySweep:         "gate:cells",
		liPerturbUnionSingleShape:   "gate:union-shapes",
		liPerturbSkipRegions:        "gate:corrupt-regions",
		liPerturbEmptyTarget:        "gate:corrupt-populated",
		liPerturbBoundaryControlBad: "gate:boundary-control",
		liPerturbRangeTierFlat:      "gate:range-tier-crossover",
		liPerturbDenseSmallCtrl:     "gate:dense-small-control",
	}
}

// TestLabelIndexScoped_PerturbationsFire requires the unperturbed run to be
// silent and every perturbation to fire its named clause or gate.
//
// The control is asserted FIRST and the whole test stops if it is not silent:
// a perturbation that fires against a background of unrelated violations has
// demonstrated nothing.
func TestLabelIndexScoped_PerturbationsFire(t *testing.T) {
	defer goleak.VerifyNone(t)
	const seed = labelIndexScopedDefaultSeed

	_, control := liRunPerturbed(t, seed, liPerturbNone)
	if len(control) != 0 {
		t.Fatalf("the unperturbed control fired %v; every perturbation below would then be "+
			"unattributable", liUniqueClauses(control))
	}

	targets := liPerturbTargets()
	all := liAllPerturbs()
	if len(all) != len(targets) {
		t.Fatalf("there are %d perturbations and %d named targets; every perturbation must name "+
			"the clause it exists to fire", len(all), len(targets))
	}
	for _, p := range all {
		want, ok := targets[p]
		if !ok {
			t.Errorf("perturbation %s names no target clause", p)
			continue
		}
		_, got := liRunPerturbed(t, seed, p)
		if !liHasClause(got, want) {
			t.Errorf("perturbation %s did not fire %q; it fired %v", p, want, liUniqueClauses(got))
			continue
		}
		t.Logf("%-22s fired %-26s (all: %v)", p, want, liUniqueClauses(got))
	}
}

// TestLabelIndexScoped_ModelIsIndependent tests the ORACLE.
//
// Every membership clause in this scenario is an argument from [liModel], so a
// model that agreed with the index by accident would make the whole file
// vacuous. These cases are hand-computed from the closed-interval reading of
// "every id in [from, to]" and cover exactly the geometry the sweep drives:
// adjacency on both sides, overlap on both sides, containment, the single-element
// interval, and the two intervals that name nothing.
func TestLabelIndexScoped_ModelIsIndependent(t *testing.T) {
	defer goleak.VerifyNone(t)
	m := newLIModel()

	m.addRange(1, 10, 20)
	if got := m.count(1); got != 11 {
		t.Fatalf("[10,20] is 11 ids, the model says %d", got)
	}
	// Adjacent above: no overlap, and the two must fuse into one contiguous run.
	m.addRange(1, 21, 25)
	if got := m.count(1); got != 16 {
		t.Fatalf("[10,20] + [21,25] is 16 ids, the model says %d", got)
	}
	// Overlapping: the intersection must be counted once.
	m.addRange(1, 24, 30)
	if got := m.count(1); got != 21 {
		t.Fatalf("... + [24,30] is 21 ids, the model says %d", got)
	}
	// Inverted and off-by-one-empty name nothing.
	m.addRange(1, 30, 10)
	m.addRange(1, 11, 10)
	if got := m.count(1); got != 21 {
		t.Fatalf("an inverted and an empty AddRange changed the model to %d", got)
	}
	// Endpoints are INCLUSIVE in both directions.
	m.removeRange(1, 30, 30)
	if m.has(1, 30) || !m.has(1, 29) {
		t.Fatal("removeRange(30,30) must remove exactly id 30")
	}
	m.removeRange(1, 10, 10)
	if m.has(1, 10) || !m.has(1, 11) {
		t.Fatal("removeRange(10,10) must remove exactly id 10")
	}
	if got := m.count(1); got != 19 {
		t.Fatalf("after removing both endpoints the model holds %d, want 19", got)
	}
	// scan is ascending, and nil rather than empty when the label is gone.
	got := m.scan(1)
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("scan is not strictly ascending at %d: %v", i, got[:i+1])
		}
	}
	m.removeRange(1, 0, 1000)
	if m.scan(1) != nil || m.count(1) != 0 {
		t.Fatal("a label emptied by removeRange must scan nil and count 0")
	}
	if m.scan(999) != nil {
		t.Fatal("scan of a label the model never saw must be nil")
	}
	// union deduplicates across labels and ignores unknown ones.
	m.addRange(2, 1, 3)
	m.addRange(3, 3, 5)
	if got := m.union(2, 3); fmt.Sprint(got) != "[1 2 3 4 5]" {
		t.Fatalf("union(2,3) = %v, want [1 2 3 4 5]", got)
	}
	if got := m.union(2, 2, 777); fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("union(2,2,777) = %v, want [1 2 3]", got)
	}
	if m.union() != nil {
		t.Fatal("union of no labels must be nil in the model")
	}
}

// TestLabelIndexScoped_RelBoundsMatchTheirNames tests the FIXTURE.
//
// The sweep's value rests entirely on the thirteen relationships really having
// the geometry their names claim. A "adjacent-below" that quietly overlapped
// would delete the coverage the inclusive/exclusive conversion needs while every
// clause in the file still passed, and nothing else would notice.
func TestLabelIndexScoped_RelBoundsMatchTheirNames(t *testing.T) {
	defer goleak.VerifyNone(t)
	// Two bands: the narrowest the drawer can produce and a wide one, so the
	// geometry is not an artefact of one width.
	bands := [][2]uint64{
		{liBaseLo, liBaseLo + liBaseWidthMin},
		{liBaseLo + liBaseSpan, liBaseLo + liBaseSpan + liBaseWidthMin + liBaseWidthSpan},
	}
	for _, band := range bands {
		lo, hi := band[0], band[1]
		check := func(r liRel, ok bool, why string) {
			t.Helper()
			from, to := liRelBounds(lo, hi, r)
			if !ok {
				t.Errorf("band [%d,%d]: %s yielded [%d,%d], which is not %s", lo, hi, r, from, to, why)
			}
		}
		f := func(r liRel) (uint64, uint64) { return liRelBounds(lo, hi, r) }

		a, b := f(liRelDisjointBelow)
		check(liRelDisjointBelow, a <= b && b < lo-1, "wholly below the band with a gap")
		a, b = f(liRelAdjacentBelow)
		check(liRelAdjacentBelow, a <= b && b == lo-1, "touching lo from below with no overlap")
		a, b = f(liRelOverlapLow)
		check(liRelOverlapLow, a < lo && b >= lo && b < hi, "crossing lo only")
		a, b = f(liRelContained)
		check(liRelContained, a > lo && b < hi && a <= b, "strictly inside the band")
		a, b = f(liRelIdentical)
		check(liRelIdentical, a == lo && b == hi, "exactly the band")
		a, b = f(liRelContaining)
		check(liRelContaining, a < lo && b > hi, "extending past both endpoints")
		a, b = f(liRelOverlapHigh)
		check(liRelOverlapHigh, a > lo && a <= hi && b > hi, "crossing hi only")
		a, b = f(liRelAdjacentAbove)
		check(liRelAdjacentAbove, a == hi+1 && b >= a, "touching hi from above with no overlap")
		a, b = f(liRelDisjointAbove)
		check(liRelDisjointAbove, a > hi+1 && b >= a, "wholly above the band with a gap")
		a, b = f(liRelSingleInside)
		check(liRelSingleInside, a == b && a >= lo && a <= hi, "one id inside the band")
		a, b = f(liRelSingleOutside)
		check(liRelSingleOutside, a == b && (a < lo || a > hi), "one id outside the band")
		a, b = f(liRelInverted)
		check(liRelInverted, a > b, "inverted")
		a, b = f(liRelEmptyOffByOne)
		check(liRelEmptyOffByOne, a == b+1, "inverted by exactly one")
	}
	// And every kind must be named, so a new one cannot be added without a label.
	for r := liRel(0); r < liRelCount; r++ {
		if strings.HasPrefix(r.String(), "rel(") {
			t.Errorf("relationship %d has no name", uint8(r))
		}
	}
	for d := liDir(0); d < liDirCount; d++ {
		if s := d.String(); s != "AddRange" && s != "RemoveRange" {
			t.Errorf("direction %d renders as %q", uint8(d), s)
		}
	}
}

// TestLabelIndexScoped_GuardClassifierIsExact tests the INSTRUMENT.
//
// The corruption arm's `corrupt-guard` clause distinguishes six refusal branches
// by the wording of their messages. A classifier that matched on a single word
// would credit the bitmap-length guard's answer to the bitmap-parse guard — both
// messages contain "bitmap" — so the needles are whole phrases, and a message
// matching two of them must come back unclassified rather than approximated.
func TestLabelIndexScoped_GuardClassifierIsExact(t *testing.T) {
	defer goleak.VerifyNone(t)
	if got := liClassifyGuard(nil); got != liGuardAccepted {
		t.Fatalf("a nil error classified as %q, want %q", got, liGuardAccepted)
	}
	for guard, needle := range liGuardNeedles() {
		err := fmt.Errorf("%w: %s and some trailing detail", index.ErrIndexCorrupted, needle)
		if got := liClassifyGuard(err); got != guard {
			t.Errorf("a message carrying %q classified as %q, want %q", needle, got, guard)
		}
	}
	// Two needles in one message must NOT be silently binned into either.
	both := fmt.Errorf("%w: implausible bitmap length 1 and bitmap parse failure",
		index.ErrIndexCorrupted)
	if got := liClassifyGuard(both); got != liGuardUnknown {
		t.Errorf("an ambiguous message classified as %q, want %q", got, liGuardUnknown)
	}
	if got := liClassifyGuard(errors.New("something else entirely")); got != liGuardUnknown {
		t.Errorf("an unrecognised message classified as %q, want %q", got, liGuardUnknown)
	}
	// The needles must be pairwise non-containing, or one guard's phrase could
	// never be distinguished from another's.
	needles := liGuardNeedles()
	for ga, na := range needles {
		for gb, nb := range needles {
			if ga != gb && strings.Contains(na, nb) {
				t.Errorf("the %s needle %q contains the %s needle %q; the %s guard could never be "+
					"classified on its own", ga, na, gb, nb, gb)
			}
		}
	}
}

// TestLabelIndexScoped_SmallSetMaxMirrorMatchesSource parses the library's own
// `smallSetMax` out of graph/index/nodeset.go and requires the mirror in this
// package to match it.
//
// The constant is unexported and has no accessor. Every width in
// [liTierWidths] is at or below it, [liTierGrowTo] must exceed it, and the
// dense-small pin's treatment sits AT it while its control sits one above — so a
// silent drift would move the treatment and the control to the same side of the
// down-convert threshold and make the pin vacuous while it still passed.
func TestLabelIndexScoped_SmallSetMaxMirrorMatchesSource(t *testing.T) {
	defer goleak.VerifyNone(t)
	src := filepath.Join("..", "..", "graph", "index", "nodeset.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Clean(src), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	got, found := 0, false
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "smallSetMax" {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				t.Fatalf("smallSetMax is no longer an integer literal: %T", vs.Values[0])
			}
			n, cerr := strconv.Atoi(lit.Value)
			if cerr != nil {
				t.Fatalf("smallSetMax literal %q: %v", lit.Value, cerr)
			}
			got, found = n, true
		}
	}
	if !found {
		t.Fatalf("smallSetMax was not found in %s; the mirror in this package can no longer be "+
			"checked against it", src)
	}
	if got != liSmallSetMaxMirror {
		t.Fatalf("smallSetMax is %d in %s but liSmallSetMaxMirror is %d; the tier widths and both "+
			"halves of the dense-small pin are positioned relative to the mirror, so this drift "+
			"would make the pin vacuous", got, src, liSmallSetMaxMirror)
	}
	// The fixtures must straddle it the way the arms assume.
	for _, w := range liTierWidths() {
		if w > uint64(got) {
			t.Errorf("tier width %d is above smallSetMax=%d, so that pair compares two BITMAPS and "+
				"asserts nothing about the inline tier", w, got)
		}
	}
	if liTierGrowTo <= got {
		t.Errorf("liTierGrowTo=%d does not exceed smallSetMax=%d, so the bitmap side of the "+
			"tier-identity pair never promotes", liTierGrowTo, got)
	}
}

// TestLabelIndexScoped_ClauseNamesAreStable pins every clause and gate name the
// scenario can emit. The perturbation table addresses clauses by name, and a
// rename that slipped past it would silently stop requiring one of them.
func TestLabelIndexScoped_ClauseNamesAreStable(t *testing.T) {
	defer goleak.VerifyNone(t)
	want := map[string]bool{}
	for _, name := range liPerturbTargets() {
		want[name] = true
	}
	seen := map[string]bool{}
	for _, p := range liAllPerturbs() {
		_, vs := liRunPerturbed(t, labelIndexScopedDefaultSeed, p)
		for _, c := range liClauseNames(vs) {
			seen[c] = true
		}
	}
	for name := range seen {
		if !want[name] {
			t.Logf("clause %q fired but is not the named target of any perturbation; it is fired as "+
				"collateral by at least one of them", name)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("clause %q is a named perturbation target but never fired", name)
		}
	}
}

// liGateKnockouts names, for every non-vacuity gate, a single edit to a HEALTHY
// evidence value that must make exactly that gate fire.
//
// It exists because the perturbation table cannot reach all of them. A
// perturbation drives the whole run, so several gates are only ever fired as
// collateral by one broad perturbation (`empty-sweep` alone fires four), and
// four gates — `tier-checks`, `roundtrip`, `clean-control` and `scope-rows` —
// guard conditions no configuration knob can break, because the arms that supply
// them have no knob. Those four would otherwise be gates nobody had shown could
// fail, which this sprint has already found six of.
//
// The two tests are complementary and neither replaces the other:
// [TestLabelIndexScoped_PerturbationsFire] shows the run PATH can produce the
// evidence a clause rejects; this one shows each gate's LOGIC rejects it.
func liGateKnockouts() []struct {
	Gate  string
	Break func(*LabelIndexScopedEvidence)
} {
	return []struct {
		Gate  string
		Break func(*LabelIndexScopedEvidence)
	}{
		{"gate:cells", func(e *LabelIndexScopedEvidence) { e.Cells = nil }},
		{"gate:model-size", func(e *LabelIndexScopedEvidence) { e.FinalLabels = 0 }},
		{"gate:model-size", func(e *LabelIndexScopedEvidence) { e.PeakMembers = 0 }},
		{"gate:model-size", func(e *LabelIndexScopedEvidence) { e.RangeOps = 0 }},
		{"gate:emptied", func(e *LabelIndexScopedEvidence) { e.EmptiedLabels = 0 }},
		{"gate:promoted", func(e *LabelIndexScopedEvidence) { e.PromotedAfterAdd = 0 }},
		{"gate:tier-checks", func(e *LabelIndexScopedEvidence) { e.TierChecks-- }},
		{"gate:roundtrip", func(e *LabelIndexScopedEvidence) { e.RoundTrips = 0 }},
		{"gate:union-shapes", func(e *LabelIndexScopedEvidence) { e.UnionMultiLabel = 0 }},
		{"gate:union-shapes", func(e *LabelIndexScopedEvidence) { e.UnionUnknownLabel = 0 }},
		{"gate:union-shapes", func(e *LabelIndexScopedEvidence) { e.UnionDuplicate = 0 }},
		{"gate:union-shapes", func(e *LabelIndexScopedEvidence) { e.UnionEmptyDraws = 0 }},
		{"gate:corrupt-regions", func(e *LabelIndexScopedEvidence) {
			// Drop the first raw row, so one layout region is unswept.
			for i := range e.Corrupt {
				if e.Corrupt[i].Family == liFamilyRaw && e.Corrupt[i].Region != liCleanControlRegion {
					e.Corrupt = append(e.Corrupt[:i], e.Corrupt[i+1:]...)
					return
				}
			}
		}},
		{"gate:clean-control", func(e *LabelIndexScopedEvidence) {
			for i := range e.Corrupt {
				if e.Corrupt[i].Region == liCleanControlRegion {
					e.Corrupt = append(e.Corrupt[:i], e.Corrupt[i+1:]...)
					return
				}
			}
		}},
		{"gate:corrupt-populated", func(e *LabelIndexScopedEvidence) {
			e.Corrupt[0].TargetWasPopulated = false
		}},
		{"gate:scope-rows", func(e *LabelIndexScopedEvidence) { e.Scopes = e.Scopes[:1] }},
		{"gate:scope-rows", func(e *LabelIndexScopedEvidence) {
			// Every row expecting acceptance: the routing clause would then pass on
			// an index that consumed everything.
			for i := range e.Scopes {
				e.Scopes[i].WantAccepted = true
			}
		}},
		{"gate:boundary-control", func(e *LabelIndexScopedEvidence) { e.Boundary.AddBelowGot++ }},
		{"gate:phantom-armed", func(e *LabelIndexScopedEvidence) { e.Phantom.AfterLabelCount = 0 }},
		{"gate:dense-small-control", func(e *LabelIndexScopedEvidence) {
			e.DenseSmall.CtrlStable = false
		}},
		{"gate:range-tier-crossover", func(e *LabelIndexScopedEvidence) {
			for i := range e.RangeTier {
				e.RangeTier[i].Equal = true
			}
		}},
		{"gate:range-tier-crossover", func(e *LabelIndexScopedEvidence) {
			for i := range e.RangeTier {
				e.RangeTier[i].Equal = false
			}
		}},
	}
}

// TestLabelIndexScoped_GatesAreWired requires every non-vacuity gate to fire
// when the condition it certifies is removed, and requires the untouched
// evidence to fire nothing.
//
// It also requires the knockout table to MENTION every gate the file can emit,
// so a gate added later without a knockout is a failure rather than an omission.
func TestLabelIndexScoped_GatesAreWired(t *testing.T) {
	defer goleak.VerifyNone(t)

	healthy := func() *LabelIndexScopedEvidence {
		t.Helper()
		ev, _, err := RunLabelIndexScoped(context.Background(),
			DefaultLabelIndexScopedConfig(labelIndexScopedDefaultSeed))
		if err != nil {
			t.Fatalf("run error: %v", err)
		}
		return ev
	}
	if vs := checkLabelIndexScopedNonVacuity(healthy()); len(vs) != 0 {
		t.Fatalf("the untouched evidence already fires %v; no knockout below would be "+
			"attributable", liUniqueClauses(vs))
	}

	covered := map[string]bool{}
	for i, k := range liGateKnockouts() {
		ev := healthy()
		k.Break(ev)
		vs := checkLabelIndexScopedNonVacuity(ev)
		if !liHasClause(vs, k.Gate) {
			t.Errorf("knockout %d for %s fired %v instead", i, k.Gate, liUniqueClauses(vs))
			continue
		}
		covered[k.Gate] = true
	}

	// Every gate the file can emit must have a knockout. The gate names are
	// recovered by scanning this package's own source for the liViolation calls
	// whose clause begins "gate:", so a new gate cannot be added without one.
	for _, gate := range liGateNamesFromSource(t) {
		if !covered[gate] {
			t.Errorf("%s has no knockout in liGateKnockouts, so nothing has shown it can fire", gate)
		}
	}
}

// liGateNamesFromSource returns every gate name the scenario source constructs,
// parsed out of the liViolation call sites rather than listed by hand — a list
// maintained by hand is a list that goes stale.
func liGateNamesFromSource(t *testing.T) []string {
	t.Helper()
	src := filepath.Clean("label_index_scoped.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	seen := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "liViolation" {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		name, uerr := strconv.Unquote(lit.Value)
		if uerr == nil && strings.HasPrefix(name, "gate:") {
			seen[name] = true
		}
		return true
	})
	if len(seen) == 0 {
		t.Fatalf("no gate names were found in %s; the coverage check below would be vacuous", src)
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}
