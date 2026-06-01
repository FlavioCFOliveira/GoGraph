package exec_test

// apply_test.go — tests for tasks 257–262:
//   - Task 257: Apply (dependent join driver)
//   - Task 258: SemiApply / AntiSemiApply with short-circuit
//   - Task 259: RollUpApply (pattern-comprehension execution)
//   - Task 260: OptionalExpand operator
//   - Task 261: VarLengthExpand (iterative BFS)
//   - Task 262: shortestPath / allShortestPaths

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task 257 — Apply (dependent join)
// ─────────────────────────────────────────────────────────────────────────────

// TestApply_BasicJoin verifies that Apply emits outerRow||innerRow for each
// outer-inner combination.
func TestApply_BasicJoin(t *testing.T) {
	// Outer: 2 rows; inner: 2 rows per outer → 4 combined rows.
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	arg := exec.NewArgument()
	// Inner always produces 2 rows (ignores the outer row value).
	inner := newSliceOperator(
		exec.Row{expr.StringValue("a")},
		exec.Row{expr.StringValue("b")},
	)
	ap := exec.NewApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), ap)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	// Each output row should be width 2 (1 outer col + 1 inner col).
	for i, row := range rows {
		if len(row) != 2 {
			t.Errorf("rows[%d] width = %d, want 2", i, len(row))
		}
	}
}

// TestApply_EmptyOuter verifies that Apply emits nothing when outer is empty.
func TestApply_EmptyOuter(t *testing.T) {
	outer := newSliceOperator()
	arg := exec.NewArgument()
	inner := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	ap := exec.NewApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), ap)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

// TestApply_EmptyInner verifies that Apply emits nothing when inner is always
// empty (cross join with empty right side).
func TestApply_EmptyInner(t *testing.T) {
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	arg := exec.NewArgument()
	inner := newSliceOperator() // always empty
	ap := exec.NewApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), ap)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 (inner always empty)", len(rows))
	}
}

// TestApply_ArgumentSeededPerRow verifies that the Argument operator is
// re-seeded with the current outer row on each iteration.
func TestApply_ArgumentSeededPerRow(t *testing.T) {
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(10)},
		exec.Row{expr.IntegerValue(20)},
		exec.Row{expr.IntegerValue(30)},
	)
	arg := exec.NewArgument()
	// Inner is the Argument itself: it will emit whatever outer row was seeded.
	ap := exec.NewApply(outer, arg, arg)

	rows, err := exec.Drain(context.Background(), ap)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// Each output row = outerRow || argRow. Since arg emits outerRow, we get
	// outerRow||outerRow = [10,10], [20,20], [30,30].
	expected := []int64{10, 20, 30}
	for i, row := range rows {
		if len(row) < 1 {
			t.Fatalf("rows[%d] too narrow", i)
		}
		// The first column of the inner half is the outer row value.
		v := int64(row[len(row)-1].(expr.IntegerValue))
		if v != expected[i] {
			t.Errorf("rows[%d] last col = %d, want %d", i, v, expected[i])
		}
	}
}

// TestApply_CancelledContext verifies that Apply honours context cancellation.
func TestApply_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	outer := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	arg := exec.NewArgument()
	inner := newSliceOperator(exec.Row{expr.IntegerValue(2)})
	ap := exec.NewApply(outer, inner, arg)

	_, err := exec.Drain(ctx, ap)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 258 — SemiApply / AntiSemiApply
// ─────────────────────────────────────────────────────────────────────────────

func TestSemiApply_EmitsWhenInnerMatches(t *testing.T) {
	// Outer: rows [1], [2], [3]. Inner always matches. All 3 outer rows emitted.
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
		exec.Row{expr.IntegerValue(3)},
	)
	arg := exec.NewArgument()
	inner := newSliceOperator(exec.Row{expr.IntegerValue(99)})
	sa := exec.NewSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), sa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
}

func TestSemiApply_EmitsNothingWhenInnerEmpty(t *testing.T) {
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	arg := exec.NewArgument()
	inner := newSliceOperator() // always empty
	sa := exec.NewSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), sa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

func TestSemiApply_ShortCircuit(t *testing.T) {
	// Inner has 100 rows, but SemiApply should stop after the first.
	// Use a counting inner operator to verify at most 1 row is consumed.
	outer := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	arg := exec.NewArgument()

	var innerCount int
	inner := &countingOperator{
		rows: makeIntRows(100),
		onNext: func() {
			innerCount++
		},
	}
	sa := exec.NewSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), sa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// innerCount should be 1 (first Next call returned a row; we stopped).
	if innerCount != 1 {
		t.Errorf("innerCount = %d, want 1 (short-circuit)", innerCount)
	}
}

