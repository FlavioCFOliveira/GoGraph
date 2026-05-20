package exec

// apply.go — Apply (dependent join) operator (task-257).
//
// Apply is the Volcano-model equivalent of a dependent (correlated) join. For
// each row produced by the outer (left) plan, it:
//
//  1. Seeds the [Argument] operator at the root of the inner (right) plan with
//     the current outer row.
//  2. Re-initialises the entire inner plan via a single Init call (which
//     propagates down to Argument.Init, resetting its emit flag).
//  3. Drains all rows from the inner plan, emitting each combined
//     (outer || inner) row to the caller.
//
// The outer row is injected exclusively through [Argument]; the inner sub-plan
// must not reference any outer state through any other mechanism.
//
// # Schema
//
// Output row = outerRow || innerRow.
// The width of the output varies with the inner plan's output schema.
//
// # Concurrency
//
// Apply is NOT safe for concurrent use.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call. Long inner drains
// propagate cancellation through the inner plan's own operator chain.

import (
	"context"

	"gograph/cypher/expr"
)

// Apply is a Volcano pipeline operator that performs a dependent (correlated)
// join: for each outer row, it re-runs the inner plan seeded with that row and
// emits one output row per inner result.
//
// Apply is NOT safe for concurrent use.
type Apply struct {
	outer Operator
	inner Operator
	arg   *Argument

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// current state
	outerRow Row  // current outer row; nil when no outer row has been fetched
	outerEOS bool // true after outer plan is exhausted
	outBuf   []expr.Value
}

// NewApply creates an Apply operator.
//   - outer is the left (driving) plan.
//   - inner is the right (sub) plan; its leaf must be the provided arg.
//   - arg is the [Argument] node at the root of inner; Apply seeds it before
//     each inner Init call.
//
// Apply takes ownership of both plans. The caller must not use outer or inner
// directly after calling NewApply.
func NewApply(outer, inner Operator, arg *Argument) *Apply {
	return &Apply{
		outer: outer,
		inner: inner,
		arg:   arg,
	}
}

// Init initialises both the outer plan and stores ctx for subsequent Next calls.
// The inner plan is initialised lazily on the first outer row.
func (op *Apply) Init(ctx context.Context) error {
	op.ctx = ctx
	op.outerRow = nil
	op.outerEOS = false
	op.outBuf = op.outBuf[:0]
	return op.outer.Init(ctx)
}

// Next advances the Apply operator:
//   - If there is no current inner row available, it pulls the next outer row,
//     seeds and re-inits the inner plan, then pulls from the inner plan.
//   - Returns each combined (outer || inner) row.
//   - Returns (false, nil) when the outer plan is exhausted.
func (op *Apply) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// If we have an active outer row, try to get the next inner row.
		if op.outerRow != nil {
			innerRow := Row{}
			ok, err := op.inner.Next(&innerRow)
			if err != nil {
				return false, err
			}
			if ok {
				op.buildRow(out, op.outerRow, innerRow)
				return true, nil
			}
			// Inner exhausted for this outer row; move to the next outer row.
			op.outerRow = nil
		}

		if op.outerEOS {
			return false, nil
		}

		// Pull the next outer row.
		var outerRow Row
		ok, err := op.outer.Next(&outerRow)
		if err != nil {
			return false, err
		}
		if !ok {
			op.outerEOS = true
			return false, nil
		}

		// Copy outer row so we own a stable snapshot across inner iterations.
		cp := make(Row, len(outerRow))
		copy(cp, outerRow)

		// Seed the Argument operator and re-initialise the inner plan.
		op.arg.SetOuterRow(cp)
		if err := op.inner.Init(op.ctx); err != nil {
			return false, err
		}
		op.outerRow = cp
	}
}

// buildRow writes (outerRow... || innerRow...) into out.
func (op *Apply) buildRow(out *Row, outer, inner Row) {
	need := len(outer) + len(inner)
	if cap(op.outBuf) < need {
		op.outBuf = make([]expr.Value, need)
	}
	op.outBuf = op.outBuf[:need]
	copy(op.outBuf, outer)
	copy(op.outBuf[len(outer):], inner)
	*out = op.outBuf
}

// Close releases resources and closes both the outer and inner plans.
func (op *Apply) Close() error {
	outerErr := op.outer.Close()
	innerErr := op.inner.Close()
	op.outerRow = nil
	op.outBuf = nil
	if outerErr != nil {
		return outerErr
	}
	return innerErr
}
