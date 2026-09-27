package cypher

// row_typed_predicate_internal_test.go — the typed row Filter fast path of
// rmp #2892 against the boxed evaluator it short-cuts.
//
// For every predicate shape the typed grammar accepts, and every row a Filter
// can hand it, a DECIDED typed verdict must be the exact value the boxed
// evaluator (evalRowPooled) returns for the same row — TRUE, FALSE and NULL
// kept apart — and the shapes the typed path must not decide (cross-kind
// numerics, temporal-tagged strings, non-node cells) must stay undecided.
// Each case also asserts how many rows WERE decided, so a regression that
// silently sends every row to the boxed path fails rather than passing
// vacuously.
//
// Layer: short.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func nProp(key string) *ast.Property {
	return &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: key}
}

func cmpOp(op string, l, r ast.Expression) *ast.BinaryOp {
	return &ast.BinaryOp{Operator: op, Left: l, Right: r}
}

func intLit(v int64) *ast.IntLiteral { return &ast.IntLiteral{Value: v} }

// typedPredGraph seeds four nodes whose properties cover every kind the typed
// comparison core distinguishes, and returns their ids ordered by the seeded k.
func typedPredGraph(t *testing.T) (*lpg.Graph[string, float64], []graph.NodeID) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngineWithOptions(g, EngineOptions{})
	for _, q := range []string{
		`CREATE (:A:B {k: 0, v: 1000, f: 1.5, s: 'x', b: true, d: date('2025-01-01')})`,
		`CREATE (:A {k: 1, v: 5, f: 2.5, s: 'y', b: false})`,
		`CREATE (:B {k: 2, v: 1000.0, s: 'x'})`,
		`CREATE (:C {k: 3})`,
	} {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("%s: close: %v", q, err)
		}
	}
	var ids []graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		ids = append(ids, id)
		return true
	})
	if len(ids) != 4 {
		t.Fatalf("seeded %d nodes, want 4", len(ids))
	}
	// Order the ids by the seeded k, so a test can name a node by its index.
	view := g.ReadAt(nil)
	k := func(id graph.NodeID) int64 {
		pv, _ := view.NodePropertyByID(id, "k")
		v, _ := pv.Int64()
		return v
	}
	slices.SortFunc(ids, func(a, b graph.NodeID) int { return int(k(a) - k(b)) })
	return g, ids
}

func TestRowTypedPredicate_MatchesBoxedEvaluator(t *testing.T) {
	g, ids := typedPredGraph(t)
	view := g.ReadAt(nil)
	params := map[string]expr.Value{"p": expr.IntegerValue(1000)}
	reg := newNowAwareRegistry(newGraphAwareRegistry(funcs.DefaultRegistry, view), time.Unix(0, 0))

	rows := make([]exec.Row, 0, len(ids)+3)
	for _, id := range ids {
		rows = append(rows, exec.Row{expr.IntegerValue(id)})
	}
	// Cells the typed path must never decide: a non-node value, a NULL cell and
	// an integer that is no node's id.
	rows = append(rows, exec.Row{expr.StringValue("n")}, exec.Row{nil}, exec.Row{expr.IntegerValue(1 << 40)})
	const nonNodeRows = 3

	for _, tc := range []struct {
		name string
		pred ast.Expression
		// minDecided is the number of node rows the typed path must decide.
		minDecided int
		// boxedRows are node rows the typed path must leave to the boxed path.
		boxedRows []int
	}{
		{"int equality", cmpOp("=", nProp("v"), intLit(1000)), 3, []int{2}}, // node 2 holds 1000.0: cross-kind
		{"int equality flipped", cmpOp("=", intLit(5), nProp("v")), 2, nil},
		{"int equality parameter", cmpOp("=", nProp("v"), &ast.Parameter{Name: "p"}), 2, nil},
		{"int ordering", cmpOp("<", nProp("v"), intLit(10)), 2, nil},
		{"int inequality", cmpOp("<>", nProp("v"), intLit(5)), 2, nil},
		{"absent property is NULL", cmpOp("=", nProp("missing"), intLit(1)), 4, nil},
		{"float ordering", cmpOp(">=", nProp("f"), &ast.FloatLiteral{Value: 2.0}), 2, nil},
		{"string equality", cmpOp("=", nProp("s"), &ast.StringLiteral{Value: "x"}), 3, nil},
		{"bool equality", cmpOp("=", nProp("b"), &ast.BoolLiteral{Value: true}), 2, nil},
		{"temporal-tagged string stays boxed", cmpOp("=", nProp("d"), &ast.StringLiteral{Value: "2025-01-01"}), 3, []int{0}},
		{"label", &ast.LabelPredicate{Receiver: &ast.Variable{Name: "n"}, Labels: []string{"A"}}, 4, nil},
		{"labels", &ast.LabelPredicate{Receiver: &ast.Variable{Name: "n"}, Labels: []string{"A", "B"}}, 4, nil},
		{"in", cmpOp("IN", nProp("v"), &ast.ListLiteral{Elements: []ast.Expression{intLit(5), &ast.StringLiteral{Value: "z"}}}), 2, nil},
		{"in miss", cmpOp("IN", nProp("v"), &ast.ListLiteral{Elements: []ast.Expression{intLit(2), intLit(3)}}), 2, nil},
		{"conjunction", cmpOp("AND", &ast.LabelPredicate{Receiver: &ast.Variable{Name: "n"}, Labels: []string{"A"}}, cmpOp("=", nProp("v"), intLit(1000))), 2, nil},
		{"NULL AND TRUE is NULL", cmpOp("AND", cmpOp("=", nProp("missing"), intLit(1)), cmpOp("=", nProp("v"), intLit(1000))), 2, nil},
		{"NULL AND FALSE is FALSE", cmpOp("AND", cmpOp("=", nProp("missing"), intLit(1)), cmpOp("=", nProp("v"), intLit(6))), 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := map[string]int{"n": 0}
			bopts := &buildOpts{}
			rs := newRowSchema(copySchema(schema))
			scalarUse, bail := analyseNodeScalarUseFor(bopts, tc.pred)
			if bail {
				t.Fatalf("scalar-use analysis bailed on %s", tc.name)
			}
			bp := newRowBindPlan(rs, bopts, view, scalarUse)
			rp, ok := newRowTypedPredicate(tc.pred, bp, params, reg)
			if !ok {
				t.Fatalf("typed grammar rejected %s", tc.name)
			}
			decided := 0
			for i, row := range rows {
				got, isDecided := rp.eval(row)
				want, err := evalRowPooled(tc.pred, row, bp, params, reg)
				if isDecided && slices.Contains(tc.boxedRows, i) {
					t.Errorf("row %d was decided %v; it must be left to the boxed path", i, got)
				}
				if !isDecided {
					continue
				}
				if i >= len(ids) {
					t.Errorf("row %d (a non-node cell) was decided %v", i, got)
					continue
				}
				decided++
				if err != nil {
					t.Errorf("row %d: typed path decided %v where the boxed path raised %v", i, got, err)
					continue
				}
				if expr.IsNull(want) != expr.IsNull(got) || (!expr.IsNull(want) && want != got) {
					t.Errorf("row %d: typed %v, boxed %v", i, got, want)
				}
			}
			if decided < tc.minDecided {
				t.Errorf("typed path decided %d of %d node rows, want at least %d", decided, len(rows)-nonNodeRows, tc.minDecided)
			}
		})
	}
}

