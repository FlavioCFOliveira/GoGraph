package exec

// semi_apply.go — SemiApply and AntiSemiApply operators (task-258).
//
// SemiApply implements the dependent semi-join: for each outer row, it runs the
// inner sub-plan and emits the outer row iff the inner plan produces at least
// one result. After the first inner row is found, the inner plan is closed early
// (short-circuit).
//
// AntiSemiApply is the complement: it emits the outer row iff the inner plan
// produces zero results.
//
// Both operators follow openCypher 9 NULL semantics: neither operator performs
// 3VL on individual columns — the existence test is purely row-count based.
// The inner plan must handle NULL propagation itself if needed.
//
// # Schema
//
// Output row = outerRow (unchanged). The inner plan's output is consumed but
// not included in the output schema.
//
// # Concurrency
//
// SemiApply and AntiSemiApply are NOT safe for concurrent use.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call.

import "context"

// SemiApply emits each outer row for which the inner sub-plan produces at least
// one row. The inner plan is closed after the first match (short-circuit).
//
// SemiApply is NOT safe for concurrent use.
type SemiApply struct {
	outer Operator
	inner Operator
	arg   *Argument

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewSemiApply creates a SemiApply operator.
//   - outer is the driving (left) plan.
//   - inner is the correlated (right) sub-plan whose leaf is arg.
//   - arg is the [Argument] node seeded with each outer row before inner Init.
func NewSemiApply(outer, inner Operator, arg *Argument) *SemiApply {
	return &SemiApply{outer: outer, inner: inner, arg: arg}
}

// Init initialises the outer plan.
func (op *SemiApply) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.outer.Init(ctx)
}

// Next advances to the next outer row for which the inner plan has ≥1 result.
func (op *SemiApply) Next(out *Row) (bool, error) {
	for {
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

		// Copy outer row: stable snapshot across inner re-init.
		cp := make(Row, len(outerRow))
		copy(cp, outerRow)

		// Seed and re-init inner plan.
		op.arg.SetOuterRow(cp)
		if err := op.inner.Init(op.ctx); err != nil {
			return false, err
		}

		// Check whether inner produces ≥1 row.
		var dummy Row
		innerOK, err := op.inner.Next(&dummy)
		if err != nil {
			return false, err
		}

		// Short-circuit: close inner immediately after result is known.
		if err := op.inner.Close(); err != nil {
			return false, err
		}

		if innerOK {
			*out = cp
			return true, nil
		}
		// No inner row → skip this outer row; continue to next.
	}
}

// Close closes the outer plan. The inner plan is already closed per-row inside
// Next; a redundant close here is safe because Close on a closed operator must
// be a no-op per the Operator contract.
func (op *SemiApply) Close() error {
	return op.outer.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// AntiSemiApply
// ─────────────────────────────────────────────────────────────────────────────

// AntiSemiApply emits each outer row for which the inner sub-plan produces zero
// rows.
//
// AntiSemiApply is NOT safe for concurrent use.
type AntiSemiApply struct {
	outer Operator
	inner Operator
	arg   *Argument

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewAntiSemiApply creates an AntiSemiApply operator.
//   - outer is the driving (left) plan.
//   - inner is the correlated (right) sub-plan whose leaf is arg.
//   - arg is the [Argument] node seeded with each outer row before inner Init.
func NewAntiSemiApply(outer, inner Operator, arg *Argument) *AntiSemiApply {
	return &AntiSemiApply{outer: outer, inner: inner, arg: arg}
}

// Init initialises the outer plan.
func (op *AntiSemiApply) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.outer.Init(ctx)
}

// Next advances to the next outer row for which the inner plan has zero results.
func (op *AntiSemiApply) Next(out *Row) (bool, error) {
	for {
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

		// Copy outer row: stable snapshot across inner re-init.
		cp := make(Row, len(outerRow))
		copy(cp, outerRow)

		// Seed and re-init inner plan.
		op.arg.SetOuterRow(cp)
		if err := op.inner.Init(op.ctx); err != nil {
			return false, err
		}

		// Check whether inner produces any row.
		var dummy Row
		innerOK, err := op.inner.Next(&dummy)
		if err != nil {
			return false, err
		}

		// Close inner immediately after the existence check (short-circuit).
		if err := op.inner.Close(); err != nil {
			return false, err
		}

		if !innerOK {
			// Inner produced zero rows → emit outer row.
			*out = cp
			return true, nil
		}
		// Inner matched → skip this outer row; continue to next.
	}
}

// Close closes the outer plan.
func (op *AntiSemiApply) Close() error {
	return op.outer.Close()
}
