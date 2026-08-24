package sim

// pagerank_ranker_test.go — the short-layer gate for the stateful-PageRanker
// scenario (rmp #2495).
//
// The structure follows the standing shape of this package: the scenario runs
// clean; the evidence is asserted as NUMBERS, so "it passed" is a separate
// question from "it exercised something"; each non-vacuity gate is shown to be
// WIRED by driving a plan that cannot satisfy it; and every clause is shown to be
// able to FAIL under a perturbation that reproduces the output the real defect
// would produce, with the unperturbed control required to stay silent first.
//
// Three tests here are unusual and deliberate:
//
//   - [TestPageRankRanker_ThresholdMirrorMatchesSource] parses the library's own
//     unexported `pageRankParallelThreshold` out of its source. The constant has
//     no accessor, so this file mirrors it — and a mirror that silently drifts
//     would push the fixture below the parallel threshold and make every parallel
//     clause VACUOUS while the scenario still passed. The tripwire converts that
//     into a build failure at the moment the library changes.
//   - [TestPageRankRanker_LabelProbeSeesWorkerSpawns] tests the INSTRUMENT rather
//     than the library: it drives both regimes directly and requires the probe to
//     read zero in one and a full worker pool in the other. An observation nobody
//     has shown can distinguish the two cases is not an observation.
//   - [TestPageRankRanker_RestoresGOMAXPROCS] asserts the clamp is given back. The
//     clamp is process-global, so a leaked one would slow every test after it and
//     silently change which regime unrelated code takes. It mirrors the same
//     assertion cpu_starvation_test.go makes for the only other clamp in this
//     package.
//
// NOTHING in this file calls t.Parallel(). The scenario clamps GOMAXPROCS, which
// is process-global; Go's testing package releases parallel tests only after
// every sequential test has finished (VERIFIED empirically for this task: a
// sequential test sampling a shared counter 50 times saw a parallel test in
// flight zero times), so staying sequential is what keeps the clamp invisible to
// the rest of the package.

import (
	"context"
	"encoding/binary"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// prClauseNames returns the clause name of every violation, in order.
func prClauseNames(vs []Violation) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		name := v.Op
		if len(name) > 2 && name[0] == '<' {
			name = name[1 : len(name)-1]
		}
		if i := len("pagerank-ranker:"); len(name) > i && name[:i] == "pagerank-ranker:" {
			name = name[i:]
		}
		out = append(out, name)
	}
	return out
}

// prHasClause reports whether the named clause fired.
func prHasClause(vs []Violation, clause string) bool {
	for _, got := range prClauseNames(vs) {
		if got == clause {
			return true
		}
	}
	return false
}

// prUniqueClauses returns the sorted set of clause names that fired.
func prUniqueClauses(vs []Violation) []string {
	seen := map[string]struct{}{}
	for _, c := range prClauseNames(vs) {
		seen[c] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// prAdjudicate re-runs both check families over the evidence, so a test reads the
// COMPLETE clause list rather than the report's bounded sample.
func prAdjudicate(ev *PageRankRankerEvidence) []Violation {
	return append(checkPageRankRanker(ev), checkPageRankRankerNonVacuity(ev)...)
}

// prRunPerturbed drives one run with the given perturbation and returns the
// evidence and the full violation list.
func prRunPerturbed(t *testing.T, seed uint64, p prPerturb) (*PageRankRankerEvidence, []Violation) {
	t.Helper()
	cfg := DefaultPageRankRankerConfig(seed)
	cfg.Perturb = p
	ev, _, err := RunPageRankRanker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("perturbation %s: run error: %v", p, err)
	}
	return ev, prAdjudicate(ev)
}

// TestPageRankRanker_ScenarioPasses runs the registered scenario at its
// catalogue seed and requires a clean report.
func TestPageRankRanker_ScenarioPasses(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioPageRankRanker)
	if !ok {
		t.Fatalf("scenario %q is not registered", ScenarioPageRankRanker)
	}
	if sc.Mode != ModeDeterministic {
		t.Fatalf("scenario mode = %s, want deterministic", sc.Mode)
	}
	if !sc.Mode.Reproducible() {
		t.Fatalf("scenario mode %s is not reproducible, but every measured fact is a function of the "+
			"seed and the CLI should be recording and replaying it", sc.Mode)
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("the pagerank-ranker scenario reported a violation:\n%s", report)
	}
}

