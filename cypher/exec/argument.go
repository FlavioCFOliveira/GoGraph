package exec

// argument.go — Argument operator (sub-plan root for Apply) (task-245).
//
// Argument is the leaf operator at the root of an Apply inner plan. It holds a
// reference to the current outer row injected by the Apply driver and emits
// exactly that one row per Init call. On the next Next call after the row has
// been emitted, it returns end-of-stream.
//
// # Usage
//
// The Apply driver:
//  1. Calls arg.SetOuterRow(outerRow) with the current outer row.
//  2. Re-initialises the inner plan root via innerPlan.Init(ctx), which
//     propagates down to Argument.Init and resets the emitted flag.
//  3. Drives the inner plan to completion via repeated Next calls.
//  4. Repeats for each outer row.
//
// # Concurrency
//
// Argument is NOT safe for concurrent use. Each Apply instance owns its
// Argument exclusively.

import "context"

// Argument is a Volcano leaf operator that emits the single outer Row injected
// by an Apply driver. It emits exactly one row per Init/Next cycle.
//
// Argument is NOT safe for concurrent use.
type Argument struct {
	outerRow Row             // current outer row set by the Apply driver
	emitted  bool            // true after the row has been emitted in the current cycle
	ctx      context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewArgument creates an Argument operator with no initial row. The Apply
// driver must call [SetOuterRow] before the first Init/Next cycle.
func NewArgument() *Argument {
	return &Argument{}
}

// SetOuterRow injects the current outer row. It must be called by the Apply
// driver before each Init/Next cycle for the inner plan.
func (op *Argument) SetOuterRow(row Row) {
	op.outerRow = row
}

// Init resets the emitted flag so that the next Next call returns the outer
// row. It does not require a child operator.
func (op *Argument) Init(ctx context.Context) error {
	op.ctx = ctx
	op.emitted = false
	return nil
}

// Next emits the outer row exactly once per Init call. Subsequent calls return
// (false, nil) until Init is called again.
func (op *Argument) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.emitted {
		return false, nil
	}
	*out = op.outerRow
	op.emitted = true
	return true, nil
}

// Close is a no-op; Argument holds no resources beyond the outer row reference.
func (op *Argument) Close() error {
	op.outerRow = nil
	return nil
}