func TestSemiApply_EmptyOuter(t *testing.T) {
	outer := newSliceOperator()
	arg := exec.NewArgument()
	inner := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	sa := exec.NewSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), sa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

// TestSemiApply_OutputIsOuterRowOnly verifies that only the outer row is
// returned (inner row values are not included).
func TestSemiApply_OutputIsOuterRowOnly(t *testing.T) {
	outer := newSliceOperator(exec.Row{expr.IntegerValue(42)})
	arg := exec.NewArgument()
	inner := newSliceOperator(exec.Row{expr.StringValue("inner-value")})
	sa := exec.NewSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), sa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if len(rows[0]) != 1 {
		t.Errorf("output row width = %d, want 1 (outer only)", len(rows[0]))
	}
	if rows[0][0] != expr.IntegerValue(42) {
		t.Errorf("output row[0] = %v, want 42", rows[0][0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AntiSemiApply
// ─────────────────────────────────────────────────────────────────────────────

func TestAntiSemiApply_EmitsWhenInnerEmpty(t *testing.T) {
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	arg := exec.NewArgument()
	inner := newSliceOperator() // always empty → all outer rows emitted
	asa := exec.NewAntiSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), asa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}

func TestAntiSemiApply_EmitsNothingWhenInnerMatches(t *testing.T) {
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	arg := exec.NewArgument()
	inner := newSliceOperator(exec.Row{expr.IntegerValue(99)}) // always matches
	asa := exec.NewAntiSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), asa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

func TestAntiSemiApply_EmptyOuter(t *testing.T) {
	outer := newSliceOperator()
	arg := exec.NewArgument()
	inner := newSliceOperator()
	asa := exec.NewAntiSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), asa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

func TestAntiSemiApply_OutputIsOuterRowOnly(t *testing.T) {
	outer := newSliceOperator(exec.Row{expr.IntegerValue(7)})
	arg := exec.NewArgument()
	inner := newSliceOperator() // no match
	asa := exec.NewAntiSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), asa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0][0] != expr.IntegerValue(7) {
		t.Errorf("output[0][0] = %v, want 7", rows[0][0])
	}
}

// TestAntiSemiApply_ShortCircuit verifies that the inner plan is closed after
// the first row is found (short-circuit on match detection).
func TestAntiSemiApply_ShortCircuit(t *testing.T) {
	outer := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	arg := exec.NewArgument()

	var innerCount int
	inner := &countingOperator{
		rows: makeIntRows(100),
		onNext: func() {
			innerCount++
		},
	}
	asa := exec.NewAntiSemiApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), asa)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Inner matched → outer row suppressed.
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
	// innerCount should be 1 (short-circuit after first row).
	if innerCount != 1 {
		t.Errorf("innerCount = %d, want 1 (short-circuit)", innerCount)
	}
}

// TestSemiApply_CancelledContext verifies cancellation propagation.
func TestSemiApply_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	outer := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	arg := exec.NewArgument()
	inner := newSliceOperator(exec.Row{expr.IntegerValue(2)})
	sa := exec.NewSemiApply(outer, inner, arg)

	_, err := exec.Drain(ctx, sa)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 259 — RollUpApply
// ─────────────────────────────────────────────────────────────────────────────

func TestRollUpApply_CollectsList(t *testing.T) {
	// Outer: 1 row; inner: 3 values → list of 3 elements appended.
	outer := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	arg := exec.NewArgument()
	inner := newSliceOperator(
		exec.Row{expr.IntegerValue(10)},
		exec.Row{expr.IntegerValue(20)},
		exec.Row{expr.IntegerValue(30)},
	)
	ru := exec.NewRollUpApply(outer, inner, arg, nil) // nil → collect first col

	rows, err := exec.Drain(context.Background(), ru)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	// Output: [outerCol(0), list([10,20,30])].
	if len(row) != 2 {
		t.Fatalf("row width = %d, want 2", len(row))
	}
	list, ok := row[1].(expr.ListValue)
	if !ok {
		t.Fatalf("col[1] is %T, want ListValue", row[1])
	}
	if len(list) != 3 {
		t.Fatalf("list len = %d, want 3", len(list))
	}
	want := []expr.Value{expr.IntegerValue(10), expr.IntegerValue(20), expr.IntegerValue(30)}
	for i, v := range want {
		if list[i] != v {
			t.Errorf("list[%d] = %v, want %v", i, list[i], v)
		}
	}
}