// TestPageRankRanker_EvidenceIsNonVacuous asserts on the MEASURED numbers rather
// than on the absence of a violation.
//
// It duplicates the terminal gate deliberately: the gate fails the RUN, this test
// fails the BUILD with the numbers printed, and a plan change that quietly stops
// reaching a clause is far easier to diagnose from the second.
func TestPageRankRanker_EvidenceIsNonVacuous(t *testing.T) {
	ev, report, err := RunPageRankRanker(context.Background(),
		DefaultPageRankRankerConfig(pageRankRankerDefaultSeed))
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("reported a violation:\n%s", report)
	}
	if ev.Live < pageRankerThresholdMirror {
		t.Errorf("fixture has %d live nodes, below the %d parallel threshold: no clamp could reach "+
			"the parallel path", ev.Live, pageRankerThresholdMirror)
	}
	if ev.SerialWindows == 0 || ev.ParallelWindows == 0 {
		t.Errorf("both regimes must be reached: serial=%d parallel=%d", ev.SerialWindows, ev.ParallelWindows)
	}
	if ev.FirstParallel <= 0 {
		t.Errorf("the first parallel window is at index %d; the lazy transpose must be built "+
			"MID-sequence, not on the first Run", ev.FirstParallel)
	}
	// AT MOST two, not exactly two: which buffer a Run returns is the parity of its
	// iteration count, so a plan whose runs all take an even number of iterations
	// legitimately returns the same array every time. MEASURED, one seed of the
	// soak sweep does exactly that.
	if ev.DistinctBuffers < 1 || ev.DistinctBuffers > 2 || ev.BufferRepeats == 0 {
		t.Errorf("Run must alias one of at most TWO internal buffers, with at least one repeat: "+
			"distinct=%d repeats=%d", ev.DistinctBuffers, ev.BufferRepeats)
	}
	if ev.AliasArmed == 0 {
		t.Errorf("the aliasing pin was never armed: no window differed from the one before it")
	}
	if ev.ConvergedRuns == 0 || ev.CappedRuns == 0 {
		t.Errorf("both power-iteration exits must be reached: converged=%d capped=%d",
			ev.ConvergedRuns, ev.CappedRuns)
	}
	if ev.DistinctIters < 2 {
		t.Errorf("the varying options never varied the trajectory: %d distinct iteration count(s)",
			ev.DistinctIters)
	}
	if ev.MaxEmptyRanges == 0 {
		t.Errorf("the derived edge-balanced partition never collapsed a boundary, so the fixture's "+
			"hub (%d of %d in-edges) no longer reaches the shape edgeBalancedBounds documents",
			ev.HubInDeg, ev.Edges)
	}
	if ev.FirstParallelAlloc < ev.TransposeFloor {
		t.Errorf("the first parallel window allocated %d byte(s), below the %d-byte transpose floor",
			ev.FirstParallelAlloc, ev.TransposeFloor)
	}
	if len(ev.CrossRegime) != len(pageRankerCrossRegimeWorkers()) {
		t.Errorf("cross-regime arm compared %d worker count(s), want %d",
			len(ev.CrossRegime), len(pageRankerCrossRegimeWorkers()))
	}
	if ev.RefMaxDev == 0 {
		t.Errorf("the reference comparison found an EXACTLY zero deviation, which on a 1e-13-residual " +
			"reference against a 1e-9 one means the two are the same computation, not two")
	}
	// Every parallel window must have observed a real worker pool, and every
	// serial window must have observed none. This is the regime claim as numbers.
	for i := range ev.Windows {
		w := &ev.Windows[i]
		if w.ExpectParallel && w.LabelLookups < int64(w.ExpectWorkers) {
			t.Errorf("window %d: parallel path expected %d worker spawn(s), observed %d lookup(s)",
				i, w.ExpectWorkers, w.LabelLookups)
		}
		if !w.ExpectParallel && w.LabelLookups != 0 {
			t.Errorf("window %d: serial path observed %d worker-spawn lookup(s)", i, w.LabelLookups)
		}
	}
	t.Logf("pagerank-ranker evidence: %s", ev.String())
}

