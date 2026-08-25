package sim

// count_store_test.go — the short-layer gate for the count-store oracle
// scenario (rmp #2494).
//
// The structure follows the standing shape of this package: the scenario runs
// clean, the evidence is asserted as NUMBERS (so "it passed" is separated from
// "it exercised something"), each non-vacuity gate is shown to be WIRED by
// driving a configuration that cannot satisfy it, and every clause is shown to
// be able to FAIL by a perturbation that reproduces the output the real defect
// would produce.
//
// Two of the tests here are unusual and deliberate:
//
//   - [TestCountStore_RecoveryHealsDirtyAndNegative] asserts the pre-recovery
//     state is genuinely WRONG (two cells absent where the model says non-zero,
//     one cell negative) before asserting the post-recovery state is right. A
//     heal test that only checked the "after" would pass against a store that
//     was never broken, which is the whole failure mode this scenario exists to
//     rule out.
//   - [TestCountStore_AnchorSwapRetainsAnonymousSourceRows] is the regression gate
//     for the DEFECT this scenario surfaced (rmp #2603): a single-edge pattern with
//     an anonymous, labelled SOURCE lost every row once the anchor-swap peephole
//     re-rooted it. It replaced the pin that held the wrong answer with its A/B
//     attribution attached, and it asserts BOTH the swap-enabled default and the
//     swap-disabled control.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestCountStore_ScenarioPasses runs the registered scenario at its catalogue
// seed and requires a clean report.
func TestCountStore_ScenarioPasses(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioCountStore)
	if !ok {
		t.Fatalf("scenario %q is not registered", ScenarioCountStore)
	}
	if sc.Mode != ModeDeterministic {
		t.Fatalf("scenario mode = %s, want deterministic", sc.Mode)
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("the count-store scenario reported a violation:\n%s", report)
	}
}

// TestCountStore_EvidenceIsNonVacuous asserts on the MEASURED numbers rather
// than on the absence of a violation.
//
// It duplicates the terminal gate deliberately: the gate fails the RUN, this
// test fails the BUILD with the numbers printed, and a budget change that
// quietly stops reaching a clause is far easier to diagnose from the second.
func TestCountStore_EvidenceIsNonVacuous(t *testing.T) {
	ev, report, err := RunCountStore(context.Background(),
		DefaultCountStoreConfig(countStoreDefaultSeed))
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("reported a violation:\n%s", report)
	}
	if ev.LiveChecks == 0 || ev.RecoveredChecks == 0 {
		t.Errorf("parity phases not both reached: live=%d recovered=%d", ev.LiveChecks, ev.RecoveredChecks)
	}
	for f := csFamily(0); f < csFamilyCount; f++ {
		if ev.ComparedLive[f] == 0 {
			t.Errorf("family %s compared NO cell while live (held cells in %d observations): every "+
				"cell was skipped as dirty-covered", f, ev.CellFamilies[f])
		}
		if ev.ComparedRecovered[f] == 0 {
			t.Errorf("family %s compared NO cell after a reopen", f)
		}
	}
	if ev.DirtyLiveChecks == 0 {
		t.Errorf("no live observation was dirty, so the heal claim had nothing to heal")
	}
	if ev.NegativeLiveChecks == 0 {
		t.Errorf("no live observation held a negative cell; the prologue constructs one deliberately")
	}
	if ev.HealedFromDirty == 0 || ev.HealedNegative == 0 {
		t.Errorf("the heal transition was never witnessed: dirty=%d negative=%d",
			ev.HealedFromDirty, ev.HealedNegative)
	}
	if ev.DistinctTLabelsMax < 3 {
		t.Errorf("the T family saw only %d distinct a-position label(s); Person, Vip and Hub are all "+
			"expected", ev.DistinctTLabelsMax)
	}
	for i, sh := range csShapes() {
		if ev.ShapeProbes[i] == 0 {
			t.Errorf("shape %q was never issued", sh.name)
		}
		if sh.srcLabel != "" && sh.dstLabel != "" && ev.ShapeCellChecks[i] == 0 {
			t.Errorf("shape %q never compared its serving count-store cell (skipped %d times)",
				sh.name, ev.ShapeCellSkipped[i])
		}
	}
	// The Hub shapes exist precisely so a labelled cell is comparable while LIVE.
	// If they ever start being skipped, the live three-way comparison has silently
	// gone away.
	for i, sh := range csShapes() {
		if sh.srcLabel == csLabelHub || sh.dstLabel == csLabelHub {
			if ev.ShapeCellSkipped[i] != 0 {
				t.Errorf("Hub shape %q had its cell skipped %d times; a Hub cell must never be dirty",
					sh.name, ev.ShapeCellSkipped[i])
			}
		}
	}
	if ev.MaxBound == 0 || ev.MaxCells == 0 {
		t.Errorf("the Cells() bound clause was never armed: cells=%d bound=%d", ev.MaxCells, ev.MaxBound)
	}
	if ev.MaxCells > ev.MaxBound {
		t.Errorf("Cells()=%d exceeded the ceiling %d", ev.MaxCells, ev.MaxBound)
	}
	if ev.Relabels == 0 || ev.Crashes == 0 || ev.Checkpoints == 0 {
		t.Errorf("coverage missing: relabels=%d crashes=%d checkpoints=%d",
			ev.Relabels, ev.Crashes, ev.Checkpoints)
	}
	t.Logf("count-store evidence: %s", ev.String())
}