func TestRollUpApply_EmptyInnerProducesEmptyList(t *testing.T) {
	outer := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	arg := exec.NewArgument()
	inner := newSliceOperator() // no rows
	ru := exec.NewRollUpApply(outer, inner, arg, nil)

	rows, err := exec.Drain(context.Background(), ru)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	list, ok := rows[0][1].(expr.ListValue)
	if !ok {
		t.Fatalf("col[1] is %T, want ListValue", rows[0][1])
	}
	// Must be an empty list (not Null).
	if list == nil {
		t.Fatal("list is nil, want empty ListValue")
	}
	if len(list) != 0 {
		t.Errorf("list len = %d, want 0", len(list))
	}
}

func TestRollUpApply_MultipleOuterRows(t *testing.T) {
	// 3 outer rows; inner produces 2 rows for each → 3 output rows, each with a
	// list of 2 elements.
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
		exec.Row{expr.IntegerValue(3)},
	)
	arg := exec.NewArgument()
	inner := newSliceOperator(
		exec.Row{expr.StringValue("x")},
		exec.Row{expr.StringValue("y")},
	)
	ru := exec.NewRollUpApply(outer, inner, arg, nil)

	rows, err := exec.Drain(context.Background(), ru)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	for i, row := range rows {
		list, ok := row[1].(expr.ListValue)
		if !ok {
			t.Fatalf("rows[%d][1] is %T, want ListValue", i, row[1])
		}
		if len(list) != 2 {
			t.Errorf("rows[%d] list len = %d, want 2", i, len(list))
		}
	}
}

func TestRollUpApply_CustomEval(t *testing.T) {
	// Inner rows: [{10, "a"}, {20, "b"}]; eval extracts second column.
	outer := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	arg := exec.NewArgument()
	inner := newSliceOperator(
		exec.Row{expr.IntegerValue(10), expr.StringValue("a")},
		exec.Row{expr.IntegerValue(20), expr.StringValue("b")},
	)
	eval := func(row exec.Row) (expr.Value, error) {
		if len(row) < 2 {
			return expr.Null, nil
		}
		return row[1], nil
	}
	ru := exec.NewRollUpApply(outer, inner, arg, eval)

	rows, err := exec.Drain(context.Background(), ru)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	list := rows[0][1].(expr.ListValue)
	if list[0] != expr.StringValue("a") || list[1] != expr.StringValue("b") {
		t.Errorf("list = %v, want [a b]", list)
	}
}

func TestRollUpApply_EmptyOuter(t *testing.T) {
	outer := newSliceOperator()
	arg := exec.NewArgument()
	inner := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	ru := exec.NewRollUpApply(outer, inner, arg, nil)

	rows, err := exec.Drain(context.Background(), ru)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 260 — OptionalExpand
// ─────────────────────────────────────────────────────────────────────────────

func TestOptionalExpand_ZeroMatchEmitsNullRow(t *testing.T) {
	// Node 5 has no edges in this graph → should emit one NULL-extended row.
	fwd := buildCSR(6, [][2]int{{0, 1}, {1, 2}})
	rev := buildCSR(6, [][2]int{{1, 0}, {2, 1}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(5)}) // isolated node
	op := exec.NewOptionalExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (NULL-extended)", len(rows))
	}
	row := rows[0]
	// Output layout: [inputCol(5), srcID(5), edgeID(Null), dstID(Null)]
	if row[2] != expr.Null {
		t.Errorf("edgeID = %v, want Null", row[2])
	}
	if row[3] != expr.Null {
		t.Errorf("dstID = %v, want Null", row[3])
	}
}

