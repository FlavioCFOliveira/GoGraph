package cypher

// parallel_agg_project_budget_test.go — rmp #2668: the per-worker aggregation
// pre-projection built by tryBuildParallelAggregateScan carried no row byte budget.
//
// It was the THIRD instance of the gap #2655 closed for the other two. The row
// pre-projection is guarded by applyProjectionRowBudget (pre-existing) and the
// columnar one by applyColumnarAggRowBudget → WithChunkRowByteBudget (#2655); the
// per-worker one was built with a bare exec.NewProject and wrapped in nothing.
// #2655 could not have widened to it: this path requires a BARE scan, so any WHERE
// excludes it.
//
// exec.ParallelAggregateScan.WithByteBudget does not subsume the gap — it charges the
// RETAINED GROUP KEYS once per NEW GROUP (#1841) and never charges an aggregate
// ARGUMENT column — which is exactly why the columnar arm needed its own guard.
//
// Every test here asserts the parallel path ACTUALLY ENGAGED before drawing any
// conclusion from the verdict: a query that silently fell back to a serial arm would
// report the serial arm's (already guarded) behaviour and prove nothing.
//
// Engagement is read from the query's OWN physical plan, not from
// parallelAggregateScanBuildCount. That counter is process-global and the tests in
// this file and in agg_columnar_source_test.go run t.Parallel() and both engage the
// parallel path, so a before/after delta can be borrowed from a sibling — a FALSE
// PASS on exactly the non-vacuity condition it is supposed to establish. Measured
// during development: the min_arg row read "engaged" from a sibling and therefore
// survived the guard-unwired mutation.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	// parAggBudgetNodes exceeds psTestThreshold (50) so useParallelScan admits the
	// graph and the morsel-parallel aggregate path engages.
	parAggBudgetNodes = 60
	// parAggBudgetListLen sizes the oversized property: estimateValueSize charges
	// perValueOverhead (16) per value plus 16 per list element, so the estimate is
	// 16 + 8000*16 = 128016 bytes — above parAggBudgetCeiling and therefore
	// refusable, while keeping the fixture's own footprint near 8 MB.
	parAggBudgetListLen = 8000
	// parAggBudgetEstimate is the estimateValueSize result for one `big` value.
	parAggBudgetEstimate = 16 + parAggBudgetListLen*16
	// parAggBudgetCeiling is the engine-wide MaxResultBytes both arms are held to.
	parAggBudgetCeiling = 100000
)

// parAggBudgetGraph builds parAggBudgetNodes nodes, each labelled P and carrying an
// oversized list property `big` plus two small scalars. Every node's list has a
// distinct first element so a grouping key over `big` opens one group per node.
func parAggBudgetGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := range parAggBudgetNodes {
		k := fmt.Sprintf("p%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "tag", lpg.Int64Value(int64(i%2))); err != nil {
			t.Fatalf("SetNodeProperty tag: %v", err)
		}
		if err := g.SetNodeProperty(k, "small", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty small: %v", err)
		}
		elems := make([]lpg.PropertyValue, parAggBudgetListLen)
		for j := range elems {
			elems[j] = lpg.Int64Value(int64(j))
		}
		elems[0] = lpg.Int64Value(int64(i))
		if err := g.SetNodeProperty(k, "big", lpg.ListValue(elems)); err != nil {
			t.Fatalf("SetNodeProperty big: %v", err)
		}
	}
	return g
}

// parAggEngaged reports whether q's physical plan on eng really is the
// morsel-parallel aggregate scan, by name, from the plan the engine built for THIS
// query. exec.RenderPlan labels each node with its concrete operator type, so the
// operator's own type name is the label to look for.
func parAggEngaged(t *testing.T, eng *Engine, q string) (bool, string) {
	t.Helper()
	plan := explainOK(t, eng, q)
	return strings.Contains(plan, "ParallelAggregateScan"), plan
}

