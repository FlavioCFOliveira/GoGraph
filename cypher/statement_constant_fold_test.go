package cypher_test

// statement_constant_fold_test.go — engine-level semantics of the per-statement
// call-site memo of rmp #2891.
//
// Folding must be invisible: a statement-constant call such as date($ref)
// yields the value it always yielded, a call over a row variable is still
// evaluated per row, a non-deterministic function is never folded, and a
// memoised value never outlives its statement — the next execution of the same
// cached plan with different parameters sees its own value.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func foldEngine() *cypher.Engine {
	return cypher.NewEngineWithOptions(lpg.New[string, float64](adjlist.Config{Directed: true}), cypher.EngineOptions{})
}

// foldColumn runs q and returns column col of every row.
func foldColumn(t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value, col string) []expr.Value {
	t.Helper()
	res, err := eng.Run(context.Background(), q, params)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var out []expr.Value
	for res.Next() {
		v, ok := res.Record()[col].(expr.Value)
		if !ok {
			t.Fatalf("%s: column %s holds %T, want an expr.Value", q, col, res.Record()[col])
		}
		out = append(out, v)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("%s: close: %v", q, err)
	}
	return out
}

func TestStatementConstantFold_Semantics(t *testing.T) {
	eng := foldEngine()
	d := func(y, m, day int) expr.Value { return expr.DateValue{Year: y, Month: m, Day: day} }

	t.Run("parameter call is constant and per statement", func(t *testing.T) {
		const q = "UNWIND range(1, 5) AS i RETURN date($ref) AS d"
		for _, tc := range []struct {
			ref  string
			want expr.Value
		}{{"2025-01-01", d(2025, 1, 1)}, {"2026-03-04", d(2026, 3, 4)}} {
			got := foldColumn(t, eng, q, map[string]expr.Value{"ref": expr.StringValue(tc.ref)}, "d")
			if len(got) != 5 {
				t.Fatalf("ref %s: %d rows, want 5", tc.ref, len(got))
			}
			for i, v := range got {
				if v != tc.want {
					t.Errorf("ref %s row %d: %v, want %v", tc.ref, i, v, tc.want)
				}
			}
		}
	})

	t.Run("variable argument is evaluated per row", func(t *testing.T) {
		got := foldColumn(t, eng, "UNWIND ['2025-01-01', '2025-02-01'] AS s RETURN date(s) AS d", nil, "d")
		if len(got) != 2 || got[0] != d(2025, 1, 1) || got[1] != d(2025, 2, 1) {
			t.Errorf("date(s) per row = %v, want [2025-01-01 2025-02-01]", got)
		}
	})

	t.Run("rand is never folded", func(t *testing.T) {
		got := foldColumn(t, eng, "UNWIND range(1, 200) AS i WITH rand() AS r RETURN count(DISTINCT r) AS c", nil, "c")
		if len(got) != 1 {
			t.Fatalf("%d rows, want 1", len(got))
		}
		if c, ok := got[0].(expr.IntegerValue); !ok || c < 2 {
			t.Errorf("count(DISTINCT rand()) over 200 rows = %v, want > 1", got[0])
		}
	})

	t.Run("zero-argument form agrees within the statement", func(t *testing.T) {
		got := foldColumn(t, eng, "UNWIND range(1, 50) AS i WITH date() AS d RETURN count(DISTINCT d) AS c", nil, "c")
		if len(got) != 1 || got[0] != expr.IntegerValue(1) {
			t.Errorf("count(DISTINCT date()) = %v, want [1]", got)
		}
	})
}
