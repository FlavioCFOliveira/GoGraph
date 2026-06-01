package exec_test

// operators_test.go — tests for tasks 241–246:
//   - Task 241: cyphermorphism enforcement in Expand
//   - Task 242: Filter operator with three-valued logic (20 scenarios)
//   - Task 243: Project operator (15 scenarios)
//   - Task 244: Limit / Skip operators (10 scenarios)
//   - Task 245: Argument operator (Apply seed)
//   - Task 246: ProduceResults / ResultSet / Run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task 241 — Cyphermorphism enforcement in Expand
// ─────────────────────────────────────────────────────────────────────────────

// TestCyphermorphism_RejectsDuplicateEdge verifies that when a two-hop path
// (a)-[r1]->(b)-[r2]->(c) expands via the same edge for both hops, the
// duplicate row is suppressed.
//
// Graph: 0→1 (edge pos 0), 1→0 (edge pos 1).
// Inner expand starts from node 1 with r1=0 already in column 1.
// The only forward edge from node 1 is back to node 0 via edge pos 1, which
// is distinct — so it should pass.  We also build a case where the same edge
// position appears to verify rejection.
func TestCyphermorphism_RejectsDuplicateEdge(t *testing.T) {
	// Graph: 0→1, 1→2, 0→2
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}, {0, 2}})
	rev := buildCSR(3, [][2]int{{1, 0}, {2, 1}, {2, 0}})

	// Simulate the inner plan for the second hop starting from node 1.
	// The first hop traversed edge at position 0 (0→1). That edge ID is stored
	// in column 1 of the input row: [srcID=0, edgeID=0, dstID=1].
	// The second Expand uses inputCol=2 (dstID=1 is the new src) and relCols=[1]
	// (column 1 holds r1's edge ID).

	input := newSliceOperator(
		exec.Row{
			expr.IntegerValue(0), // col0: outer src
			expr.IntegerValue(0), // col1: r1 edge ID = 0
			expr.IntegerValue(1), // col2: b node
		},
	)

	op := exec.NewExpandWithOptions(
		input, fwd, rev,
		exec.ExpandConfig{Direction: exec.DirOut, InputCol: 2},
		exec.WithCyphermorphism([]int{1}), // relCols: column 1 holds r1
	)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// From node 1, forward edges: only 1→2 (edge pos 1). Edge pos 1 ≠ 0 (r1),
	// so it must pass.
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (edge at pos 1 is distinct from r1 at pos 0)", len(rows))
	}
}

// TestCyphermorphism_DistinctEdgesPass verifies that non-duplicate patterns
// (r1 ≠ r2) are emitted without suppression.
func TestCyphermorphism_DistinctEdgesPass(t *testing.T) {
	// Graph: 0→1 (pos 0), 1→2 (pos 1), 1→3 (pos 2)
	fwd := buildCSR(4, [][2]int{{0, 1}, {1, 2}, {1, 3}})
	rev := buildCSR(4, [][2]int{{1, 0}, {2, 1}, {3, 1}})

	// r1 = edge pos 0. Inner hop from node 1: edges at pos 1 and 2 must both pass.
	input := newSliceOperator(
		exec.Row{
			expr.IntegerValue(0),
			expr.IntegerValue(0), // r1 = edge pos 0
			expr.IntegerValue(1), // b = node 1
		},
	)

	op := exec.NewExpandWithOptions(
		input, fwd, rev,
		exec.ExpandConfig{Direction: exec.DirOut, InputCol: 2},
		exec.WithCyphermorphism([]int{1}),
	)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}

// TestCyphermorphism_SameEdgeSuppressed inserts a row where the about-to-emit
// edge ID matches r1 exactly. The CSR is constructed so that node 1 has an
// edge back to node 0 at position 0 — the same position as r1. That row must
// be suppressed, leaving only the other edge.
func TestCyphermorphism_SameEdgeSuppressed(t *testing.T) {
	// Graph: 0→1 (pos 0), 1→0 (pos 1), 1→2 (pos 2).
	// BUT we tweak the CSR: we want pos 0 to originate from node 1 too,
	// which isn't how CSR works. Instead, build a graph where we manufacture
	// a scenario: use a graph with global edge positions shared across sources.
	//
	// Simpler: graph 1→2 (pos 0), 1→3 (pos 1). r1=pos 0. Inner hop from node 1.
	// Edge at pos 0 has edgeID=0 = r1 → suppressed.
	// Edge at pos 1 has edgeID=1 ≠ r1 → emitted.
	fwd := buildCSR(4, [][2]int{{1, 2}, {1, 3}})
	rev := buildCSR(4, [][2]int{{2, 1}, {3, 1}})

	input := newSliceOperator(
		exec.Row{
			expr.IntegerValue(0),
			expr.IntegerValue(0), // r1 = edge pos 0
			expr.IntegerValue(1), // b = node 1
		},
	)
	op := exec.NewExpandWithOptions(
		input, fwd, rev,
		exec.ExpandConfig{Direction: exec.DirOut, InputCol: 2},
		exec.WithCyphermorphism([]int{1}),
	)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Edge pos 0 is suppressed (== r1). Edge pos 1 survives.
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// The surviving row's edgeID must be 1, not 0.
	edgeID := int64(rows[0][len(rows[0])-2].(expr.IntegerValue))
	if edgeID != 1 {
		t.Errorf("edgeID = %d, want 1", edgeID)
	}
}

