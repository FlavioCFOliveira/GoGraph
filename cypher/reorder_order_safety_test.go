package cypher

// reorder_order_safety_test.go — unit tests for [SuppressReorder] (#2092),
// covering every operator class in the order-safety taxonomy: neutral pass-
// through, each observer, the total-sort RESET enabler, the non-total-sort
// observer, collect() suppression in both EagerAggregation and Projection, and
// the order-blind aggregation that does NOT suppress.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// cartAB is a disjoint two-scan Cartesian used as the subtree below a spine
// operator; its columns are {a, b} so a totality check has a known column set.
func cartAB() ir.LogicalPlan {
	return ir.NewApply(
		ir.NewNodeByLabelScan("a", "A"),
		ir.NewNodeByLabelScan("b", "B"),
	)
}

func varExpr(name string) ast.Expression { return &ast.Variable{Name: name} }

func collectItemProjection() *ir.Projection {
	return ir.NewProjection([]ir.ProjectionItem{
		{Expr: &ast.FunctionInvocation{Name: "collect", Args: []ast.Expression{varExpr("a")}}, Name: "c"},
	}, cartAB())
}

func scalarProjection() *ir.Projection {
	return ir.NewProjection([]ir.ProjectionItem{
		{Expr: varExpr("a"), Name: "a"},
		{Expr: varExpr("b"), Name: "b"},
	}, cartAB())
}

func collectAggregation() *ir.EagerAggregation {
	return ir.NewEagerAggregation(nil, []ir.AggregateExpr{
		{Function: "collect", Argument: "a", OutputName: "c"},
	}, cartAB())
}

func orderBlindAggregation(fn string) *ir.EagerAggregation {
	return ir.NewEagerAggregation(nil, []ir.AggregateExpr{
		{Function: fn, Argument: "a", OutputName: "r"},
	}, cartAB())
}

// totalSort orders by every column of {a, b} → total.
func totalSort() *ir.Sort {
	return ir.NewSort([]ir.SortItem{
		{Expr: varExpr("a")}, {Expr: varExpr("b")},
	}, cartAB())
}

// nonTotalSort orders by only one of {a, b} → tie-groups remain in arrival order.
func nonTotalSort() *ir.Sort {
	return ir.NewSort([]ir.SortItem{{Expr: varExpr("a")}}, cartAB())
}

// totalTop is the fused ORDER BY … LIMIT variant of totalSort.
func totalTop() *ir.Top {
	return ir.NewTop([]ir.SortItem{
		{Expr: varExpr("a")}, {Expr: varExpr("b")},
	}, 10, cartAB())
}

