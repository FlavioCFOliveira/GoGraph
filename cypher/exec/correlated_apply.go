package exec

// correlated_apply.go — CorrelatedApply and OptionalApply operators (task-392).
//
// CorrelatedApply implements a dependent (correlated) join in which the inner
// sub-plan is expected to have a leading [Argument] operator that re-emits the
// outer row. The inner sub-plan therefore produces rows whose leading columns
// are a copy of the outer row's columns; any additional columns the inner
// pipeline appends represent newly-bound variables.
//
// Unlike [Apply], CorrelatedApply does NOT concatenate outerRow with innerRow:
// the inner row already carries the outer columns at its leading positions
// (because Argument re-emits them), so concatenation would duplicate them.
// CorrelatedApply emits the inner row verbatim.
//
// OptionalApply is the left-outer variant: when the inner sub-plan produces
// zero rows for an outer row, OptionalApply emits a single NULL-extended row
// containing the outer columns followed by NULL placeholders for every column
// the inner pipeline would have introduced.
//
// # Schema
//
// Output row = innerRow (verbatim). The expected layout is
// [outerCol0, outerCol1, …, outerColK-1, innerAddedCol0, …, innerAddedColM-1]
// where K = outerWidth and M = innerExtraCols.
//
// # Concurrency
//
// Both operators are NOT safe for concurrent use.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call. Long inner drains
// propagate cancellation through the inner plan's own operator chain.

import (
	"context"

	"gograph/cypher/expr"
)

// CorrelatedApply is a Volcano pipeline operator that performs a dependent
// (correlated) join with the convention that the inner pipeline begins with an
// [Argument] leaf re-emitting the outer row. The inner row is forwarded
// verbatim as the operator's output; no concatenation with the outer row is
// performed (the outer columns are already present in the inner row).
//
// CorrelatedApply is NOT safe for concurrent use.
type CorrelatedApply struct {
	outer Operator
	inner Operator
	arg   *Argument

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// current state
	outerRow Row  // current outer row; nil when no outer row has been fetched
	outerEOS bool // true after outer plan is exhausted
	outBuf   []expr.Value
}

// NewCorrelatedApply creates a CorrelatedApply operator.
//   - outer is the left (driving) plan.
//   - inner is the right (sub) plan whose leftmost leaf is the provided arg.
//   - arg is the [Argument] node at the inner leaf; CorrelatedApply seeds it
//     before each inner Init call so that the inner pipeline observes the
//     current outer row.
//
// CorrelatedApply takes ownership of both plans. The caller must not use outer
// or inner directly after calling NewCorrelatedApply.
func NewCorrelatedApply(outer, inner Operator, arg *Argument) *CorrelatedApply {
	return &CorrelatedApply{outer: outer, inner: inner, arg: arg}
}

// Init initialises the outer plan and stores ctx for subsequent Next calls.
// The inner plan is initialised lazily on the first outer row.
func (op *CorrelatedApply) Init(ctx context.Context) error {
	op.ctx = ctx
	op.outerRow = nil
	op.outerEOS = false
	op.outBuf = op.outBuf[:0]
	return op.outer.Init(ctx)
}

