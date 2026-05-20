package exec

// limit.go — Limit and Skip operators (task-244).
//
// Limit stops the pipeline after N rows have been emitted. Skip discards the
// first N rows and forwards all subsequent rows. Both reset their counters on
// each Init call, which means they work correctly when used as the inner plan
// of an Apply loop.
//
// # Concurrency
//
// Limit and Skip are NOT safe for concurrent use.

import (
	"context"
	"fmt"
)

// ─────────────────────────────────────────────────────────────────────────────
// Limit
// ─────────────────────────────────────────────────────────────────────────────

// Limit is a Volcano pipeline operator that forwards at most n rows from its
// child operator and then signals end-of-stream.
//
// Limit is NOT safe for concurrent use.
type Limit struct {
	child   Operator
	n       int64           // maximum rows to emit
	emitted int64           // rows emitted so far
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewLimit creates a Limit operator that passes at most n rows from child.
// n must be ≥ 0; a limit of 0 emits no rows.
func NewLimit(child Operator, n int64) (*Limit, error) {
	if n < 0 {
		return nil, fmt.Errorf("exec: Limit n must be ≥ 0, got %d", n)
	}
	return &Limit{child: child, n: n}, nil
}

// Init initialises the operator and resets the emission counter.
func (op *Limit) Init(ctx context.Context) error {
	op.ctx = ctx
	op.emitted = 0
	return op.child.Init(ctx)
}

// Next forwards the next row from the child, unless the limit has been
// reached, in which case it returns (false, nil) immediately.
func (op *Limit) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.emitted >= op.n {
		return false, nil
	}
	ok, err := op.child.Next(out)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	op.emitted++
	return true, nil
}

// Close releases resources and closes the child operator.
func (op *Limit) Close() error {
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Skip
// ─────────────────────────────────────────────────────────────────────────────

// Skip is a Volcano pipeline operator that discards the first n rows from its
// child operator and then forwards all remaining rows.
//
// Skip is NOT safe for concurrent use.
type Skip struct {
	child   Operator
	n       int64           // number of rows to skip
	skipped int64           // rows discarded so far
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewSkip creates a Skip operator that discards the first n rows from child.
// n must be ≥ 0; a skip of 0 is a no-op pass-through.
func NewSkip(child Operator, n int64) (*Skip, error) {
	if n < 0 {
		return nil, fmt.Errorf("exec: Skip n must be ≥ 0, got %d", n)
	}
	return &Skip{child: child, n: n}, nil
}

// Init initialises the operator and resets the skip counter.
func (op *Skip) Init(ctx context.Context) error {
	op.ctx = ctx
	op.skipped = 0
	return op.child.Init(ctx)
}

// Next discards rows until n have been skipped, then forwards subsequent rows.
func (op *Skip) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}
		ok, err := op.child.Next(out)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if op.skipped < op.n {
			op.skipped++
			continue
		}
		return true, nil
	}
}

// Close releases resources and closes the child operator.
func (op *Skip) Close() error {
	return op.child.Close()
}
