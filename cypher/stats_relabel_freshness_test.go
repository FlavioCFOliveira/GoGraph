package cypher

// stats_relabel_freshness_test.go — the population-shrinkage term in the staleness
// screen (rmp #2785).
//
// Layer: short.
//
// # What was wrong
//
// [statsSnapshotFresh] screened a most-common-value count on Δ, the accumulated
// write counter, and Δ is bumped from ONE place: the four
// [recordStatsNodePropertyWrite] call sites in cypher/api.go, all on the node-property
// write path. A `REMOVE p:Person` writes no property, so it passes none of them, and
// the snapshot stayed pristine by every counter it maintains while its MCV entry
// became an arbitrary over-count. Measured at rmp #2785 on the seedStaleMCVGraph
// shape, 990 of 1400 :Person rows taken out of the predicate by each of the four
// routes that can do it:
//
//	route              N0     live   Δ     deletes   verdict
//	SET p.grp          1400   1400   990   990       demoted
//	DETACH DELETE      1400    410   990   990       demoted
//	REMOVE p.grp       1400   1400   990   990       demoted
//	REMOVE p:Person    1400    410     0     0       EXACT, and 100x wrong
//
// The live count DID fall — the information was there — and nothing consulted it.
//
// # What each gate rules out
//
//   - every one of the four routes now demotes, and the relabel route is still the
//     one whose counters are clean, so the table above is pinned rather than
//     remembered;
//   - a FRESH statistic still renders as a bare, unmarked exact number, so the fix
//     is a screen and not a blanket demotion;
//   - a relabel too SMALL to matter is still trusted, which is the same claim made
//     against the boundary rather than against the extremes;
//   - the arithmetic: the two drift terms ADD, growth does not offset Δ, and an
//     unknown population contributes no shrinkage.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/stats"
)

// relabelHot removes :Person from the `n` hot nodes with the highest rank, leaving
// the rest. It writes no property, so it moves neither staleness counter.
func relabelHot(t *testing.T, e *Engine, keep int) {
	t.Helper()
	q := `MATCH (p:Person) WHERE p.grp = 'hot' AND p.rank >= $keep REMOVE p:Person`
	if _, err := e.RunInTx(context.Background(), q,
		map[string]expr.Value{"keep": expr.IntegerValue(int64(keep))}); err != nil {
		t.Fatalf("REMOVE label: %v", err)
	}
}

// TestStatsRelabelFreshness_EveryRouteThatInvalidatesTheMCVDemotes is the acceptance
// gate, and it is a table because the FOUR routes are the finding.
//
// Each row takes the same 990 of 1400 :Person rows out of the `grp = 'hot'` predicate
// by a different mutation, so the MCV entry is equally wrong in all four — and until
// rmp #2785 exactly one of them was still tagged estExact. The row asserts the
// verdict AND the counters behind it, because "the relabel route demotes" is only
// the finding while that route's counters stay at zero: a future change that started
// bumping Δ on the label-write path would make this gate pass for a different reason
// than the one it documents.
//
// The `fresh` row is the other direction and is not decoration. A screen that
// demoted everything would satisfy every other row in this table.
func TestStatsRelabelFreshness_EveryRouteThatInvalidatesTheMCVDemotes(t *testing.T) {
	const (
		hot  = 1000
		cold = 400
		keep = 10
	)
	for _, tc := range []struct {
		name string
		// mutate takes `hot - keep` rows out of the `grp = 'hot'` predicate.
		mutate func(t *testing.T, e *Engine)
		// wantSource is the verdict the equality provider must reach.
		wantSource estSource
		// wantCounters reports whether the route is expected to move Δ and the
		// delete counter at all. The relabel route is the one that does not.
		wantCounters bool
	}{
		{"fresh", func(*testing.T, *Engine) {}, estExact, false},
		{"SET p.grp", func(t *testing.T, e *Engine) {
			runRelabelRoute(t, e, `MATCH (p:Person) WHERE p.grp = 'hot' AND p.rank >= 10 SET p.grp = 'moved'`)
		}, estFallback, true},
		{"DETACH DELETE", func(t *testing.T, e *Engine) {
			runRelabelRoute(t, e, `MATCH (p:Person) WHERE p.grp = 'hot' AND p.rank >= 10 DETACH DELETE p`)
		}, estFallback, true},
		{"REMOVE p.grp", func(t *testing.T, e *Engine) {
			runRelabelRoute(t, e, `MATCH (p:Person) WHERE p.grp = 'hot' AND p.rank >= 10 REMOVE p.grp`)
		}, estFallback, true},
		{"REMOVE p:Person", func(t *testing.T, e *Engine) {
			relabelHot(t, e, keep)
		}, estFallback, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := seedMCVEngine(t, hot, cold)
			tc.mutate(t, e)

			src := liveResolver(e)
			st, ok := lookupStats(src, "Person", "grp")
			if !ok {
				t.Fatal("the (Person, grp) statistic vanished; this row has no subject")
			}
			pop := resolveLabelPopulation(src, "Person")
			got := statsEqualityEstimateWith(src, "Person", "grp", expr.StringValue("hot"), pop)

			if got.source != tc.wantSource {
				t.Errorf("the most-common-value count for 'hot' is tagged %v, want %v "+
					"(rows=%v, n0=%d live=%v known=%v delta=%d deletes=%d). Every route in "+
					"this table leaves the same %d rows answering the predicate, so a row "+
					"that disagrees with the others is a route the screen cannot see",
					got.source, tc.wantSource, got.rows, st.LabelCount(), pop.n, pop.known,
					st.Delta(), st.Deletes(), keep)
			}
			if moved := st.Delta() != 0 || st.Deletes() != 0; moved != tc.wantCounters {
				t.Errorf("this route moved the staleness counters = %v, want %v "+
					"(delta=%d deletes=%d). The route that does NOT move them is the whole "+
					"finding of rmp #2785: its demotion has to come from the population "+
					"shrinkage, and if the counters started moving here the row above would "+
					"be passing for a reason this gate does not document",
					moved, tc.wantCounters, st.Delta(), st.Deletes())
			}
		})
	}
}