// TestPageRankRanker_IsDeterministic requires the same seed to produce the same
// evidence, twice. A harness that drifts makes every failure it ever reports
// unreplayable, so the claim is worth its own test.
func TestPageRankRanker_IsDeterministic(t *testing.T) {
	cfg := DefaultPageRankRankerConfig(pageRankRankerDefaultSeed)
	first, report, err := RunPageRankRanker(context.Background(), cfg)
	if err != nil || report != nil {
		t.Fatalf("first run: err=%v report=%v", err, report)
	}
	second, report, err := RunPageRankRanker(context.Background(), cfg)
	if err != nil || report != nil {
		t.Fatalf("second run: err=%v report=%v", err, report)
	}
	if a, b := first.ReproducibleSummary(), second.ReproducibleSummary(); a != b {
		t.Fatalf("same seed produced different evidence:\n first: %s\nsecond: %s", a, b)
	}
	// A different seed must produce different evidence, or the "reproducible"
	// claim is being satisfied by a summary that ignores the run.
	other := DefaultPageRankRankerConfig(pageRankRankerDefaultSeed ^ 0xABCD)
	third, report, err := RunPageRankRanker(context.Background(), other)
	if err != nil || report != nil {
		t.Fatalf("other-seed run: err=%v report=%v", err, report)
	}
	if third.Digest == first.Digest {
		t.Fatalf("two different seeds produced the same digest %#016x, so the digest is not a "+
			"function of the run", first.Digest)
	}
	// And the digest must exclude the two quantities that are NOT functions of the
	// seed, or the claim above would be luck: both runs saw the same fixture, so
	// their per-window allocation deltas and label-lookup counts are the only
	// things that could differ, and the summary must be blind to them.
	if first.Digest != second.Digest {
		t.Fatalf("digest differs across two runs of one seed: %#016x vs %#016x",
			first.Digest, second.Digest)
	}
}

