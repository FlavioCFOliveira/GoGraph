package cypher_ldbc_test

// label_intersect_bench_test.go — benchmark for the set-at-a-time multi-label
// conjunction (#2133, measured under #2136).
//
// GoGraph stored labels as Roaring bitmaps and already had a container-wise AND,
// but the planner used it with one label: `MATCH (n:LabA:LabB)` scanned one label
// and re-checked the rest per row. On the audit's fixture that is ~100 000 rows
// materialised to produce 100.
//
// # The measurement is END-TO-END, deliberately
//
// The round-2 audit reported an 8127× ratio and flagged it itself as overstated,
// because it compared an end-to-end engine query against a BARE access path. The
// engine's fixed parse / plan / result overhead does not disappear when the access
// path gets cheaper, so this benchmark drives everything through Engine.Run on both
// arms and the recorded figure is the real deliverable gain. Sprint 311 measured a
// 100-row answer through the full engine at ≈31 µs, which is the floor that bounds
// any claim here.
//
// # Two fixtures, on purpose
//
// SELECTIVE — two labels of 100 000 with an intersection of 100. The path fires.
//
// BREAKEVEN — the intersection EQUALS the smaller label (nested labels). The gate
// vetoes, because there are no rows left to remove and a rewrite may pre-empt
// another only when it removes rows. This arm exists to show the veto does not
// REGRESS: enabled and disabled must be indistinguishable, since both plan the same
// thing.
//
// TestLabelIntersectBenchAgree asserts both arms of both fixtures return the same
// rows before any timing is believed.
//
// Run:
//
//	go test -run='^$' -bench=BenchmarkLabelIntersect -benchmem -count=10 ./bench/cypher_ldbc/...

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	// liBenchPop is each label's population — the audit's 100 000.
	liBenchPop = 100_000
	// liBenchOverlap is |LabA ∩ LabB| for the selective fixture.
	liBenchOverlap = 100
	// liBenchNested is the population of the nested label in the break-even
	// fixture; its intersection with the outer label equals its own size.
	liBenchNested = 2000
)

const (
	// liBenchQuery is the audit's query. count(n) keeps the result one row, so the
	// measurement is dominated by the access path rather than by result
	// materialisation — the honest place to see a scan replaced by a set operation.
	liBenchQuery = `MATCH (n:LabA:LabB) RETURN count(n) AS c`
	// liBenchRowsQuery returns the rows themselves, so the win is also measured on
	// the shape a user actually writes, where result materialisation is paid too.
	liBenchRowsQuery = `MATCH (n:LabA:LabB) RETURN n.k AS k`
	// liBenchNestedQuery is the break-even shape: :Inner is a subset of :Outer.
	liBenchNestedQuery = `MATCH (n:Inner:Outer) RETURN count(n) AS c`
)

// buildLabelIntersectBench seeds |LabA| = |LabB| = liBenchPop with an intersection
// of liBenchOverlap, plus the nested :Inner ⊂ :Outer pair for the break-even arm.
func buildLabelIntersectBench(tb testing.TB, disable bool) *cypher.Engine {
	tb.Helper()
	// Directed + Multigraph is the openCypher storage model, and it also stops the
	// engine logging its non-directed / non-multigraph warnings, which would
	// interleave with the benchmark output and make benchstat drop samples.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	set := func(key string, labels ...string) {
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode %q: %v", key, err)
		}
		for _, l := range labels {
			if err := g.SetNodeLabel(key, l); err != nil {
				tb.Fatalf("SetNodeLabel %q: %v", key, err)
			}
		}
		if err := g.SetNodeProperty(key, "k", lpg.StringValue(key)); err != nil {
			tb.Fatalf("SetNodeProperty %q: %v", key, err)
		}
	}

	for i := 0; i < liBenchPop; i++ {
		if i < liBenchOverlap {
			set(fmt.Sprintf("a%06d", i), "LabA", "LabB")
			continue
		}
		set(fmt.Sprintf("a%06d", i), "LabA")
	}
	// The rest of LabB, disjoint from LabA beyond the overlap.
	for i := liBenchOverlap; i < liBenchPop; i++ {
		set(fmt.Sprintf("b%06d", i), "LabB")
	}
	// Break-even pair: every :Inner node is also :Outer, so the intersection is
	// exactly |Inner| and the gate must veto.
	for i := 0; i < liBenchPop; i++ {
		key := fmt.Sprintf("o%06d", i)
		if i < liBenchNested {
			set(key, "Outer", "Inner")
			continue
		}
		set(key, "Outer")
	}

	return cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		DisableBitmapIntersection: disable,
		MaxResultRows:             cypher.MaxResultRowsUnlimited,
	})
}

