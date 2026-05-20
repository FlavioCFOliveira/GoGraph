package exec

// union.go — Union and UnionAll operators.
//
// UnionAll emits all rows from the left child followed by all rows from the
// right child, without deduplication. It validates that both children produce
// rows with the same number of columns.
//
// Union wraps UnionAll with a Distinct operator to remove duplicate rows.
//
// Schema validation: if the left and right children produce rows with different
// column counts, [ErrSchemaMismatch] is returned on the first row from the
// right child that reveals the difference. The check is done lazily on the
// first right-side row because column widths are not known at construction time
// in this executor model.
//
// # Concurrency
//
// UnionAll and Union are NOT safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"
)

// ErrSchemaMismatch is returned when the left and right operands of a UNION
// produce rows with different column counts.
var ErrSchemaMismatch = errors.New("exec: union schema mismatch: column counts differ")

// ─────────────────────────────────────────────────────────────────────────────
// UnionAll
// ─────────────────────────────────────────────────────────────────────────────

// UnionAll is a Volcano operator that concatenates the output of left and right
// children without deduplication. It validates that both sides produce rows of
// the same width (column count).
//
// UnionAll is NOT safe for concurrent use.
type UnionAll struct {
	left  Operator
	right Operator

	// Runtime state.
	ctx       context.Context //nolint:containedctx // stored for per-Next ctx check
	leftDone  bool
	leftWidth int // width of the first emitted row from the left side; -1 = unset
}

// NewUnionAll creates a UnionAll operator that concatenates left then right.
func NewUnionAll(left, right Operator) *UnionAll {
	return &UnionAll{left: left, right: right, leftWidth: -1}
}

// Init initialises both child operators.
func (op *UnionAll) Init(ctx context.Context) error {
	op.ctx = ctx
	op.leftDone = false
	op.leftWidth = -1
	if err := op.left.Init(ctx); err != nil {
		return fmt.Errorf("exec: UnionAll left init: %w", err)
	}
	if err := op.right.Init(ctx); err != nil {
		return fmt.Errorf("exec: UnionAll right init: %w", err)
	}
	return nil
}

// Next emits rows from left until exhausted, then emits rows from right.
// Returns ErrSchemaMismatch if the first right-side row has a different column
// count than the first left-side row.
func (op *UnionAll) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	if !op.leftDone {
		ok, err := op.left.Next(out)
		if err != nil {
			return false, err
		}
		if ok {
			if op.leftWidth < 0 {
				op.leftWidth = len(*out)
			}
			return true, nil
		}
		op.leftDone = true
		// If left emitted zero rows we have no reference width — skip schema check.
	}

	// Right side.
	ok, err := op.right.Next(out)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	// Schema check on first right-side row.
	if op.leftWidth >= 0 && len(*out) != op.leftWidth {
		return false, ErrSchemaMismatch
	}

	return true, nil
}

// Close closes both child operators. Both are always attempted; if both fail
// the errors are joined.
func (op *UnionAll) Close() error {
	lErr := op.left.Close()
	rErr := op.right.Close()
	if lErr != nil && rErr != nil {
		return fmt.Errorf("exec: UnionAll close: left: %w; right: %w", lErr, rErr)
	}
	if lErr != nil {
		return fmt.Errorf("exec: UnionAll close left: %w", lErr)
	}
	if rErr != nil {
		return fmt.Errorf("exec: UnionAll close right: %w", rErr)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Union
// ─────────────────────────────────────────────────────────────────────────────

// Union emits the set-union of left and right: all rows from both sides with
// duplicates removed. It is implemented as UnionAll wrapped in a Distinct
// operator.
//
// Schema mismatch detection is inherited from [UnionAll].
//
// Union is NOT safe for concurrent use.
type Union struct {
	inner *Distinct // Distinct wrapping a UnionAll
}

// NewUnion creates a Union operator that deduplicates the concatenation of left
// and right.
//
//   - maxDistinct: upper bound on distinct rows; pass 0 to use DefaultMaxDistinct.
func NewUnion(left, right Operator, maxDistinct int) *Union {
	ua := NewUnionAll(left, right)
	d := NewDistinct(ua, maxDistinct)
	return &Union{inner: d}
}

// Init initialises the operator.
func (op *Union) Init(ctx context.Context) error {
	return op.inner.Init(ctx)
}

// Next emits the next unique row from the union.
func (op *Union) Next(out *Row) (bool, error) {
	return op.inner.Next(out)
}

// Close releases all resources.
func (op *Union) Close() error {
	return op.inner.Close()
}