func TestSuppressReorder_OperatorClasses(t *testing.T) {
	tests := []struct {
		name  string
		spine []ir.LogicalPlan
		want  bool // true = suppress (order-unsafe)
	}{
		// Reaching the root with no observer → safe.
		{"empty spine", nil, false},

		// NEUTRAL pass-through: each alone reaches the root without an observer.
		{"selection", []ir.LogicalPlan{ir.NewSelection("x", cartAB())}, false},
		{"distinct", []ir.LogicalPlan{ir.NewDistinct(cartAB())}, false},
		{"apply", []ir.LogicalPlan{cartAB()}, false},
		{"scalar projection", []ir.LogicalPlan{scalarProjection()}, false},
		{"produce results", []ir.LogicalPlan{ir.NewProduceResults([]string{"a", "b"}, cartAB())}, false},
		{"union all", []ir.LogicalPlan{ir.NewUnionAll(cartAB(), cartAB())}, false},
		{
			"neutral chain to root",
			[]ir.LogicalPlan{
				ir.NewSelection("x", cartAB()),
				ir.NewDistinct(cartAB()),
				scalarProjection(),
				ir.NewProduceResults([]string{"a", "b"}, cartAB()),
			},
			false,
		},

		// OBSERVERS: each suppresses.
		{"bare limit", []ir.LogicalPlan{ir.NewLimit(5, cartAB())}, true},
		{"bare skip", []ir.LogicalPlan{ir.NewSkip(5, cartAB())}, true},
		{"collect in aggregation", []ir.LogicalPlan{collectAggregation()}, true},
		{"collect in projection", []ir.LogicalPlan{collectItemProjection()}, true},
		{"rollup apply (pattern comprehension)", []ir.LogicalPlan{ir.NewRollUpApply(cartAB(), cartAB(), "list")}, true},
		{"procedure call", []ir.LogicalPlan{ir.NewProcedureCall(nil, "db.labels", nil, []string{"label"}, cartAB())}, true},
		{"unwind", []ir.LogicalPlan{ir.NewUnwind("[1,2]", "x", cartAB())}, true},
		{"non-total sort", []ir.LogicalPlan{nonTotalSort()}, true},
		{"non-total top", []ir.LogicalPlan{ir.NewTop([]ir.SortItem{{Expr: varExpr("a")}}, 3, cartAB())}, true},

		// RESET enabler: a total sort/top erases arrival order → safe, even with an
		// observer above it (the total sort is the nearest ancestor and wins).
		{"total sort", []ir.LogicalPlan{totalSort()}, false},
		{"total top", []ir.LogicalPlan{totalTop()}, false},
		{"total sort masks limit above", []ir.LogicalPlan{totalSort(), ir.NewLimit(3, cartAB())}, false},
		{"total sort masks collect above", []ir.LogicalPlan{totalSort(), collectAggregation()}, false},

		// A bare limit BELOW a total sort (nearest first) still observes which rows
		// survive → suppress.
		{"limit below total sort", []ir.LogicalPlan{ir.NewLimit(3, cartAB()), totalSort()}, true},

		// ORDER-BLIND aggregations do NOT suppress.
		{"count aggregation", []ir.LogicalPlan{orderBlindAggregation("count")}, false},
		{"sum aggregation", []ir.LogicalPlan{orderBlindAggregation("sum")}, false},
		{"avg aggregation", []ir.LogicalPlan{orderBlindAggregation("avg")}, false},
		{"min aggregation", []ir.LogicalPlan{orderBlindAggregation("min")}, false},
		{"max aggregation", []ir.LogicalPlan{orderBlindAggregation("max")}, false},
		{"stDev aggregation", []ir.LogicalPlan{orderBlindAggregation("stDev")}, false},
		{"stDevP aggregation", []ir.LogicalPlan{orderBlindAggregation("stDevP")}, false},
		{"percentileCont aggregation", []ir.LogicalPlan{orderBlindAggregation("percentileCont")}, false},
		{"percentileDisc aggregation", []ir.LogicalPlan{orderBlindAggregation("percentileDisc")}, false},

		// First decisive observer wins: an observer nearer than a neutral suppresses.
		{
			"observer above neutrals",
			[]ir.LogicalPlan{
				ir.NewSelection("x", cartAB()),
				collectAggregation(),
				ir.NewProduceResults([]string{"c"}, cartAB()),
			},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SuppressReorder(tc.spine); got != tc.want {
				t.Errorf("SuppressReorder(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestIsTotalOrderSort_Provable checks the totality prover directly: bare-tuple
// cover and id/elementId pinning prove totality; a partial cover, a property
// key, or an empty key list do not.
func TestIsTotalOrderSort_Provable(t *testing.T) {
	elementIDCall := func(v string) ast.Expression {
		return &ast.FunctionInvocation{Name: "elementId", Args: []ast.Expression{varExpr(v)}}
	}
	idCall := func(v string) ast.Expression {
		return &ast.FunctionInvocation{Name: "id", Args: []ast.Expression{varExpr(v)}}
	}
	propKey := func(v, k string) ast.Expression {
		return &ast.Property{Receiver: varExpr(v), Key: k}
	}
	tests := []struct {
		name  string
		items []ir.SortItem
		want  bool
	}{
		{"empty", nil, false},
		{"full bare-tuple cover", []ir.SortItem{{Expr: varExpr("a")}, {Expr: varExpr("b")}}, true},
		{"partial cover", []ir.SortItem{{Expr: varExpr("a")}}, false},
		{"elementId cover", []ir.SortItem{{Expr: elementIDCall("a")}, {Expr: elementIDCall("b")}}, true},
		{"id cover", []ir.SortItem{{Expr: idCall("a")}, {Expr: idCall("b")}}, true},
		{"mixed id + bare cover", []ir.SortItem{{Expr: idCall("a")}, {Expr: varExpr("b")}}, true},
		{"property keys do not pin", []ir.SortItem{{Expr: propKey("a", "x")}, {Expr: propKey("b", "y")}}, false},
		{"partial id cover", []ir.SortItem{{Expr: idCall("a")}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := ir.NewSort(tc.items, cartAB())
			if got := isTotalOrderSort(tc.items, node); got != tc.want {
				t.Errorf("isTotalOrderSort(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
