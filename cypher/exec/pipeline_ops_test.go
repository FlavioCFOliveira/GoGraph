package exec_test

// pipeline_ops_test.go — tests for tasks 251–256:
//   - Task 251: EagerAggregation operator
//   - Task 253: Sort operator
//   - Task 254: Top operator
//   - Task 255: Distinct operator
//   - Task 256: Union / UnionAll operators

import (
	"context"
	"errors"
	"testing"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/cypher/funcs"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task 251 — EagerAggregation
// ─────────────────────────────────────────────────────────────────────────────

// makeRows builds rows from a flat list of (key, value) pairs.
func makeAggRows(pairs [][2]int64) []exec.Row {
	rows := make([]exec.Row, len(pairs))
	for i, p := range pairs {
		rows[i] = exec.Row{expr.IntegerValue(p[0]), expr.IntegerValue(p[1])}
	}
	return rows
}

func TestEagerAggregation_GroupBy1Col(t *testing.T) {
	// GROUP BY col0, SUM(col1).
	// Input: (1,10), (2,20), (1,30), (2,5)
	// Expected: group 1 → sum=40, group 2 → sum=25.
	rows := makeAggRows([][2]int64{{1, 10}, {2, 20}, {1, 30}, {2, 5}})
	op, err := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		[]int{0}, // key = col0
		[]funcs.AggregatorFactory{funcs.NewSumAgg()},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("got %d groups, want 2", len(result))
	}
	// Build map key → sum for assertion.
	groupSum := make(map[int64]int64)
	for _, row := range result {
		k := int64(row[0].(expr.IntegerValue))
		v := int64(row[1].(expr.IntegerValue))
		groupSum[k] = v
	}
	if groupSum[1] != 40 {
		t.Errorf("group 1: sum = %d, want 40", groupSum[1])
	}
	if groupSum[2] != 25 {
		t.Errorf("group 2: sum = %d, want 25", groupSum[2])
	}
}

func TestEagerAggregation_GroupBy2Cols(t *testing.T) {
	// GROUP BY (col0, col1), COUNT(*).
	// Input: (A,1),(A,2),(A,1),(B,1)
	// Groups: (A,1)→2, (A,2)→1, (B,1)→1.
	rows := []exec.Row{
		{expr.StringValue("A"), expr.IntegerValue(1), expr.IntegerValue(0)},
		{expr.StringValue("A"), expr.IntegerValue(2), expr.IntegerValue(0)},
		{expr.StringValue("A"), expr.IntegerValue(1), expr.IntegerValue(0)},
		{expr.StringValue("B"), expr.IntegerValue(1), expr.IntegerValue(0)},
	}
	op, err := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		[]int{0, 1},
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("got %d groups, want 3", len(result))
	}
	type gk struct {
		s string
		n int64
	}
	counts := make(map[gk]int64)
	for _, row := range result {
		k := gk{string(row[0].(expr.StringValue)), int64(row[1].(expr.IntegerValue))}
		counts[k] = int64(row[2].(expr.IntegerValue))
	}
	if counts[gk{"A", 1}] != 2 {
		t.Errorf("(A,1)=%d, want 2", counts[gk{"A", 1}])
	}
	if counts[gk{"A", 2}] != 1 {
		t.Errorf("(A,2)=%d, want 1", counts[gk{"A", 2}])
	}
	if counts[gk{"B", 1}] != 1 {
		t.Errorf("(B,1)=%d, want 1", counts[gk{"B", 1}])
	}
}

func TestEagerAggregation_GlobalAggregate(t *testing.T) {
	// No GROUP BY (empty keyCols) → single global aggregate.
	// SUM of [1,2,3,4,5] = 15.
	rows := []exec.Row{
		{expr.IntegerValue(1)},
		{expr.IntegerValue(2)},
		{expr.IntegerValue(3)},
		{expr.IntegerValue(4)},
		{expr.IntegerValue(5)},
	}
	op, err := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		nil, // no group keys
		[]funcs.AggregatorFactory{funcs.NewSumAgg()},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d rows, want 1", len(result))
	}
	if result[0][0] != expr.IntegerValue(15) {
		t.Errorf("sum = %v, want 15", result[0][0])
	}
}

