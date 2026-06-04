package exec_test

// eager_aggregation_budget_test.go — regression coverage for the per-group
// element budget enforced by buffering aggregators inside EagerAggregation
// (task-1294).
//
// The group-count cap (maxGroups) bounds the NUMBER of groups, but a
// grouping-key-free aggregate such as `RETURN collect(n)` forms exactly one
// group, so that cap never fires. Without a per-aggregator element budget, the
// single CollectAgg would buffer every input row into one unbounded list inside
// the visibility barrier. These tests pin that the budget trips during the
// blocking consume phase (surfaced through Next) rather than after the whole
// list has been built.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// countingSource emits n single-column rows, each carrying its 0-based index as
// an IntegerValue. It never materialises all rows at once, so a large n stays
// cheap.
type countingSource struct {
	n   int
	idx int
}

func (s *countingSource) Init(_ context.Context) error { s.idx = 0; return nil }
func (s *countingSource) Close() error                 { return nil }
func (s *countingSource) Next(out *exec.Row) (bool, error) {
	if s.idx >= s.n {
		return false, nil
	}
	*out = exec.Row{expr.IntegerValue(int64(s.idx))}
	s.idx++
	return true, nil
}

// drain runs op to completion and returns the emitted rows or the first error.
func drain(t *testing.T, op exec.Operator) ([]exec.Row, error) {
	t.Helper()
	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() {
		if err := op.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	var rows []exec.Row
	for {
		var r exec.Row
		ok, err := op.Next(&r)
		if err != nil {
			return rows, err
		}
		if !ok {
			return rows, nil
		}
		rows = append(rows, r)
	}
}

// TestEagerAggregation_CollectBudgetExceeded is the task-1294 acceptance
// criterion: an EagerAggregation with empty keyCols (one global group) and a
// CollectAgg factory must error once the per-group element budget is exceeded,
// rather than silently building an N-element list.
//
// Pre-fix (CollectAgg.Step had no cap) this builds the full list and never
// errors; post-fix it returns ErrCollectItemsExceeded from Next.
func TestEagerAggregation_CollectBudgetExceeded(t *testing.T) {
	t.Parallel()

	const budget = 8
	const rows = budget + 5 // strictly more than the budget

	child := &countingSource{n: rows}
	op, err := exec.NewEagerAggregation(
		child,
		nil, // empty keyCols → exactly one global group
		[]funcs.AggregatorFactory{funcs.NewCollectAggN(budget)},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}

	_, gotErr := drain(t, op)
	if !errors.Is(gotErr, funcs.ErrCollectItemsExceeded) {
		t.Fatalf("collect over %d rows with budget %d: got error %v, want ErrCollectItemsExceeded",
			rows, budget, gotErr)
	}
}

// TestEagerAggregation_CollectUnderBudget is the control: a small collect that
// stays within the budget returns the full list with no error.
func TestEagerAggregation_CollectUnderBudget(t *testing.T) {
	t.Parallel()

	const budget = 8
	const rows = budget - 3 // safely under the budget

	child := &countingSource{n: rows}
	op, err := exec.NewEagerAggregation(
		child,
		nil,
		[]funcs.AggregatorFactory{funcs.NewCollectAggN(budget)},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}

	out, drainErr := drain(t, op)
	if drainErr != nil {
		t.Fatalf("collect over %d rows with budget %d: unexpected error %v", rows, budget, drainErr)
	}
	if len(out) != 1 {
		t.Fatalf("got %d output rows, want exactly 1 global-aggregate row", len(out))
	}
	list, ok := out[0][0].(expr.ListValue)
	if !ok {
		t.Fatalf("aggregate column is %T, want expr.ListValue", out[0][0])
	}
	if len(list) != rows {
		t.Fatalf("collected list has %d elements, want %d", len(list), rows)
	}
}

// TestEagerAggregation_CollectAtBudget pins the exact boundary: collecting
// exactly budget elements succeeds, and one more trips the cap. This proves the
// cap counts elements, not rows-after-the-limit, and that the boundary is
// inclusive of the budget value.
func TestEagerAggregation_CollectAtBudget(t *testing.T) {
	t.Parallel()

	const budget = 6

	t.Run("exactly_budget_ok", func(t *testing.T) {
		t.Parallel()
		op, err := exec.NewEagerAggregation(
			&countingSource{n: budget},
			nil,
			[]funcs.AggregatorFactory{funcs.NewCollectAggN(budget)},
			0,
		)
		if err != nil {
			t.Fatalf("NewEagerAggregation: %v", err)
		}
		out, drainErr := drain(t, op)
		if drainErr != nil {
			t.Fatalf("collect of exactly %d: unexpected error %v", budget, drainErr)
		}
		if list, ok := out[0][0].(expr.ListValue); !ok || len(list) != budget {
			t.Fatalf("collect of exactly %d: got %v, want %d-element list", budget, out[0][0], budget)
		}
	})

	t.Run("budget_plus_one_errors", func(t *testing.T) {
		t.Parallel()
		op, err := exec.NewEagerAggregation(
			&countingSource{n: budget + 1},
			nil,
			[]funcs.AggregatorFactory{funcs.NewCollectAggN(budget)},
			0,
		)
		if err != nil {
			t.Fatalf("NewEagerAggregation: %v", err)
		}
		if _, drainErr := drain(t, op); !errors.Is(drainErr, funcs.ErrCollectItemsExceeded) {
			t.Fatalf("collect of %d with budget %d: got %v, want ErrCollectItemsExceeded", budget+1, budget, drainErr)
		}
	})
}

// TestEagerAggregation_PercentileBudgetExceeded mirrors the collect AC for a
// percentile aggregator, which buffers all values just like collect.
func TestEagerAggregation_PercentileBudgetExceeded(t *testing.T) {
	t.Parallel()

	const budget = 8
	const rows = budget + 5

	for _, tc := range []struct {
		name    string
		factory funcs.AggregatorFactory
	}{
		{"percentileCont", funcs.NewPercentileContAggN(0.5, budget)},
		{"percentileDisc", funcs.NewPercentileDiscAggN(0.5, budget)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op, err := exec.NewEagerAggregation(
				&countingSource{n: rows},
				nil,
				[]funcs.AggregatorFactory{tc.factory},
				0,
			)
			if err != nil {
				t.Fatalf("NewEagerAggregation: %v", err)
			}
			if _, drainErr := drain(t, op); !errors.Is(drainErr, funcs.ErrCollectItemsExceeded) {
				t.Fatalf("%s over %d rows with budget %d: got %v, want ErrCollectItemsExceeded",
					tc.name, rows, budget, drainErr)
			}
		})
	}
}

// TestEagerAggregation_BudgetDisabled confirms the explicit opt-out: a zero
// budget disables the cap, so a collect larger than DefaultMaxCollectItems would
// be allowed (verified here with a small input and a zero budget — the cap must
// not trip).
func TestEagerAggregation_BudgetDisabled(t *testing.T) {
	t.Parallel()

	const rows = 32

	op, err := exec.NewEagerAggregation(
		&countingSource{n: rows},
		nil,
		[]funcs.AggregatorFactory{funcs.NewCollectAggN(0)}, // 0 disables the cap
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	out, drainErr := drain(t, op)
	if drainErr != nil {
		t.Fatalf("collect with disabled cap: unexpected error %v", drainErr)
	}
	if list, ok := out[0][0].(expr.ListValue); !ok || len(list) != rows {
		t.Fatalf("collect with disabled cap: got %v, want %d-element list", out[0][0], rows)
	}
}