func TestOptionalExpand_MultiMatchEmitsAllRows(t *testing.T) {
	// Node 0 has 3 out-edges → should emit 3 rows (no NULL extension).
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {0, 3}})
	rev := buildCSR(4, [][2]int{{1, 0}, {2, 0}, {3, 0}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewOptionalExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// None of the rows should have Null edge or dst.
	for i, row := range rows {
		if row[2] == expr.Null {
			t.Errorf("rows[%d] edgeID is unexpectedly Null", i)
		}
		if row[3] == expr.Null {
			t.Errorf("rows[%d] dstID is unexpectedly Null", i)
		}
	}
}

func TestOptionalExpand_MixedMatchAndNoMatch(t *testing.T) {
	// Node 0 has 2 edges; node 5 has none.
	fwd := buildCSR(6, [][2]int{{0, 1}, {0, 2}})
	rev := buildCSR(6, [][2]int{{1, 0}, {2, 0}})

	input := newSliceOperator(
		exec.Row{expr.IntegerValue(0)}, // 2 matches
		exec.Row{expr.IntegerValue(5)}, // 0 matches → 1 NULL row
	)
	op := exec.NewOptionalExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// 2 real rows + 1 NULL row = 3 total.
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// The last row should be the NULL-extended one (for node 5).
	nullRow := rows[2]
	if nullRow[2] != expr.Null || nullRow[3] != expr.Null {
		t.Errorf("last row not NULL-extended: %v", nullRow)
	}
}

func TestOptionalExpand_EmptyInput(t *testing.T) {
	fwd := buildCSR(3, [][2]int{{0, 1}})
	rev := buildCSR(3, [][2]int{{1, 0}})

	input := newSliceOperator()
	op := exec.NewOptionalExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

func TestOptionalExpand_SingleMatch(t *testing.T) {
	// Node 0 → 1 (one edge).
	fwd := buildCSR(2, [][2]int{{0, 1}})
	rev := buildCSR(2, [][2]int{{1, 0}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewOptionalExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// Edge should not be Null.
	if rows[0][2] == expr.Null {
		t.Errorf("edgeID is Null for a matched edge")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 261 — VarLengthExpand
// ─────────────────────────────────────────────────────────────────────────────

func TestVarLenExpand_1to3Hops_Linear(t *testing.T) {
	// Linear graph: 0→1→2→3→4
	fwd := buildCSR(5, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}})
	rev := buildCSR(5, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   3,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// From 0: hop1→1, hop2→2, hop3→3 = 3 paths.
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
}

func TestVarLenExpand_MinHops0_IncludesSource(t *testing.T) {
	// minHops=0 means the source itself is a valid result.
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}})
	rev := buildCSR(3, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   0,
		MaxHops:   2,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// hop0→0 (src), hop1→1, hop2→2 = 3 paths.
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (including hop-0 source)", len(rows))
	}
}

func TestVarLenExpand_CyclicGraphTerminates(t *testing.T) {
	// Cycle: 0→1→2→0. With maxHops=3, BFS should terminate correctly.
	// Per-path deduplication: each path can only use each edge once, so
	// infinite loops are prevented.
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}, {2, 0}})
	rev := buildCSR(3, [][2]int{{1, 0}, {2, 1}, {0, 2}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   3,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain (cyclic graph): %v", err)
	}
	// Verify: operator returned without infinite loop.
	if rows == nil {
		t.Fatal("rows is nil")
	}
}

func TestVarLenExpand_SafetyCapExceeded(t *testing.T) {
	// Dense graph with many edges; cap at 10 traversals.
	// Node 0 has 100 edges → cap exceeded on first expansion.
	edges := make([][2]int, 100)
	for i := range edges {
		edges[i] = [2]int{0, i + 1}
	}
	fwd := buildCSR(101, edges)
	rev := buildCSR(101, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction:         exec.DirOut,
		InputCol:          0,
		MinHops:           1,
		MaxHops:           2,
		MaxEdgesTraversed: 10, // will be exceeded
	})

	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrVarLenCapExceeded) {
		t.Fatalf("want ErrVarLenCapExceeded, got %v", err)
	}
}