// TestPageRankRanker_ClausesFire requires every contract clause to fire under a
// perturbation that reproduces the output the real defect would produce, with the
// unperturbed control silent first.
//
// `also` lists the clauses a perturbation legitimately co-fires, and each entry
// has a reason:
//
//   - alias-copy-aliases makes the "copy" the SAME array as the returned slice,
//     so the change-detection comparison compares that array with itself and
//     reads zero; both aliasing clauses therefore fire, which is precisely the
//     caller mistake the contract warns about;
//   - fresh-buffers reports one distinct backing array per window, which is both
//     "more than two arrays" and "no repeat at all".
func TestPageRankRanker_ClausesFire(t *testing.T) {
	cases := []struct {
		target  string
		also    []string
		perturb prPerturb
	}{
		{perturb: prPerturbFlipResultBit, target: "bit-identity"},
		{perturb: prPerturbDropIteration, target: "iteration-parity"},
		{perturb: prPerturbHideWorkers, target: "regime"},
		{perturb: prPerturbForeignLookup, target: "label-probe"},
		{perturb: prPerturbInflateTransposeFloor, target: "transpose-alloc"},
		{perturb: prPerturbAliasCopyAliases, target: "alias-copy-intact",
			also: []string{"alias-invalidated", "gate:alias-armed"}},
		{perturb: prPerturbFreezePrevSlice, target: "alias-invalidated"},
		{perturb: prPerturbFreshBuffers, target: "buffer-recycling",
			also: []string{"gate:buffer-repeat"}},
		{perturb: prPerturbShiftReference, target: "reference"},
		{perturb: prPerturbBreakMass, target: "mass"},
		{perturb: prPerturbCrossRegimeBit, target: "cross-regime"},
	}
	// The control FIRST: a clause that fires under perturbation but also fires
	// without it proves nothing.
	_, control := prRunPerturbed(t, pageRankRankerDefaultSeed, prPerturbNone)
	if len(control) != 0 {
		t.Fatalf("the unperturbed control reported violations: %v", prUniqueClauses(control))
	}
	for _, tc := range cases {
		t.Run(tc.perturb.String(), func(t *testing.T) {
			_, vs := prRunPerturbed(t, pageRankRankerDefaultSeed, tc.perturb)
			if !prHasClause(vs, tc.target) {
				t.Fatalf("perturbation %s did not fire %q; clauses fired: %v",
					tc.perturb, tc.target, prUniqueClauses(vs))
			}
			allowed := map[string]struct{}{tc.target: {}}
			for _, c := range tc.also {
				allowed[c] = struct{}{}
			}
			for _, got := range prUniqueClauses(vs) {
				if _, ok := allowed[got]; !ok {
					t.Errorf("perturbation %s fired unexpected clause %q; all fired: %v",
						tc.perturb, got, prUniqueClauses(vs))
				}
			}
			t.Logf("%s fired %d violation(s) across clauses %v", tc.perturb, len(vs), prUniqueClauses(vs))
		})
	}
}

// TestPageRankRanker_GatesFire requires each non-vacuity gate to be WIRED, by
// driving a plan that cannot satisfy its precondition.
//
// These two perturbations act on the PLAN rather than on a comparison, so each
// needs its own run — and each reproduces a real way this scenario could decay
// into a green run that tested nothing: a host or a plan that never reaches the
// parallel path, and a plan whose windows all converge to the same vector.
func TestPageRankRanker_GatesFire(t *testing.T) {
	cases := []struct {
		target  string
		also    []string
		perturb prPerturb
	}{
		{perturb: prPerturbSerialOnly, target: "gate:both-regimes",
			// With every clamp at 1 there is no parallel window at all, so the
			// transpose is never built mid-sequence and the derived partition never
			// collapses a boundary.
			also: []string{"gate:mid-sequence-build", "gate:empty-range"}},
		{perturb: prPerturbSameDamping, target: "gate:alias-armed",
			// Identical options in every window mean identical results, so the
			// iteration counts collapse to one value — and the plan's capped window
			// loses its 12-iteration budget along with its damping, so no window
			// reaches the cap either.
			also: []string{"gate:iteration-spread", "gate:convergence-mix"}},
	}
	for _, tc := range cases {
		t.Run(tc.perturb.String(), func(t *testing.T) {
			_, vs := prRunPerturbed(t, pageRankRankerDefaultSeed, tc.perturb)
			if !prHasClause(vs, tc.target) {
				t.Fatalf("perturbation %s did not fire %q; clauses fired: %v",
					tc.perturb, tc.target, prUniqueClauses(vs))
			}
			allowed := map[string]struct{}{tc.target: {}}
			for _, c := range tc.also {
				allowed[c] = struct{}{}
			}
			for _, got := range prUniqueClauses(vs) {
				if _, ok := allowed[got]; !ok {
					t.Errorf("perturbation %s fired unexpected clause %q; all fired: %v",
						tc.perturb, got, prUniqueClauses(vs))
				}
			}
			t.Logf("%s fired %d violation(s) across clauses %v", tc.perturb, len(vs), prUniqueClauses(vs))
		})
	}
}

