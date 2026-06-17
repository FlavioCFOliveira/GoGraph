package cypher_test

// procs_engine_test.go — integration tests for CALL procedure queries (task-301).
//
// These tests drive CALL statements end-to-end through Engine.Run to verify
// that the procedure registry, ProcedureCallOp, and buildPlanEngine are wired
// correctly.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// newProcTestGraph creates a new directed graph for procedure tests.
func newProcTestGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{Directed: true})
}

// collectProc drains a Result and returns all rows as []map[string]string for
// easy assertion. expr.StringValue columns are unquoted; other values use
// their raw string representation.
func collectProc(t *testing.T, res *cypher.Result) []map[string]string {
	t.Helper()
	defer func() {
		if err := res.Close(); err != nil {
			t.Errorf("result.Close: %v", err)
		}
	}()
	cols := res.Columns()
	var rows []map[string]string
	for res.Next() {
		rec := res.Record()
		row := make(map[string]string, len(cols))
		for _, col := range cols {
			v, ok := rec[col]
			if !ok {
				continue
			}
			if sv, isSV := v.(expr.StringValue); isSV {
				row[col] = string(sv)
			}
		}
		rows = append(rows, row)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result iteration error: %v", err)
	}
	return rows
}

// ─────────────────────────────────────────────────────────────────────────────
// db.indexes()
// ─────────────────────────────────────────────────────────────────────────────

func TestProcsEngine_DbIndexes_Empty(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), `CALL db.indexes() YIELD name, type`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectProc(t, res)
	// No indexes registered yet; expect zero rows.
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty graph, got %d", len(rows))
	}
}

func TestProcsEngine_DbIndexes_AfterCreateIndex(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Create an index.
	_, err := eng.Run(ctx, `CREATE INDEX FOR (n:Person) ON (n.name)`, nil)
	if err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}

	res, err := eng.Run(ctx, `CALL db.indexes() YIELD name, type`, nil)
	if err != nil {
		t.Fatalf("CALL db.indexes(): %v", err)
	}
	rows := collectProc(t, res)
	if len(rows) == 0 {
		t.Error("expected at least 1 index row after CREATE INDEX, got 0")
	}
	// Verify the columns are present.
	for i, row := range rows {
		if _, ok := row["name"]; !ok {
			t.Errorf("row[%d] missing 'name' column", i)
		}
		if _, ok := row["type"]; !ok {
			t.Errorf("row[%d] missing 'type' column", i)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.labels()
// ─────────────────────────────────────────────────────────────────────────────

func TestProcsEngine_DbLabels_Empty(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), `CALL db.labels() YIELD label`, nil)
	if err != nil {
		t.Fatalf("Run CALL db.labels(): %v", err)
	}
	rows := collectProc(t, res)
	// No nodes, so no labels are in use; expect zero rows.
	if len(rows) != 0 {
		t.Errorf("expected 0 label rows on empty graph, got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Unknown procedure
// ─────────────────────────────────────────────────────────────────────────────

func TestProcsEngine_UnknownProcedure(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	_, err := eng.Run(context.Background(), `CALL custom.myProc() YIELD result`, nil)
	if err == nil {
		t.Fatal("expected error for unknown procedure, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.constraints()
// ─────────────────────────────────────────────────────────────────────────────

func TestProcsEngine_DbConstraints_Empty(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`CALL db.constraints() YIELD name, type, label, property`, nil)
	if err != nil {
		t.Fatalf("CALL db.constraints(): %v", err)
	}
	rows := collectProc(t, res)
	if len(rows) != 0 {
		t.Errorf("expected 0 constraint rows on empty graph, got %d", len(rows))
	}
}
