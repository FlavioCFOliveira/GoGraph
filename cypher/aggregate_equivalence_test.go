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
//
// Also gates the 2026-07-02 production-readiness audit finding F2:
// DISTINCT/grouping/UNION dedup by [expr.EquivalentHash], which used to
// disagree with [expr.Equivalent] for a cross-type Integer/Float pair (see
// cypher/expr/equiv_test.go for the unit-level fix), causing an over-count
// whenever the same logical number appeared as both an INTEGER and a FLOAT.

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

// F2 — count(DISTINCT …) must collapse an INTEGER and a numerically-equal
// FLOAT into one equivalence class, exactly as it already does for NaN/null.
func TestAggDistinct_IntFloat_Equivalence(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `UNWIND [1, 1.0, 2] AS x RETURN count(DISTINCT x) AS c`)
	got, ok := row["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("c: want IntegerValue, got %T (%v)", row["c"], row["c"])
	}
	if int64(got) != 2 {
		t.Errorf("count(DISTINCT 1, 1.0, 2) = %d, want 2 (1 and 1.0 are equivalent)", int64(got))
	}
}

// F2 — collect(DISTINCT …) over an Integer/Float pair keeps a single element,
// not a duplicate.
func TestAggDistinct_IntFloat_Collect(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `UNWIND [1, 1.0] AS x RETURN collect(DISTINCT x) AS c`)
	lst, ok := row["c"].(expr.ListValue)
	if !ok {
		t.Fatalf("c: want ListValue, got %T (%v)", row["c"], row["c"])
	}
	if len(lst) != 1 {
		t.Errorf("collect(DISTINCT 1, 1.0) = %v (len %d), want a single-element list", lst, len(lst))
	}
}

// F2 — grouping by an expression must merge rows whose group key is an
// Integer in one row and the equal Float in another into one group.
func TestAggGroupBy_IntFloat_Equivalence(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	res, err := eng.Run(context.Background(), `UNWIND [1, 1.0, 2] AS x RETURN x, count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 2 {
		t.Fatalf("grouping [1, 1.0, 2] by x produced %d groups, want 2 (1 and 1.0 merge)", len(rows))
	}
	var total int64
	for _, r := range rows {
		total += int64(r["c"].(expr.IntegerValue))
	}
	if total != 3 {
		t.Errorf("group counts sum to %d, want 3 (one per input row)", total)
	}
}

// F2 — UNION deduplicates by equivalence, so RETURN 1 UNION RETURN 1.0 must
// yield a single row.
func TestUnion_IntFloat_Equivalence(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	res, err := eng.Run(context.Background(), `RETURN 1 AS x UNION RETURN 1.0 AS x`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Errorf("(RETURN 1) UNION (RETURN 1.0) produced %d rows, want 1 (1 and 1.0 are equivalent)", len(rows))
	}
}

// #1868 (F2 follow-up) — count(DISTINCT …) must collapse a NodeValue and an
// IntegerValue carrying its raw ID into one equivalence class, exactly as
// F2 fixed for Integer/Float. This is the audit's exact repro: `n` (a
// NodeValue after WITH re-binds it) and `id(n)` (an IntegerValue) refer to
// the same node, so NodeValue.Equal already treats them as equal — the bug
// was EquivalentHash disagreeing and hiding that equality from DISTINCT.
func TestAggDistinct_NodeInteger_Equivalence(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	drainRunInTx(t, eng, `CREATE (:P {k: 'a'})`)
	row := singleRow(t, eng, `MATCH (n:P {k: 'a'}) WITH n, id(n) AS nid UNWIND [n, nid] AS x RETURN count(DISTINCT x) AS c`)
	got, ok := row["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("c: want IntegerValue, got %T (%v)", row["c"], row["c"])
	}
	if int64(got) != 1 {
		t.Errorf("count(DISTINCT n, id(n)) = %d, want 1 (n and id(n) refer to the same node)", int64(got))
	}
}

// #1868 (F2 follow-up) — negative control: a node and an unrelated integer
// must NOT collapse.
func TestAggDistinct_NodeInteger_NotCollapsedForDistinctValues(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	drainRunInTx(t, eng, `CREATE (:P {k: 'a'})`)
	row := singleRow(t, eng, `MATCH (n:P {k: 'a'}) WITH n, id(n) AS nid UNWIND [n, nid + 1] AS x RETURN count(DISTINCT x) AS c`)
	got := row["c"].(expr.IntegerValue)
	if int64(got) != 2 {
		t.Errorf("count(DISTINCT n, id(n)+1) = %d, want 2 (distinct node and integer)", int64(got))
	}
}