// runRelabelRoute executes one of the table's mutating queries.
func runRelabelRoute(t *testing.T, e *Engine, q string) {
	t.Helper()
	if _, err := e.RunInTx(context.Background(), q, nil); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// TestStatsRelabelFreshness_TheRenderedFigureFollowsTheVerdict is the user-visible
// half: a relabelled statistic must stop printing as a bare, unmarked number, and a
// fresh one must keep printing as one.
//
// ProfileTable is the surface asserted because that is where the estimate sits
// immediately left of the measured row count, which is the comparison the column
// exists to make. The other two renderers are not re-gated here: the demotion travels
// through the shared provider verdict, and
// [TestStatsEqualityFreshness_StaleMCVIsNotRenderedAsExact] already holds all three
// surfaces against a demoted verdict. What is new at rmp #2785 is which mutations
// produce that verdict, not how one is drawn.
func TestStatsRelabelFreshness_TheRenderedFigureFollowsTheVerdict(t *testing.T) {
	t.Run("relabelled", func(t *testing.T) {
		e := seedMCVEngine(t, 1000, 400)
		relabelHot(t, e, 10)
		if got := estCellFor(t, mustProfileTable(t, e, mcvQuery), "Filter"); got != "-" {
			t.Errorf("ProfileTable Est.Rows for the Filter = %q, want %q. 990 of the "+
				"1000 rows the most-common-value list counted for 'hot' no longer carry "+
				"the label, and a bare number in that column claims a maintained exact "+
				"count", got, "-")
		}
	})
	t.Run("fresh", func(t *testing.T) {
		e := seedMCVEngine(t, 1000, 400)
		if got := estCellFor(t, mustProfileTable(t, e, mcvQuery), "Filter"); got != "1000" {
			t.Errorf("ProfileTable Est.Rows for the Filter = %q, want %q. Nothing has "+
				"touched this snapshot, so the shrinkage term must contribute zero and "+
				"the count must still render as a bare number", got, "1000")
		}
	})
}

// TestStatsRelabelFreshness_ARelabelInsideTheFiringRegionIsStillTrusted holds the
// boundary, and it is the gate that makes the fix a SCREEN rather than a rule that
// demotes whenever a label ever shrank.
//
// The firing region is b − 1/B = 0.0961 of the population. 40 of 1400 :Person rows
// lose the label — 40/1360 = 0.0294, well inside it — so the snapshot stays trusted
// and its count stays exact, even though it is now wrong by 40 rows. That is the
// design's rule applied consistently and not an oversight: the same 0.0961 has always
// governed Δ, and a statistic is demoted on the FRACTION of the population that
// moved, never on the fact that something did.
//
// The premise is asserted rather than assumed, because a fixture that drifted over
// the threshold would leave this gate passing while testing the opposite claim.
func TestStatsRelabelFreshness_ARelabelInsideTheFiringRegionIsStillTrusted(t *testing.T) {
	const (
		hot     = 1000
		cold    = 400
		keep    = 960 // 40 rows lose the label
		shrunk  = hot - keep
		n0      = hot + cold
		wantMCV = float64(hot)
	)
	e := seedMCVEngine(t, hot, cold)
	relabelHot(t, e, keep)

	src := liveResolver(e)
	st, ok := lookupStats(src, "Person", "grp")
	if !ok {
		t.Fatal("the (Person, grp) statistic vanished; the fixture has no subject")
	}
	pop := resolveLabelPopulation(src, "Person")
	thr := statsRangeBreakEven - 1.0/float64(statsHistogramBuckets)
	frac := float64(shrunk) / pop.n
	if st.Delta() != 0 || st.Deletes() != 0 || !pop.known ||
		st.LabelCount() != n0 || pop.n != n0-shrunk || !(frac < thr) {
		t.Fatalf("the isolating window is gone: delta=%d deletes=%d n0=%d "+
			"live=%v(known=%v) -> shrinkage fraction %.4f against threshold %.4f",
			st.Delta(), st.Deletes(), st.LabelCount(), pop.n, pop.known, frac, thr)
	}

	got := statsEqualityEstimateWith(src, "Person", "grp", expr.StringValue("hot"), pop)
	if got.source != estExact || got.rows != wantMCV {
		t.Errorf("a %.2f%% relabel demoted the count to %+v, want {rows:%v source:%v}. "+
			"A screen that fires on any shrinkage at all demotes every statistic on a "+
			"graph that has ever lost a label, which is the same as having no statistics",
			frac*100, got, wantMCV, estExact)
	}
}

// TestStatsSnapshotFresh_TheDriftNumeratorArithmetic pins the formula itself, where
// each case can be isolated and no fixture is needed to reach it.
//
// The rows that carry the fix are `terms add` and `shrinkage past the region`: either
// term alone leaves the same snapshot fresh, and only their SUM crosses the
// threshold. A formulation that took the larger of the two, or that screened them
// separately, would pass every other row here.
func TestStatsSnapshotFresh_TheDriftNumeratorArithmetic(t *testing.T) {
	// The threshold is 0.09609375 of N, and N is the smaller of n0 and the live
	// population when the latter is known.
	for _, tc := range []struct {
		name  string
		n0    int64
		delta int64
		pop   labelPopulation
		want  bool
		why   string
	}{
		{"pristine", 1000, 0, labelPopulation{n: 1000, known: true}, true,
			"nothing has moved; 0/1000 is not staleness"},
		{"writes inside the region", 1000, 90, labelPopulation{n: 1000, known: true}, true,
			"0.0900 is under 0.0961"},
		{"writes past the region", 1000, 97, labelPopulation{n: 1000, known: true}, false,
			"0.0970 is over 0.0961"},
		{"shrinkage inside the region", 1000, 0, labelPopulation{n: 950, known: true}, true,
			"50/950 = 0.0526; a small relabel is still trusted"},
		{"shrinkage past the region", 1000, 0, labelPopulation{n: 900, known: true}, false,
			"100/900 = 0.1111; this is the rmp #2785 case, and it used to read 0/900"},
		{"terms add", 1000, 45, labelPopulation{n: 950, known: true}, false,
			"45/950 = 0.0474 and 50/950 = 0.0526 are each inside the region; " +
				"(45+50)/950 = 0.1000 is not, so the two terms must be summed"},
		{"growth does not offset writes", 1000, 100, labelPopulation{n: 1100, known: true}, false,
			"N is the smaller 1000 and the shrinkage term is clamped at zero, so " +
				"100/1000 = 0.1000 demotes; subtracting the growth would report 0"},
		{"an unknown population contributes no shrinkage", 1000, 90,
			labelPopulation{}, true,
			"there is no live count to difference against, so the rule is Δ/N0 as it was"},
		{"an emptied label", 1000, 0, labelPopulation{n: 0, known: true}, false,
			"N is zero; the fraction is undefined and the snapshot describes a " +
				"population that no longer exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := stats.NewStats(stats.Input[expr.Value]{
				NDV:        stats.NewHLL(),
				MCV:        stats.BuildTopK[expr.Value](nil, statsMCVSize),
				Histograms: nil,
				Generation: 1,
				LabelCount: tc.n0,
				Buckets:    statsHistogramBuckets,
			})
			for i := int64(0); i < tc.delta; i++ {
				st.RecordWrite()
			}
			if got := statsSnapshotFresh(st, tc.pop); got != tc.want {
				t.Errorf("statsSnapshotFresh(n0=%d delta=%d pop=%+v) = %v, want %v — %s",
					tc.n0, tc.delta, tc.pop, got, tc.want, tc.why)
			}
		})
	}
}