func TestEagerAggregation_Count(t *testing.T) {
	rows := makeIntRows(7)
	op, err := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		nil,
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if result[0][0] != expr.IntegerValue(7) {
		t.Errorf("count(*) = %v, want 7", result[0][0])
	}
}

func TestEagerAggregation_AvgMinMax(t *testing.T) {
	// Each row has 3 value columns (no group keys).
	// The convention: col[len(keyCols)+i] feeds aggregator i.
	// With keyCols=nil: agg0=col0 (avg), agg1=col1 (min), agg2=col2 (max).
	// All three columns carry the same value so avg=min=max per row;
	// overall avg=20, min=10, max=30.
	rows := []exec.Row{
		{expr.IntegerValue(10), expr.IntegerValue(10), expr.IntegerValue(10)},
		{expr.IntegerValue(20), expr.IntegerValue(20), expr.IntegerValue(20)},
		{expr.IntegerValue(30), expr.IntegerValue(30), expr.IntegerValue(30)},
	}
	op, err := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		nil,
		[]funcs.AggregatorFactory{
			funcs.NewAvgAgg(),
			funcs.NewMinAgg(),
			funcs.NewMaxAgg(),
		},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d rows, want 1", len(result))
	}
	row := result[0]
	if _, ok := row[0].(expr.FloatValue); !ok {
		t.Errorf("avg: got %T, want FloatValue", row[0])
	}
	if row[1] != expr.IntegerValue(10) {
		t.Errorf("min = %v, want 10", row[1])
	}
	if row[2] != expr.IntegerValue(30) {
		t.Errorf("max = %v, want 30", row[2])
	}
}

func TestEagerAggregation_NullGroupKey(t *testing.T) {
	// NULLs should group together.
	rows := []exec.Row{
		{expr.Null, expr.IntegerValue(1)},
		{expr.Null, expr.IntegerValue(2)},
		{expr.IntegerValue(1), expr.IntegerValue(3)},
	}
	op, err := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		[]int{0},
		[]funcs.AggregatorFactory{funcs.NewSumAgg()},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// 2 groups: NULL→3, 1→3.
	if len(result) != 2 {
		t.Fatalf("got %d groups, want 2", len(result))
	}
}

func TestEagerAggregation_MemoryCapEnforced(t *testing.T) {
	// maxGroups=2 but 3 distinct groups → should error.
	rows := []exec.Row{
		{expr.IntegerValue(1), expr.IntegerValue(0)},
		{expr.IntegerValue(2), expr.IntegerValue(0)},
		{expr.IntegerValue(3), expr.IntegerValue(0)}, // triggers cap
	}
	op, err := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		[]int{0},
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
		2,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	_, err = exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrAggMemoryExceeded) {
		t.Fatalf("want ErrAggMemoryExceeded, got %v", err)
	}
}

func TestEagerAggregation_EmptyInput(t *testing.T) {
	op, err := exec.NewEagerAggregation(
		newSliceOperator(),
		[]int{0},
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("got %d rows, want 0", len(result))
	}
}

func TestEagerAggregation_Collect(t *testing.T) {
	// GROUP BY col0, COLLECT(col1).
	rows := []exec.Row{
		{expr.StringValue("A"), expr.IntegerValue(1)},
		{expr.StringValue("A"), expr.IntegerValue(2)},
		{expr.StringValue("B"), expr.IntegerValue(3)},
	}
	op, err := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		[]int{0},
		[]funcs.AggregatorFactory{funcs.NewCollectAgg()},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("got %d groups, want 2", len(result))
	}
	// Build map for assertion.
	collected := make(map[string]int)
	for _, row := range result {
		k := string(row[0].(expr.StringValue))
		lv := row[1].(expr.ListValue) //nolint:forcetypeassert // test assertion
		collected[k] = len(lv)
	}
	if collected["A"] != 2 {
		t.Errorf("A collected %d items, want 2", collected["A"])
	}
	if collected["B"] != 1 {
		t.Errorf("B collected %d items, want 1", collected["B"])
	}
}