// Next advances the CorrelatedApply operator. The inner row is forwarded
// verbatim; CorrelatedApply does not concatenate the outer row because the
// inner row already carries the outer columns (the inner pipeline's leaf
// Argument re-emitted them).
func (op *CorrelatedApply) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// If we have an active outer row, try to get the next inner row.
		if op.outerRow != nil {
			var innerRow Row
			ok, err := op.inner.Next(&innerRow)
			if err != nil {
				return false, err
			}
			if ok {
				// Copy the inner row into a stable buffer because the inner
				// operator may reuse its backing slice across Next calls.
				need := len(innerRow)
				if cap(op.outBuf) < need {
					op.outBuf = make([]expr.Value, need)
				}
				op.outBuf = op.outBuf[:need]
				copy(op.outBuf, innerRow)
				*out = op.outBuf
				return true, nil
			}
			// Inner exhausted for this outer row; move to the next.
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

		// Copy the outer row so we own a stable snapshot across inner inits.
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

// Close releases resources and closes both the outer and inner plans.
func (op *CorrelatedApply) Close() error {
	outerErr := op.outer.Close()
	innerErr := op.inner.Close()
	op.outerRow = nil
	op.outBuf = nil
	if outerErr != nil {
		return outerErr
	}
	return innerErr
}

// ─────────────────────────────────────────────────────────────────────────────
// OptionalApply
// ─────────────────────────────────────────────────────────────────────────────

// OptionalApply is the left-outer variant of [CorrelatedApply]. For every
// outer row, the inner pipeline is driven exactly like CorrelatedApply; when
// the inner pipeline produces zero rows for a given outer row, OptionalApply
// emits a single NULL-extended row whose width equals the configured padded
// width, holding the outer columns followed by NULL placeholders for the
// inner-introduced columns.
//
// OptionalApply is NOT safe for concurrent use.
type OptionalApply struct {
	outer       Operator
	inner       Operator
	arg         *Argument
	paddedWidth int // total output width = outerWidth + innerExtraCols

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// per-outer-row state
	outerRow     Row  // stable snapshot of the current outer row
	pendingInner bool // true when inner is initialised for outerRow but not drained
	emittedAny   bool // true when ≥1 inner row was emitted for this outer
	outerEOS     bool // true when outer plan is exhausted

	outBuf []expr.Value
}

// NewOptionalApply creates an OptionalApply operator.
//   - outer is the left (driving) plan.
//   - inner is the right (sub) plan whose leftmost leaf is the provided arg.
//   - arg is the [Argument] node at the inner leaf; OptionalApply seeds it
//     before each inner Init call.
//   - paddedWidth is the total width of an output row, i.e. outerWidth plus
//     the number of columns the inner pipeline introduces. When the inner
//     pipeline emits zero rows for an outer, OptionalApply emits a row of
//     this width whose first outerWidth columns are the outer row and whose
//     trailing columns are [expr.Null].
//
// OptionalApply takes ownership of both plans.
func NewOptionalApply(outer, inner Operator, arg *Argument, paddedWidth int) *OptionalApply {
	return &OptionalApply{
		outer:       outer,
		inner:       inner,
		arg:         arg,
		paddedWidth: paddedWidth,
	}
}

// Init initialises both the outer plan and stores ctx for subsequent Next calls.
func (op *OptionalApply) Init(ctx context.Context) error {
	op.ctx = ctx
	op.outerRow = nil
	op.outerEOS = false
	op.pendingInner = false
	op.emittedAny = false
	op.outBuf = op.outBuf[:0]
	return op.outer.Init(ctx)
}

// Next emits the next output row. The semantics are:
//   - For each outer row, drain the inner pipeline.
//   - If the inner pipeline emits ≥1 row, those rows are forwarded verbatim.
//   - If the inner pipeline emits 0 rows, a single NULL-extended row is emitted
//     consisting of the outer columns followed by [expr.Null] for every inner
//     column the pipeline would have introduced.
func (op *OptionalApply) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// If we have an outer row being processed, pull from the inner plan.
		if op.pendingInner {
			var innerRow Row
			ok, err := op.inner.Next(&innerRow)
			if err != nil {
				return false, err
			}
			if ok {
				op.emittedAny = true
				need := len(innerRow)
				if cap(op.outBuf) < need {
					op.outBuf = make([]expr.Value, need)
				}
				op.outBuf = op.outBuf[:need]
				copy(op.outBuf, innerRow)
				*out = op.outBuf
				return true, nil
			}
			// Inner exhausted for this outer row.
			op.pendingInner = false
			if !op.emittedAny {
				// Emit a single NULL-extended row.
				row := op.buildNullRow()
				*out = row
				return true, nil
			}
			// At least one inner row was emitted; move to the next outer row.
			continue
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

		// Stable snapshot of the outer row.
		cp := make(Row, len(outerRow))
		copy(cp, outerRow)
		op.outerRow = cp

		// Seed and re-initialise the inner plan.
		op.arg.SetOuterRow(cp)
		if err := op.inner.Init(op.ctx); err != nil {
			return false, err
		}
		op.pendingInner = true
		op.emittedAny = false
	}
}

// buildNullRow constructs the NULL-extended row used when the inner pipeline
// produced zero rows. Layout: outerRow... || Null × (paddedWidth - len(outer)).
func (op *OptionalApply) buildNullRow() Row {
	w := op.paddedWidth
	if w < len(op.outerRow) {
		w = len(op.outerRow)
	}
	if cap(op.outBuf) < w {
		op.outBuf = make([]expr.Value, w)
	}
	op.outBuf = op.outBuf[:w]
	copy(op.outBuf, op.outerRow)
	for i := len(op.outerRow); i < w; i++ {
		op.outBuf[i] = expr.Null
	}
	return op.outBuf
}

// Close releases resources and closes both the outer and inner plans.
func (op *OptionalApply) Close() error {
	outerErr := op.outer.Close()
	innerErr := op.inner.Close()
	op.outerRow = nil
	op.outBuf = nil
	if outerErr != nil {
		return outerErr
	}
	return innerErr
}
