package cypher

// plan_qerror_test.go — the gates on the estimate-quality observation (rmp #2767).
//
// Layer: short.
//
// # What each gate rules out
//
// The metric's failure modes are all failures of MEANING rather than of arithmetic:
// a number that is emitted where none should be, or one that is suppressed where a
// reader needed it. Each gate below fixes one of them:
//
//   - an EXACT estimate scores 1, so the instrument itself is calibrated;
//   - a STALE exact estimate scores the factor it is wrong by, so the metric can
//     actually EXPOSE a bad statistic rather than only confirm a good one;
//   - an estimate the planner may not act on scores NOTHING, and in particular not
//     the 1 that the zero-value estimate would otherwise produce for an operator
//     that emitted no rows;
//   - a RE-INITIALISED operator scores nothing, because its row count is a sum over
//     invocations while the estimate is for one pass — measured here on a two-line
//     query where the un-guarded number would be 3, i.e. past the reporting
//     threshold;
//   - an ABANDONED operator scores nothing, because a LIMIT is not a planner error —
//     the un-guarded number for the fixture below is 280;
//   - Run and both EXPLAIN surfaces emit nothing at all, and the emission has
//     exactly ONE call site so that stays true by construction.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	cypherast "github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ─────────────────────────────────────────────────────────────────────────────
// Harness
// ─────────────────────────────────────────────────────────────────────────────

// qerrSink records the q-error surface only. It filters by NAME rather than
// recording everything, so an unrelated series emitted by a background goroutine
// (the MVCC vacuum, for one) cannot be mistaken for a sample.
//
// Safe for concurrent use: the metrics backend is global and may be hit from any
// goroutine.
type qerrSink struct {
	mu      sync.Mutex
	samples []float64 // q-errors, decoded back from the carrier duration
	high    uint64
}

func (s *qerrSink) IncCounter(name string, delta uint64) {
	if name != statsMetricQErrorHigh {
		return
	}
	s.mu.Lock()
	s.high += delta
	s.mu.Unlock()
}

func (s *qerrSink) SetGauge(string, float64) {}

func (s *qerrSink) ObserveLatency(name string, d time.Duration) {
	if name != statsMetricQError {
		return
	}
	s.mu.Lock()
	s.samples = append(s.samples, float64(d)/float64(statsQErrorUnit))
	s.mu.Unlock()
}

// read returns the samples and the high counter under the lock.
func (s *qerrSink) read() ([]float64, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]float64, len(s.samples))
	copy(out, s.samples)
	return out, s.high
}

// qerrProfile installs a fresh sink, PROFILEs q, and returns what was emitted.
//
// The backend is global, so this must never run under t.Parallel.
func qerrProfile(t *testing.T, e *Engine, q string) ([]float64, uint64) {
	t.Helper()
	s := &qerrSink{}
	metrics.SetBackend(s)
	defer metrics.SetBackend(nil)
	if _, err := e.ProfileTable(context.Background(), q, nil); err != nil {
		t.Fatalf("ProfileTable(%q): %v", q, err)
	}
	return s.read()
}

// qerrOnly asserts exactly one sample was emitted and returns it. A count other
// than one is a HARNESS failure as much as a defect: the gates below reason about
// which operator produced the sample, and they can only do that when there is one.
func qerrOnly(t *testing.T, got []float64, q string) float64 {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("PROFILE of %q emitted %d q-error samples %v, want exactly 1 — the "+
			"assertion below identifies the operator by elimination and cannot do so "+
			"otherwise", q, len(got), got)
	}
	return got[0]
}

// qerrNear reports whether got is within 1% of want, which is the tolerance a ratio
// of two exact integer counts needs (none) plus room for a histogram-derived
// estimate that lands one row away.
func qerrNear(got, want float64) bool { return math.Abs(got-want) <= want/100 }

// The shape of [seedStaleMCVGraph]. They are file-scope so the gates below can
// derive their expectations from the fixture rather than restate them: the
// misestimation factor is staleMCVHot/staleMCVRemain, and the live :Person count
// after the relabel is staleMCVHot + staleMCVCold − (staleMCVHot − staleMCVRemain).
//
// The three numbers are chosen together, and the reasoning is on the function.
const (
	staleMCVHot    = 100
	staleMCVCold   = 1300
	staleMCVRemain = 2
)