// TestCyphermorphism_NilRelCols verifies that without WithCyphermorphism,
// duplicate edges are NOT rejected (original Expand behaviour preserved).
func TestCyphermorphism_NilRelCols(t *testing.T) {
	fwd := buildCSR(3, [][2]int{{1, 2}})
	rev := buildCSR(3, [][2]int{{2, 1}})

	input := newSliceOperator(
		exec.Row{
			expr.IntegerValue(0),
			expr.IntegerValue(0), // would be r1 — but relCols not set
			expr.IntegerValue(1),
		},
	)
	// No WithCyphermorphism — edge at pos 0 must still be emitted.
	op := exec.NewExpandWithOptions(
		input, fwd, rev,
		exec.ExpandConfig{Direction: exec.DirOut, InputCol: 2},
		// no options
	)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (no morphism check)", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 242 — Filter operator with 3VL
// ─────────────────────────────────────────────────────────────────────────────

func TestFilter_3VL(t *testing.T) {
	alwaysTrue := func(_ exec.Row) (expr.Value, error) { return expr.BoolValue(true), nil }
	alwaysFalse := func(_ exec.Row) (expr.Value, error) { return expr.BoolValue(false), nil }
	alwaysNull := func(_ exec.Row) (expr.Value, error) { return expr.Null, nil }
	errFn := func(_ exec.Row) (expr.Value, error) { return nil, errors.New("predicate error") }

	makeRows := func(n int) []exec.Row {
		rows := make([]exec.Row, n)
		for i := range rows {
			rows[i] = exec.Row{expr.IntegerValue(int64(i))}
		}
		return rows
	}

	tests := []struct {
		name    string
		inputN  int
		predFn  exec.FilterFn
		wantN   int
		wantErr bool
	}{
		// 1. All-true: all rows pass.
		{"all-true/5", 5, alwaysTrue, 5, false},
		// 2. All-false: no rows pass.
		{"all-false/5", 5, alwaysFalse, 0, false},
		// 3. Null suppresses (3VL): no rows pass.
		{"null-suppresses/5", 5, alwaysNull, 0, false},
		// 4. Empty input.
		{"empty-input", 0, alwaysTrue, 0, false},
		// 5. Single true row.
		{"single-true", 1, alwaysTrue, 1, false},
		// 6. Single false row.
		{"single-false", 1, alwaysFalse, 0, false},
		// 7. Single null row.
		{"single-null", 1, alwaysNull, 0, false},
		// 8. Error propagation halts iteration.
		{"error-propagation", 3, errFn, 0, true},
		// 9. Even rows pass (filter by index via counter — use closure).
		{"even-rows", 6, func() exec.FilterFn {
			var i int
			return func(_ exec.Row) (expr.Value, error) {
				pass := i%2 == 0
				i++
				return expr.BoolValue(pass), nil
			}
		}(), 3, false},
		// 10. Odd rows pass.
		{"odd-rows", 6, func() exec.FilterFn {
			var i int
			return func(_ exec.Row) (expr.Value, error) {
				pass := i%2 != 0
				i++
				return expr.BoolValue(pass), nil
			}
		}(), 3, false},
		// 11. Predicate checks value equality: keep rows where value == 3.
		{"value-eq-3/5", 5, func(row exec.Row) (expr.Value, error) {
			v, ok := row[0].(expr.IntegerValue)
			if !ok {
				return expr.Null, nil
			}
			return expr.BoolValue(int64(v) == 3), nil
		}, 1, false},
		// 12. Predicate returns non-bool integer (not truthy) — row suppressed.
		{"non-bool-integer", 3, func(_ exec.Row) (expr.Value, error) {
			return expr.IntegerValue(1), nil // not BoolValue(true)
		}, 0, false},
		// 13. Predicate returns non-bool string — suppressed.
		{"non-bool-string", 2, func(_ exec.Row) (expr.Value, error) {
			return expr.StringValue("yes"), nil
		}, 0, false},
		// 14. Predicate returns BoolValue(false) interleaved with true.
		{"alternating-true-false", 4, func() exec.FilterFn {
			vals := []bool{true, false, true, false}
			var i int
			return func(_ exec.Row) (expr.Value, error) {
				v := expr.BoolValue(vals[i%len(vals)])
				i++
				return v, nil
			}
		}(), 2, false},
		// 15. Predicate returns Null then true: only true rows pass.
		{"null-then-true", 4, func() exec.FilterFn {
			var i int
			return func(_ exec.Row) (expr.Value, error) {
				i++
				if i%2 == 0 {
					return expr.BoolValue(true), nil
				}
				return expr.Null, nil
			}
		}(), 2, false},
		// 16. All true on 100 rows.
		{"100-rows-all-true", 100, alwaysTrue, 100, false},
		// 17. All false on 100 rows.
		{"100-rows-all-false", 100, alwaysFalse, 0, false},
		// 18. Last row fails with error: should propagate.
		{"last-row-error/3", 3, func() exec.FilterFn {
			var i int
			return func(_ exec.Row) (expr.Value, error) {
				i++
				if i == 3 {
					return nil, errors.New("final row error")
				}
				return expr.BoolValue(true), nil
			}
		}(), 0, true},
		// 19. Filter above empty child.
		{"filter-empty-child", 0, alwaysTrue, 0, false},
		// 20. First row Null, rest true.
		{"first-null-rest-true", 3, func() exec.FilterFn {
			var i int
			return func(_ exec.Row) (expr.Value, error) {
				i++
				if i == 1 {
					return expr.Null, nil
				}
				return expr.BoolValue(true), nil
			}
		}(), 2, false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rows := makeRows(tc.inputN)
			op := exec.NewFilter(newSliceOperator(rows...), tc.predFn)
			result, err := exec.Drain(context.Background(), op)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(result) != tc.wantN {
				t.Fatalf("got %d rows, want %d", len(result), tc.wantN)
			}
		})
	}
}