// runParAggBudget runs q to completion on eng and returns the terminal error. A nil
// error means the query completed. Engagement is asserted separately, by
// [parAggEngaged].
func runParAggBudget(t *testing.T, eng *Engine, q string) error {
	t.Helper()
	res, runErr := eng.Run(context.Background(), q, nil)
	if runErr != nil {
		return runErr
	}
	for res.Next() { // drain so the pre-projection actually runs
	}
	drainErr := res.Err()
	_ = res.Close()
	return drainErr
}

// parAggBudgetEngine is the engine under test: the byte ceiling of the #2655
// measurement plus a threshold low enough for parAggBudgetNodes to engage the
// parallel path. The global ceiling is lifted so the per-row guard, not the
// process-wide one, is what decides.
func parAggBudgetEngine(g *lpg.Graph[string, float64]) *Engine {
	return NewEngineWithOptions(g, EngineOptions{
		MaxResultBytes:        parAggBudgetCeiling,
		GlobalMaxResultBytes:  GlobalMaxResultBytesUnlimited,
		ParallelScanThreshold: psTestThreshold,
	})
}

// parAggBudgetQueries are the bare-scan shapes that reach
// tryBuildParallelAggregateScan: an unlabelled leaf (the labelled one is
// deliberately not admitted, see #2187), no Selection between the aggregate and the
// scan, and every aggregate an admitted count/min/max reducer. Each carries the
// oversized `big` value through the per-worker pre-projection — as an aggregate
// ARGUMENT, which the aggregation's own WithByteBudget never charges, and as a
// GROUPING KEY, which it charges only once per new group.
//
// MEASURED, guard unwired (applyWorkerAggRowBudget → return p), which rows actually
// depend on THIS guard — 3 of the 4:
//
//   - global_count_arg  RED: completes, where it must refuse.
//   - grouped_count_arg RED: completes, where it must refuse.
//   - grouped_by_big    RED: refuses, but with "exec: aggregation memory cap
//     exceeded" instead of the projection sentinel — the retained GROUP KEY is
//     charged by WithByteBudget, so what this row pins is WHICH verdict is raised.
//   - min_arg           GREEN: not differential for this guard. min retains the
//     oversized value and emits it, so the OUTER RETURN projection's own
//     pre-existing row budget refuses the result row either way. It is kept as a
//     parity row, not as a defence of the per-worker guard.
var parAggBudgetQueries = []struct {
	name string
	q    string
}{
	{"global_count_arg", `MATCH (n) RETURN count(n.big) AS c`},
	{"grouped_count_arg", `MATCH (n) RETURN n.small AS s, count(n.big) AS c`},
	{"grouped_by_big", `MATCH (n) RETURN n.big AS b, count(*) AS c`},
	{"min_arg", `MATCH (n) RETURN min(n.big) AS lo`},
}

// TestParallelAggPreProjection_RowByteBudget is the #2668 blocking test: a bare-scan
// parallel aggregate over an oversized property must raise
// exec.ErrProjectionRowTooLarge — the same sentinel the row and columnar
// pre-projections raise — instead of completing.
//
// It is non-vacuous by measurement, not by assertion: with applyWorkerAggRowBudget
// unwired (returning p untouched) 3 of the 4 subtests below go red. The fourth is
// answered by a different, pre-existing guard; see the note on parAggBudgetQueries.
func TestParallelAggPreProjection_RowByteBudget(t *testing.T) {
	t.Parallel()
	eng := parAggBudgetEngine(parAggBudgetGraph(t))
	for _, tc := range parAggBudgetQueries {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			engaged, plan := parAggEngaged(t, eng, tc.q)
			if !engaged {
				t.Fatalf("the morsel-parallel aggregate path did not engage for %q, so this "+
					"test does not exercise the per-worker pre-projection at all:\n%s",
					tc.q, plan)
			}
			err := runParAggBudget(t, eng, tc.q)
			if err == nil {
				t.Fatalf("%q completed under a %d-byte per-row ceiling while carrying a "+
					"%d-byte value: the per-worker pre-projection is bounded by nothing",
					tc.q, parAggBudgetCeiling, parAggBudgetEstimate)
			}
			if !errors.Is(err, exec.ErrProjectionRowTooLarge) {
				t.Fatalf("%q terminal error = %v; want exec.ErrProjectionRowTooLarge, the "+
					"sentinel the other two pre-projections raise", tc.q, err)
			}
		})
	}
}