// seedStaleMCVGraph builds an engine whose statistics were correct when they were
// built and are now wrong by a factor of exactly 50 for one value.
//
// The construction targets the ONE provenance that can be both trustworthy and
// stale: the most-common-value list's EXACT per-value count, which
// `statsEqualityEstimateInner` tags estExact.
//
//	seed     'hot' on 100 nodes, a distinct value on 1300 → the MCV records hot → 100
//	refresh  publishes that snapshot
//	relabel  98 of them lose :Person → 2 nodes still answer p.grp = 'hot'
//
// The planner still predicts 100 and the query returns 2. Both numbers are real: the
// estimate is what the planner derived from the statistics it holds, and the row
// count is what the query actually returned.
//
// # Why the mutation is a RELABEL and not a property write (rmp #2772)
//
// It used to be `SET p.grp = 'moved'`, and that route no longer produces a
// trustworthy estimate to score. rmp #2772 gave the equality provider the staleness
// screen its range sibling always had, and a property write moves BOTH staleness
// counters: measured on the 1000/400 shape this fixture used to have, Δ = 990 and
// deletes = 990 against a build-time population of 1400, either of which demotes the
// estimate to estFallback. A demoted estimate is [exec.EstimateAbsent] and
// contributes no sample at all, so the SET route would leave this file's central gate
// with nothing to measure.
//
// Removing the LABEL instead touches no property, so Δ and the delete counter both
// stay at ZERO — the snapshot is pristine by every measure it maintains.
//
// # Why the shape moved from 1000/400/10 to 100/1300/2 (rmp #2785)
//
// The relabel route alone stopped being enough. rmp #2785 gave [statsSnapshotFresh] a
// population-shrinkage term, so the drift numerator is now Δ plus max(0, N0 − live):
// the old shape shrank :Person from 1400 to 410, a fraction of 2.41, and the estimate
// is now correctly demoted. That is the defect being fixed, not a regression, and the
// fixture has to move rather than the screen.
//
// What is left, and what this fixture now is: the screen is a fraction of the
// POPULATION, so it cannot see a small shrinkage CONCENTRATED on one most-common
// value. 98 of 1400 :Person rows lose the label — 98/1302 = 0.0753, comfortably
// inside the 0.0961 firing region, so the snapshot is fresh by the drift rule as well
// as by its counters — and yet every one of the 98 carried grp='hot', so the MCV
// entry for that one value is 50x wrong. That blindness is inherent to a
// population-relative rule, which Δ has always shared (a small Δ concentrated on one
// value is wrong in exactly the same way), and closing it would need per-value
// bookkeeping docs/statistics-design.md §2 deliberately does not maintain.
//
// So the fixture is still the case this metric exists for — a misestimate no counter
// and no fraction in the module can see — and the gates below are unchanged in what
// they claim. Only the arithmetic moved.
//
// staleMCVRemain is 2 and not 1 for a reason that is easy to lose: after a REFRESH
// the MCV list is rebuilt over the live rows, and [TestQError_MisestimatedPairsClearOnRefresh]
// needs 'hot' to still be IN it. The list is an exact top-32
// ([stats.BuildTopK]), the 1300 cold values are singletons, and a count of 2 is
// therefore the strict maximum and certain to be retained. A count of 1 would tie
// with every cold value and its retention would rest on the heap's hash tie-break.
func seedStaleMCVGraph(t *testing.T) (e *Engine, hotAfter int64) {
	t.Helper()
	const (
		hot    = staleMCVHot
		cold   = staleMCVCold
		remain = staleMCVRemain
	)
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	add := func(key, label, grp string, rank int64) {
		t.Helper()
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := g.SetNodeLabel(key, label); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", key, err)
		}
		if err := g.SetNodeProperty(key, "grp", lpg.StringValue(grp)); err != nil {
			t.Fatalf("SetNodeProperty(%s, grp): %v", key, err)
		}
		if err := g.SetNodeProperty(key, "rank", lpg.Int64Value(rank)); err != nil {
			t.Fatalf("SetNodeProperty(%s, rank): %v", key, err)
		}
	}
	for i := 0; i < hot; i++ {
		add(fmt.Sprintf("hot%d", i), "Person", "hot", int64(i))
	}
	// Distinct values, so the NDV is large enough that the 1/NDV fallback would be
	// visibly different from the MCV hit — without them a defect that read the wrong
	// provider could still land on a plausible number.
	for i := 0; i < cold; i++ {
		add(fmt.Sprintf("cold%d", i), "Person", fmt.Sprintf("cold%d", i), -1)
	}
	// A second label, for the Apply gate: three nodes, so a self-join emits nine.
	for i := 0; i < 3; i++ {
		add(fmt.Sprintf("city%d", i), "City", "c", -1)
	}
	e = NewEngine(g)
	t.Cleanup(func() { _ = e.Close() })
	ctx := context.Background()
	if err := e.RefreshStatistics(ctx); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	// The hot nodes carry rank 0..hot-1, so this keeps exactly `remain` of them.
	if _, err := e.RunInTx(ctx, fmt.Sprintf(
		`MATCH (p:Person) WHERE p.grp = 'hot' AND p.rank >= %d REMOVE p:Person`, remain),
		nil); err != nil {
		t.Fatalf("REMOVE label: %v", err)
	}
	// The premise of every gate built on this fixture: the statistic is stale in
	// FACT and TRUSTED by the module. A future change to the write path that started
	// bumping Δ here, or a tightening of the staleness screen, would silently demote
	// the estimate and turn those gates into assertions about an absent sample, which
	// is not what they claim to hold.
	src := liveResolver(e)
	st, okStats := lookupStats(src, "Person", "grp")
	if !okStats {
		t.Fatal("the (Person, grp) statistic vanished; the fixture has no subject")
	}
	if st.Delta() != 0 || st.Deletes() != 0 {
		t.Fatalf("removing a label moved the staleness counters (delta=%d deletes=%d); "+
			"the estimate is now demoted and this fixture no longer produces the stale "+
			"EXACT estimate every gate below scores", st.Delta(), st.Deletes())
	}
	// Asserted on the VERDICT and not only on the counters (rmp #2785). The counters
	// staying at zero was the whole premise until the drift numerator grew a
	// population-shrinkage term; now the shrinkage fraction has to stay inside the
	// firing region too, and that is a property of the three constants above rather
	// than of the mutation. Reading the provider is the one check that cannot go out
	// of date.
	got := statsEqualityEstimateWith(src, "Person", "grp",
		expr.StringValue("hot"), resolveLabelPopulation(src, "Person"))
	if got.source != estExact {
		t.Fatalf("the stale most-common-value count is tagged %v, want %v (rows=%v). "+
			"n0=%d live=%v: the shrinkage this fixture causes has moved outside the "+
			"freshness screen's firing region, so there is no trustworthy-but-stale "+
			"estimate for the gates below to score — see the fixture's own comment for "+
			"how the three constants are chosen",
			got.source, estExact, got.rows, st.LabelCount(),
			resolveLabelPopulation(src, "Person").n)
	}
	return e, remain
}