func TestEagerAggregation_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows := makeIntRows(5)
	op, _ := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		nil,
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
		0,
	)
	_, err := exec.Drain(ctx, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestEagerAggregation_GroupBy3Cols(t *testing.T) {
	// GROUP BY (a,b,c) — verify multi-col key correctness.
	rows := []exec.Row{
		{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3), expr.IntegerValue(10)},
		{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3), expr.IntegerValue(20)},
		{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(4), expr.IntegerValue(5)},
	}
	op, _ := exec.NewEagerAggregation(
		newSliceOperator(rows...),
		[]int{0, 1, 2},
		[]funcs.AggregatorFactory{funcs.NewSumAgg()},
		0,
	)
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("got %d groups, want 2", len(result))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 253 — Sort
// ─────────────────────────────────────────────────────────────────────────────

func TestSort_SingleKeyAsc(t *testing.T) {
	rows := []exec.Row{
		{expr.IntegerValue(3)},
		{expr.IntegerValue(1)},
		{expr.IntegerValue(2)},
	}
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	want := []int64{1, 2, 3}
	for i, row := range result {
		got := int64(row[0].(expr.IntegerValue))
		if got != want[i] {
			t.Errorf("result[%d] = %d, want %d", i, got, want[i])
		}
	}
}

func TestSort_SingleKeyDesc(t *testing.T) {
	rows := []exec.Row{
		{expr.IntegerValue(1)},
		{expr.IntegerValue(3)},
		{expr.IntegerValue(2)},
	}
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: false}}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	want := []int64{3, 2, 1}
	for i, row := range result {
		if int64(row[0].(expr.IntegerValue)) != want[i] {
			t.Errorf("result[%d] = %v, want %d", i, row[0], want[i])
		}
	}
}

func TestSort_MultiKey(t *testing.T) {
	// ORDER BY col0 ASC, col1 DESC.
	rows := []exec.Row{
		{expr.IntegerValue(1), expr.IntegerValue(2)},
		{expr.IntegerValue(1), expr.IntegerValue(3)},
		{expr.IntegerValue(2), expr.IntegerValue(1)},
		{expr.IntegerValue(1), expr.IntegerValue(1)},
	}
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{
			{ColIdx: 0, Ascending: true},
			{ColIdx: 1, Ascending: false},
		}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Expected: (1,3),(1,2),(1,1),(2,1)
	expected := [][2]int64{{1, 3}, {1, 2}, {1, 1}, {2, 1}}
	for i, row := range result {
		a := int64(row[0].(expr.IntegerValue))
		b := int64(row[1].(expr.IntegerValue))
		if a != expected[i][0] || b != expected[i][1] {
			t.Errorf("result[%d] = (%d,%d), want (%d,%d)", i, a, b, expected[i][0], expected[i][1])
		}
	}
}

func TestSort_NullLastInAsc(t *testing.T) {
	// NULL sorts last in ASC.
	rows := []exec.Row{
		{expr.IntegerValue(3)},
		{expr.Null},
		{expr.IntegerValue(1)},
	}
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("got %d rows, want 3", len(result))
	}
	// NULL must be last.
	if !expr.IsNull(result[2][0]) {
		t.Errorf("last row[0] = %v, want NULL", result[2][0])
	}
}

func TestSort_NullFirstInDesc(t *testing.T) {
	// NULL sorts first in DESC.
	rows := []exec.Row{
		{expr.IntegerValue(3)},
		{expr.Null},
		{expr.IntegerValue(1)},
	}
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: false}}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !expr.IsNull(result[0][0]) {
		t.Errorf("first row[0] = %v, want NULL", result[0][0])
	}
}

func TestSort_EmptyInput(t *testing.T) {
	op, err := exec.NewSort(newSliceOperator(),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("got %d rows, want 0", len(result))
	}
}

func TestSort_AllSameKey(t *testing.T) {
	// All rows have the same key — stable order should be preserved.
	rows := []exec.Row{
		{expr.IntegerValue(1), expr.StringValue("a")},
		{expr.IntegerValue(1), expr.StringValue("b")},
		{expr.IntegerValue(1), expr.StringValue("c")},
	}
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("got %d rows, want 3", len(result))
	}
	// Stable: original order of equal rows preserved.
	order := []string{"a", "b", "c"}
	for i, row := range result {
		if string(row[1].(expr.StringValue)) != order[i] {
			t.Errorf("result[%d][1] = %v, want %q", i, row[1], order[i])
		}
	}
}

func TestSort_MemoryCapEnforced(t *testing.T) {
	rows := makeIntRows(5)
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 3)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	_, err = exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrSortMemoryExceeded) {
		t.Fatalf("want ErrSortMemoryExceeded, got %v", err)
	}
}

