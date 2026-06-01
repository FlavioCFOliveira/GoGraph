package cypher_test

// order_tie_aggregation_test.go — ORDER BY on aggregated results including
// tie-breaking via secondary sort key (T752).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newTieredGraph creates an engine with two categories:
//
//	category "A": scores [1, 2, 3]  → sum = 6
//	category "B": scores [4, 5, 6]  → sum = 15
func newTieredGraph(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		`CREATE (:Item {cat: 'A', score: 1})`,
		`CREATE (:Item {cat: 'A', score: 2})`,
		`CREATE (:Item {cat: 'A', score: 3})`,
		`CREATE (:Item {cat: 'B', score: 4})`,
		`CREATE (:Item {cat: 'B', score: 5})`,
		`CREATE (:Item {cat: 'B', score: 6})`,
	} {
		runSetup(t, eng, q)
	}
	return eng
}

// TestOrderTieAggregation_SumASC verifies that aggregated rows are returned in
// ascending sum order.
//
// Expected: A (sum=6) before B (sum=15).
func TestOrderTieAggregation_SumASC(t *testing.T) {
	t.Parallel()
	eng := newTieredGraph(t)

	const q = `MATCH (n:Item) RETURN n.cat AS cat, sum(n.score) AS total ORDER BY total ASC`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 2 {
		t.Fatalf("expected 2 group rows, got %d", len(rows))
	}

	cat0 := rows[0]["cat"]
	cat1 := rows[1]["cat"]
	total0 := rows[0]["total"]
	total1 := rows[1]["total"]

	// Row 0 must be category A (sum=6).
	if s, ok := cat0.(expr.StringValue); !ok || string(s) != "A" {
		t.Errorf("row 0 cat = %v (%T), want A", cat0, cat0)
	}
	mustInt(t, "row0 total", total0, 6)

	// Row 1 must be category B (sum=15).
	if s, ok := cat1.(expr.StringValue); !ok || string(s) != "B" {
		t.Errorf("row 1 cat = %v (%T), want B", cat1, cat1)
	}
	mustInt(t, "row1 total", total1, 15)
}

// TestOrderTieAggregation_SumDESC verifies descending sort on aggregated total.
//
// Expected: B (sum=15) before A (sum=6).
func TestOrderTieAggregation_SumDESC(t *testing.T) {
	t.Parallel()
	eng := newTieredGraph(t)

	const q = `MATCH (n:Item) RETURN n.cat AS cat, sum(n.score) AS total ORDER BY total DESC`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 2 {
		t.Fatalf("expected 2 group rows, got %d", len(rows))
	}

	if s, ok := rows[0]["cat"].(expr.StringValue); !ok || string(s) != "B" {
		t.Errorf("row 0 cat = %v (%T), want B (DESC)", rows[0]["cat"], rows[0]["cat"])
	}
	mustInt(t, "row0 total DESC", rows[0]["total"], 15)

	if s, ok := rows[1]["cat"].(expr.StringValue); !ok || string(s) != "A" {
		t.Errorf("row 1 cat = %v (%T), want A (DESC)", rows[1]["cat"], rows[1]["cat"])
	}
	mustInt(t, "row1 total DESC", rows[1]["total"], 6)
}

// TestOrderTieAggregation_TieBreakByCat builds two groups with the SAME total
// and uses a secondary ORDER BY cat ASC to break ties deterministically.
//
// Graph:
//
//	category "X": scores [5, 5]  → sum = 10
//	category "Y": scores [4, 6]  → sum = 10  (tie)
//
// ORDER BY total ASC, cat ASC → X before Y (same sum, X < Y lexicographically).
func TestOrderTieAggregation_TieBreakByCat(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	for _, q := range []string{
		`CREATE (:Tie {cat: 'X', score: 5})`,
		`CREATE (:Tie {cat: 'X', score: 5})`,
		`CREATE (:Tie {cat: 'Y', score: 4})`,
		`CREATE (:Tie {cat: 'Y', score: 6})`,
	} {
		runSetup(t, eng, q)
	}

	const q = `MATCH (n:Tie) RETURN n.cat AS cat, sum(n.score) AS total ORDER BY total ASC, cat ASC`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 2 {
		t.Fatalf("expected 2 group rows, got %d", len(rows))
	}

	// Both groups have sum=10; secondary key cat ASC: X before Y.
	mustInt(t, "tie row0 total", rows[0]["total"], 10)
	mustInt(t, "tie row1 total", rows[1]["total"], 10)

	cat0, ok0 := rows[0]["cat"].(expr.StringValue)
	cat1, ok1 := rows[1]["cat"].(expr.StringValue)
	if !ok0 || !ok1 {
		t.Fatalf("cat types: (%T, %T), both want StringValue", rows[0]["cat"], rows[1]["cat"])
	}
	if string(cat0) != "X" || string(cat1) != "Y" {
		t.Errorf("tie-break order: got [%s, %s], want [X, Y]", cat0, cat1)
	}
}

// TestOrderTieAggregation_SingleGroup verifies that aggregation with ORDER BY
// on a single group returns exactly one row.
func TestOrderTieAggregation_SingleGroup(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	for _, q := range []string{
		`CREATE (:Solo {score: 10})`,
		`CREATE (:Solo {score: 20})`,
		`CREATE (:Solo {score: 30})`,
	} {
		runSetup(t, eng, q)
	}

	const q = `MATCH (n:Solo) RETURN sum(n.score) AS total ORDER BY total ASC`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	mustInt(t, "solo total", rows[0]["total"], 60)
}