// ─────────────────────────────────────────────────────────────────────────────
// Acceptance gates
// ─────────────────────────────────────────────────────────────────────────────

// TestQError_AnExactEstimateSamplesOne calibrates the instrument: when the planner
// predicted a maintained exact count and the query returned exactly that many rows,
// the q-error is 1.
//
// It is the gate that makes every other reading interpretable. A metric that could
// not produce 1 for a perfect estimate would say nothing about the ones that are
// not 1.
func TestQError_AnExactEstimateSamplesOne(t *testing.T) {
	const n = 25
	e, _, _ := seedPersonGraph(t, n, 0.0)
	const q = "MATCH (p:Person) RETURN p"
	got, high := qerrProfile(t, e, q)

	if s := qerrOnly(t, got, q); s != 1 {
		t.Errorf("the label scan's q-error is %v, want exactly 1. Its estimate is the "+
			"label's LIVE exact count and the scan returned every one of them, so any "+
			"other value means the comparison is not reading the two numbers it claims to", s)
	}
	if high != 0 {
		t.Errorf("a perfect estimate incremented %s %d times; the counter is for "+
			"misestimates at or above %v", statsMetricQErrorHigh, high, statsQErrorHighFactor)
	}
	if got := e.StatsMisestimatedPairs(); got != 0 {
		t.Errorf("StatsMisestimatedPairs = %d after a perfect estimate, want 0", got)
	}
}

// TestQError_AStaleExactEstimateSamplesTheFactorItIsWrongBy is the gate the metric
// exists for: a statistic that was right when it was built and is now wrong by 50x
// is reported as wrong by 50x.
//
// Without it the surface would be decoration — a distribution that can only ever
// show agreement measures nothing. The fixture also demonstrates the finding that
// motivates the whole accessor: the estimate here is tagged EXACT even though it is
// 50x wrong, because the staleness screen is a fraction of the POPULATION (rmp #2772,
// rmp #2785) and cannot see a small shrinkage concentrated on one most-common value.
func TestQError_AStaleExactEstimateSamplesTheFactorItIsWrongBy(t *testing.T) {
	e, hotAfter := seedStaleMCVGraph(t)
	const q = "MATCH (p:Person) WHERE p.grp = 'hot' RETURN p"
	got, high := qerrProfile(t, e, q)

	// Two samples: the Filter (the stale estimate) and the scan beneath it (an
	// exact live count, so 1). Asserting the pair rather than just "some sample is
	// 50" is what stops a defect that emits the same number twice from passing.
	if len(got) != 2 {
		t.Fatalf("PROFILE of %q emitted %d samples %v, want 2 — one for the Filter "+
			"carrying the stale estimate and one for the exact scan beneath it", q, len(got), got)
	}
	const wantQ = float64(staleMCVHot) / float64(staleMCVRemain)
	if !qerrNear(got[0], wantQ) {
		t.Errorf("the Filter's q-error is %v, want %v. The most-common-value list still "+
			"records %d nodes for 'hot' and the query returned %d; a q-error that is not "+
			"%d/%d means the comparison is not reading the estimate and the measurement "+
			"it claims to", got[0], wantQ, staleMCVHot, hotAfter, staleMCVHot, hotAfter)
	}
	if got[1] != 1 {
		t.Errorf("the scan's q-error is %v, want 1; its estimate is a live exact count", got[1])
	}
	if high != 1 {
		t.Errorf("%s = %d, want 1 — exactly one of the two samples is at or above %v",
			statsMetricQErrorHigh, high, statsQErrorHighFactor)
	}
	if got := e.StatsMisestimatedPairs(); got != 1 {
		t.Errorf("StatsMisestimatedPairs = %d, want 1 — the miss is attributable to the "+
			"(Person, grp) statistic, which is exactly what a caller needs in order to know "+
			"WHAT to refresh", got)
	}
}