func TestSort_StringsAsc(t *testing.T) {
	rows := []exec.Row{
		{expr.StringValue("cherry")},
		{expr.StringValue("apple")},
		{expr.StringValue("banana")},
	}
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	want := []string{"apple", "banana", "cherry"}
	for i, row := range result {
		if string(row[0].(expr.StringValue)) != want[i] {
			t.Errorf("result[%d] = %v, want %q", i, row[0], want[i])
		}
	}
}

func TestSort_100Rows(t *testing.T) {
	// Build rows [99, 98, ..., 0] (reverse order) and sort ascending.
	rows := make([]exec.Row, 100)
	for i := range 100 {
		rows[i] = exec.Row{expr.IntegerValue(int64(99 - i))}
	}
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	for i, row := range result {
		if row[0] != expr.IntegerValue(int64(i)) {
			t.Errorf("result[%d] = %v, want %d", i, row[0], i)
		}
	}
}

func TestSort_MultipleNulls(t *testing.T) {
	// Multiple NULLs and non-nulls; NULLs last (ASC).
	rows := []exec.Row{
		{expr.Null},
		{expr.IntegerValue(2)},
		{expr.Null},
		{expr.IntegerValue(1)},
		{expr.Null},
	}
	op, err := exec.NewSort(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if result[0][0] != expr.IntegerValue(1) {
		t.Errorf("result[0] = %v, want 1", result[0][0])
	}
	if result[1][0] != expr.IntegerValue(2) {
		t.Errorf("result[1] = %v, want 2", result[1][0])
	}
	for _, row := range result[2:] {
		if !expr.IsNull(row[0]) {
			t.Errorf("expected NULL in last positions, got %v", row[0])
		}
	}
}

func TestSort_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op, _ := exec.NewSort(newSliceOperator(makeIntRows(10)...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 0)
	_, err := exec.Drain(ctx, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 254 — Top
// ─────────────────────────────────────────────────────────────────────────────

func TestTop_SmallN(t *testing.T) {
	// TOP 3 from [5,2,8,1,9,3] ASC → [1,2,3].
	rows := []exec.Row{
		{expr.IntegerValue(5)}, {expr.IntegerValue(2)}, {expr.IntegerValue(8)},
		{expr.IntegerValue(1)}, {expr.IntegerValue(9)}, {expr.IntegerValue(3)},
	}
	op, err := exec.NewTop(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 3)
	if err != nil {
		t.Fatalf("NewTop: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("got %d rows, want 3", len(result))
	}
	want := []int64{1, 2, 3}
	for i, row := range result {
		if row[0] != expr.IntegerValue(want[i]) {
			t.Errorf("result[%d] = %v, want %d", i, row[0], want[i])
		}
	}
}

func TestTop_NLargerThanInput(t *testing.T) {
	// N=100, input=3 rows → return all 3 rows in sorted order.
	rows := []exec.Row{
		{expr.IntegerValue(3)}, {expr.IntegerValue(1)}, {expr.IntegerValue(2)},
	}
	op, err := exec.NewTop(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 100)
	if err != nil {
		t.Fatalf("NewTop: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("got %d rows, want 3", len(result))
	}
}

func TestTop_EmptyInput(t *testing.T) {
	op, err := exec.NewTop(newSliceOperator(),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 5)
	if err != nil {
		t.Fatalf("NewTop: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("got %d rows, want 0", len(result))
	}
}

func TestTop_DESC(t *testing.T) {
	// TOP 2 DESC from [1,2,3,4,5] → [5,4].
	rows := makeIntRows(5)
	op, err := exec.NewTop(newSliceOperator(rows...),
		[]exec.SortKey{{ColIdx: 0, Ascending: false}}, 2)
	if err != nil {
		t.Fatalf("NewTop: %v", err)
	}
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("got %d rows, want 2", len(result))
	}
	if result[0][0] != expr.IntegerValue(4) || result[1][0] != expr.IntegerValue(3) {
		t.Errorf("got %v %v, want [4] [3]", result[0][0], result[1][0])
	}
}

func TestTop_MatchesSortLimit(t *testing.T) {
	// Verify Top produces the same result as Sort+Limit.
	const N = 5
	rows := make([]exec.Row, 50)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(50 - i))}
	}
	keys := []exec.SortKey{{ColIdx: 0, Ascending: true}}

	opTop, _ := exec.NewTop(newSliceOperator(rows...), keys, N)
	resultTop, err := exec.Drain(context.Background(), opTop)
	if err != nil {
		t.Fatalf("Top Drain: %v", err)
	}

	opSort, _ := exec.NewSort(newSliceOperator(rows...), keys, 0)
	opLim, _ := exec.NewLimit(opSort, int64(N))
	resultSort, err := exec.Drain(context.Background(), opLim)
	if err != nil {
		t.Fatalf("Sort+Limit Drain: %v", err)
	}

	if len(resultTop) != len(resultSort) {
		t.Fatalf("Top len=%d, Sort+Limit len=%d", len(resultTop), len(resultSort))
	}
	for i := range resultTop {
		if resultTop[i][0] != resultSort[i][0] {
			t.Errorf("row[%d]: Top=%v Sort+Limit=%v", i, resultTop[i][0], resultSort[i][0])
		}
	}
}

func TestTop_InvalidN(t *testing.T) {
	_, err := exec.NewTop(newSliceOperator(),
		[]exec.SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err == nil {
		t.Fatal("expected error for n=0, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 255 — Distinct
// ─────────────────────────────────────────────────────────────────────────────

func TestDistinct_RemovesDuplicates(t *testing.T) {
	rows := []exec.Row{
		{expr.IntegerValue(1)},
		{expr.IntegerValue(2)},
		{expr.IntegerValue(1)},
		{expr.IntegerValue(3)},
		{expr.IntegerValue(2)},
	}
	op := exec.NewDistinct(newSliceOperator(rows...), 0)
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("got %d rows, want 3", len(result))
	}
}

func TestDistinct_MultiColRow(t *testing.T) {
	rows := []exec.Row{
		{expr.IntegerValue(1), expr.StringValue("a")},
		{expr.IntegerValue(1), expr.StringValue("b")},
		{expr.IntegerValue(1), expr.StringValue("a")}, // dup of first
		{expr.IntegerValue(2), expr.StringValue("a")},
	}
	op := exec.NewDistinct(newSliceOperator(rows...), 0)
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("got %d rows, want 3", len(result))
	}
}

func TestDistinct_AllUnique(t *testing.T) {
	rows := makeIntRows(5)
	op := exec.NewDistinct(newSliceOperator(rows...), 0)
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 5 {
		t.Errorf("got %d rows, want 5", len(result))
	}
}

func TestDistinct_AllSame(t *testing.T) {
	rows := []exec.Row{
		{expr.IntegerValue(7)},
		{expr.IntegerValue(7)},
		{expr.IntegerValue(7)},
	}
	op := exec.NewDistinct(newSliceOperator(rows...), 0)
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("got %d rows, want 1", len(result))
	}
}

func TestDistinct_EmptyInput(t *testing.T) {
	op := exec.NewDistinct(newSliceOperator(), 0)
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("got %d rows, want 0", len(result))
	}
}

func TestDistinct_NullHandling(t *testing.T) {
	// Two NULL rows → should be treated as one distinct group.
	rows := []exec.Row{
		{expr.Null},
		{expr.Null},
		{expr.IntegerValue(1)},
	}
	op := exec.NewDistinct(newSliceOperator(rows...), 0)
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("got %d rows, want 2", len(result))
	}
}

func TestDistinct_MemoryCapEnforced(t *testing.T) {
	rows := makeIntRows(5)
	op := exec.NewDistinct(newSliceOperator(rows...), 3)
	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrDistinctMemoryExceeded) {
		t.Fatalf("want ErrDistinctMemoryExceeded, got %v", err)
	}
}

