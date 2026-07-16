package exec

// foreach.go — Foreach operator (the FOREACH updating clause).
//
// Foreach drives a correlated body sub-plan once per outer row, purely for its
// side-effects, then emits the outer row unchanged. It backs the openCypher
// FOREACH clause:
//
//	FOREACH (x IN list | <updating clauses>)
//
// The body sub-plan is Argument(outer vars) → Unwind(list AS x) → the updating
// operators. For each outer row Foreach seeds the [Argument] leaf with that row,
// re-initialises the body, and drains it to completion — running the updating
// clauses once per element of the list — then forwards the ORIGINAL outer row.
// Draining and discarding the body's rows is what makes FOREACH a side-effecting
// loop that preserves the surrounding query's cardinality (one output row per
// outer row, regardless of the list length).
//
// The outer row is injected into the body exclusively through [Argument]; the
// body must not reference outer state by any other mechanism.
//
// # Concurrency
//
// Foreach is NOT safe for concurrent use.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and inside the body drain,
// so a long FOREACH over a large list honours cancellation promptly.

import (
	"context"
)

// Foreach runs a correlated body sub-plan per outer row for its writes, then
// passes the outer row through unchanged.
//
// Foreach is NOT safe for concurrent use.
type Foreach struct {
	outer Operator
	inner Operator
	arg   *Argument

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewForeach creates a Foreach operator.
//   - outer is the driving (left) plan.
//   - inner is the body sub-plan whose leftmost leaf is the provided arg
//     (Argument → Unwind(list) → updating operators).
//   - arg is the [Argument] leaf; Foreach seeds it with the current outer row
//     before each body Init so the body observes that row and the loop element.
//
// Foreach takes ownership of both plans.
func NewForeach(outer, inner Operator, arg *Argument) *Foreach {
	return &Foreach{outer: outer, inner: inner, arg: arg}
}

// Init initialises the outer plan and stores ctx. The body plan is initialised
// per outer row inside Next.
func (op *Foreach) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.outer.Init(ctx)
}

// Next pulls the next outer row, drives the body sub-plan over the list for its
// side-effects, and emits the outer row unchanged. Returns (false, nil) once
// the outer plan is exhausted.
func (op *Foreach) Next(out *Row) (bool, error) {
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

	// Stable snapshot of the outer row across the body drain.
	cp := make(Row, len(outerRow))
	copy(cp, outerRow)

	// Seed the body with this outer row and drain it — applying the updating
	// clauses once per list element. The body's rows are discarded.
	op.arg.SetOuterRow(cp)
	if err := op.inner.Init(op.ctx); err != nil {
		return false, err
	}
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}
		var r Row
		iok, ierr := op.inner.Next(&r)
		if ierr != nil {
			return false, ierr
		}
		if !iok {
			break
		}
	}

	*out = cp
	return true, nil
}

// Close closes both the outer and body plans.
func (op *Foreach) Close() error {
	outerErr := op.outer.Close()
	innerErr := op.inner.Close()
	if outerErr != nil {
		return outerErr
	}
	return innerErr
}
