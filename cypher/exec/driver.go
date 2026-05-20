package exec

import (
	"context"
	"fmt"
)

// Drain initialises op, pulls every row from the pipeline, and returns the
// collected rows as a []Row. Close is always called before Drain returns,
// regardless of whether an error occurred.
//
// Cancellation: Drain honours ctx.Done() via the per-Next check inside each
// operator. If ctx is cancelled, Drain returns the partial result set and the
// context error.
//
// The returned rows are independent copies: each element is a snapshot of the
// Row written by the operator at that iteration. Callers own the returned
// slice.
func Drain(ctx context.Context, op Operator) ([]Row, error) {
	if err := op.Init(ctx); err != nil {
		_ = op.Close() // best-effort; Init failed so Close still must run
		return nil, fmt.Errorf("exec: operator init: %w", err)
	}

	var (
		result []Row
		row    Row
	)

	for {
		// Respect context cancellation before each Next call.
		if err := ctx.Err(); err != nil {
			closeErr := op.Close()
			if closeErr != nil {
				// Return context error as primary; log close error implicitly
				// by wrapping in a multi-error.
				return result, fmt.Errorf("exec: drain cancelled (%w); close: %w", err, closeErr)
			}
			return result, fmt.Errorf("exec: drain cancelled: %w", err)
		}

		ok, err := op.Next(&row)
		if err != nil {
			closeErr := op.Close()
			if closeErr != nil {
				return result, fmt.Errorf("exec: operator next: %w; close: %w", err, closeErr)
			}
			return result, fmt.Errorf("exec: operator next: %w", err)
		}
		if !ok {
			break
		}

		// Copy the row so the caller owns immutable data even if the operator
		// reuses the underlying buffer.
		cp := make(Row, len(row))
		copy(cp, row)
		result = append(result, cp)
	}

	if err := op.Close(); err != nil {
		return result, fmt.Errorf("exec: operator close: %w", err)
	}
	return result, nil
}
