package exec_test

// row_pull_alloc_test.go pins the allocation cost of a parent operator's per-row
// child pull (rmp #2926). The receiver a parent passes to its child's Next used to
// be a fresh local (`var row Row; child.Next(&row)`), which escape analysis moves
// to the heap: one 24-byte slice header per row pulled. The receiver is now an
// operator-owned field reached through nextRow, so the pull itself allocates
// nothing and only the operator's own, documented per-row work remains.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// rowPullAllocRows is the number of rows each measured drain pulls. It is large
// enough that a single per-drain allocation contributes well under the tolerance.
const rowPullAllocRows = 512

// drainAllocsPerRow initialises op, drains it with one reused receiver, and
// returns the average number of heap allocations per row pulled from the child
// the test cares about (perRowDivisor rows per drain).
func drainAllocsPerRow(t *testing.T, op exec.Operator, perRowDivisor int) float64 {
	t.Helper()
	ctx := context.Background()
	var out exec.Row
	var emitted int
	drain := func() {
		if err := op.Init(ctx); err != nil {
			t.Fatalf("Init: %v", err)
		}
		emitted = 0
		for {
			ok, err := op.Next(&out)
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if !ok {
				break
			}
			emitted++
		}
	}
	drain() // warm every lazily sized buffer before measuring
	allocs := testing.AllocsPerRun(10, drain)
	return allocs / float64(perRowDivisor)
}

func integerRows(n int, v func(i int) int64) []exec.Row {
	rows := make([]exec.Row, n)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(v(i))}
	}
	return rows
}

// TestRowPull_AllocationPerRow pins the per-row allocation of the operators whose
// child pull the sweep measured (Apply, Expand) and of a representative
// short-circuit apply (SemiApply). Before rmp #2926 the budgets were exceeded by
// exactly one allocation per pulled row (two for SemiApply, which pulls twice).
func TestRowPull_AllocationPerRow(t *testing.T) {
	// No t.Parallel: testing.AllocsPerRun panics in a parallel test.
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under the race detector")
	}
	const tolerance = 0.05 // one per-drain allocation over 512 rows is ~0.002

	cases := []struct {
		name string
		// build returns the operator and the number of child pulls per drain the
		// budget is expressed against.
		build func() (exec.Operator, int)
		// budget is the operator's own per-row allocation, excluding the pull.
		budget float64
		// emitted is the number of rows the drain must produce, so a drain that
		// silently produced nothing cannot pass.
		emitted int
	}{
		{
			// One outer row, rowPullAllocRows inner rows: every emitted row is an
			// inner pull, and buildRow reuses one output buffer.
			name: "Apply/inner",
			build: func() (exec.Operator, int) {
				outer := newSliceOperator(exec.Row{expr.IntegerValue(1)})
				inner := newSliceOperator(integerRows(rowPullAllocRows, func(i int) int64 { return int64(i) })...)
				return exec.NewApply(outer, inner, exec.NewArgument()), rowPullAllocRows
			},
			budget:  0,
			emitted: rowPullAllocRows,
		},
		{
			// Every input row names an isolated node, so each Next pull loads an
			// empty adjacency and emits nothing: the drain is pure input pulls.
			name: "Expand/input",
			build: func() (exec.Operator, int) {
				fwd := buildCSR(8, [][2]int{{0, 1}})
				rev := buildCSR(8, [][2]int{{1, 0}})
				input := newSliceOperator(integerRows(rowPullAllocRows, func(int) int64 { return 5 })...)
				return exec.NewExpand(input, exec.StaticAdjacency(fwd, rev, nil), exec.ExpandConfig{
					Direction: exec.DirOut,
					InputCol:  0,
				}), rowPullAllocRows
			},
			budget:  0,
			emitted: 0,
		},
		{
			// Each outer row costs one owned snapshot (the documented copy that
			// seeds the Argument); the outer pull and the inner probe cost nothing.
			name: "SemiApply/outer+inner",
			build: func() (exec.Operator, int) {
				outer := newSliceOperator(integerRows(rowPullAllocRows, func(i int) int64 { return int64(i) })...)
				inner := newSliceOperator(exec.Row{expr.IntegerValue(0)})
				return exec.NewSemiApply(outer, inner, exec.NewArgument()), rowPullAllocRows
			},
			budget:  1,
			emitted: rowPullAllocRows,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, pulls := tc.build()
			got := drainAllocsPerRow(t, op, pulls)
			t.Logf("%.3f allocations per pulled row (budget %.0f)", got, tc.budget)
			rows, err := exec.Drain(context.Background(), op)
			if err != nil {
				t.Fatalf("Drain: %v", err)
			}
			if len(rows) != tc.emitted {
				t.Fatalf("drain emitted %d rows, want %d", len(rows), tc.emitted)
			}
			if got > tc.budget+tolerance {
				t.Errorf("%.3f allocations per pulled row; want at most %.0f (+%.2f tolerance)", got, tc.budget, tolerance)
			}
		})
	}
}