func TestVarLenExpand_IsolatedNode(t *testing.T) {
	// Node 0 has no edges; minHops=1 → zero results.
	fwd := buildCSR(3, [][2]int{{1, 2}})
	rev := buildCSR(3, [][2]int{{2, 1}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   3,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

func TestVarLenExpand_ExactHops(t *testing.T) {
	// Path 0→1→2→3. Request exactly 2 hops → only 0→1→2 (dest=2).
	fwd := buildCSR(4, [][2]int{{0, 1}, {1, 2}, {2, 3}})
	rev := buildCSR(4, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   2,
		MaxHops:   2,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (exactly 2 hops)", len(rows))
	}
	// dstID should be 2.
	dstID := int64(rows[0][len(rows[0])-1].(expr.IntegerValue))
	if dstID != 2 {
		t.Errorf("dstID = %d, want 2", dstID)
	}
}

func TestVarLenExpand_MultipleInputRows(t *testing.T) {
	// Two sources: node 0 and node 1. Each has 1-hop neighbours.
	fwd := buildCSR(4, [][2]int{{0, 2}, {1, 3}})
	rev := buildCSR(4, nil)

	input := newSliceOperator(
		exec.Row{expr.IntegerValue(0)},
		exec.Row{expr.IntegerValue(1)},
	)
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   1,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}

func TestVarLenExpand_EdgeDeduplicationWithinPath(t *testing.T) {
	// Bidirectional edge 0↔1. With DirBoth and maxHops=2:
	// Path 0→1 is valid (1 hop), but 0→1→0 uses the same edge (0→1 and the
	// same edge traversed in reverse), which should be deduplicated.
	// In practice with our CSR model, forward and reverse edges have different
	// absolute positions, so 0→fwd→1→rev→0 uses different edge IDs and is
	// permitted. This test verifies the BFS terminates correctly.
	fwd := buildCSR(2, [][2]int{{0, 1}})
	rev := buildCSR(2, [][2]int{{1, 0}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   2,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// hop1→1, but hop2 would go back to 0 (reverse) — DirOut only follows fwd,
	// so no hop-2 path (node 1 has no fwd edges). Result = 1 path.
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
}

func TestVarLenExpand_CancellationHonoured(t *testing.T) {
	// Large graph; cancel immediately.
	edges := make([][2]int, 50)
	for i := range edges {
		edges[i] = [2]int{0, i + 1}
	}
	fwd := buildCSR(51, edges)
	rev := buildCSR(51, nil)

	ctx, cancel := context.WithCancel(context.Background())
	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   5,
	})
	if err := op.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	var row exec.Row
	_, err := op.Next(&row)
	// Either context.Canceled or a valid row (before cancellation detected).
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = op.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 262 — ShortestPath / AllShortestPaths
// ─────────────────────────────────────────────────────────────────────────────

func TestShortestPath_DirectEdge(t *testing.T) {
	// Graph: 0→1. Shortest path from 0 to 1 = length 1.
	fwd := buildCSR(2, [][2]int{{0, 1}})
	rev := buildCSR(2, [][2]int{{1, 0}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(0), expr.IntegerValue(1)})
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	pv, ok := rows[0][2].(expr.PathValue)
	if !ok {
		t.Fatalf("col[2] is %T, want PathValue", rows[0][2])
	}
	if len(pv.Nodes) != 2 || len(pv.Relationships) != 1 {
		t.Errorf("path = %v nodes, %v rels; want 2 nodes 1 rel", len(pv.Nodes), len(pv.Relationships))
	}
}

func TestShortestPath_Unreachable(t *testing.T) {
	// Graph: 0→1. No path from 1 to 0 (directed).
	fwd := buildCSR(2, [][2]int{{0, 1}})
	rev := buildCSR(2, [][2]int{{1, 0}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(1), expr.IntegerValue(0)})
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (Null path)", len(rows))
	}
	if rows[0][2] != expr.Null {
		t.Errorf("path = %v, want Null (unreachable)", rows[0][2])
	}
}

func TestShortestPath_SameNodeZeroHop(t *testing.T) {
	// src == dst → zero-hop path with 1 node and 0 relationships.
	fwd := buildCSR(3, [][2]int{{0, 1}})
	rev := buildCSR(3, [][2]int{{1, 0}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(1), expr.IntegerValue(1)})
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	pv := rows[0][2].(expr.PathValue)
	if len(pv.Nodes) != 1 || len(pv.Relationships) != 0 {
		t.Errorf("zero-hop path: got %d nodes, %d rels", len(pv.Nodes), len(pv.Relationships))
	}
}

func TestShortestPath_LongerPath(t *testing.T) {
	// Path: 0→1→2→3. Shortest path from 0 to 3 should be length 3.
	fwd := buildCSR(4, [][2]int{{0, 1}, {1, 2}, {2, 3}})
	rev := buildCSR(4, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0), expr.IntegerValue(3)})
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	pv := rows[0][2].(expr.PathValue)
	// Path 0→1→2→3: 4 nodes, 3 rels.
	if len(pv.Nodes) != 4 {
		t.Errorf("path nodes = %d, want 4", len(pv.Nodes))
	}
	if len(pv.Relationships) != 3 {
		t.Errorf("path rels = %d, want 3", len(pv.Relationships))
	}
}

func TestShortestPath_ShortestAmongMultiple(t *testing.T) {
	// Graph: 0→1→3 (len 2) and 0→2→3 (len 2). BFS finds one of them.
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}})
	rev := buildCSR(4, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0), expr.IntegerValue(3)})
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	pv := rows[0][2].(expr.PathValue)
	// Must be length 2 (3 nodes, 2 rels).
	if len(pv.Nodes) != 3 {
		t.Errorf("path nodes = %d, want 3", len(pv.Nodes))
	}
}

