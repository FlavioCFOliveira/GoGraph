package cypher_test

// pattern_comp_budget_test.go — black-box behavioural tests for the
// pattern-comprehension result-list budget (#1298), exercising the public Engine
// API exactly as a consumer would. A comprehension over a high-degree (supernode)
// anchor grows its inner list to the anchor's degree; with a test-lowered budget
// that list must stop with funcs.ErrCollectItemsExceeded, surfaced via
// Result.Err() — the AC's bounded-resource error. The budget shares the
// buffering-aggregator knob (EngineOptions.MaxCollectItems), so the cap is
// consistent with collect() (#1294).
//
// The default budget (DefaultMaxCollectItems = 10M) is exercised cheaply by
// lowering it through EngineOptions rather than building a multi-million-edge
// anchor; the evaluator-level proof lives in
// pattern_comp_budget_internal_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newStar builds an lpg.Graph with one centre node and `degree` outgoing edges
// to distinct leaves — a supernode anchor for pattern-comprehension tests.
func newStar(tb testing.TB, degree int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("c"); err != nil {
		tb.Fatalf("AddNode centre: %v", err)
	}
	for i := 0; i < degree; i++ {
		leaf := "leaf-" + leafID(i)
		if err := g.AddNode(leaf); err != nil {
			tb.Fatalf("AddNode %s: %v", leaf, err)
		}
		if err := g.AddEdge("c", leaf, 1); err != nil {
			tb.Fatalf("AddEdge c->%s: %v", leaf, err)
		}
	}
	return g
}

// leafID renders i in base 10 without importing strconv for a single call.
func leafID(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[p:])
}

// TestEngine_PatternCompBudget_TripsBoundedError lowers the per-query element
// budget below the anchor's degree and asserts that
// `MATCH (a) RETURN [(a)-->(b) | b]` over the supernode centre stops with
// ErrCollectItemsExceeded reported by Result.Err(). This reproduces the AC's
// failure mode (a comprehension list growing to a supernode's degree) at small
// scale via the public knob. Pre-fix the evaluator built the full list and
// Result.Err() was nil; post-fix it carries the typed cap error.
func TestEngine_PatternCompBudget_TripsBoundedError(t *testing.T) {
	const (
		degree = 64
		budget = 8
	)
	g := newStar(t, degree)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxCollectItems: budget})

	res, err := eng.Run(context.Background(), "MATCH (a) RETURN [(a)-->(b) | b] AS bs", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	for res.Next() {
	}
	if got := res.Err(); !errors.Is(got, funcs.ErrCollectItemsExceeded) {
		t.Fatalf("Result.Err() = %v, want ErrCollectItemsExceeded", got)
	}
}

// TestEngine_PatternCompBudget_DefaultAllowsSmall is the control: a small
// comprehension under the finite default evaluates fully and returns the correct
// list. It guards that the default ceiling is high enough for the TCK and the
// examples. The centre node yields a `degree`-element list; every leaf yields an
// empty list (no outgoing edges).
func TestEngine_PatternCompBudget_DefaultAllowsSmall(t *testing.T) {
	const degree = 6
	g := newStar(t, degree)
	eng := cypher.NewEngine(g) // default budget

	res, err := eng.Run(context.Background(), "MATCH (a) RETURN [(a)-->(b) | b] AS bs", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var (
		rows    int
		maxLen  int
		anchors int
	)
	for res.Next() {
		rows++
		bs, ok := res.Record()["bs"].(expr.ListValue)
		if !ok {
			t.Fatalf("bs is %T, want expr.ListValue", res.Record()["bs"])
		}
		if len(bs) == degree {
			anchors++
		}
		if len(bs) > maxLen {
			maxLen = len(bs)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil under the default budget", err)
	}
	if rows != degree+1 {
		t.Fatalf("got %d rows, want %d (centre + %d leaves)", rows, degree+1, degree)
	}
	if maxLen != degree {
		t.Fatalf("largest comprehension list has %d elements, want %d (centre degree)", maxLen, degree)
	}
	if anchors != 1 {
		t.Fatalf("got %d full-degree rows, want exactly 1 (the centre)", anchors)
	}
}

// TestEngine_PatternCompBudget_UnlimitedOptOut proves the explicit opt-out:
// MaxCollectItemsUnlimited disables the budget, so a comprehension over an anchor
// whose degree exceeds a small budget returns the full list with no error.
func TestEngine_PatternCompBudget_UnlimitedOptOut(t *testing.T) {
	const degree = 64
	g := newStar(t, degree)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		MaxCollectItems: cypher.MaxCollectItemsUnlimited,
	})

	res, err := eng.Run(context.Background(), "MATCH (a) RETURN [(a)-->(b) | b] AS bs", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var maxLen int
	for res.Next() {
		if bs, ok := res.Record()["bs"].(expr.ListValue); ok && len(bs) > maxLen {
			maxLen = len(bs)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil for the unlimited opt-out", err)
	}
	if maxLen != degree {
		t.Fatalf("opt-out: largest list has %d elements, want %d", maxLen, degree)
	}
}
