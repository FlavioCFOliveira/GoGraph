package cypher

// pattern_comp_budget_internal_test.go — white-box tests for the
// pattern-comprehension result-list budget and the per-appended-result context
// check (#1298). These assert wiring and loop behaviour a black-box test cannot
// observe directly:
//
//   - resolvePatternCompBudget maps the EngineOptions.MaxCollectItems encoding
//     (zero / negative sentinel / positive) onto the resolved ceiling exactly as
//     buildEagerAggregation does for the buffering aggregators (#1294);
//   - newPatternEvaluator stores that resolved ceiling;
//   - EvalPatternComp returns funcs.ErrCollectItemsExceeded once a single
//     comprehension's result list would exceed the budget, instead of growing it
//     to the anchor's full degree;
//   - a cancelled context aborts EvalPatternComp mid-enumeration with
//     context.Canceled rather than running the enumeration to completion.
//
// The budget is exercised at small scale by lowering it directly rather than
// building a multi-million-edge anchor; the engine-level surfacing through
// Result.Err() lives in pattern_comp_budget_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// starGraph builds a single centre node ("c") with degree outgoing edges to
// distinct leaf nodes, returning the graph. It is the canonical high-degree
// (supernode) anchor used to drive the comprehension result list past a lowered
// budget.
func starGraph(tb testing.TB, degree int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("c"); err != nil {
		tb.Fatalf("AddNode centre: %v", err)
	}
	for i := 0; i < degree; i++ {
		leaf := "leaf-" + itoa(i)
		if err := g.AddNode(leaf); err != nil {
			tb.Fatalf("AddNode %s: %v", leaf, err)
		}
		if err := g.AddEdge("c", leaf, 1); err != nil {
			tb.Fatalf("AddEdge c->%s: %v", leaf, err)
		}
	}
	return g
}

// itoa is a tiny base-10 formatter that avoids pulling strconv into the test for
// a single conversion (keeps the helper allocation-light and dependency-free).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// singleHopComprehension builds the AST for `[(c)-->(b) | b]` with the anchor
// variable c already bound by the surrounding row. It mirrors the shape the
// parser produces for the FR's `RETURN [(a)-->(b) | b]`.
func singleHopComprehension(anchorVar, endVar string) *ast.PatternComprehension {
	anchor := anchorVar
	end := endVar
	head := &ast.PathElement{
		Node: &ast.NodePattern{Variable: &anchor},
		Next: &ast.PathElement{
			Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionOutgoing},
			Node:         &ast.NodePattern{Variable: &end},
		},
	}
	return &ast.PatternComprehension{
		Pattern:    &ast.PathPattern{Head: head},
		Projection: &ast.Variable{Name: end},
	}
}

// boundAnchorRow returns a RowContext binding anchorVar to the NodeID of the
// star-graph centre ("c"), so EvalPatternComp enumerates exactly that node's
// out-neighbours.
func boundAnchorRow(tb testing.TB, g *lpg.Graph[string, float64], anchorVar string) expr.RowContext {
	tb.Helper()
	id, ok := g.AdjList().Mapper().Lookup("c")
	if !ok {
		tb.Fatal("centre node 'c' has no interned NodeID")
	}
	return expr.RowContext{anchorVar: expr.NodeValue{ID: uint64(id)}}
}