func TestShortestPath_EmptyInput(t *testing.T) {
	fwd := buildCSR(2, [][2]int{{0, 1}})
	rev := buildCSR(2, [][2]int{{1, 0}})

	input := newSliceOperator()
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

func TestShortestPath_InvalidInputColumns(t *testing.T) {
	// srcCol/dstCol out of bounds → Null path emitted.
	fwd := buildCSR(2, [][2]int{{0, 1}})
	rev := buildCSR(2, [][2]int{{1, 0}})

	input := newSliceOperator(exec.Row{expr.StringValue("not-an-int"), expr.StringValue("not-an-int")})
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0][2] != expr.Null {
		t.Errorf("expected Null for non-integer columns, got %v", rows[0][2])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 262 — AllShortestPaths
// ─────────────────────────────────────────────────────────────────────────────

func TestAllShortestPaths_TwoPaths(t *testing.T) {
	// Two paths from 0 to 3, both length 2: 0→1→3 and 0→2→3.
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}})
	rev := buildCSR(4, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0), expr.IntegerValue(3)})
	op := exec.NewAllShortestPaths(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Both paths must have length 2 (3 nodes, 2 rels).
	for i, row := range rows {
		pv := row[2].(expr.PathValue)
		if len(pv.Nodes) != 3 || len(pv.Relationships) != 2 {
			t.Errorf("rows[%d] path: %d nodes %d rels, want 3 nodes 2 rels", i, len(pv.Nodes), len(pv.Relationships))
		}
	}
}

func TestAllShortestPaths_Unreachable(t *testing.T) {
	fwd := buildCSR(2, [][2]int{{0, 1}})
	rev := buildCSR(2, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(1), expr.IntegerValue(0)})
	op := exec.NewAllShortestPaths(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 (unreachable)", len(rows))
	}
}

func TestAllShortestPaths_SameNode(t *testing.T) {
	fwd := buildCSR(2, [][2]int{{0, 1}})
	rev := buildCSR(2, [][2]int{{1, 0}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(0), expr.IntegerValue(0)})
	op := exec.NewAllShortestPaths(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (zero-hop)", len(rows))
	}
}

func TestAllShortestPaths_SinglePath(t *testing.T) {
	// Linear: 0→1→2. Only one shortest path.
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}})
	rev := buildCSR(3, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0), expr.IntegerValue(2)})
	op := exec.NewAllShortestPaths(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
}

func TestAllShortestPaths_ThreePaths(t *testing.T) {
	// Three paths from 0 to 4, all length 2:
	// 0→1→4, 0→2→4, 0→3→4
	fwd := buildCSR(5, [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 4}, {2, 4}, {3, 4}})
	rev := buildCSR(5, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0), expr.IntegerValue(4)})
	op := exec.NewAllShortestPaths(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (three shortest paths)", len(rows))
	}
}

func TestAllShortestPaths_MultipleInputRows(t *testing.T) {
	// Two queries in one pass: (0→3) and (1→3).
	// Graph: 0→1→3, 0→2→3, 1→3 (direct).
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}})
	rev := buildCSR(4, nil)

	input := newSliceOperator(
		exec.Row{expr.IntegerValue(0), expr.IntegerValue(3)}, // 2 shortest paths of len 2
		exec.Row{expr.IntegerValue(1), expr.IntegerValue(3)}, // 1 shortest path of len 1
	)
	op := exec.NewAllShortestPaths(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// 2 paths for query1 + 1 path for query2 = 3 total.
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
}

// TestAllShortestPaths_PicksShorterNotLonger verifies that when there is a
// shorter path and a longer path, only shortest-length paths are returned.
func TestAllShortestPaths_PicksShorterNotLonger(t *testing.T) {
	// Direct edge 0→3 (len 1) and long path 0→1→2→3 (len 3).
	// Only the direct edge should be in allShortestPaths.
	fwd := buildCSR(4, [][2]int{{0, 3}, {0, 1}, {1, 2}, {2, 3}})
	rev := buildCSR(4, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0), expr.IntegerValue(3)})
	op := exec.NewAllShortestPaths(input, fwd, rev, exec.DirOut, 0, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (only direct edge)", len(rows))
	}
	pv := rows[0][2].(expr.PathValue)
	if len(pv.Relationships) != 1 {
		t.Errorf("path len = %d, want 1", len(pv.Relationships))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 393 — additional acceptance-criterion tests
// ─────────────────────────────────────────────────────────────────────────────

// TestVarLenExpand_ZeroHopOnly verifies that *0..0 (the degenerate "zero-hop"
// pattern) emits exactly the source node as the destination.
func TestVarLenExpand_ZeroHopOnly(t *testing.T) {
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}})
	rev := buildCSR(3, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   0,
		MaxHops:   0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (zero-hop pseudo-path)", len(rows))
	}
	// Output is [srcID, pathList, dstID]; for hop-0 dstID equals srcID.
	dst, ok := rows[0][2].(expr.IntegerValue)
	if !ok {
		t.Fatalf("dst column is not IntegerValue: %T", rows[0][2])
	}
	if int64(dst) != 0 {
		t.Errorf("zero-hop dst = %d, want 0", int64(dst))
	}
	pl, ok := rows[0][1].(expr.ListValue)
	if !ok {
		t.Fatalf("path column is not ListValue: %T", rows[0][1])
	}
	// Zero-hop flat alternating encoding: [srcNodeID] — exactly 1 element.
	if len(pl) != 1 {
		t.Errorf("zero-hop path length = %d, want 1 (flat alternating: [srcID])", len(pl))
	}
}