func TestDistinct_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op := exec.NewDistinct(newSliceOperator(makeIntRows(5)...), 0)
	_, err := exec.Drain(ctx, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 256 — Union / UnionAll
// ─────────────────────────────────────────────────────────────────────────────

func TestUnionAll_PreservesDuplicates(t *testing.T) {
	left := []exec.Row{
		{expr.IntegerValue(1)},
		{expr.IntegerValue(2)},
	}
	right := []exec.Row{
		{expr.IntegerValue(1)},
		{expr.IntegerValue(3)},
	}
	op := exec.NewUnionAll(newSliceOperator(left...), newSliceOperator(right...))
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 4 {
		t.Fatalf("got %d rows, want 4", len(result))
	}
}

func TestUnionAll_EmptyLeft(t *testing.T) {
	right := makeIntRows(3)
	op := exec.NewUnionAll(newSliceOperator(), newSliceOperator(right...))
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("got %d rows, want 3", len(result))
	}
}

func TestUnionAll_EmptyRight(t *testing.T) {
	left := makeIntRows(3)
	op := exec.NewUnionAll(newSliceOperator(left...), newSliceOperator())
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("got %d rows, want 3", len(result))
	}
}

func TestUnionAll_SchemaMismatch(t *testing.T) {
	left := []exec.Row{{expr.IntegerValue(1), expr.StringValue("a")}}
	right := []exec.Row{{expr.IntegerValue(2)}} // width 1, not 2
	op := exec.NewUnionAll(newSliceOperator(left...), newSliceOperator(right...))
	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrSchemaMismatch) {
		t.Fatalf("want ErrSchemaMismatch, got %v", err)
	}
}

