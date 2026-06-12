package exec

// eager.go — Eager pipeline barrier for write-before-read isolation.
//
// Eager fully consumes its child during Init and re-emits the buffered rows
// on subsequent Next calls. The eager drain in Init guarantees write-side
// operators run to completion even when a downstream short-circuit operator
// (LIMIT 0 in particular) would otherwise return false before pulling its
// child.
//
// # Memory cap
//
// The number of buffered rows is bounded by maxRows (default 10 000 000).
// Exceeding the cap aborts the Init drain with [ErrEagerMemoryExceeded],
// mirroring the sibling pipeline breakers [Sort], [Distinct] and
// [EagerAggregation].
//
// # Concurrency
//
// Eager is NOT safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"
)

// DefaultMaxEagerRows is the default upper bound on rows that Eager holds in
// memory. It matches the sibling pipeline-breaker caps ([DefaultMaxSortRows],
// [DefaultMaxDistinct]).
const DefaultMaxEagerRows = 10_000_000

// ErrEagerMemoryExceeded is returned by Eager.Init when the drained child
// produces more than maxRows rows.
var ErrEagerMemoryExceeded = errors.New("exec: eager memory cap exceeded")

// Eager is a pipeline-breaking barrier. Init drains every row from the child
// into an internal buffer; Next then re-emits the buffered rows in insertion
// order.
//
// Eager is NOT safe for concurrent use: it holds unsynchronised buffer and
// cursor state mutated across Init/Next/Close, so a single goroutine must
// drive one operator tree, like every other [Operator].
type Eager struct {
	child   Operator
	maxRows int

	// Runtime state.
	rows    []Row
	emitIdx int
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewEager wraps child in an Eager barrier.
//
//   - child: the upstream operator to drain.
//   - maxRows: upper bound on rows held in memory; pass 0 to use
//     [DefaultMaxEagerRows].
func NewEager(child Operator, maxRows int) *Eager {
	if maxRows <= 0 {
		maxRows = DefaultMaxEagerRows
	}
	return &Eager{child: child, maxRows: maxRows}
}

// Init initialises the operator AND drains every child row into the buffer
// so the downstream pipeline can apply LIMIT / SKIP / DISTINCT without
// starving any write-side child of its driving rows.
//
// The drain is bounded by maxRows ([ErrEagerMemoryExceeded] when exceeded) and
// honours context cancellation: a cancelled or expired ctx aborts the drain
// promptly rather than buffering the entire child first.
func (op *Eager) Init(ctx context.Context) error {
	op.ctx = ctx
	op.rows = nil
	op.emitIdx = 0
	if err := op.child.Init(ctx); err != nil {
		return err
	}
	var row Row
	iter := 0
	for {
		if iter%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		iter++

		ok, err := op.child.Next(&row)
		if err != nil {
			return fmt.Errorf("exec: Eager: drain child: %w", err)
		}
		if !ok {
			break
		}
		if len(op.rows) >= op.maxRows {
			return ErrEagerMemoryExceeded
		}
		cp := make(Row, len(row))
		copy(cp, row)
		op.rows = append(op.rows, cp)
	}
	return nil
}

// Next emits the next buffered row.
func (op *Eager) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.emitIdx >= len(op.rows) {
		return false, nil
	}
	*out = op.rows[op.emitIdx]
	op.emitIdx++
	return true, nil
}

// Close releases the buffer and closes the child.
func (op *Eager) Close() error {
	op.rows = nil
	return op.child.Close()
}