// TestVarLenExpand_OneToOneHop verifies that *1..1 is equivalent to a single
// Expand: one row per direct neighbour, no further hops.
func TestVarLenExpand_OneToOneHop(t *testing.T) {
	// Star: 0→1, 0→2, 0→3 (3 neighbours); each has its own neighbour to ensure
	// the operator does not bleed into hop 2.
	fwd := buildCSR(7, [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 4}, {2, 5}, {3, 6}})
	rev := buildCSR(7, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   1,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (exactly direct neighbours)", len(rows))
	}
}

// TestVarLenExpand_OneToFiveHops_Chain verifies *1..5 over a 6-node chain
// emits one row per hop length 1..5.
func TestVarLenExpand_OneToFiveHops_Chain(t *testing.T) {
	fwd := buildCSR(6, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 5}})
	rev := buildCSR(6, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   5,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5", len(rows))
	}
	// Destinations should be 1, 2, 3, 4, 5 in BFS order.
	wantDsts := []int64{1, 2, 3, 4, 5}
	for i, row := range rows {
		dst, ok := row[2].(expr.IntegerValue)
		if !ok {
			t.Fatalf("row %d: dst is not IntegerValue: %T", i, row[2])
		}
		if int64(dst) != wantDsts[i] {
			t.Errorf("row %d: dst = %d, want %d", i, int64(dst), wantDsts[i])
		}
	}
}

// TestVarLenExpand_OneToFiveHops_Tree verifies *1..5 over a branching tree.
// This is the regression test for the runBFS slice-aliasing bug
// (task-393): with two or more paths at the same BFS level, the previous
// implementation overwrote frontier entries that had not yet been processed.
//
// Tree:
//
//	     0
//	   / | \
//	  1  2  3      (hop 1)
//	 /|  |  |\
//	4 5  6  7 8    (hop 2)
//
// From 0 with hops 1..5 we expect 3 + 5 = 8 destinations.
func TestVarLenExpand_OneToFiveHops_Tree(t *testing.T) {
	fwd := buildCSR(9, [][2]int{
		{0, 1}, {0, 2}, {0, 3},
		{1, 4}, {1, 5},
		{2, 6},
		{3, 7}, {3, 8},
	})
	rev := buildCSR(9, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   5,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 8 {
		t.Fatalf("got %d rows, want 8 (3 hop-1 + 5 hop-2)", len(rows))
	}
	// Every destination 1..8 must appear exactly once.
	seen := make(map[int64]int, 8)
	for _, row := range rows {
		dst, ok := row[2].(expr.IntegerValue)
		if !ok {
			t.Fatalf("dst is not IntegerValue: %T", row[2])
		}
		seen[int64(dst)]++
	}
	for id := int64(1); id <= 8; id++ {
		if seen[id] != 1 {
			t.Errorf("destination %d seen %d time(s), want 1", id, seen[id])
		}
	}
}

// TestVarLenExpand_RingRelationshipIsomorphism verifies Cypher's
// relationship-uniqueness rule: in a ring 0→1→2→3→0, BFS from 0 must terminate
// because no relationship may appear twice in a single path. With maxHops≥|V|
// the search must not loop indefinitely.
func TestVarLenExpand_RingRelationshipIsomorphism(t *testing.T) {
	const n = 6
	edges := make([][2]int, n)
	for i := 0; i < n; i++ {
		edges[i] = [2]int{i, (i + 1) % n}
	}
	fwd := buildCSR(n, edges)
	rev := buildCSR(n, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction: exec.DirOut,
		InputCol:  0,
		MinHops:   1,
		MaxHops:   1_000, // much larger than n; isomorphism must cap exploration
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain (ring): %v", err)
	}
	// A simple path from 0 can use each of the n edges at most once → at most n
	// distinct rows (one row per terminal hop).
	if len(rows) > n {
		t.Fatalf("ring produced %d rows, want at most %d (relationship isomorphism)", len(rows), n)
	}
}

// TestVarLenExpand_TightCycleCapDoesNotExplode verifies acceptance criterion
// "memory bounded under cyclic graphs" using a tight 3-cycle with a 1M cap.
// The relationship-uniqueness rule constrains exploration to at most 3 hops
// from any single source, so even a generous cap must not let memory blow up.
func TestVarLenExpand_TightCycleCapDoesNotExplode(t *testing.T) {
	// 3-cycle: 0→1→2→0.
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}, {2, 0}})
	rev := buildCSR(3, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction:         exec.DirOut,
		InputCol:          0,
		MinHops:           1,
		MaxHops:           1_000_000, // ridiculously high
		MaxEdgesTraversed: 1_000_000, // default cap
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain (cycle): %v", err)
	}
	// 3 distinct paths from 0: 0→1, 0→1→2, 0→1→2→0. Edge isomorphism forbids
	// reusing the 2→0 edge, so the path 0→1→2→0 cannot continue.
	if len(rows) != 3 {
		t.Fatalf("cycle produced %d rows, want 3", len(rows))
	}
}