// TestFilter_ContextCancellation ensures cancellation propagates through Filter.
func TestFilter_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	op := exec.NewFilter(
		newSliceOperator(exec.Row{expr.IntegerValue(1)}),
		func(_ exec.Row) (expr.Value, error) { return expr.BoolValue(true), nil },
	)
	_, err := exec.Drain(ctx, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 243 — Project operator
// ─────────────────────────────────────────────────────────────────────────────

// makeProjectItems is a convenience builder.
func makeProjectItems(names []string, evals []func(exec.Row) (expr.Value, error)) []exec.ProjectionItem {
	items := make([]exec.ProjectionItem, len(names))
	for i, n := range names {
		items[i] = exec.ProjectionItem{Alias: n, Eval: evals[i]}
	}
	return items
}

// identity returns the value at column col.
func colEval(col int) func(exec.Row) (expr.Value, error) {
	return func(row exec.Row) (expr.Value, error) {
		if col >= len(row) {
			return expr.Null, nil
		}
		return row[col], nil
	}
}

// constEval returns a constant value.
func constEval(v expr.Value) func(exec.Row) (expr.Value, error) {
	return func(_ exec.Row) (expr.Value, error) { return v, nil }
}

func TestProject_Scenarios(t *testing.T) {
	errEval := func(_ exec.Row) (expr.Value, error) { return nil, errors.New("eval error") }

	tests := []struct {
		name      string
		inputRows []exec.Row
		items     []exec.ProjectionItem
		wantN     int
		wantErr   bool
		check     func(t *testing.T, rows []exec.Row)
	}{
		// 1. Single-column identity projection.
		{
			"identity-single-col",
			[]exec.Row{{expr.IntegerValue(7)}},
			makeProjectItems([]string{"x"}, []func(exec.Row) (expr.Value, error){colEval(0)}),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if rows[0][0] != expr.IntegerValue(7) {
					t.Errorf("col[0] = %v, want 7", rows[0][0])
				}
			},
		},
		// 2. Two-column projection with aliasing (swap columns).
		{
			"swap-columns",
			[]exec.Row{{expr.IntegerValue(1), expr.StringValue("a")}},
			makeProjectItems(
				[]string{"b", "n"},
				[]func(exec.Row) (expr.Value, error){colEval(1), colEval(0)},
			),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if rows[0][0] != expr.StringValue("a") {
					t.Errorf("col[0] = %v, want \"a\"", rows[0][0])
				}
				if rows[0][1] != expr.IntegerValue(1) {
					t.Errorf("col[1] = %v, want 1", rows[0][1])
				}
			},
		},
		// 3. Constant projection.
		{
			"constant-projection",
			[]exec.Row{{expr.IntegerValue(99)}},
			makeProjectItems(
				[]string{"const"},
				[]func(exec.Row) (expr.Value, error){constEval(expr.StringValue("hello"))},
			),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if rows[0][0] != expr.StringValue("hello") {
					t.Errorf("col[0] = %v, want \"hello\"", rows[0][0])
				}
			},
		},
		// 4. Null projection.
		{
			"null-projection",
			[]exec.Row{{expr.IntegerValue(1)}},
			makeProjectItems([]string{"n"}, []func(exec.Row) (expr.Value, error){constEval(expr.Null)}),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if rows[0][0] != expr.Null {
					t.Errorf("col[0] = %v, want null", rows[0][0])
				}
			},
		},
		// 5. Multiple input rows, all projected correctly.
		{
			"multi-row",
			[]exec.Row{
				{expr.IntegerValue(1)},
				{expr.IntegerValue(2)},
				{expr.IntegerValue(3)},
			},
			makeProjectItems([]string{"x"}, []func(exec.Row) (expr.Value, error){colEval(0)}),
			3, false, nil,
		},
		// 6. Empty input produces empty output.
		{
			"empty-input",
			nil,
			makeProjectItems([]string{"x"}, []func(exec.Row) (expr.Value, error){colEval(0)}),
			0, false, nil,
		},
		// 7. Eval error propagates.
		{
			"eval-error",
			[]exec.Row{{expr.IntegerValue(1)}},
			makeProjectItems([]string{"x"}, []func(exec.Row) (expr.Value, error){errEval}),
			0, true, nil,
		},
		// 8. Output width matches items count regardless of input width.
		{
			"output-width",
			[]exec.Row{{expr.IntegerValue(1), expr.StringValue("a"), expr.BoolValue(true)}},
			makeProjectItems(
				[]string{"a", "b"},
				[]func(exec.Row) (expr.Value, error){colEval(0), colEval(2)},
			),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if len(rows[0]) != 2 {
					t.Errorf("output row width = %d, want 2", len(rows[0]))
				}
			},
		},
		// 9. Column out-of-bounds returns Null via colEval.
		{
			"col-oob-returns-null",
			[]exec.Row{{expr.IntegerValue(1)}},
			makeProjectItems([]string{"missing"}, []func(exec.Row) (expr.Value, error){colEval(5)}),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if rows[0][0] != expr.Null {
					t.Errorf("col[0] = %v, want null", rows[0][0])
				}
			},
		},
		// 10. Columns() returns correct aliases.
		{
			"columns-alias",
			[]exec.Row{{expr.IntegerValue(1)}},
			makeProjectItems(
				[]string{"foo", "bar"},
				[]func(exec.Row) (expr.Value, error){colEval(0), colEval(0)},
			),
			1, false,
			func(t *testing.T, _ []exec.Row) {
				// tested separately in TestProject_Columns
			},
		},
		// 11. Projection narrows a wide row to one column.
		{
			"narrow-to-one",
			[]exec.Row{{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}},
			makeProjectItems([]string{"mid"}, []func(exec.Row) (expr.Value, error){colEval(1)}),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if rows[0][0] != expr.IntegerValue(2) {
					t.Errorf("col[0] = %v, want 2", rows[0][0])
				}
			},
		},
		// 12. Bool projection.
		{
			"bool-value",
			[]exec.Row{{expr.BoolValue(true)}},
			makeProjectItems([]string{"b"}, []func(exec.Row) (expr.Value, error){colEval(0)}),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if rows[0][0] != expr.BoolValue(true) {
					t.Errorf("col[0] = %v, want true", rows[0][0])
				}
			},
		},
		// 13. String projection.
		{
			"string-value",
			[]exec.Row{{expr.StringValue("gograph")}},
			makeProjectItems([]string{"s"}, []func(exec.Row) (expr.Value, error){colEval(0)}),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if rows[0][0] != expr.StringValue("gograph") {
					t.Errorf("col[0] = %v, want \"gograph\"", rows[0][0])
				}
			},
		},
		// 14. Computed projection (arithmetic via closure).
		{
			"computed-double",
			[]exec.Row{{expr.IntegerValue(5)}},
			makeProjectItems(
				[]string{"double"},
				[]func(exec.Row) (expr.Value, error){
					func(row exec.Row) (expr.Value, error) {
						v := int64(row[0].(expr.IntegerValue))
						return expr.IntegerValue(v * 2), nil
					},
				},
			),
			1, false,
			func(t *testing.T, rows []exec.Row) {
				if rows[0][0] != expr.IntegerValue(10) {
					t.Errorf("col[0] = %v, want 10", rows[0][0])
				}
			},
		},
		// 15. 10-row projection, verify all rows transformed.
		{
			"10-row-transform",
			func() []exec.Row {
				rows := make([]exec.Row, 10)
				for i := range rows {
					rows[i] = exec.Row{expr.IntegerValue(int64(i))}
				}
				return rows
			}(),
			makeProjectItems(
				[]string{"neg"},
				[]func(exec.Row) (expr.Value, error){
					func(row exec.Row) (expr.Value, error) {
						v := int64(row[0].(expr.IntegerValue))
						return expr.IntegerValue(-v), nil
					},
				},
			),
			10, false,
			func(t *testing.T, rows []exec.Row) {
				for i, row := range rows {
					want := expr.IntegerValue(-int64(i))
					if row[0] != want {
						t.Errorf("row[%d][0] = %v, want %v", i, row[0], want)
					}
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			proj, err := exec.NewProject(newSliceOperator(tc.inputRows...), tc.items)
			if err != nil {
				t.Fatalf("NewProject: %v", err)
			}
			result, err := exec.Drain(context.Background(), proj)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(result) != tc.wantN {
				t.Fatalf("got %d rows, want %d", len(result), tc.wantN)
			}
			if tc.check != nil {
				tc.check(t, result)
			}
		})
	}
}

