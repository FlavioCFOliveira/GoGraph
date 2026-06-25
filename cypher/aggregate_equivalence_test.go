package cypher_test

// aggregate_equivalence_test.go — regression gates for the 2026-06-25
// reliability audit:
//
//   #1757  count(DISTINCT …)/collect(DISTINCT …) must dedup by EQUIVALENCE
//          (NaN ≡ NaN, nested nulls collapse), not comparability.
//   #1759  sum() over an empty / all-NULL input must return integer 0, not NULL.
//
// These drive the real aggregate path through Engine.Run (the bug lived in the
// distinctAggregator wrapper and SumAgg.Result, not in the standalone DISTINCT
// operator that the cypher/exec unit tests exercise).

import (
	"context"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newAggEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

func singleRow(t *testing.T, eng *cypher.Engine, q string) map[string]interface{} {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("Run(%q): got %d rows, want 1", q, len(rows))
	}
	return rows[0]
}

// #1757 — count(DISTINCT NaN, NaN) must be 1 (NaN ≡ NaN under equivalence).
func TestAggDistinct_NaN_Equivalence(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `UNWIND [0.0/0.0, 0.0/0.0] AS x RETURN count(DISTINCT x) AS c`)
	got, ok := row["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("c: want IntegerValue, got %T (%v)", row["c"], row["c"])
	}
	if int64(got) != 1 {
		t.Errorf("count(DISTINCT NaN, NaN) = %d, want 1 (equivalence: NaN ≡ NaN)", int64(got))
	}
}

// #1757 — collect(DISTINCT NaN, NaN) must be a single-element list [NaN].
func TestAggDistinct_NaN_Collect(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `UNWIND [0.0/0.0, 0.0/0.0] AS x RETURN collect(DISTINCT x) AS c`)
	lst, ok := row["c"].(expr.ListValue)
	if !ok {
		t.Fatalf("c: want ListValue, got %T (%v)", row["c"], row["c"])
	}
	if len(lst) != 1 {
		t.Fatalf("collect(DISTINCT NaN, NaN) = %v (len %d), want a single-element [NaN]", lst, len(lst))
	}
	fv, ok := lst[0].(expr.FloatValue)
	if !ok || !math.IsNaN(float64(fv)) {
		t.Errorf("collect(DISTINCT NaN, NaN)[0] = %v, want NaN", lst[0])
	}
}

// #1757 — count(DISTINCT [1,null], [1,null]) must be 1 (nested nulls collapse).
func TestAggDistinct_NestedNull_Equivalence(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `UNWIND [[1,null],[1,null]] AS x RETURN count(DISTINCT x) AS c`)
	got, ok := row["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("c: want IntegerValue, got %T (%v)", row["c"], row["c"])
	}
	if int64(got) != 1 {
		t.Errorf("count(DISTINCT [1,null],[1,null]) = %d, want 1 (nested-null equivalence)", int64(got))
	}
}

// #1757 — negative control: genuinely distinct values are NOT collapsed.
func TestAggDistinct_DistinctValues_NotCollapsed(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `UNWIND [1, 2, 2, 3] AS x RETURN count(DISTINCT x) AS c`)
	got := row["c"].(expr.IntegerValue)
	if int64(got) != 3 {
		t.Errorf("count(DISTINCT 1,2,2,3) = %d, want 3", int64(got))
	}
}

// #1759 — sum over an all-NULL input is integer 0, not NULL.
func TestAggSum_AllNull_IsZero(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `UNWIND [null, null] AS x RETURN sum(x) AS s`)
	got, ok := row["s"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("sum(all-null) = %T (%v), want IntegerValue(0)", row["s"], row["s"])
	}
	if int64(got) != 0 {
		t.Errorf("sum(all-null) = %d, want 0", int64(got))
	}
}

// #1759 — sum over an empty MATCH (zero input rows) is integer 0.
func TestAggSum_EmptyInput_IsZero(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `MATCH (n:Nonexist) RETURN sum(n.x) AS s`)
	got, ok := row["s"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("sum(empty) = %T (%v), want IntegerValue(0)", row["s"], row["s"])
	}
	if int64(got) != 0 {
		t.Errorf("sum(empty) = %d, want 0", int64(got))
	}
}

// #1759 — controls: sum still skips nulls; avg/collect/count keep their own
// empty-input contracts (avg → NULL, collect → [], count → 0).
func TestAggSum_Controls(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)

	if s := singleRow(t, eng, `UNWIND [1, null, 2] AS x RETURN sum(x) AS s`)["s"]; int64(s.(expr.IntegerValue)) != 3 {
		t.Errorf("sum(1,null,2) = %v, want 3", s)
	}
	if a := singleRow(t, eng, `MATCH (n:Nonexist) RETURN avg(n.x) AS a`)["a"]; !expr.IsNull(a.(expr.Value)) {
		t.Errorf("avg(empty) = %v, want NULL", a)
	}
	if c := singleRow(t, eng, `MATCH (n:Nonexist) RETURN collect(n.x) AS c`)["c"]; len(c.(expr.ListValue)) != 0 {
		t.Errorf("collect(empty) = %v, want []", c)
	}
	if c := singleRow(t, eng, `MATCH (n:Nonexist) RETURN count(n.x) AS c`)["c"]; int64(c.(expr.IntegerValue)) != 0 {
		t.Errorf("count(empty) = %v, want 0", c)
	}
}