// TestQError_AnUntrustworthyEstimateEmitsNoSampleRatherThanOne holds the boundary.
//
// Two shapes are covered, and they fail differently if the boundary moves:
//
//   - the HEURISTIC estimate (1/NDV x N) under-predicts a skewed value by an order
//     of magnitude. It is a real error and it is deliberately NOT reported: the
//     planner refuses to act on the provenance, and re-scanning the graph would
//     produce the same average, so the error is neither attributable to a plan nor
//     fixable by the refresh this metric exists to trigger.
//   - the ABSENT estimate is the dangerous one. Its zero value carries Rows=0,
//     which the q-error clamps to 1 — so an operator that emitted no rows would
//     score a PERFECT 1 for an estimate that does not exist. That is the inversion
//     the qualification on the SOURCE prevents.
func TestQError_AnUntrustworthyEstimateEmitsNoSampleRatherThanOne(t *testing.T) {
	t.Run("heuristic", func(t *testing.T) {
		e, warm := seedSkewedGroupGraph(t)
		const q = `MATCH (p:Person) WHERE p.grp = 'warm' RETURN p`
		got, high := qerrProfile(t, e, q)
		// The scan below the Filter has an exact estimate and does qualify, so the
		// count is 1 and not 0. A Filter that wrongly qualified would make it 2, and
		// the second sample would be about 10 (the fixture's real heuristic error).
		if s := qerrOnly(t, got, q); s != 1 {
			t.Errorf("the single sample is %v, want 1 (the exact scan). The Filter's "+
				"estimate is a 1/NDV average that under-predicts %d rows by an order of "+
				"magnitude, and the planner may not act on that provenance — so it must "+
				"contribute no sample at all", s, warm)
		}
		if high != 0 {
			t.Errorf("%s = %d, want 0", statsMetricQErrorHigh, high)
		}
	})

	t.Run("absent", func(t *testing.T) {
		// A query whose only estimated operator returns ZERO rows. The Project above
		// it has no estimate and also returns zero: if absence were scored, it would
		// score 1 — "the estimate was perfect" — for a prediction nobody made.
		e, _, _ := seedPersonGraph(t, 12, 0.0)
		const q = `MATCH (p:Nonexistent) RETURN p`
		got, _ := qerrProfile(t, e, q)
		if s := qerrOnly(t, got, q); s != 1 {
			t.Fatalf("the single sample is %v, want 1 — the scan of an absent label is a "+
				"genuine exact estimate of 0 against a measured 0", s)
		}
		// The point of the gate: exactly ONE operator qualified, not every operator in
		// the tree. A defect that scored the zero-value estimate would emit one sample
		// per node and every one of them would read 1.
	})
}

// TestQError_ARepeatedlyInitialisedOperatorEmitsNoSample covers the comparability
// guard that is not obvious and is not theoretical.
//
// `MATCH (a:City), (b:City)` plans as an Apply whose INNER scan is re-Init'd once
// per outer row. Its estimate is 3 (one pass over the label) and its measured row
// count is 9 (three passes), so an unguarded comparison reports the planner as 3x
// wrong — which is not merely noise, it is exactly at the threshold that increments
// the counter and admits a pair. The nested loop is not a planner error.
func TestQError_ARepeatedlyInitialisedOperatorEmitsNoSample(t *testing.T) {
	e, _ := seedStaleMCVGraph(t)
	const q = `MATCH (a:City), (b:City) RETURN a, b`
	got, high := qerrProfile(t, e, q)

	if s := qerrOnly(t, got, q); s != 1 {
		t.Errorf("the single sample is %v, want 1 (the OUTER scan, Init'd once and "+
			"drained). The inner scan of the same label emits 9 rows against an estimate "+
			"of 3 because it runs three times; reporting that as a 3x planner error would "+
			"blame the estimate for the join", s)
	}
	if high != 0 {
		t.Errorf("%s = %d, want 0 — the only misestimate here is an artefact of "+
			"re-initialisation", statsMetricQErrorHigh, high)
	}
}