// countLabelIntersectRows drains q and returns the row count.
func countLabelIntersectRows(tb testing.TB, eng *cypher.Engine, q string) int {
	tb.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		tb.Fatalf("Run %q: %v", q, err)
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("Err %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("Close %q: %v", q, err)
	}
	return n
}

// firstInt drains q and returns its single integer cell — the aggregate's value,
// so the correctness gate checks the ANSWER and not merely the row count.
func firstInt(tb testing.TB, eng *cypher.Engine, q string) int64 {
	tb.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		tb.Fatalf("Run %q: %v", q, err)
	}
	var got int64 = -1
	for res.Next() {
		if _, serr := fmt.Sscanf(res.ValueAt(0).String(), "%d", &got); serr != nil {
			tb.Fatalf("value %q is not an integer: %v", res.ValueAt(0).String(), serr)
		}
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("Err %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("Close %q: %v", q, err)
	}
	return got
}

// TestLabelIntersectBenchAgree is the companion correctness gate: both arms of both
// fixtures must produce the same ANSWER. Without it the benchmark could report a
// large win that is really a wrong answer.
func TestLabelIntersectBenchAgree(t *testing.T) {
	on := buildLabelIntersectBench(t, false)
	off := buildLabelIntersectBench(t, true)

	if got := firstInt(t, on, liBenchQuery); got != liBenchOverlap {
		t.Fatalf("enabled count = %d, want %d", got, liBenchOverlap)
	}
	if got := firstInt(t, off, liBenchQuery); got != liBenchOverlap {
		t.Fatalf("disabled count = %d, want %d", got, liBenchOverlap)
	}
	if a, b := countLabelIntersectRows(t, on, liBenchRowsQuery),
		countLabelIntersectRows(t, off, liBenchRowsQuery); a != b || a != liBenchOverlap {
		t.Fatalf("row arms disagree: enabled=%d disabled=%d want %d", a, b, liBenchOverlap)
	}
	// The break-even fixture: the gate vetoes, so both arms plan the same thing and
	// must of course agree.
	if a, b := firstInt(t, on, liBenchNestedQuery), firstInt(t, off, liBenchNestedQuery); a != b || a != liBenchNested {
		t.Fatalf("break-even arms disagree: enabled=%d disabled=%d want %d", a, b, liBenchNested)
	}
}

func benchLabelIntersect(b *testing.B, eng *cypher.Engine, q string) {
	b.Helper()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			b.Fatalf("Err: %v", err)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// BenchmarkLabelIntersect_Intersection measures the set-at-a-time plan.
func BenchmarkLabelIntersect_Intersection(b *testing.B) {
	benchLabelIntersect(b, buildLabelIntersectBench(b, false), liBenchQuery)
}

// BenchmarkLabelIntersect_ScanFilter measures the same query with the path
// disabled — the label scan plus residual Filter it replaces.
func BenchmarkLabelIntersect_ScanFilter(b *testing.B) {
	benchLabelIntersect(b, buildLabelIntersectBench(b, true), liBenchQuery)
}

// BenchmarkLabelIntersect_RowsIntersection measures the row-returning shape, where
// result materialisation is paid as well as the access path.
func BenchmarkLabelIntersect_RowsIntersection(b *testing.B) {
	benchLabelIntersect(b, buildLabelIntersectBench(b, false), liBenchRowsQuery)
}

// BenchmarkLabelIntersect_RowsScanFilter is its disabled counterpart.
func BenchmarkLabelIntersect_RowsScanFilter(b *testing.B) {
	benchLabelIntersect(b, buildLabelIntersectBench(b, true), liBenchRowsQuery)
}

// BenchmarkLabelIntersect_BreakevenEnabled measures the nested-label fixture with
// the path ENABLED. The gate vetoes here, so this must be indistinguishable from
// the disabled arm below — the point being that a veto costs nothing.
func BenchmarkLabelIntersect_BreakevenEnabled(b *testing.B) {
	benchLabelIntersect(b, buildLabelIntersectBench(b, false), liBenchNestedQuery)
}

// BenchmarkLabelIntersect_BreakevenDisabled is the control for the veto.
func BenchmarkLabelIntersect_BreakevenDisabled(b *testing.B) {
	benchLabelIntersect(b, buildLabelIntersectBench(b, true), liBenchNestedQuery)
}
