package cypher

// label_estimate_cancel_3010_test.go — rmp #3010 and #3011, white-box.
//
// Planner sites that only CHOOSE a plan read an O(1) count (exact, or an upper
// bound under MVCC churn). Sites whose correctness rests on the number — the
// index nested-loop join's coverage proof and the statistics staleness
// denominator — read the exact count, and that read must abandon its MVCC
// correction when the statement is cancelled. A read build memoises one
// corrected bitmap per label for the statement.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// churnedStatsResolver returns a resolver pinned at a snapshot that sees the
// 200 committed :A nodes of buildRangeSkewGraph, with statistics built and 5000
// more :A nodes created by a transaction left open for the test's life.
func churnedStatsResolver(t *testing.T) *lpgLabelResolver {
	t.Helper()
	g := buildRangeSkewGraph(t, 200, 10)
	on, _ := statsReorderPair(t, g)
	snap := g.BeginRead()
	t.Cleanup(func() { g.EndRead(snap) })
	tx, err := on.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	res, err := tx.Exec("UNWIND range(1, $n) AS i CREATE (:A {x: i})", map[string]expr.Value{"n": expr.IntegerValue(5000)})
	if err != nil {
		t.Fatalf("uncommitted create: %v", err)
	}
	for res.Next() {
	}
	if err := res.Close(); err != nil {
		t.Fatalf("uncommitted create: %v", err)
	}
	return &lpgLabelResolver{g: g.ReadAt(snap), eng: on}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestLabelEstimates_PlanChoiceIsBoundExactIsCancellable_3010 pins the split.
func TestLabelEstimates_PlanChoiceIsBoundExactIsCancellable_3010(t *testing.T) {
	src := churnedStatsResolver(t)

	// Plan choice: an O(1) upper bound, tagged so the veto accepts it, never
	// below the snapshot's 200.
	e := labelCardinalityEstimate(src, "A")
	if e.source != estBound || !e.trustworthy() || e.rows < 200 {
		t.Fatalf("plan-choice estimate = %+v, want a trustworthy estBound >= 200", e)
	}
	if n, ok := estimateLeadingScanRows(&ir.NodeByLabelScan{NodeVar: "a", Label: "A"}, src); !ok || float64(n) != e.rows {
		t.Fatalf("estimateLeadingScanRows = (%d, %v), want the bound %v", n, ok, e.rows)
	}

	// Exact: the snapshot's count when live, the context's error when cancelled.
	if n, err := labelExactRows(context.Background(), src, "A"); err != nil || n != 200 {
		t.Fatalf("labelExactRows = (%d, %v), want (200, nil)", n, err)
	}
	if _, err := labelExactRows(cancelledContext(), src, "A"); !errors.Is(err, context.Canceled) {
		t.Fatalf("labelExactRows on a cancelled context = %v, want context.Canceled", err)
	}
	if _, _, err := exactLeadingScanRows(cancelledContext(), &ir.NodeByLabelScan{NodeVar: "a", Label: "A"}, src); !errors.Is(err, context.Canceled) {
		t.Fatalf("exactLeadingScanRows (index nested-loop coverage) on a cancelled context = %v, want context.Canceled", err)
	}

	// Statistics denominator: exact and cancellable when a statistic exists ...
	pop, err := reorderPopulation(context.Background(), src, src, "A", "x", e)
	if err != nil || !pop.known || pop.n != 200 {
		t.Fatalf("reorderPopulation = (%+v, %v), want a known exact 200", pop, err)
	}
	if _, err := reorderPopulation(cancelledContext(), src, src, "A", "x", e); !errors.Is(err, context.Canceled) {
		t.Fatalf("reorderPopulation on a cancelled context = %v, want context.Canceled", err)
	}
	// ... and not resolved at all when none does: the providers would never read
	// it, so even a cancelled context costs nothing and reports nothing.
	pop, err = reorderPopulation(cancelledContext(), src, src, "A", "no_such_property", e)
	if err != nil || pop.known {
		t.Fatalf("reorderPopulation without a statistic = (%+v, %v), want unknown and no error", pop, err)
	}
	// An exact drain is still read as N, with no resolution.
	pop, err = reorderPopulation(cancelledContext(), src, src, "A", "x", estimate{rows: 7, source: estExact})
	if err != nil || !pop.known || pop.n != 7 {
		t.Fatalf("reorderPopulation with an exact drain = (%+v, %v), want a known 7", pop, err)
	}
}

// TestLabelBitmapMemo_ReusesOneCorrectionPerLabel_3011 pins the memo: one
// corrected bitmap per label per statement, bounded, and absent when the
// resolver has none.
func TestLabelBitmapMemo_ReusesOneCorrectionPerLabel_3011(t *testing.T) {
	src := churnedStatsResolver(t)
	ctx := context.Background()

	a1, err := src.ResolveLabelBitmapContext(ctx, "A")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := src.ResolveLabelBitmapContext(ctx, "A")
	if a1 == a2 {
		t.Fatal("without a memo two resolutions returned one instance: the precondition is wrong")
	}

	var m labelBitmapMemo
	src.memo = &m
	b1, err := src.ResolveLabelBitmapContext(ctx, "A")
	if err != nil {
		t.Fatal(err)
	}
	if b1.GetCardinality() != 200 {
		t.Fatalf("memoised bitmap holds %d, want the snapshot's 200", b1.GetCardinality())
	}
	b2, err := src.ResolveLabelBitmapContext(cancelledContext(), "A")
	if err != nil || b2 != b1 {
		t.Fatalf("second resolution = (%p, %v), want the memoised %p and no correction to cancel", b2, err, b1)
	}
	if n, ok, err := src.ResolveLabelCountAsOfContext(cancelledContext(), "A"); err != nil || !ok || n != 200 {
		t.Fatalf("count from the memo = (%d, %v, %v), want (200, true, nil)", n, ok, err)
	}
	for i := range 2 * labelBitmapMemoSlots {
		m.put(lpg.LabelID(1000+i), b1)
	}
	if m.n != labelBitmapMemoSlots {
		t.Fatalf("memo holds %d entries, want the bound %d", m.n, labelBitmapMemoSlots)
	}
}

// TestLabelEstimates_HybridIsExactBelowTheBacklogThreshold_3010 pins the hybrid
// the user chose for rmp #3010: below [planExactBacklog] a plan-choice count is
// the EXACT snapshot count; a cancelled statement still gets an answer — the
// bound — because a plan choice needs no exact figure to be sound.
func TestLabelEstimates_HybridIsExactBelowTheBacklogThreshold_3010(t *testing.T) {
	g := buildRangeSkewGraph(t, 200, 10)
	on, _ := statsReorderPair(t, g)
	snap := g.BeginRead()
	t.Cleanup(func() { g.EndRead(snap) })
	tx, err := on.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	res, err := tx.Exec("UNWIND range(1, 100) AS i CREATE (:A {x: i})", nil)
	if err != nil {
		t.Fatalf("uncommitted create: %v", err)
	}
	for res.Next() {
	}
	if err := res.Close(); err != nil {
		t.Fatalf("uncommitted create: %v", err)
	}
	if b := g.LabelHistoryBacklog(); b == 0 || b > planExactBacklog {
		t.Fatalf("precondition: backlog %d, want live history below the threshold %d", b, planExactBacklog)
	}
	src := &lpgLabelResolver{g: g.ReadAt(snap), eng: on}
	if e := labelCardinalityEstimate(src, "A"); e.source != estExact || e.rows != 200 {
		t.Fatalf("below the threshold the estimate = %+v, want the exact 200", e)
	}
	src.ctx = cancelledContext()
	if e := labelCardinalityEstimate(src, "A"); e.source != estBound || e.rows < 200 {
		t.Fatalf("a cancelled statement's estimate = %+v, want the bound (>= 200)", e)
	}
}