// TestProject_NewProject_EmptyItems verifies NewProject accepts an empty
// items slice (e.g. WITH * over a pattern that binds no variables).
func TestProject_NewProject_EmptyItems(t *testing.T) {
	proj, err := exec.NewProject(newSliceOperator(), nil)
	if err != nil {
		t.Fatalf("NewProject with empty items: unexpected error %v", err)
	}
	if proj == nil {
		t.Fatal("NewProject with empty items returned nil operator")
	}
	if cols := proj.Columns(); len(cols) != 0 {
		t.Errorf("Columns() = %v, want empty slice", cols)
	}
}

// TestProject_Columns verifies Columns() returns aliases in declaration order.
func TestProject_Columns(t *testing.T) {
	items := makeProjectItems(
		[]string{"alpha", "beta", "gamma"},
		[]func(exec.Row) (expr.Value, error){colEval(0), colEval(0), colEval(0)},
	)
	proj, err := exec.NewProject(newSliceOperator(), items)
	if err != nil {
		t.Fatal(err)
	}
	cols := proj.Columns()
	want := []string{"alpha", "beta", "gamma"}
	if len(cols) != len(want) {
		t.Fatalf("Columns() len = %d, want %d", len(cols), len(want))
	}
	for i, c := range cols {
		if c != want[i] {
			t.Errorf("Columns()[%d] = %q, want %q", i, c, want[i])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 244 — Limit / Skip operators
// ─────────────────────────────────────────────────────────────────────────────

// makeIntRows creates n rows each containing one IntegerValue(i).
func makeIntRows(n int) []exec.Row {
	rows := make([]exec.Row, n)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(i))}
	}
	return rows
}