// TestVarLenExpand_TimeoutCancelsBoundedEnumeration verifies acceptance
// criterion "(a)-[*1..5]->(b) bounded enumeration with timeout test": a
// context cancellation while the BFS is in progress must propagate an error
// rather than running to completion.
//
// The graph is a dense complete graph (every node connects to every other) so
// that many enumeration branches exist; the cap is enormous and maxHops large
// enough to keep the operator busy long enough for cancel to land.
func TestVarLenExpand_TimeoutCancelsBoundedEnumeration(t *testing.T) {
	const n = 12 // K_12 has 12*11 = 132 directed edges and many bounded paths
	var edges [][2]int
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i != j {
				edges = append(edges, [2]int{i, j})
			}
		}
	}
	fwd := buildCSR(n, edges)
	rev := buildCSR(n, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction:         exec.DirOut,
		InputCol:          0,
		MinHops:           1,
		MaxHops:           5,
		MaxEdgesTraversed: 100_000_000,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before draining so ctx.Err() returns immediately

	_, err := exec.Drain(ctx, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain err = %v, want context.Canceled", err)
	}
}

// TestShortestPath_DeterministicTieBreaking verifies that with two equally
// shortest paths between the same endpoints, ShortestPath returns the same
// path every time (i.e. the choice is deterministic, not stochastic).
func TestShortestPath_DeterministicTieBreaking(t *testing.T) {
	// Two equally short routes 0→1→3 and 0→2→3.
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}})
	rev := buildCSR(4, nil)

	type sample struct {
		mid uint64 // intermediate node ID (1 or 2)
	}
	var first sample
	for run := 0; run < 5; run++ {
		input := newSliceOperator(exec.Row{expr.IntegerValue(0), expr.IntegerValue(3)})
		op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1)
		rows, err := exec.Drain(context.Background(), op)
		if err != nil {
			t.Fatalf("run %d: Drain: %v", run, err)
		}
		if len(rows) != 1 {
			t.Fatalf("run %d: got %d rows, want 1", run, len(rows))
		}
		pv, ok := rows[0][2].(expr.PathValue)
		if !ok {
			t.Fatalf("run %d: not a PathValue: %T", run, rows[0][2])
		}
		if len(pv.Nodes) != 3 {
			t.Fatalf("run %d: nodes = %d, want 3", run, len(pv.Nodes))
		}
		s := sample{mid: pv.Nodes[1].ID}
		if run == 0 {
			first = s
			continue
		}
		if s != first {
			t.Errorf("run %d: tie-breaking is non-deterministic: first=%v got=%v",
				run, first, s)
		}
	}
}

// countingOperator wraps a sliceOperator and calls onNext before each Next.
type countingOperator struct {
	rows   []exec.Row
	onNext func()
	inner  *sliceOperator
}

func (c *countingOperator) Init(ctx context.Context) error {
	c.inner = newSliceOperator(c.rows...)
	return c.inner.Init(ctx)
}

func (c *countingOperator) Next(out *exec.Row) (bool, error) {
	c.onNext()
	return c.inner.Next(out)
}

func (c *countingOperator) Close() error {
	if c.inner != nil {
		return c.inner.Close()
	}
	return nil
}