// TestPageRankRanker_ThresholdMirrorMatchesSource parses the library's own
// `pageRankParallelThreshold` out of its source and requires the mirror in this
// package to match it.
//
// The constant is unexported and has no accessor, so the scenario has to mirror
// it to size its fixture and to derive the regime. A silent drift would be the
// worst kind of failure: a RAISED threshold would put the fixture back on the
// serial path, every parallel clause would go vacuous, and the scenario would
// keep passing.
func TestPageRankRanker_ThresholdMirrorMatchesSource(t *testing.T) {
	const src = "../../search/centrality/pagerank.go"
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
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "pageRankParallelThreshold" {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				t.Fatalf("pageRankParallelThreshold is no longer an integer literal: %T", vs.Values[0])
			}
			n, cerr := strconv.Atoi(lit.Value)
			if cerr != nil {
				t.Fatalf("pageRankParallelThreshold literal %q: %v", lit.Value, cerr)
			}
			got, found = n, true
		}
	}
	if !found {
		t.Fatalf("pageRankParallelThreshold was not found in %s; the mirror in this package can no "+
			"longer be checked against it", src)
	}
	if got != pageRankerThresholdMirror {
		t.Fatalf("pageRankParallelThreshold is %d in %s but pageRankerThresholdMirror is %d; the "+
			"fixture is sized from the mirror, so this drift would make every parallel clause vacuous",
			got, src, pageRankerThresholdMirror)
	}
	// And the fixture must clear it with the margin the constants promise.
	if pageRankerMinNodes < got+got/4 {
		t.Fatalf("pageRankerMinNodes=%d leaves less than the intended 25%% margin over the %d-node "+
			"threshold", pageRankerMinNodes, got)
	}
}

// TestPageRankRanker_NodeIDWidthIsPinned holds the two widths the transpose floor
// is computed from against the real types, so the floor cannot silently become
// wrong if graph.NodeID's underlying type ever changes.
func TestPageRankRanker_NodeIDWidthIsPinned(t *testing.T) {
	if got := binary.Size(graph.NodeID(0)); got != prNodeIDBytes {
		t.Fatalf("graph.NodeID is %d byte(s) wide, prNodeIDBytes says %d; the reverse-CSR floor is "+
			"computed from it", got, prNodeIDBytes)
	}
	if got := binary.Size(uint64(0)); got != prUint64Bytes {
		t.Fatalf("uint64 is %d byte(s) wide, prUint64Bytes says %d", got, prUint64Bytes)
	}
}

// TestPageRankRanker_LabelProbeSeesWorkerSpawns tests the INSTRUMENT, not the
// library: it drives the same fixture in both regimes and requires the probe to
// tell them apart.
//
// An observation nobody has shown can distinguish the two cases is not an
// observation, and this is the one assertion in the file that would catch the
// standard library changing how pprof.Do reads its parent label set — the whole
// regime clause rests on it.
func TestPageRankRanker_LabelProbeSeesWorkerSpawns(t *testing.T) {
	fx := prGenFixture(NewSeed(pageRankRankerDefaultSeed ^ pageRankRankerSeedMix))
	if fx.live < pageRankerThresholdMirror {
		t.Fatalf("fixture has %d live nodes, below the %d threshold", fx.live, pageRankerThresholdMirror)
	}
	opts := centrality.PageRankOptions{
		Damping: pageRankerDampingLow, MaxIterations: pageRankerCapIters, Tolerance: pageRankerCapTol,
	}
	for _, tc := range []struct {
		clamp int
		want  bool
	}{{clamp: 1, want: false}, {clamp: 4, want: true}} {
		probe := newPRLabelProbe(context.Background())
		var lookups, other int64
		err := prWithClamp(tc.clamp, func() error {
			if _, _, rerr := centrality.NewPageRanker(fx.c).Run(probe, opts); rerr != nil {
				return rerr
			}
			lookups, other = probe.label.Load(), probe.other.Load()
			return nil
		})
		if err != nil {
			t.Fatalf("clamp %d: %v", tc.clamp, err)
		}
		if other != 0 {
			t.Errorf("clamp %d: %d lookup(s) under a key that is not %q; the probe is reading "+
				"something else now", tc.clamp, other, prLabelKeyType)
		}
		switch {
		case !tc.want && lookups != 0:
			t.Errorf("clamp %d: the serial path creates no worker pool, yet the probe counted %d "+
				"label lookup(s)", tc.clamp, lookups)
		case tc.want && lookups < int64(tc.clamp):
			t.Errorf("clamp %d: the parallel path spawns %d worker(s), each reading the parent label "+
				"set before its first iteration, yet the probe counted only %d lookup(s) — if this is "+
				"zero, runtime/pprof no longer reads the parent set through Context.Value and the "+
				"regime clause has lost its instrument", tc.clamp, tc.clamp, lookups)
		case tc.want && lookups > 2*int64(tc.clamp):
			t.Errorf("clamp %d: the probe counted %d label lookup(s), above the %d ceiling of one "+
				"spawn plus one exit per worker", tc.clamp, lookups, 2*tc.clamp)
		}
	}
}