// TestQError_AnAbandonedOperatorEmitsNoSample covers the other comparability guard:
// an operator stopped before end-of-stream has a row count that is a LOWER BOUND.
//
// The fixture's scan is estimated at the live :Person count — 1302 — and returns 5
// because of the LIMIT. An unguarded comparison reports 260x.
func TestQError_AnAbandonedOperatorEmitsNoSample(t *testing.T) {
	e, _ := seedStaleMCVGraph(t)
	const q = `MATCH (p:Person) RETURN p LIMIT 5`
	got, high := qerrProfile(t, e, q)

	if len(got) != 0 {
		t.Errorf("PROFILE of %q emitted %v, want no samples at all. The scan stopped at "+
			"the LIMIT, so its row count is a lower bound and not an outcome the estimate "+
			"can be scored against", q, got)
	}
	if high != 0 {
		t.Errorf("%s = %d, want 0", statsMetricQErrorHigh, high)
	}
}

// TestQError_RunAndExplainEmitNothing is the behavioural half of the PROFILE-only
// property. The structural half is the gate below it.
func TestQError_RunAndExplainEmitNothing(t *testing.T) {
	e, _ := seedStaleMCVGraph(t)
	ctx := context.Background()
	const q = "MATCH (p:Person) WHERE p.grp = 'hot' RETURN p"

	s := &qerrSink{}
	metrics.SetBackend(s)
	for i := 0; i < 20; i++ {
		r, err := e.Run(ctx, q, nil)
		if err != nil {
			metrics.SetBackend(nil)
			t.Fatalf("Run: %v", err)
		}
		_ = r.Close()
	}
	if _, err := e.Explain(q, nil); err != nil {
		metrics.SetBackend(nil)
		t.Fatalf("Explain: %v", err)
	}
	if _, err := e.ExplainTable(q, nil); err != nil {
		metrics.SetBackend(nil)
		t.Fatalf("ExplainTable: %v", err)
	}
	if _, err := e.ExplainLogical(q, nil); err != nil {
		metrics.SetBackend(nil)
		t.Fatalf("ExplainLogical: %v", err)
	}
	metrics.SetBackend(nil)

	samples, high := s.read()
	if len(samples) != 0 || high != 0 {
		t.Errorf("20 Runs and three EXPLAIN surfaces emitted %d q-error samples %v and "+
			"%d high counts, want none of either. This query's estimate is 50x wrong, so "+
			"a leak onto the Run path would be both loud and permanent",
			len(samples), samples, high)
	}
	if got := e.StatsMisestimatedPairs(); got != 0 {
		t.Errorf("StatsMisestimatedPairs = %d after Run and EXPLAIN only, want 0", got)
	}

	// The control. Without it this test would pass on an engine that never emits.
	if got, _ := qerrProfile(t, e, q); len(got) == 0 {
		t.Fatal("the same query under PROFILE emitted nothing either, so the silence " +
			"asserted above proves nothing about the Run path")
	}
}

// TestQError_ObservationIsReachableOnlyFromTheProfilePath is the STRUCTURAL half:
// the emission is PROFILE-only because of where it is called from, not because of a
// runtime test it performs.
//
// A behavioural gate cannot hold that. It shows the paths it drives are silent; it
// cannot show that a future change did not add a call somewhere else. This parses
// the package and fails if [Engine.observeEstimateQuality] acquires a second caller
// or if its one caller stops being [Engine.profileMaterialised] — the single funnel
// of all three PROFILE surfaces.
func TestQError_ObservationIsReachableOnlyFromTheProfilePath(t *testing.T) {
	const (
		fn   = "observeEstimateQuality"
		want = "profileMaterialised"
	)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cypher/: %v", err)
	}
	fset := token.NewFileSet()
	var callers []string
	decls := 0
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse cypher/%s: %v", name, perr)
		}
		for _, d := range f.Decls {
			fd, isFunc := d.(*ast.FuncDecl)
			if !isFunc {
				continue
			}
			if fd.Name.Name == fn {
				decls++
			}
			if fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && sel.Sel.Name == fn {
					callers = append(callers, fd.Name.Name)
				}
				return true
			})
		}
	}
	if decls != 1 {
		t.Fatalf("%s is declared %d times, want 1 — the gate below identifies the "+
			"emission by name and cannot do so if there are two", fn, decls)
	}
	if len(callers) != 1 || callers[0] != want {
		t.Errorf("%s is called from %v, want exactly [%s]. The metric is PROFILE-only "+
			"because that is the one function a Profiler is installed in; a call from "+
			"anywhere else would put an estimate comparison on a path that has no "+
			"measurement to compare against, or on the query hot path", fn, callers, want)
	}
}