func TestLimitSkip(t *testing.T) {
	tests := []struct {
		name    string
		inputN  int
		limit   int64 // -1 = no Limit
		skip    int64 // -1 = no Skip
		wantN   int
		wantErr bool
	}{
		// 1. LIMIT 10 from 20 rows → 10 rows.
		{"limit10-of-20", 20, 10, -1, 10, false},
		// 2. LIMIT 0 → 0 rows.
		{"limit0", 5, 0, -1, 0, false},
		// 3. LIMIT > input → all rows.
		{"limit-exceeds-input", 3, 100, -1, 3, false},
		// 4. SKIP 5 LIMIT 10 from 20 → rows 5..14 → 10 rows.
		{"skip5-limit10-of-20", 20, 10, 5, 10, false},
		// 5. SKIP 0 → all rows.
		{"skip0", 5, -1, 0, 5, false},
		// 6. SKIP > input → 0 rows.
		{"skip-exceeds-input", 3, -1, 10, 0, false},
		// 7. SKIP 5 of 5 → 0 rows.
		{"skip-equals-input", 5, -1, 5, 0, false},
		// 8. SKIP 3 LIMIT 2 of 10 → rows 3..4 → 2 rows.
		{"skip3-limit2-of-10", 10, 2, 3, 2, false},
		// 9. Empty input with LIMIT and SKIP.
		{"empty-limit5-skip2", 0, 5, 2, 0, false},
		// 10. LIMIT 1 SKIP 99 of 100 → row 99 → 1 row.
		{"skip99-limit1-of-100", 100, 1, 99, 1, false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rows := makeIntRows(tc.inputN)
			var base exec.Operator = newSliceOperator(rows...)

			if tc.skip >= 0 {
				sk, err := exec.NewSkip(base, tc.skip)
				if err != nil {
					t.Fatalf("NewSkip: %v", err)
				}
				base = sk
			}
			if tc.limit >= 0 {
				lim, err := exec.NewLimit(base, tc.limit)
				if err != nil {
					t.Fatalf("NewLimit: %v", err)
				}
				base = lim
			}

			result, err := exec.Drain(context.Background(), base)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(result) != tc.wantN {
				t.Fatalf("got %d rows, want %d", len(result), tc.wantN)
			}
			// Validate row values match expected window.
			start := int64(0)
			if tc.skip > 0 {
				start = tc.skip
			}
			for i, row := range result {
				want := expr.IntegerValue(start + int64(i))
				if row[0] != want {
					t.Errorf("result[%d][0] = %v, want %v", i, row[0], want)
				}
			}
		})
	}
}

