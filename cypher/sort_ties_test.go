package cypher_test

// sort_ties_test.go — T682
//
// Integration tests for multi-key ORDER BY with tied primary key values.
// Verifies that the secondary sort key correctly breaks ties and that the
// full ordering is deterministic.
//
// Graph:
//   - "alice"   age=30, score=90
//   - "bob"     age=30, score=80
//   - "charlie" age=25, score=95
//
// ORDER BY n.age ASC, n.score DESC expected sequence:
//   1. charlie (25, 95) — lowest age
//   2. alice   (30, 90) — tied age=30, higher score
//   3. bob     (30, 80) — tied age=30, lower score

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newTiesGraph creates the three-node graph described in the file header.
func newTiesGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	nodes := []struct {
		name  string
		age   int
		score int
	}{
		{"alice", 30, 90},
		{"bob", 30, 80},
		{"charlie", 25, 95},
	}
	for _, n := range nodes {
		q := fmt.Sprintf(
			`CREATE (x {name: '%s', age: %d, score: %d})`,
			n.name, n.age, n.score,
		)
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("newTiesGraph CREATE %s: %v", n.name, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("newTiesGraph drain %s: %v", n.name, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("newTiesGraph close %s: %v", n.name, err)
		}
	}
	return g
}

// tiesRow holds the three projected columns for a single result row.
type tiesRow struct {
	name  string
	age   int64
	score int64
}

// collectTiesRows drains the result and returns a slice of tiesRow values in
// iteration order.
func collectTiesRows(t *testing.T, res *cypher.Result) []tiesRow {
	t.Helper()
	defer res.Close()
	var out []tiesRow
	for res.Next() {
		rec := res.Record()
		row := tiesRow{}

		sv, ok := rec["n.name"].(expr.StringValue)
		if !ok {
			t.Errorf("n.name: expected StringValue, got %T (%v)", rec["n.name"], rec["n.name"])
		} else {
			row.name = string(sv)
		}

		av, ok := rec["n.age"].(expr.IntegerValue)
		if !ok {
			t.Errorf("n.age: expected IntegerValue, got %T (%v)", rec["n.age"], rec["n.age"])
		} else {
			row.age = int64(av)
		}

		sc, ok := rec["n.score"].(expr.IntegerValue)
		if !ok {
			t.Errorf("n.score: expected IntegerValue, got %T (%v)", rec["n.score"], rec["n.score"])
		} else {
			row.score = int64(sc)
		}

		out = append(out, row)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("collectTiesRows: %v", err)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Multi-key ORDER BY: primary ASC, secondary DESC
// ─────────────────────────────────────────────────────────────────────────────

// TestSortTies_AgeThenScoreDesc verifies that ORDER BY n.age ASC, n.score DESC
// sorts charlie first (lowest age), then alice before bob (tied age, but
// alice has a higher score and the secondary sort is DESC).
func TestSortTies_AgeThenScoreDesc(t *testing.T) {
	g := newTiesGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name, n.age, n.score ORDER BY n.age ASC, n.score DESC`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectTiesRows(t, res)

	want := []tiesRow{
		{"charlie", 25, 95},
		{"alice", 30, 90},
		{"bob", 30, 80},
	}
	if len(rows) != len(want) {
		t.Fatalf("expected %d rows, got %d: %v", len(want), len(rows), rows)
	}
	for i, w := range want {
		got := rows[i]
		if got.name != w.name || got.age != w.age || got.score != w.score {
			t.Errorf("row[%d] = %+v, want %+v", i, got, w)
		}
	}
}

// TestSortTies_AgeThenScoreAsc verifies the complementary ordering:
// ORDER BY n.age ASC, n.score ASC — both keys ascending.
// charlie (25,95), then bob (30,80) before alice (30,90).
func TestSortTies_AgeThenScoreAsc(t *testing.T) {
	g := newTiesGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name, n.age, n.score ORDER BY n.age ASC, n.score ASC`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectTiesRows(t, res)

	want := []tiesRow{
		{"charlie", 25, 95},
		{"bob", 30, 80},
		{"alice", 30, 90},
	}
	if len(rows) != len(want) {
		t.Fatalf("expected %d rows, got %d: %v", len(want), len(rows), rows)
	}
	for i, w := range want {
		got := rows[i]
		if got.name != w.name || got.age != w.age || got.score != w.score {
			t.Errorf("row[%d] = %+v, want %+v", i, got, w)
		}
	}
}
