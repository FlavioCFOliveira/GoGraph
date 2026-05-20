package ir_test

import (
	"strings"
	"testing"

	"gograph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper
// ─────────────────────────────────────────────────────────────────────────────

func assertExplain(t *testing.T, plan ir.LogicalPlan, want string) {
	t.Helper()
	got := ir.Explain(plan)
	wantTrimmed := strings.TrimSpace(want)
	gotTrimmed := strings.TrimSpace(got)
	if gotTrimmed != wantTrimmed {
		t.Errorf("Explain() mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 01: single AllNodesScan + ProduceResults
// MATCH (n) RETURN n
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_AllNodesScan(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewAllNodesScan("n"),
	)
	want := `
ProduceResults [n]
└─ AllNodesScan [n]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 02: NodeByLabelScan + Projection + ProduceResults
// MATCH (n:Person) RETURN n
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_NodeByLabelScan(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewProjection(
			[]ir.ProjectionItem{{Name: "n", Expression: "n"}},
			ir.NewNodeByLabelScan("n", "Person"),
		),
	)
	want := `
ProduceResults [n]
└─ Projection [n]
   └─ NodeByLabelScan [n:Person]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 03: Selection above NodeByLabelScan
// MATCH (n:Person) WHERE n.age > 18 RETURN n
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_Selection(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewSelection(
			"n.age > 18",
			ir.NewNodeByLabelScan("n", "Person"),
		),
	)
	want := `
ProduceResults [n]
└─ Selection [n.age > 18]
   └─ NodeByLabelScan [n:Person]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 04: NodeByIndexSeek
// MATCH (n:Person {email: "alice@example.com"}) RETURN n
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_NodeByIndexSeek(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewNodeByIndexSeek("n", "Person", "email", `"alice@example.com"`),
	)
	want := `
ProduceResults [n]
└─ NodeByIndexSeek [n:Person.email = "alice@example.com"]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 05: NodeByIndexRangeScan
// MATCH (n:Product) WHERE n.price >= 10 AND n.price < 100 RETURN n
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_NodeByIndexRangeScan(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewNodeByIndexRangeScan(
			"n", "Product", "price",
			&ir.Bound{Value: "10", Inclusive: true},
			&ir.Bound{Value: "100", Inclusive: false},
		),
	)
	want := `
ProduceResults [n]
└─ NodeByIndexRangeScan [n:Product.price >= 10 < 100]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 06: Expand
// MATCH (n:Person)-[r:KNOWS]->(m) RETURN n, r, m
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_Expand(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n", "r", "m"},
		ir.NewExpand(
			"n", "r", []string{"KNOWS"}, ir.DirectionOutgoing, "m",
			ir.NewNodeByLabelScan("n", "Person"),
		),
	)
	want := `
ProduceResults [n, r, m]
└─ Expand [r, m]
   └─ NodeByLabelScan [n:Person]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 07: EagerAggregation + Sort
// MATCH (n:Person) RETURN n.city, count(*) ORDER BY count(*) DESC
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_EagerAggregation(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n.city", "cnt"},
		ir.NewSort(
			[]ir.SortItem{{Expression: "cnt", Descending: true}},
			ir.NewEagerAggregation(
				[]string{"n.city"},
				[]ir.AggregateExpr{{OutputName: "cnt", Function: "count", Argument: ""}},
				ir.NewNodeByLabelScan("n", "Person"),
			),
		),
	)
	want := `
ProduceResults [n.city, cnt]
└─ Sort [n.city, cnt]
   └─ EagerAggregation [n.city, cnt]
      └─ NodeByLabelScan [n:Person]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 08: Union of two branches
// MATCH (n:Person) RETURN n UNION MATCH (n:Company) RETURN n
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_Union(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewUnion(
			ir.NewNodeByLabelScan("n", "Person"),
			ir.NewNodeByLabelScan("n", "Company"),
		),
	)
	want := `
ProduceResults [n]
└─ Union [n]
   ├─ NodeByLabelScan [n:Person]
   └─ NodeByLabelScan [n:Company]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 09: Apply (correlated subquery)
// Outer: AllNodesScan(n), Inner: AllNodesScan(m)
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_Apply(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n", "m"},
		ir.NewApply(
			ir.NewAllNodesScan("n"),
			ir.NewAllNodesScan("m"),
		),
	)
	want := `
ProduceResults [n, m]
└─ Apply [n, m]
   ├─ AllNodesScan [n]
   └─ AllNodesScan [m]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 10: Limit + Skip + Distinct deep chain
// MATCH (n) RETURN DISTINCT n SKIP 5 LIMIT 10
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_LimitSkipDistinct(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewLimit(10,
			ir.NewSkip(5,
				ir.NewDistinct(
					ir.NewAllNodesScan("n"),
				),
			),
		),
	)
	want := `
ProduceResults [n]
└─ Limit [n]
   └─ Skip [n]
      └─ Distinct [n]
         └─ AllNodesScan [n]
`
	assertExplain(t, plan, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Determinism: Explain must return the same string on repeated calls.
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_Deterministic(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewSelection("n.active = true",
			ir.NewNodeByLabelScan("n", "User"),
		),
	)
	first := ir.Explain(plan)
	for i := 0; i < 20; i++ {
		if got := ir.Explain(plan); got != first {
			t.Fatalf("Explain is non-deterministic: iteration %d differs", i+1)
		}
	}
}
