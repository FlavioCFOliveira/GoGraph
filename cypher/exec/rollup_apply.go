package exec

// rollup_apply.go — RollUpApply operator (pattern comprehension execution) (task-259).
//
// RollUpApply implements pattern comprehension: for each outer row, it drains
// the inner sub-plan into an [expr.ListValue] and appends that list as a new
// column to the outer row. An empty inner plan produces an empty list (not
// NULL), consistent with openCypher's pattern comprehension semantics.
//
// # Schema
//
// Output row = outerRow || ListValue(inner rows projected through listEval).
// listEval is called once per inner row to extract the value to collect.
// If listEval is nil, the entire inner Row (as a ListValue of its column
// values) is collected.
//
// # Concurrency
//
// RollUpApply is NOT safe for concurrent use.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and inside the inner drain
// loop.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// RollUpApply is a Volcano pipeline operator that performs pattern-comprehension
// execution: for each outer row, it drains the entire inner sub-plan into a
// [expr.ListValue] and emits (outerRow... || listValue) as a single output row.
//
// RollUpApply is NOT safe for concurrent use.
type RollUpApply struct {
	outer    Operator
	inner    Operator
	arg      *Argument
	listEval func(Row) (expr.Value, error) // optional: extract one value from each inner row

	ctx    context.Context //nolint:containedctx // stored for per-Next ctx check
	outBuf []expr.Value
}

// NewRollUpApply creates a RollUpApply operator.
//   - outer is the driving (left) plan.
//   - inner is the correlated (right) sub-plan whose leaf is arg.
//   - arg is the [Argument] node seeded with each outer row before inner Init.
//   - listEval, when non-nil, is called for each inner row to extract the value
//     to collect into the list. When nil, the first column of each inner row is
//     collected.
func NewRollUpApply(outer, inner Operator, arg *Argument, listEval func(Row) (expr.Value, error)) *RollUpApply {
	return &RollUpApply{
		outer:    outer,
		inner:    inner,
		arg:      arg,
		listEval: listEval,
	}
}

// Init initialises the outer plan.
func (op *RollUpApply) Init(ctx context.Context) error {
	op.ctx = ctx
	op.outBuf = op.outBuf[:0]
	return op.outer.Init(ctx)
}

// Next emits one output row per outer row. For each outer row, the entire inner
// plan is drained into a ListValue appended as a new column.
func (op *RollUpApply) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	var outerRow Row
	ok, err := op.outer.Next(&outerRow)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	// Stable snapshot of the outer row.
	cp := make(Row, len(outerRow))
	copy(cp, outerRow)

	// Seed and re-init the inner plan.
	op.arg.SetOuterRow(cp)
	if err := op.inner.Init(op.ctx); err != nil {
		return false, err
	}

	// Drain inner plan into a list.
	list, err := op.drainInner()
	if err != nil {
		return false, err
	}

	// Build output: outerRow... || list.
	need := len(cp) + 1
	if cap(op.outBuf) < need {
		op.outBuf = make([]expr.Value, need)
	}
	op.outBuf = op.outBuf[:need]
	copy(op.outBuf, cp)
	op.outBuf[len(cp)] = list
	*out = op.outBuf
	return true, nil
}

// drainInner drains all rows from the inner plan into an [expr.ListValue].
// An empty inner plan produces an empty (non-nil) list.
func (op *RollUpApply) drainInner() (expr.ListValue, error) {
	var list expr.ListValue
	var innerRow Row
	for {
		if err := op.ctx.Err(); err != nil {
			return nil, err
		}
		ok, err := op.inner.Next(&innerRow)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		var v expr.Value
		if op.listEval != nil {
			v, err = op.listEval(innerRow)
			if err != nil {
				return nil, err
			}
		} else {
			// Default: collect first column, or Null if row is empty.
			if len(innerRow) > 0 {
				v = innerRow[0]
			} else {
				v = expr.Null
			}
		}
		list = append(list, v)
	}
	if list == nil {
		list = expr.ListValue{} // empty list, not nil
	}
	return list, nil
}

// Close closes both the outer and inner plans.
func (op *RollUpApply) Close() error {
	outerErr := op.outer.Close()
	innerErr := op.inner.Close()
	op.outBuf = nil
	if outerErr != nil {
		return outerErr
	}
	return innerErr
}