// TestParallelAggPreProjection_ThreeSitesIdenticalVerdict is the #2668 acceptance
// criterion that a caller cannot tell which of the THREE aggregation
// pre-projections ran from the error it gets.
//
// The same aggregate (count over the oversized `big`) and the same ceiling are put
// to all three sites, each reached by the one route that admits it:
//
//   - the COLUMNAR arm: a bare labelled scan below the parallel threshold
//     (tryBuildColumnarAggInput claims it);
//   - the ROW arm: the same query with a WHERE, which the columnar
//     pre-projection's own decline seam forces onto exec.Project;
//   - the PARALLEL arm: an unlabelled bare scan above the threshold
//     (tryBuildParallelAggregateScan claims it).
//
// A single query text cannot reach all three — the routes are mutually exclusive by
// construction, which is precisely why the third site went unguarded — so the
// invariant asserted is that the three ERROR STRINGS are byte-identical.
func TestParallelAggPreProjection_ThreeSitesIdenticalVerdict(t *testing.T) {
	t.Parallel()
	g := parAggBudgetGraph(t)

	// Serial engine: default threshold (50 000) keeps the labelled shapes off the
	// parallel path, so they exercise the columnar and row pre-projections.
	serial := NewEngineWithOptions(g, EngineOptions{
		MaxResultBytes:       parAggBudgetCeiling,
		GlobalMaxResultBytes: GlobalMaxResultBytesUnlimited,
	})
	rowOnly := NewEngineWithOptions(g, EngineOptions{
		MaxResultBytes:       parAggBudgetCeiling,
		GlobalMaxResultBytes: GlobalMaxResultBytesUnlimited,
	})
	rowOnly.forceColumnarChainDeclineForTest = true
	parallel := parAggBudgetEngine(g)

	const (
		bareQ  = `MATCH (n:P) RETURN count(n.big) AS c`
		parQ   = `MATCH (n) RETURN count(n.big) AS c`
		siteNS = 3
	)

	drain := func(eng *Engine, q string) error {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			return err
		}
		for res.Next() { // drain so the pre-projection actually runs
		}
		drainErr := res.Err()
		_ = res.Close()
		return drainErr
	}

	type arm struct {
		site string
		err  error
	}
	arms := make([]arm, 0, siteNS)

	colErr := drain(serial, bareQ)
	arms = append(arms, arm{"columnar", colErr})

	rowErr := drain(rowOnly, bareQ)
	arms = append(arms, arm{"row", rowErr})

	engaged, parPlan := parAggEngaged(t, parallel, parQ)
	if !engaged {
		t.Fatalf("the parallel arm did not engage for %q; the three-way comparison would "+
			"compare two serial arms and a third serial arm:\n%s", parQ, parPlan)
	}
	arms = append(arms, arm{"parallel", runParAggBudget(t, parallel, parQ)})

	for _, a := range arms {
		if a.err == nil {
			t.Fatalf("the %s pre-projection COMPLETED under a %d-byte ceiling while carrying "+
				"a %d-byte value", a.site, parAggBudgetCeiling, parAggBudgetEstimate)
		}
		if !errors.Is(a.err, exec.ErrProjectionRowTooLarge) {
			t.Fatalf("the %s pre-projection raised %v; want exec.ErrProjectionRowTooLarge",
				a.site, a.err)
		}
	}
	want := arms[0].err.Error()
	for _, a := range arms[1:] {
		if got := a.err.Error(); got != want {
			t.Fatalf("the %s pre-projection's error string is not byte-identical to the "+
				"%s one, so a caller CAN tell which one ran:\n %s: %q\n %s: %q",
				a.site, arms[0].site, arms[0].site, want, a.site, got)
		}
	}
	if !strings.Contains(want, exec.ErrProjectionRowTooLarge.Error()) {
		t.Fatalf("the shared verdict %q does not carry the sentinel's own text %q",
			want, exec.ErrProjectionRowTooLarge.Error())
	}
}