// TestResolvePatternCompBudget_Policy pins the zero/sentinel/positive mapping
// resolvePatternCompBudget implements. It must match the resolution
// buildEagerAggregation applies (#1294): zero selects the finite default, a
// negative sentinel disables the cap (resolves to 0), and any positive value
// passes through verbatim.
func TestResolvePatternCompBudget_Policy(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero selects default", 0, funcs.DefaultMaxCollectItems},
		{"unlimited sentinel disables cap", MaxCollectItemsUnlimited, 0},
		{"arbitrary negative disables cap", -7, 0},
		{"positive passes through", 42, 42},
		{"large positive passes through", funcs.DefaultMaxCollectItems + 1, funcs.DefaultMaxCollectItems + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePatternCompBudget(tc.in); got != tc.want {
				t.Fatalf("resolvePatternCompBudget(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestNewPatternEvaluator_BudgetWiring confirms the constructor stores the
// resolved ceiling for each interpreted band, including the unlimited opt-out.
func TestNewPatternEvaluator_BudgetWiring(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"default", 0, funcs.DefaultMaxCollectItems},
		{"unlimited opt-out", MaxCollectItemsUnlimited, 0},
		{"explicit override", 1000, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := newPatternEvaluator(g, tc.in)
			if pe.maxCollectItems != tc.want {
				t.Fatalf("maxCollectItems = %d, want %d", pe.maxCollectItems, tc.want)
			}
		})
	}
}

// TestPatternEvaluator_EvalPatternComp_BudgetTrips is the core budget regression
// for #1298: a comprehension whose anchor degree exceeds the (lowered) budget
// must stop with funcs.ErrCollectItemsExceeded instead of building the full
// degree-length list. Pre-fix EvalPatternComp had no cap and returned the
// complete list with a nil error; this assertion failed. Post-fix it returns the
// typed cap error.
func TestPatternEvaluator_EvalPatternComp_BudgetTrips(t *testing.T) {
	const (
		degree = 64
		budget = 8
	)
	g := starGraph(t, degree)
	pe := newPatternEvaluator(g, budget)

	pc := singleHopComprehension("a", "b")
	row := boundAnchorRow(t, g, "a")

	g.View(func() {
		_, err := pe.EvalPatternComp(context.Background(), pc, row, nil, funcs.DefaultRegistry)
		if !errors.Is(err, funcs.ErrCollectItemsExceeded) {
			t.Fatalf("EvalPatternComp over degree %d with budget %d: err = %v, want ErrCollectItemsExceeded",
				degree, budget, err)
		}
	})
}

// TestPatternEvaluator_EvalPatternComp_DefaultDoesNotTrip is the guard that the
// finite default does not interfere with an ordinary, small comprehension: the
// full list is returned with no error. This pins that the default ceiling is high
// enough for the TCK and the examples.
func TestPatternEvaluator_EvalPatternComp_DefaultDoesNotTrip(t *testing.T) {
	const degree = 6
	g := starGraph(t, degree)
	pe := newPatternEvaluator(g, 0) // default budget

	pc := singleHopComprehension("a", "b")
	row := boundAnchorRow(t, g, "a")

	g.View(func() {
		v, err := pe.EvalPatternComp(context.Background(), pc, row, nil, funcs.DefaultRegistry)
		if err != nil {
			t.Fatalf("EvalPatternComp under default budget: unexpected err %v", err)
		}
		list, ok := v.(expr.ListValue)
		if !ok {
			t.Fatalf("result is %T, want expr.ListValue", v)
		}
		if len(list) != degree {
			t.Fatalf("result list has %d elements, want %d (full anchor degree)", len(list), degree)
		}
	})
}

// TestPatternEvaluator_EvalPatternComp_CancelAborts asserts that a cancelled
// context aborts EvalPatternComp with context.Canceled rather than enumerating
// the whole high-degree anchor. The unlimited budget (no cap) isolates the abort
// to the context check: were the context check absent, the call would instead run
// to completion and return the full list with a nil error.
func TestPatternEvaluator_EvalPatternComp_CancelAborts(t *testing.T) {
	const degree = 256
	g := starGraph(t, degree)
	pe := newPatternEvaluator(g, MaxCollectItemsUnlimited) // no cap: isolate the ctx abort

	pc := singleHopComprehension("a", "b")
	row := boundAnchorRow(t, g, "a")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before evaluation begins

	g.View(func() {
		v, err := pe.EvalPatternComp(ctx, pc, row, nil, funcs.DefaultRegistry)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EvalPatternComp with cancelled context: err = %v, want context.Canceled", err)
		}
		if v != nil {
			t.Fatalf("EvalPatternComp aborted: value = %v, want nil", v)
		}
	})
}