// TestRowTypedPredicate_ScalarColumnNotTyped pins the binding gate: a variable
// registered as a scalar column is not a node, so the typed grammar must not
// claim it.
func TestRowTypedPredicate_ScalarColumnNotTyped(t *testing.T) {
	g, _ := typedPredGraph(t)
	view := g.ReadAt(nil)
	pred := cmpOp("=", nProp("v"), intLit(1))
	bopts := &buildOpts{scalarCols: map[string]struct{}{"n": {}}}
	bp := newRowBindPlan(newRowSchema(map[string]int{"n": 0}), bopts, view, nil)
	if _, ok := newRowTypedPredicate(pred, bp, nil, funcs.DefaultRegistry); ok {
		t.Fatal("typed grammar accepted a predicate over a scalar column")
	}
}

// TestNowAwareRegistry_BindResetsCallSites pins that a wrapper re-bound for a
// later statement never serves the previous statement's call-site memo.
func TestNowAwareRegistry_BindResetsCallSites(t *testing.T) {
	var r nowAwareRegistry
	r.bind(funcs.DefaultRegistry, time.Unix(0, 0))
	if !r.FoldsStatementConstants() {
		t.Fatal("a wrapper over the default registry must fold statement constants")
	}
	key := &ast.FunctionInvocation{Name: "date"}
	if got := r.StoreCallSite(key, "site"); got != "site" {
		t.Fatalf("StoreCallSite = %v, want the stored site", got)
	}
	r.bind(funcs.DefaultRegistry, time.Unix(0, 0))
	if _, ok := r.LoadCallSite(key); ok {
		t.Fatal("bind kept the previous statement's call site")
	}
	custom := funcs.NewRegistry()
	r.bind(custom, time.Unix(0, 0))
	if r.FoldsStatementConstants() {
		t.Fatal("a wrapper over a user registry must not fold")
	}
}

// TestNowAwareRegistry_CallSiteTableIsBounded pins the table's bound.
func TestNowAwareRegistry_CallSiteTableIsBounded(t *testing.T) {
	var r nowAwareRegistry
	for i := range maxStatementCallSites {
		if r.StoreCallSite(&ast.FunctionInvocation{Name: "f"}, i) == nil {
			t.Fatalf("store %d refused below the bound", i)
		}
	}
	if got := r.StoreCallSite(&ast.FunctionInvocation{Name: "f"}, -1); got != nil {
		t.Fatalf("store past the bound returned %v, want nil", got)
	}
}