// TestPageRankRanker_RestoresGOMAXPROCS asserts the scenario gives the clamp
// back. A leaked clamp would slow every test after it and silently change which
// regime unrelated code takes; cpu_starvation_test.go makes the same assertion
// for the only other clamp in this package.
func TestPageRankRanker_RestoresGOMAXPROCS(t *testing.T) {
	before := runtime.GOMAXPROCS(0)
	if _, _, err := RunPageRankRanker(context.Background(),
		DefaultPageRankRankerConfig(pageRankRankerDefaultSeed)); err != nil {
		t.Fatalf("run error: %v", err)
	}
	if after := runtime.GOMAXPROCS(0); after != before {
		t.Fatalf("pagerank-ranker leaked a GOMAXPROCS clamp: before=%d after=%d", before, after)
	}
	// And prWithClamp must restore even when the body fails, or a harness error
	// inside a window would leave the process clamped for every later test.
	sentinel := errors.New("deliberate")
	if err := prWithClamp(1, func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("prWithClamp swallowed the body's error: %v", err)
	}
	if after := runtime.GOMAXPROCS(0); after != before {
		t.Fatalf("prWithClamp leaked a clamp on the error path: before=%d after=%d", before, after)
	}
}

// TestPageRankRanker_FixtureShapeIsWhatTheClausesNeed pins the three fixture
// properties every clause depends on, separately from a whole run, so a
// generator change is diagnosed here rather than as a mystery gate failure.
func TestPageRankRanker_FixtureShapeIsWhatTheClausesNeed(t *testing.T) {
	for i := 0; i < 8; i++ {
		seed := pageRankRankerDefaultSeed ^ (uint64(i+1) * 0x9E37_79B9_7F4A_7C15)
		fx := prGenFixture(NewSeed(seed ^ pageRankRankerSeedMix))
		if fx.live != fx.n {
			t.Errorf("seed %#x: %d of %d nodes are live; the spine is supposed to make every node "+
				"live so the threshold margin is a property of n", seed, fx.live, fx.n)
		}
		if fx.live < pageRankerThresholdMirror {
			t.Errorf("seed %#x: %d live nodes is below the %d threshold", seed, fx.live, pageRankerThresholdMirror)
		}
		if got := prInDegree(fx, fx.hub); got < fx.n-2 {
			t.Errorf("seed %#x: the hub holds %d in-edges of %d nodes; the skew the collapsed-boundary "+
				"shape needs is gone", seed, got, fx.n)
		}
		if got := prDerivedEmptyRanges(fx, 8); got == 0 {
			t.Errorf("seed %#x: the derived edge-balanced partition at 8 workers leaves no empty "+
				"range", seed)
		}
		// The sink must stay dangling, which is what drives the dangling-mass
		// redistribution both the library and the reference implement.
		for _, e := range fx.edges {
			if e[0] == fx.n-1 {
				t.Fatalf("seed %#x: node %d is a source, so the fixture has no dangling sink",
					seed, fx.n-1)
			}
		}
	}
}