// TestCountStore_IsDeterministic requires the same seed to produce the same
// evidence, twice. A harness that drifts makes every failure it ever reports
// unreplayable, so the claim is worth its own test.
func TestCountStore_IsDeterministic(t *testing.T) {
	cfg := DefaultCountStoreConfig(countStoreDefaultSeed)
	cfg.MaxTicks = 120

	first, report, err := RunCountStore(context.Background(), cfg)
	if err != nil || report != nil {
		t.Fatalf("first run: err=%v report=%v", err, report)
	}
	second, report, err := RunCountStore(context.Background(), cfg)
	if err != nil || report != nil {
		t.Fatalf("second run: err=%v report=%v", err, report)
	}
	if a, b := first.ReproducibleSummary(), second.ReproducibleSummary(); a != b {
		t.Fatalf("same seed produced different evidence:\n first: %s\nsecond: %s", a, b)
	}
	// A different seed must produce different evidence, or the "reproducible"
	// claim is being satisfied by an evidence summary that ignores the run.
	other, report, err := RunCountStore(context.Background(), func() CountStoreConfig {
		c := cfg
		c.Seed = cfg.Seed ^ 0xABCD
		return c
	}())
	if err != nil || report != nil {
		t.Fatalf("other-seed run: err=%v report=%v", err, report)
	}
	if other.Digest == first.Digest {
		t.Fatalf("two different seeds produced the same digest %#016x, so the digest is not a "+
			"function of the run", first.Digest)
	}
}

// -----------------------------------------------------------------------------
// The fixture
// -----------------------------------------------------------------------------

// countStoreFixture is a prologue-built simulator plus its probes, the shared
// starting point of every clause test. It is built from the scenario's own
// prologue rather than a second, drifting copy of it.
type countStoreFixture struct {
	sm     *Simulator
	probes *CountStoreProbes
}