func TestUnionAll_OrderPreserved(t *testing.T) {
	// Verify left rows come before right rows.
	left := []exec.Row{{expr.StringValue("L1")}, {expr.StringValue("L2")}}
	right := []exec.Row{{expr.StringValue("R1")}, {expr.StringValue("R2")}}
	op := exec.NewUnionAll(newSliceOperator(left...), newSliceOperator(right...))
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	want := []string{"L1", "L2", "R1", "R2"}
	for i, row := range result {
		if string(row[0].(expr.StringValue)) != want[i] {
			t.Errorf("result[%d] = %v, want %q", i, row[0], want[i])
		}
	}
}

func TestUnion_RemovesDuplicates(t *testing.T) {
	left := []exec.Row{
		{expr.IntegerValue(1)},
		{expr.IntegerValue(2)},
		{expr.IntegerValue(3)},
	}
	right := []exec.Row{
		{expr.IntegerValue(2)},
		{expr.IntegerValue(3)},
		{expr.IntegerValue(4)},
	}
	op := exec.NewUnion(newSliceOperator(left...), newSliceOperator(right...), 0)
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 4 {
		t.Errorf("got %d rows, want 4 (unique values 1,2,3,4)", len(result))
	}
}

func TestUnion_SchemaMismatch(t *testing.T) {
	left := []exec.Row{{expr.IntegerValue(1), expr.StringValue("x")}}
	right := []exec.Row{{expr.IntegerValue(2)}}
	op := exec.NewUnion(newSliceOperator(left...), newSliceOperator(right...), 0)
	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrSchemaMismatch) {
		t.Fatalf("want ErrSchemaMismatch, got %v", err)
	}
}

func TestUnion_EmptyBothSides(t *testing.T) {
	op := exec.NewUnion(newSliceOperator(), newSliceOperator(), 0)
	result, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("got %d rows, want 0", len(result))
	}
}

func TestUnion_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op := exec.NewUnion(
		newSliceOperator(makeIntRows(3)...),
		newSliceOperator(makeIntRows(3)...),
		0,
	)
	_, err := exec.Drain(ctx, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Benchmarks
// ─────────────────────────────────────────────────────────────────────────────

func BenchmarkSort_10k(b *testing.B) {
	const n = 10_000
	rows := make([]exec.Row, n)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(n - i))}
	}
	keys := []exec.SortKey{{ColIdx: 0, Ascending: true}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		op, _ := exec.NewSort(newSliceOperator(rows...), keys, 0)
		_, err := exec.Drain(context.Background(), op)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTop_10kN100(b *testing.B) {
	const n = 10_000
	rows := make([]exec.Row, n)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(n - i))}
	}
	keys := []exec.SortKey{{ColIdx: 0, Ascending: true}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		op, _ := exec.NewTop(newSliceOperator(rows...), keys, 100)
		_, err := exec.Drain(context.Background(), op)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDistinct_10k(b *testing.B) {
	const n = 10_000
	rows := make([]exec.Row, n)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(i % 1000))} // 1000 unique
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		op := exec.NewDistinct(newSliceOperator(rows...), 0)
		_, err := exec.Drain(context.Background(), op)
		if err != nil {
			b.Fatal(err)
		}
	}
}