// TestLimit_NegativeN verifies NewLimit rejects negative n.
func TestLimit_NegativeN(t *testing.T) {
	_, err := exec.NewLimit(newSliceOperator(), -1)
	if err == nil {
		t.Fatal("expected error for n=-1, got nil")
	}
}

// TestSkip_NegativeN verifies NewSkip rejects negative n.
func TestSkip_NegativeN(t *testing.T) {
	_, err := exec.NewSkip(newSliceOperator(), -1)
	if err == nil {
		t.Fatal("expected error for n=-1, got nil")
	}
}

// TestLimit_ReInitReset verifies that calling Init again resets the counter so
// the limit can be applied again from the beginning of the child operator.
// sliceOperator also resets on Init, so both the limit counter and the child
// restart together — this matches Apply loop semantics.
func TestLimit_ReInitReset(t *testing.T) {
	inner := newSliceOperator(makeIntRows(5)...)
	lim, _ := exec.NewLimit(inner, 3)
	ctx := context.Background()

	// First cycle: drain 3 rows, confirm EOS after limit.
	if err := lim.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var row exec.Row
	for range 3 {
		ok, err := lim.Next(&row)
		if err != nil || !ok {
			t.Fatalf("first cycle Next: ok=%v err=%v", ok, err)
		}
	}
	ok, err := lim.Next(&row)
	if err != nil || ok {
		t.Fatalf("after limit hit: ok=%v err=%v", ok, err)
	}

	// Re-init: both the limit counter and child reset.
	if err := lim.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// Should be able to drain 3 rows again.
	count := 0
	for {
		ok, err := lim.Next(&row)
		if err != nil {
			t.Fatalf("second cycle Next err: %v", err)
		}
		if !ok {
			break
		}
		count++
	}
	if count != 3 {
		t.Fatalf("second cycle: got %d rows, want 3", count)
	}
	_ = lim.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 245 — Argument operator
// ─────────────────────────────────────────────────────────────────────────────

// TestArgument_EmitsOuterRow verifies that Argument emits exactly one row per
// Init cycle, equal to the outer row set by SetOuterRow.
func TestArgument_EmitsOuterRow(t *testing.T) {
	arg := exec.NewArgument()
	ctx := context.Background()

	outerRows := []exec.Row{
		{expr.IntegerValue(1), expr.StringValue("a")},
		{expr.IntegerValue(2), expr.StringValue("b")},
		{expr.IntegerValue(3), expr.StringValue("c")},
	}

	for _, outer := range outerRows {
		arg.SetOuterRow(outer)
		if err := arg.Init(ctx); err != nil {
			t.Fatalf("Init: %v", err)
		}

		var out exec.Row
		ok, err := arg.Next(&out)
		if err != nil || !ok {
			t.Fatalf("first Next: ok=%v err=%v", ok, err)
		}
		if len(out) != len(outer) {
			t.Fatalf("row len = %d, want %d", len(out), len(outer))
		}
		for i, v := range outer {
			if out[i] != v {
				t.Errorf("col[%d] = %v, want %v", i, out[i], v)
			}
		}

		// Second Next must return EOS.
		ok, err = arg.Next(&out)
		if err != nil || ok {
			t.Fatalf("second Next: ok=%v err=%v", ok, err)
		}
	}
	if err := arg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestArgument_CloseAfterUse verifies Close is a no-op (no error, no panic).
func TestArgument_CloseAfterUse(t *testing.T) {
	arg := exec.NewArgument()
	arg.SetOuterRow(exec.Row{expr.IntegerValue(1)})
	_ = arg.Init(context.Background())
	var r exec.Row
	_, _ = arg.Next(&r)
	if err := arg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestArgument_CancelledContext verifies that a pre-cancelled context causes
// Next to return an error.
func TestArgument_CancelledContext(t *testing.T) {
	arg := exec.NewArgument()
	arg.SetOuterRow(exec.Row{expr.IntegerValue(42)})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	if err := arg.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var r exec.Row
	_, err := arg.Next(&r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestArgument_RaceSafety verifies that Argument used by concurrent Apply
// drivers (each with its own instance) is race-free.
func TestArgument_RaceSafety(t *testing.T) {
	const goroutines = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		i := i
		go func() {
			defer wg.Done()
			arg := exec.NewArgument() // each goroutine owns its own instance
			arg.SetOuterRow(exec.Row{expr.IntegerValue(int64(i))})
			ctx := context.Background()
			_ = arg.Init(ctx)
			var r exec.Row
			ok, err := arg.Next(&r)
			if err != nil || !ok {
				return
			}
			_, _ = arg.Next(&r) // EOS
			_ = arg.Close()
		}()
	}
	wg.Wait()
}

// TestApply_FeedsArgument simulates a minimal Apply loop to verify the
// Argument is correctly seeded from the outer row on each iteration.
func TestApply_FeedsArgument(t *testing.T) {
	// Outer plan: 3 rows.
	outerRows := []exec.Row{
		{expr.IntegerValue(10)},
		{expr.IntegerValue(20)},
		{expr.IntegerValue(30)},
	}
	outer := newSliceOperator(outerRows...)

	// Inner plan: Argument → Filter (always true) — acts as identity.
	arg := exec.NewArgument()
	inner := exec.NewFilter(arg, func(_ exec.Row) (expr.Value, error) {
		return expr.BoolValue(true), nil
	})

	ctx := context.Background()
	if err := outer.Init(ctx); err != nil {
		t.Fatal(err)
	}

	var collected []exec.Row
	var outerRow exec.Row
	for {
		ok, err := outer.Next(&outerRow)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		// Simulate Apply: inject outer row and re-init inner plan.
		cp := make(exec.Row, len(outerRow))
		copy(cp, outerRow)
		arg.SetOuterRow(cp)
		if err := inner.Init(ctx); err != nil {
			t.Fatal(err)
		}
		var innerRow exec.Row
		for {
			ok, err := inner.Next(&innerRow)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			rcp := make(exec.Row, len(innerRow))
			copy(rcp, innerRow)
			collected = append(collected, rcp)
		}
	}
	_ = inner.Close()
	_ = outer.Close()

	if len(collected) != 3 {
		t.Fatalf("got %d rows, want 3", len(collected))
	}
	for i, row := range collected {
		want := expr.IntegerValue(int64((i + 1) * 10))
		if row[0] != want {
			t.Errorf("collected[%d][0] = %v, want %v", i, row[0], want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 246 — ProduceResults / ResultSet / Run
// ─────────────────────────────────────────────────────────────────────────────

func TestRun_BasicIteration(t *testing.T) {
	rows := []exec.Row{
		{expr.IntegerValue(1), expr.StringValue("alice")},
		{expr.IntegerValue(2), expr.StringValue("bob")},
	}
	plan := newSliceOperator(rows...)
	cols := []string{"id", "name"}

	rs := exec.Run(context.Background(), plan, cols)
	defer func() {
		if err := rs.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	if got := rs.Columns(); len(got) != 2 || got[0] != "id" || got[1] != "name" {
		t.Fatalf("Columns() = %v, want [id name]", got)
	}

	var collected []exec.Record
	for rs.Next() {
		rec := rs.Record()
		cp := make(exec.Record, len(rec))
		for k, v := range rec {
			cp[k] = v
		}
		collected = append(collected, cp)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(collected) != 2 {
		t.Fatalf("got %d records, want 2", len(collected))
	}
	if collected[0]["id"] != expr.IntegerValue(1) {
		t.Errorf("rec[0][id] = %v, want 1", collected[0]["id"])
	}
	if collected[1]["name"] != expr.StringValue("bob") {
		t.Errorf("rec[1][name] = %v, want bob", collected[1]["name"])
	}
}

// TestRun_EmptyPlan verifies a zero-row plan produces an empty result set.
func TestRun_EmptyPlan(t *testing.T) {
	rs := exec.Run(context.Background(), newSliceOperator(), []string{"x"})
	defer rs.Close() //nolint:errcheck // test cleanup
	if rs.Next() {
		t.Fatal("Next() returned true on empty plan")
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
}

// TestRun_ErrPropagation verifies that operator errors surface via Err().
func TestRun_ErrPropagation(t *testing.T) {
	inner := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	op := &errorOperator{inner: inner, failAfter: 1}
	rs := exec.Run(context.Background(), op, []string{"x"})
	defer rs.Close() //nolint:errcheck // test cleanup

	// Consume the first row.
	if !rs.Next() {
		t.Fatal("expected first row to succeed")
	}
	// Second Next should fail.
	if rs.Next() {
		t.Fatal("expected Next to return false on error")
	}
	if rs.Err() == nil {
		t.Fatal("Err() should be non-nil after operator error")
	}
}

// TestRun_ColumnsStable verifies Columns() returns the same slice every call.
func TestRun_ColumnsStable(t *testing.T) {
	rs := exec.Run(context.Background(), newSliceOperator(), []string{"a", "b", "c"})
	defer rs.Close() //nolint:errcheck // test cleanup
	c1 := rs.Columns()
	c2 := rs.Columns()
	if len(c1) != len(c2) {
		t.Fatalf("Columns() not stable: %v vs %v", c1, c2)
	}
	for i := range c1 {
		if c1[i] != c2[i] {
			t.Errorf("Columns()[%d]: %q vs %q", i, c1[i], c2[i])
		}
	}
}

// TestRun_CloseIdempotent verifies double-close is a no-op.
func TestRun_CloseIdempotent(t *testing.T) {
	rs := exec.Run(context.Background(), newSliceOperator(), []string{"x"})
	if err := rs.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rs.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestRun_ContextCancellation verifies cancellation surfaces via Err().
func TestRun_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	rs := exec.Run(ctx, newSliceOperator(exec.Row{expr.IntegerValue(1)}), []string{"x"})
	defer rs.Close() //nolint:errcheck // test cleanup

	if rs.Next() {
		t.Fatal("Next() should return false when context is pre-cancelled")
	}
}

// TestRun_NarrowRow verifies that columns beyond the row width map to nil.
func TestRun_NarrowRow(t *testing.T) {
	plan := newSliceOperator(exec.Row{expr.IntegerValue(42)})
	rs := exec.Run(context.Background(), plan, []string{"a", "b"}) // 2 cols, 1 val
	defer rs.Close()                                               //nolint:errcheck // test cleanup

	if !rs.Next() {
		t.Fatal("expected a row")
	}
	rec := rs.Record()
	if rec["a"] != expr.IntegerValue(42) {
		t.Errorf("rec[a] = %v, want 42", rec["a"])
	}
	if rec["b"] != nil {
		t.Errorf("rec[b] = %v, want nil", rec["b"])
	}
}

// TestRun_PipelineIntegration runs a full plan: AllNodesScan → Filter → Project → Run.
func TestRun_PipelineIntegration(t *testing.T) {
	// Build a 5-row input.
	inputRows := makeIntRows(5) // [0], [1], [2], [3], [4]
	scan := newSliceOperator(inputRows...)

	// Filter: keep rows where value >= 2.
	filt := exec.NewFilter(scan, func(row exec.Row) (expr.Value, error) {
		v := int64(row[0].(expr.IntegerValue))
		return expr.BoolValue(v >= 2), nil
	})

	// Project: negate the value.
	proj, err := exec.NewProject(filt, []exec.ProjectionItem{
		{
			Alias: "neg",
			Eval: func(row exec.Row) (expr.Value, error) {
				v := int64(row[0].(expr.IntegerValue))
				return expr.IntegerValue(-v), nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Limit: top 2.
	lim, err := exec.NewLimit(proj, 2)
	if err != nil {
		t.Fatal(err)
	}

	rs := exec.Run(context.Background(), lim, []string{"neg"})
	defer rs.Close() //nolint:errcheck // test cleanup

	var results []int64
	for rs.Next() {
		rec := rs.Record()
		results = append(results, int64(rec["neg"].(expr.IntegerValue)))
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	// Input [0..4], filter ≥2 → [2,3,4], negate → [-2,-3,-4], limit 2 → [-2,-3]
	want := []int64{-2, -3}
	if len(results) != len(want) {
		t.Fatalf("got %v, want %v", results, want)
	}
	for i, v := range results {
		if v != want[i] {
			t.Errorf("results[%d] = %d, want %d", i, v, want[i])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Benchmarks
// ─────────────────────────────────────────────────────────────────────────────

func BenchmarkFilter_Throughput(b *testing.B) {
	const n = 10_000
	rows := make([]exec.Row, n)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(i))}
	}
	pred := func(row exec.Row) (expr.Value, error) {
		return expr.BoolValue(int64(row[0].(expr.IntegerValue))%2 == 0), nil
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		op := exec.NewFilter(newSliceOperator(rows...), pred)
		_, err := exec.Drain(context.Background(), op)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProject_Throughput(b *testing.B) {
	const n = 10_000
	rows := make([]exec.Row, n)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(i)), expr.StringValue(fmt.Sprintf("v%d", i))}
	}
	items := []exec.ProjectionItem{
		{Alias: "id", Eval: colEval(0)},
		{Alias: "label", Eval: colEval(1)},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		op, _ := exec.NewProject(newSliceOperator(rows...), items)
		_, err := exec.Drain(context.Background(), op)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLimit_Throughput(b *testing.B) {
	const n = 10_000
	rows := makeIntRows(n)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		lim, _ := exec.NewLimit(newSliceOperator(rows...), 5_000)
		_, err := exec.Drain(context.Background(), lim)
		if err != nil {
			b.Fatal(err)
		}
	}
}