// newCountStoreFixture builds the fixture and registers its teardown.
func newCountStoreFixture(t *testing.T, seed uint64) *countStoreFixture {
	t.Helper()
	cfg := DefaultCountStoreConfig(seed)
	sm, err := New(countStoreSimConfig(cfg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	probes := NewCountStoreProbes(NewSeed(cfg.Seed ^ countStoreSeedMix))
	vs, err := countStorePrologue(context.Background(), sm, cfg, probes)
	if err != nil {
		t.Fatalf("prologue: %v", err)
	}
	if len(vs) > 0 {
		t.Fatalf("the prologue itself reported violations: %v", vs)
	}
	return &countStoreFixture{sm: sm, probes: probes}
}

// recover forces one crash+recovery cycle on the fixture.
func (f *countStoreFixture) recover(t *testing.T) {
	t.Helper()
	report, err := f.sm.forceCrash(99, "<test forced crash>")
	if err != nil {
		t.Fatalf("forceCrash: %v", err)
	}
	if report != nil {
		t.Fatalf("forced crash reported a durability violation:\n%s", report)
	}
}

// csClauseNames returns the sorted, de-duplicated clause names in vs, with the
// `<count-store:...>` wrapper stripped.
func csClauseNames(vs []Violation) []string {
	seen := make(map[string]struct{}, len(vs))
	for _, v := range vs {
		name := strings.TrimSuffix(strings.TrimPrefix(v.Op, "<count-store:"), ">")
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// csHasClause reports whether vs contains a violation for the named clause.
func csHasClause(vs []Violation, clause string) bool {
	for _, n := range csClauseNames(vs) {
		if n == clause {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Falsifiability: every parity clause must be able to fire
// -----------------------------------------------------------------------------

// TestCountStore_ParityClausesFire drives each perturbation and requires the
// clause it targets to fire, with the unperturbed control silent in the same
// phase over the same fixture.
//
// The table records the clauses a perturbation is ALLOWED to fire beyond its
// target, and the test fails on anything outside that set. Exactly one entry has
// legitimate collateral and says why: clearing the dirty sets
// ([csPerturbUncoverNegative]) also makes the legitimately-skipped IN-side cells
// comparable, so the DIn and T clauses fire too — which is itself evidence that
// the skip was load-bearing rather than decorative.
func TestCountStore_ParityClausesFire(t *testing.T) {
	cases := []struct {
		perturb csPerturb
		phase   csPhase
		target  string
		also    []string
	}{
		{csPerturbDropE, csPhaseLive, "parity:E", nil},
		{csPerturbInflateT, csPhaseLive, "parity:T", nil},
		{csPerturbDropDOut, csPhaseLive, "parity:DOut", nil},
		{csPerturbDropDIn, csPhaseLive, "parity:DIn", nil},
		{csPerturbSumE, csPhaseLive, "sum-E", nil},
		{csPerturbShrinkBound, csPhaseLive, "cells-bound", nil},
		{csPerturbUnresolvedID, csPhaseLive, "unresolvable-id", nil},
		{csPerturbUncoverNegative, csPhaseLive, "negative-uncovered",
			[]string{"parity:DIn", "parity:T"}},
		{csPerturbFakeDirty, csPhaseRecovered, "dirty-after-recovery",
			// A fake dirty label makes the recovered phase skip nothing extra (the
			// injected label matches no cell), so the only other clause it can reach
			// is none. Listed empty on purpose.
			nil},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s", tc.phase, tc.perturb), func(t *testing.T) {
			f := newCountStoreFixture(t, countStoreDefaultSeed)
			if tc.phase == csPhaseRecovered {
				f.recover(t)
			}
			// The control FIRST, on the same fixture and in the same phase: a clause
			// that fires under perturbation but also fires without it proves nothing.
			if vs := f.probes.Parity(f.sm, 10, tc.phase, csPerturbNone); len(vs) > 0 {
				t.Fatalf("the unperturbed control reported violations in the %s phase: %v",
					tc.phase, vs)
			}
			vs := f.probes.Parity(f.sm, 11, tc.phase, tc.perturb)
			if !csHasClause(vs, tc.target) {
				t.Fatalf("perturbation %s did not fire %q; clauses fired: %v\nviolations: %v",
					tc.perturb, tc.target, csClauseNames(vs), vs)
			}
			allowed := map[string]struct{}{tc.target: {}}
			for _, c := range tc.also {
				allowed[c] = struct{}{}
			}
			for _, got := range csClauseNames(vs) {
				if _, ok := allowed[got]; !ok {
					t.Errorf("perturbation %s fired unexpected clause %q; all fired: %v",
						tc.perturb, got, csClauseNames(vs))
				}
			}
		})
	}
}

// TestCountStore_ShapeClausesFire requires every count(*) shape clause to fire
// when its model reference is perturbed, with the control silent.
func TestCountStore_ShapeClausesFire(t *testing.T) {
	f := newCountStoreFixture(t, countStoreDefaultSeed)
	ctx := context.Background()
	if vs := f.probes.Shapes(ctx, f.sm, 10, csPerturbNone); len(vs) > 0 {
		t.Fatalf("the unperturbed shape control reported violations: %v", vs)
	}
	vs := f.probes.Shapes(ctx, f.sm, 11, csPerturbSumE)
	fired := csClauseNames(vs)
	for _, sh := range csShapes() {
		want := "shape:" + sh.name
		if !csHasClause(vs, want) {
			t.Errorf("an off-by-one model reference did not fire %q; fired: %v", want, fired)
		}
	}
}

// -----------------------------------------------------------------------------
// The heal: the sharpest claim in the scenario
// -----------------------------------------------------------------------------

// TestCountStore_RecoveryHealsDirtyAndNegative asserts the transition, not the
// end state.
//
// The pre-recovery half is the load-bearing half: it requires the live store to
// be genuinely NON-EXACT — at least one model cell absent from the store, at
// least one negative cell present, and a non-empty dirty set covering both — so
// that the post-recovery assertions cannot be satisfied by a store that was
// correct all along. Without it, "after the reopen every cell is exact" would
// pass against a run in which nothing was ever wrong.
func TestCountStore_RecoveryHealsDirtyAndNegative(t *testing.T) {
	f := newCountStoreFixture(t, countStoreDefaultSeed)

	model, obs, _ := f.probes.observe(f.sm)
	if !obs.anyDirty() {
		t.Fatalf("the prologue left no dirty marking, so there is nothing for the reopen to heal "+
			"(dirty=%s)", obs.dirtyString())
	}
	if len(obs.negatives) == 0 {
		t.Fatalf("the prologue's negative-cell construction produced no negative cell; the store "+
			"holds %d T cell(s)", len(obs.t))
	}
	// Every negative cell must be dirty-COVERED: an uncovered one would be a lost
	// decrement offered to the planner as exact.
	if len(obs.negativeUncovered) > 0 {
		t.Fatalf("negative cell(s) no dirty marking covers: %v (dirty=%s)",
			obs.negativeUncovered, obs.dirtyString())
	}
	// And the live store must genuinely DISAGREE with the model somewhere, on a
	// cell the dirty markings excuse.
	var excused int
	for k, want := range model.t {
		if obs.tDirty(k) && obs.t[k] != want {
			excused++
		}
	}
	for k, want := range model.dIn {
		if obs.dInDirty(k.label) && obs.dIn[k] != want {
			excused++
		}
	}
	if excused == 0 {
		t.Fatalf("no model cell disagreed with the live store, so the dirty markings excused nothing "+
			"and the heal below would be a no-op (dirty=%s)", obs.dirtyString())
	}
	t.Logf("pre-recovery: dirty=%s negatives=%v excused-cells=%d",
		obs.dirtyString(), obs.negatives, excused)

	// The strict phase must FAIL before the reopen — that is what makes "it passes
	// after" a measurement rather than a tautology.
	if vs, _ := checkCountStoreParity(1, csPhaseRecovered, &model, &obs, 0, csPerturbNone); len(vs) == 0 {
		t.Fatalf("the recovered-phase clauses passed on the LIVE store, so they do not distinguish " +
			"a healed store from an unhealed one")
	}

	f.recover(t)

	_, after, _ := f.probes.observe(f.sm)
	if after.anyDirty() {
		t.Errorf("the reopen left dirty markings behind: %s", after.dirtyString())
	}
	if len(after.negatives) > 0 {
		t.Errorf("the reopen left negative cell(s) behind: %v", after.negatives)
	}
	if vs := f.probes.Parity(f.sm, 2, csPhaseRecovered, csPerturbNone); len(vs) > 0 {
		t.Errorf("the recovered store is not exact on every cell: %v", vs)
	}
	t.Logf("post-recovery: dirty=%s cells=%d", after.dirtyString(), after.cells)
}

// -----------------------------------------------------------------------------
// The non-vacuity gate is wired
// -----------------------------------------------------------------------------

// TestCountStore_NonVacuityGateFires drives a budget that cannot reach the
// clauses and requires the terminal gate to say so. A gate whose firing has
// never been observed is indistinguishable from a gate that was deleted.
func TestCountStore_NonVacuityGateFires(t *testing.T) {
	var ev CountStoreEvidence
	ev.ShapeProbes = make([]int, len(csShapes()))
	ev.ShapeCellChecks = make([]int, len(csShapes()))
	ev.ShapeCellSkipped = make([]int, len(csShapes()))
	vs := ev.Finish(7)
	if len(vs) == 0 {
		t.Fatal("the terminal gate reported nothing for a run that measured nothing")
	}
	for _, want := range []string{
		"vacuity:live-checks", "vacuity:recovered-checks", "vacuity:relabels",
		"vacuity:dirty-observed", "vacuity:negative-observed", "vacuity:healed-dirty",
		"vacuity:healed-negative", "vacuity:cells-bound", "vacuity:crashes",
		"vacuity:checkpoints", "vacuity:compared-live", "vacuity:compared-recovered",
		"vacuity:family", "vacuity:t-fan-out", "vacuity:shape", "vacuity:shape-cell",
	} {
		if !csHasClause(vs, want) {
			t.Errorf("the terminal gate did not report %q; reported: %v", want, csClauseNames(vs))
		}
	}
	// The arm-accounting clause has a different shape: it is an EQUALITY, so it is
	// trivially satisfied by the all-zero evidence above and needs its own case.
	mismatch := ev
	mismatch.RecoveredChecks = 3
	mismatch.NegativeArms = 1
	if vs := mismatch.Finish(7); !csHasClause(vs, "vacuity:negative-arms") {
		t.Errorf("the arm-accounting clause stayed silent for arms=1 against recovered=3; "+
			"reported: %v", csClauseNames(vs))
	}

	// And it must NOT fire on a run that reached everything.
	ev2, report, err := RunCountStore(context.Background(),
		DefaultCountStoreConfig(countStoreDefaultSeed))
	if err != nil || report != nil {
		t.Fatalf("full run: err=%v report=%v", err, report)
	}
	if vs := ev2.Finish(1); len(vs) > 0 {
		t.Fatalf("the gate fired on a complete run: %v", vs)
	}
}

// TestCountStore_BoundVacuityGateFires requires the soak arm's |E| floor to be
// enforced: a short-budget run cannot grow the edge set far, so asking for a
// large floor must fail the run rather than pass it silently.
func TestCountStore_BoundVacuityGateFires(t *testing.T) {
	cfg := DefaultCountStoreConfig(countStoreDefaultSeed)
	cfg.MaxTicks = 40
	cfg.MinEdgesForBoundClaim = 1 << 20
	_, report, err := RunCountStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report == nil {
		t.Fatal("a run that modelled far fewer edges than its floor was reported clean")
	}
	if !csHasClause(report.Violations, "vacuity:bound-edges") {
		t.Fatalf("the |E| floor gate did not fire; violations: %v", report.Violations)
	}
}

// -----------------------------------------------------------------------------
// The model reference discriminates
// -----------------------------------------------------------------------------

// TestCountStore_PatternReferenceIsLabelSensitive proves
// [GraphOracle.knowsPatternCount] is a second oracle and not a restatement of
// [GraphOracle.knowsCount].
//
// The direction that matters is the one where they DIFFER: dropping a label from
// one modelled node must lower the labelled reference while leaving the
// unlabelled one and knowsCount untouched. A reference that moved with
// knowsCount would make the labelled surface clause implied by the unlabelled
// one, and an implied clause cannot fail on its own.
func TestCountStore_PatternReferenceIsLabelSensitive(t *testing.T) {
	f := newCountStoreFixture(t, countStoreDefaultSeed)
	o := f.sm.Oracle()

	all := o.knowsPatternCount("", "")
	person := o.knowsPatternCount(csLabelPerson, csLabelPerson)
	knows := int64(o.knowsCount())
	if all != knows {
		t.Fatalf("the unlabelled reference (%d) disagrees with knowsCount (%d) on a model whose "+
			"every edge has both endpoints", all, knows)
	}
	if person != all {
		t.Fatalf("every modelled node carries Person, so the Person-Person reference (%d) must equal "+
			"the unlabelled one (%d)", person, all)
	}
	// Now make them differ. Find a node with an outgoing KNOWS edge and strip its
	// Person label in the MODEL only (this is a model-side probe, not a write).
	var target uint64
	for k := range o.edges {
		if k.label == csRelKnows {
			target = k.src
			break
		}
	}
	if target == 0 {
		t.Fatal("the model holds no KNOWS edge")
	}
	o.removeLabel(target, csLabelPerson)

	if got := o.knowsPatternCount("", ""); got != all {
		t.Errorf("stripping a label changed the UNLABELLED reference: %d -> %d", all, got)
	}
	if got := int64(o.knowsCount()); got != knows {
		t.Errorf("stripping a label changed knowsCount: %d -> %d", knows, got)
	}
	if got := o.knowsPatternCount(csLabelPerson, csLabelPerson); got >= person {
		t.Errorf("stripping the source's Person label did not lower the Person-Person reference: "+
			"%d -> %d", person, got)
	} else {
		t.Logf("label sensitivity: all=%d personPerson=%d -> %d after stripping one source label",
			all, person, got)
	}
}

// TestCountStore_SurfaceShapesAdjudicated shows the two shapes added to the
// shared surface battery are load-bearing there: silent when engine and model
// agree, and firing when the model is desynced.
func TestCountStore_SurfaceShapesAdjudicated(t *testing.T) {
	f := newCountStoreFixture(t, countStoreDefaultSeed)
	// The surface battery's Person-shaped probes need every Person to carry an
	// age, which the count-store prologue's template does.
	if vs := CheckCypherSurface(5, f.sm.Oracle(), f.sm.engine); len(vs) > 0 {
		t.Fatalf("the surface battery reported violations on an agreeing pair: %v", vs)
	}
	// Desync the MODEL by one edge, then require both new clauses to notice.
	o := f.sm.Oracle()
	var src, dst uint64
	for k := range o.edges {
		if k.label == csRelKnows {
			src, dst = k.src, k.dst
			break
		}
	}
	if src == 0 {
		t.Fatal("the model holds no KNOWS edge")
	}
	delete(o.edges, edgeKey{label: csRelKnows, src: src, dst: dst})
	vs := CheckCypherSurface(6, o, f.sm.engine)
	ops := make(map[string]struct{}, len(vs))
	for _, v := range vs {
		ops[v.Op] = struct{}{}
	}
	for _, want := range []string{"anon pattern count(*)", "anon labelled count(*)"} {
		if _, ok := ops[want]; !ok {
			t.Errorf("surface clause %q stayed silent on a desynced model; fired: %v", want, ops)
		}
	}
}

// TestCountStore_CellsBoundFormula pins the combinatorial ceiling, including the
// empty-vocabulary case that DISABLES the clause.
func TestCountStore_CellsBoundFormula(t *testing.T) {
	for _, tc := range []struct {
		labels, rels, want int
	}{
		{0, 0, 0},
		{5, 0, 0}, // no relationship type observed: no ceiling to state.
		{0, 1, 1}, // one E cell and nothing else.
		{1, 1, 4}, // 1 + 2 + 1
		{2, 1, 9}, // 1 + 4 + 4
		{3, 1, 16},
		{2, 3, 3 + 12 + 12},
	} {
		if got := csCellsBound(tc.labels, tc.rels); got != tc.want {
			t.Errorf("csCellsBound(%d, %d) = %d, want %d", tc.labels, tc.rels, got, tc.want)
		}
	}
}

// -----------------------------------------------------------------------------
// The defect this scenario surfaced
// -----------------------------------------------------------------------------

// TestCountStore_AnchorSwapRetainsAnonymousSourceRows is the regression gate that
// REPLACED the defect pin this scenario surfaced (rmp #2603).
//
// MEASURED, on a graph with one `(:Person)-[:KNOWS]->(:Person:Vip)` edge and forty
// bare Persons — no store, no recovery, no simulator — a single-edge pattern whose
// SOURCE node was ANONYMOUS and labelled returned ZERO rows once the single-edge
// anchor-swap peephole re-rooted it onto the destination. Naming the source fixed
// it; naming the destination did not. All four spellings rendered the identical
// EXPLAIN tree, so the plan text could not tell them apart; PROFILE localised the
// loss to the Filter above the re-rooted Expand, which received one row and emitted
// none.
//
// Which answer is wrong was settled by the openCypher TCK, not by inspection:
// cypher/tck/features/clauses/match/Match2.feature scenario [2] "Matching a
// relationship pattern using a label predicate on both sides" runs
// `MATCH (:A)-[r]->(:B) RETURN r` and requires one row, so the anonymous-both-
// sides spelling must return the matching rows — the 0 was wrong. That scenario
// cannot catch this defect for two independent reasons, each sufficient: its
// relationship is UNTYPED (`[r]`) and `matchAnchorSite` requires exactly one
// relationship type, and its graph is BALANCED (2 :A, 2 :B) so the 2x cost margin
// is unreachable. MEASURED: give that same fixture a relationship type and 40
// extra `(:A)` nodes and `MATCH (:A)-[r:T1]->(:B) RETURN count(r)` returned 0.
//
// The fix (rmp #2603) makes `matchAnchorSite` decline any site whose endpoint
// variable name is empty, which is what an anonymous pattern head has, so the
// written order stands for these patterns.
//
// The gate asserts BOTH halves, because either alone is weak:
//
//   - with the swap ENABLED all four spellings must answer 1 — that is the
//     correctness claim, and it FAILED (0 for the two anonymous-source spellings)
//     before the fix;
//   - with the swap DISABLED all four must also answer 1 — the control that
//     establishes the true answer independently of the peephole.
func TestCountStore_AnchorSwapRetainsAnonymousSourceRows(t *testing.T) {
	build := func(disableSwap bool) *EngineAdapter {
		t.Helper()
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableAnchorSwap: disableSwap})
		ea := NewEngineAdapter(eng)
		ctx := context.Background()
		write := func(q string) {
			res, err := ea.RunWrite(ctx, q, nil)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			for res.Next() {
			}
			if err := res.Err(); err != nil {
				t.Fatalf("%s drain: %v", q, err)
			}
			_ = res.Close()
		}
		write("CREATE (:Person)-[:KNOWS]->(:Person:Vip)")
		// Enough bare Persons that N(Person) dwarfs N(Vip) and the cost model
		// prefers the Vip anchor by more than the 2x margin. Without this skew the
		// peephole is never admitted and the gate below cannot fail.
		for i := 0; i < 40; i++ {
			write("CREATE (:Person)")
		}
		return ea
	}
	const (
		anonSrcAnonDst   = "MATCH (:Person)-[:KNOWS]->(:Vip) RETURN count(*)"
		anonSrcNamedDst  = "MATCH (:Person)-[:KNOWS]->(b:Vip) RETURN count(*)"
		namedSrcAnonDst  = "MATCH (a:Person)-[:KNOWS]->(:Vip) RETURN count(*)"
		namedSrcNamedDst = "MATCH (a:Person)-[:KNOWS]->(b:Vip) RETURN count(*)"
	)
	all := []string{anonSrcAnonDst, anonSrcNamedDst, namedSrcAnonDst, namedSrcNamedDst}
	ctx := context.Background()

	// The control: with the swap DISABLED every spelling is correct. This
	// establishes the true answer independently of the peephole.
	off := build(true)
	for _, q := range all {
		got, err := csScalar(ctx, off, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got != 1 {
			t.Fatalf("with DisableAnchorSwap=true, %s returned %d, want 1: the control itself is "+
				"broken, so nothing below is attributable", q, got)
		}
	}

	// The gate: with the swap ENABLED (the shipped default) every spelling must
	// still be correct. The two ANONYMOUS-SOURCE spellings are the ones that
	// returned 0 before rmp #2603.
	on := build(false)
	for _, q := range all {
		got, err := csScalar(ctx, on, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got != 1 {
			t.Fatalf("%s returned %d with the anchor swap ENABLED (the shipped default), want 1. "+
				"rmp #2603: re-rooting a single-edge pattern re-checks the from-label through a "+
				"variable name, and an anonymous pattern head has none, so the mirror's predicate "+
				"resolves to no column and drops every row. matchAnchorSite must decline any site "+
				"with an empty endpoint name", q, got)
		}
	}
}
