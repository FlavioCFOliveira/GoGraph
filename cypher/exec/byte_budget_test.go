package exec

// byte_budget_test.go — REGRESSION GUARD for the 2026-07-02 audit finding
// (#1841): the pipeline-breaking operators (Sort, Distinct, Eager,
// EagerAggregation, HashJoin) bounded only the COUNT of rows/groups they
// retained, never the estimated BYTES. A handful of rows carrying large values
// could therefore hold tens of gigabytes while the count stayed far below the
// cap and the engine's result-byte budget (charged only at the drain, which runs
// AFTER a breaker finishes buffering) never fired.
//
// Each breaker now accepts the engine's byte budget via WithByteBudget and
// charges an estimated size per retained row/group, returning its own typed
// memory-cap sentinel once the running total exceeds the budget. These white-box
// tests use a fixed per-row estimate so the cap trips after a deterministic
// number of retained rows (the 3rd, at 100 B/row under a 250 B budget), well
// below every operator's row/group COUNT cap — proving the BYTE dimension is
// what fires. A control with a generous budget proves a legitimate workload is
// never rejected.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// fixedEst charges a constant estimated size per row, so a byte budget trips
// after a deterministic number of retained rows regardless of the row's real
// contents.
func fixedEst(bytesPerRow int64) func(Row) int64 {
	return func(Row) int64 { return bytesPerRow }
}

// tenDistinctRows returns 10 rows each with a distinct integer at column 0, so a
// breaker that dedups or groups by column 0 retains all ten.
func tenDistinctRows() []Row {
	rows := make([]Row, 10)
	for i := range rows {
		rows[i] = Row{expr.IntegerValue(int64(i))}
	}
	return rows
}

func TestByteBudget_Sort_Trips(t *testing.T) {
	op, err := NewSort(&sliceSource{rows: tenDistinctRows()}, []SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	op.WithByteBudget(250, fixedEst(100)) // trips at the 3rd buffered row (300 > 250)
	if _, err := Drain(context.Background(), op); !errors.Is(err, ErrSortMemoryExceeded) {
		t.Fatalf("Sort byte budget not enforced: got %v, want ErrSortMemoryExceeded", err)
	}
}

func TestByteBudget_Sort_GenerousCompletes(t *testing.T) {
	op, err := NewSort(&sliceSource{rows: tenDistinctRows()}, []SortKey{{ColIdx: 0, Ascending: true}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	op.WithByteBudget(1_000_000, fixedEst(100))
	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v — a generous byte budget must not reject a legitimate sort", err)
	}
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10", len(rows))
	}
}

func TestByteBudget_Distinct_Trips(t *testing.T) {
	op := NewDistinct(&sliceSource{rows: tenDistinctRows()}, 0).WithByteBudget(250, fixedEst(100))
	if _, err := Drain(context.Background(), op); !errors.Is(err, ErrDistinctMemoryExceeded) {
		t.Fatalf("Distinct byte budget not enforced: got %v, want ErrDistinctMemoryExceeded", err)
	}
}

func TestByteBudget_Distinct_GenerousCompletes(t *testing.T) {
	op := NewDistinct(&sliceSource{rows: tenDistinctRows()}, 0).WithByteBudget(1_000_000, fixedEst(100))
	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10", len(rows))
	}
}

func TestByteBudget_Eager_Trips(t *testing.T) {
	op := NewEager(&sliceSource{rows: tenDistinctRows()}, 0).WithByteBudget(250, fixedEst(100))
	if _, err := Drain(context.Background(), op); !errors.Is(err, ErrEagerMemoryExceeded) {
		t.Fatalf("Eager byte budget not enforced: got %v, want ErrEagerMemoryExceeded", err)
	}
}

func TestByteBudget_Eager_GenerousCompletes(t *testing.T) {
	op := NewEager(&sliceSource{rows: tenDistinctRows()}, 0).WithByteBudget(1_000_000, fixedEst(100))
	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10", len(rows))
	}
}

func TestByteBudget_EagerAggregation_Trips(t *testing.T) {
	op, err := NewEagerAggregation(
		&sliceSource{rows: tenDistinctRows()},
		[]int{0}, // group by column 0 → 10 distinct groups
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	op.WithByteBudget(250, fixedEst(100)) // charges each new group's key; trips at the 3rd group
	if _, err := Drain(context.Background(), op); !errors.Is(err, ErrAggMemoryExceeded) {
		t.Fatalf("EagerAggregation byte budget not enforced: got %v, want ErrAggMemoryExceeded", err)
	}
}

func TestByteBudget_EagerAggregation_GenerousCompletes(t *testing.T) {
	op, err := NewEagerAggregation(
		&sliceSource{rows: tenDistinctRows()},
		[]int{0},
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	op.WithByteBudget(1_000_000, fixedEst(100))
	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("got %d groups, want 10", len(rows))
	}
}

func TestByteBudget_HashJoin_Trips(t *testing.T) {
	build := &sliceSource{rows: tenDistinctRows()} // 10 build rows, all joinable
	probe := &sliceSource{rows: []Row{{expr.IntegerValue(0)}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false).WithByteBudget(250, fixedEst(100))
	if _, err := Drain(context.Background(), hj); !errors.Is(err, ErrHashJoinMemoryExceeded) {
		t.Fatalf("HashJoin byte budget not enforced: got %v, want ErrHashJoinMemoryExceeded", err)
	}
}

func TestByteBudget_HashJoin_GenerousCompletes(t *testing.T) {
	build := &sliceSource{rows: tenDistinctRows()}
	probe := &sliceSource{rows: []Row{{expr.IntegerValue(0)}, {expr.IntegerValue(1)}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false).WithByteBudget(1_000_000, fixedEst(100))
	rows, err := Drain(context.Background(), hj)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Each probe key (0, 1) matches exactly one build row → 2 output rows.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}