// TestQError_MisestimatedPairsClearOnRefresh holds the accessor's contract: it
// answers "is a refresh overdue, and for what?", not "has anything ever been wrong".
func TestQError_MisestimatedPairsClearOnRefresh(t *testing.T) {
	e, _ := seedStaleMCVGraph(t)
	const q = "MATCH (p:Person) WHERE p.grp = 'hot' RETURN p"
	if _, _ = qerrProfile(t, e, q); e.StatsMisestimatedPairs() != 1 {
		t.Fatalf("StatsMisestimatedPairs = %d after the stale PROFILE, want 1",
			e.StatsMisestimatedPairs())
	}
	// A second PROFILE of the same query must not double-count: the accessor counts
	// distinct PAIRS, not observations.
	if _, _ = qerrProfile(t, e, q); e.StatsMisestimatedPairs() != 1 {
		t.Errorf("a repeat PROFILE took the count to %d; the set holds distinct "+
			"(label, property) pairs", e.StatsMisestimatedPairs())
	}
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	if got := e.StatsMisestimatedPairs(); got != 0 {
		t.Errorf("StatsMisestimatedPairs = %d after a successful refresh, want 0. Every "+
			"observation in the set was made against the snapshot that has just been "+
			"replaced; carrying them forward would make the accessor unreadable after the "+
			"first refresh", got)
	}
	// And it fills again if the FRESH statistics are still wrong — which they are
	// not here, so the same query must now score 1.
	got, _ := qerrProfile(t, e, q)
	if len(got) != 2 || !qerrNear(got[0], 1) {
		t.Errorf("after the refresh the same query's samples are %v, want the Filter to "+
			"score ~1 — the statistics now describe the graph as it is", got)
	}
	if n := e.StatsMisestimatedPairs(); n != 0 {
		t.Errorf("StatsMisestimatedPairs = %d after refreshing and re-profiling, want 0", n)
	}
}

// TestQError_RefreshUnderTheBarrierAlsoClears covers the OTHER refresh entry point.
// db.stats.refresh() runs inside query execution and takes the barrier-free path
// ([Engine.RefreshStatisticsLocked]); a reset written on only one of the two would
// leave the accessor stale for every caller that refreshes through Cypher.
func TestQError_RefreshUnderTheBarrierAlsoClears(t *testing.T) {
	e, _ := seedStaleMCVGraph(t)
	if _, _ = qerrProfile(t, e, "MATCH (p:Person) WHERE p.grp = 'hot' RETURN p"); e.StatsMisestimatedPairs() != 1 {
		t.Fatalf("StatsMisestimatedPairs = %d before the refresh, want 1", e.StatsMisestimatedPairs())
	}
	if err := e.RefreshStatisticsLocked(context.Background()); err != nil {
		t.Fatalf("RefreshStatisticsLocked: %v", err)
	}
	if got := e.StatsMisestimatedPairs(); got != 0 {
		t.Errorf("StatsMisestimatedPairs = %d after RefreshStatisticsLocked, want 0", got)
	}
}

// TestQError_TheColumnarDrainAlsoReportsACompleteCount covers the end-of-stream
// signal on the COLUMNAR path, which is a different signal from the row path's and
// was not reachable from any other gate in this file.
//
// A row-mode operator reports end-of-stream by returning false from Next. A chunk
// producer never returns false: it SHORT-FILLS, and every drain in cypher/exec
// treats `n < maxRows` as exhaustion (ColumnarFilter.FillChunk documents exactly
// that, and Result.materializeColumnar breaks on it). Waiting for a literal zero
// fill would leave every columnar operator permanently "incomplete", and the whole
// columnar half of the engine would silently emit no q-error at all — a suppression
// no assertion about row-mode plans can see.
func TestQError_TheColumnarDrainAlsoReportsACompleteCount(t *testing.T) {
	const n = 25
	e, _, _ := seedPersonGraph(t, n, 0.0)
	// A scan feeding a projection of a property is the recognised columnar chain, so
	// the scan is driven by FillChunk and never by Next.
	const q = "MATCH (p:Person) RETURN p.name"
	if !strings.Contains(mustProfileTable(t, e, q), "ColumnarProject") {
		t.Fatalf("%q did not plan a columnar chain, so this gate covers nothing:\n%s",
			q, mustProfileTable(t, e, q))
	}
	got, _ := qerrProfile(t, e, q)
	if s := qerrOnly(t, got, q); s != 1 {
		t.Errorf("the columnar scan's q-error is %v, want 1. It emitted every one of "+
			"the %d labelled nodes its estimate predicted; a missing sample means the "+
			"short-fill end-of-stream signal is not being recorded, and no columnar "+
			"operator can ever report a complete count", s, n)
	}
}

