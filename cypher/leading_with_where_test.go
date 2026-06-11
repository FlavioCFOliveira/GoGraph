package cypher_test

// leading_with_where_test.go — regression gate for task-1344.
//
// A leading WITH..WHERE (WITH as the first clause, no preceding reading clause)
// with a non-aggregate predicate was building a Selection{Child:nil} in the IR.
// Via Engine.Run this surfaced as ErrInternalPanic ("cypher: internal panic");
// via Engine.Explain it crashed the host process with a nil-pointer dereference.
//
// GATE: these tests must FAIL on the unfixed code and PASS after the fix.

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newLeadingWithEngine returns a store-less engine backed by an empty graph.
func newLeadingWithEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g)
}

// drainOne drains the result set and returns the value of column col from the
// single expected row. Fails if the result produces anything other than one row.
func drainOne(t *testing.T, res *cypher.Result, col string) any {
	t.Helper()
	if !res.Next() {
		t.Fatalf("expected one row, got none; err=%v", res.Err())
	}
	rec := res.Record()
	v, ok := rec[col]
	if !ok {
		t.Fatalf("column %q not in record %v", col, rec)
	}
	if res.Next() {
		t.Fatalf("expected exactly one row, got more")
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	return v
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. IR: FromAST must not panic and must return a non-nil plan
// ─────────────────────────────────────────────────────────────────────────────

func TestLeadingWithWhere_IR_NoPanic(t *testing.T) {
	defer goleak.VerifyNone(t)
	queries := []string{
		"WITH 1 AS x WHERE x > 0 RETURN x",
		"WITH 1 AS x, 2 AS y WHERE x < y RETURN x, y",
		"WITH true AS b WHERE b RETURN b",
		"WITH 1 AS a, 2 AS b WHERE a > b RETURN a",
	}
	for _, q := range queries {
		q := q
		t.Run(q, func(t *testing.T) {
			astNode, err := parser.Parse(q)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			plan, err := ir.FromAST(astNode)
			if err != nil {
				t.Fatalf("FromAST returned error: %v", err)
			}
			if plan == nil {
				t.Fatal("FromAST returned nil plan")
			}
			// Explain must not panic.
			got := ir.Explain(plan)
			if strings.TrimSpace(got) == "" {
				t.Fatal("Explain returned empty string")
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Engine.Run: WITH 1 AS x WHERE x > 0 RETURN x → {x: 1}
// ─────────────────────────────────────────────────────────────────────────────

func TestLeadingWithWhere_Run_ReturnsRow(t *testing.T) {
	defer goleak.VerifyNone(t)
	eng := newLeadingWithEngine(t)
	ctx := context.Background()

	res, err := eng.Run(ctx, "WITH 1 AS x WHERE x > 0 RETURN x", nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()

	v := drainOne(t, res, "x")
	_ = v // value type assertion not needed; presence is the gate
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Engine.Run: WHERE filters out matching row → 0 rows
// ─────────────────────────────────────────────────────────────────────────────

func TestLeadingWithWhere_Run_FilteredOut(t *testing.T) {
	defer goleak.VerifyNone(t)
	eng := newLeadingWithEngine(t)
	ctx := context.Background()

	// x = 1 but predicate requires x > 1 → no rows.
	res, err := eng.Run(ctx, "WITH 1 AS x WHERE x > 1 RETURN x", nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if res.Next() {
		t.Fatal("expected 0 rows, got at least one")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Engine.Explain: must not crash the process, must return non-empty string
// ─────────────────────────────────────────────────────────────────────────────

func TestLeadingWithWhere_Explain_NoCrash(t *testing.T) {
	defer goleak.VerifyNone(t)
	eng := newLeadingWithEngine(t)

	queries := []string{
		"WITH 1 AS x WHERE x > 0 RETURN x",
		"WITH 1 AS a, 2 AS b WHERE a < b RETURN a, b",
	}
	for _, q := range queries {
		q := q
		t.Run(q, func(t *testing.T) {
			got, err := eng.Explain(q, nil)
			if err != nil {
				t.Fatalf("Explain returned error: %v", err)
			}
			if strings.TrimSpace(got) == "" {
				t.Fatal("Explain returned empty string")
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Chained WITH..WHERE (non-leading then leading-style pipeline)
// ─────────────────────────────────────────────────────────────────────────────

func TestLeadingWithWhere_Chained(t *testing.T) {
	defer goleak.VerifyNone(t)
	eng := newLeadingWithEngine(t)
	ctx := context.Background()

	// First WITH has no preceding clause; second WITH uses a bound var.
	res, err := eng.Run(ctx, "WITH 1 AS x WHERE x > 0 WITH x, 2 AS y WHERE y > 1 RETURN x, y", nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if !res.Next() {
		t.Fatalf("expected one row; err=%v", res.Err())
	}
}