// mustProfileTable renders the profiled plan or fails the test.
func mustProfileTable(t *testing.T, e *Engine, q string) string {
	t.Helper()
	out, err := e.ProfileTable(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("ProfileTable(%q): %v", q, err)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Unit gates on the arithmetic and the classification
// ─────────────────────────────────────────────────────────────────────────────

// TestQError_Formula pins the q-error itself. Each row rules out a different wrong
// formula: an unsigned difference, a one-sided ratio, or a ratio that divides by
// zero.
func TestQError_Formula(t *testing.T) {
	cases := []struct {
		name     string
		est, act int64
		want     float64
	}{
		{"exact", 1400, 1400, 1},
		{"over by 100x", 1000, 10, 100},
		{"under by 100x", 10, 1000, 100},
		{"symmetric, not signed", 4, 1, 4},
		{"a hundred rows out of a million is small", 1000000, 1000100, 1.0001},
		{"zero estimate clamps to one", 0, 50, 50},
		{"zero measurement clamps to one", 50, 0, 50},
		{"both zero is a perfect estimate of nothing", 0, 0, 1},
		{"one and zero", 1, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := qError(c.est, c.act); !qerrNear(got, c.want) {
				t.Errorf("qError(%d, %d) = %v, want %v", c.est, c.act, got, c.want)
			}
		})
	}
	// Symmetry as a property, not as three examples.
	for _, p := range [][2]int64{{3, 17}, {1, 999}, {250, 251}, {0, 7}} {
		if a, b := qError(p[0], p[1]), qError(p[1], p[0]); a != b {
			t.Errorf("qError is not symmetric: (%d,%d)=%v but (%d,%d)=%v",
				p[0], p[1], a, p[1], p[0], b)
		}
	}
}

// TestQError_CarrierEncoding pins the duration the ratio travels in, because a
// reader of docs/metrics.md converts a scrape back with it.
func TestQError_CarrierEncoding(t *testing.T) {
	if got := qErrorDuration(1); got != statsQErrorUnit {
		t.Errorf("qErrorDuration(1) = %v, want %v — one unit of q-error is one %v, and "+
			"docs/metrics.md tells a reader to divide by it", got, statsQErrorUnit, statsQErrorUnit)
	}
	if got, want := qErrorDuration(100), 100*statsQErrorUnit; got != want {
		t.Errorf("qErrorDuration(100) = %v, want %v", got, want)
	}
	// The ceiling exists so an arithmetically extreme ratio cannot overflow the
	// int64 nanoseconds a Duration holds.
	if got, want := qErrorDuration(1e18), time.Duration(statsQErrorCeiling*float64(statsQErrorUnit)); got != want {
		t.Errorf("qErrorDuration(1e18) = %v, want the clamped %v", got, want)
	}
	if qErrorDuration(1e18) <= 0 {
		t.Error("an unclamped extreme q-error overflowed the carrier duration and " +
			"became non-positive, which a histogram would silently bucket as fast")
	}
}

// TestQError_QualifyingProvenances pins the boundary per SOURCE. A return-value
// mutation of the whole predicate cannot distinguish the four cases, so each is
// asserted on its own.
func TestQError_QualifyingProvenances(t *testing.T) {
	cases := []struct {
		src  exec.EstimateSource
		want bool
		why  string
	}{
		{exec.EstimateExact, true,
			"a maintained exact count is a claim about the data and, from the " +
				"most-common-value list, can be stale"},
		{exec.EstimateStats, true,
			"a histogram estimate is derived from the data and a refresh rebuilds it"},
		{exec.EstimateHeuristic, false,
			"1/NDV x N is a uniformity assumption; its error is by construction, the " +
				"planner refuses to act on it, and a refresh does not reduce it"},
		{exec.EstimateAbsent, false,
			"there is no number at all, and scoring the zero value would report a " +
				"perfect estimate for every operator that emitted no rows"},
	}
	for _, c := range cases {
		if got := qErrorQualifies(c.src); got != c.want {
			t.Errorf("qErrorQualifies(%v) = %v, want %v — %s", c.src, got, c.want, c.why)
		}
	}
}

// TestQError_HighFactorIsThePlannerOwnMargin gates the calibration. The threshold is
// not a fresh opinion: it is the factor the planner already insists on before it
// will deviate on a statistics-derived estimate. Two numbers with the same job would
// drift.
func TestQError_HighFactorIsThePlannerOwnMargin(t *testing.T) {
	if statsQErrorHighFactor != joinReorderStatsMargin {
		t.Errorf("statsQErrorHighFactor = %v but joinReorderStatsMargin = %v; the "+
			"reporting threshold is deliberately the planner's own margin for acting on a "+
			"statistic, so that one number governs both",
			statsQErrorHighFactor, joinReorderStatsMargin)
	}
	if statsQErrorHighFactor <= 1 {
		t.Errorf("statsQErrorHighFactor = %v; a threshold of 1 or less counts every "+
			"estimate, perfect ones included", statsQErrorHighFactor)
	}
}

// TestQError_MisestimatedSetIsBounded gates the ceiling, the dedup and the reset on
// the set itself, where they can be driven past their limits cheaply.
func TestQError_MisestimatedSetIsBounded(t *testing.T) {
	var m misestimatedPairs
	if got := m.size(); got != 0 {
		t.Fatalf("a zero-value set has size %d, want 0", got)
	}
	m.add(statsPair{label: "L", prop: "p"})
	m.add(statsPair{label: "L", prop: "p"})
	if got := m.size(); got != 1 {
		t.Errorf("adding the same pair twice gave size %d, want 1", got)
	}
	for i := 0; i < statsMisestimatedPairsMax+50; i++ {
		m.add(statsPair{label: "L", prop: fmt.Sprintf("p%d", i)})
	}
	if got := m.size(); got != statsMisestimatedPairsMax {
		t.Errorf("the set grew to %d, want it to saturate at %d — an unbounded set fed "+
			"from a client-invokable diagnostic is exactly what the bounded-resources "+
			"mandate forbids", got, statsMisestimatedPairsMax)
	}
	m.reset()
	if got := m.size(); got != 0 {
		t.Errorf("size after reset is %d, want 0", got)
	}
}

// TestQError_PlanRowsIsSilentOnAnUnprofiledOperator gates the accessor on an
// operator no Profiler wrapped. Nothing measured it, so its row count is not one
// execution's complete output either — and a consumer that read the structural zero
// as a measurement would score every EXPLAIN as a total planner failure.
func TestQError_PlanRowsIsSilentOnAnUnprofiledOperator(t *testing.T) {
	if _, complete := exec.PlanRows(nil); complete {
		t.Error("exec.PlanRows(nil) reported a complete measured row count")
	}
	rows, complete := exec.PlanRows(exec.NewSingleRowOperator())
	if complete {
		t.Errorf("exec.PlanRows on an unwrapped operator reported complete=true "+
			"(rows=%d); nothing counted it, so there is no count to be complete", rows)
	}
}

// TestQError_AnUnlabelledScanLeafNamesNoStatistic gates a rule the ENGINE cannot
// currently exercise, and gates it directly for that reason.
//
// A Selection over an AllNodesScan has no label, so there is no (label, property)
// statistic behind whatever estimate it gets — `lookupStats` cannot resolve a label
// id and the estimate falls to `estFallback`, which never qualifies for a q-error.
// The consequence is that removing the guard changes no observable behaviour today:
// a pair recorded under an empty label would simply never be read back. It is
// asserted here on the helper itself rather than left ungated, because the day some
// shape does make such an estimate trustworthy, `StatsMisestimatedPairs` would start
// counting a statistic that does not exist and tell a caller to refresh nothing.
func TestQError_AnUnlabelledScanLeafNamesNoStatistic(t *testing.T) {
	pred := &cypherast.BinaryOp{
		Left:     &cypherast.Property{Receiver: &cypherast.Variable{Name: "n"}, Key: "grp"},
		Operator: "=",
		Right:    &cypherast.StringLiteral{Value: "hot"},
	}
	unlabelled := ir.NewSelectionExpr("n.grp = 'hot'", pred, ir.NewAllNodesScan("n"))
	if p, ok := statsPairForSelection(unlabelled, nil); ok {
		t.Errorf("statsPairForSelection over an AllNodesScan returned %+v; an "+
			"unlabelled scan leaf has no (label, property) statistic, and naming one "+
			"would point a caller at a statistic that does not exist", p)
	}
	// The control: the same predicate over a LABELLED scan does name its statistic.
	// Without it the assertion above would pass on a helper that never resolves
	// anything.
	labelled := ir.NewSelectionExpr("n.grp = 'hot'", pred, ir.NewNodeByLabelScan("n", "Person"))
	p, ok := statsPairForSelection(labelled, nil)
	if !ok || p != (statsPair{label: "Person", prop: "grp"}) {
		t.Fatalf("statsPairForSelection over a labelled scan = (%+v, %v), want "+
			"({Person grp}, true); the assertion above proves nothing otherwise", p, ok)
	}
}

// TestQError_ANonScanChildYieldsNoShape gates the scan-leaf precondition on
// [selectionEstimateShape], which is the single decomposition both the rendered
// estimate and the q-error's (label, property) attribution are read from.
//
// Like the unlabelled case above, it is asserted on the helper because no query the
// engine can plan makes the guard observable: with it removed the node variable
// falls back to the empty string, no property receiver matches it, and the
// extraction declines one step later for a different reason. It is gated so the
// contract is written down rather than left to that coincidence.
func TestQError_ANonScanChildYieldsNoShape(t *testing.T) {
	pred := &cypherast.BinaryOp{
		Left:     &cypherast.Property{Receiver: &cypherast.Variable{Name: "n"}, Key: "grp"},
		Operator: "=",
		Right:    &cypherast.StringLiteral{Value: "hot"},
	}
	// A Selection whose child is another Selection is not a scan leaf.
	inner := ir.NewSelectionExpr("n.grp = 'hot'", pred, ir.NewNodeByLabelScan("n", "Person"))
	outer := ir.NewSelectionExpr("n.grp = 'hot'", pred, inner)
	if sh, ok := selectionEstimateShape(outer, nil); ok {
		t.Errorf("selectionEstimateShape over a non-scan child returned %+v; the "+
			"estimators it feeds are defined against a scan leaf's label count, and a "+
			"deeper child means that denominator is not the one being filtered", sh)
	}
	if _, ok := selectionEstimateShape(inner, nil); !ok {
		t.Fatal("selectionEstimateShape declined the scan-leaf Selection too, so the " +
			"assertion above proves nothing")
	}
}
