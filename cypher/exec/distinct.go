package exec

// distinct.go — Distinct operator (hash deduplication).
//
// Distinct filters its child's output to emit only unique rows. It uses a hash
// table keyed by [expr.HashRow] with full equality collision checking to handle
// hash collisions correctly.
//
// # Memory cap
//
// The number of seen distinct rows is bounded by maxDistinct (default
// 10 000 000). Exceeding the cap returns [ErrDistinctMemoryExceeded].
//
// # Concurrency
//
// Distinct is NOT safe for concurrent use.

import (
	"context"
	"errors"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// DefaultMaxDistinct is the default upper bound on distinct rows tracked by
// the Distinct operator.
const DefaultMaxDistinct = 10_000_000

// ErrDistinctMemoryExceeded is returned by Distinct.Next when the number of
// distinct rows seen exceeds the configured maxDistinct limit.
var ErrDistinctMemoryExceeded = errors.New("exec: distinct memory cap exceeded")

// ─────────────────────────────────────────────────────────────────────────────
// Distinct
// ─────────────────────────────────────────────────────────────────────────────

// Distinct is a streaming Volcano operator that emits each unique row exactly
// once. It maintains a hash set of seen rows; hash collisions are resolved by
// full equality checks.
//
// Distinct is NOT safe for concurrent use.
type Distinct struct {
	child       Operator
	maxDistinct int

	// Runtime state.
	ctx  context.Context  //nolint:containedctx // stored for per-Next ctx check
	seen map[uint64][]Row // hash → list of rows (collision chain)
}

// NewDistinct creates a Distinct operator.
//
//   - child: the upstream operator.
//   - maxDistinct: upper bound on distinct rows; pass 0 to use DefaultMaxDistinct.
func NewDistinct(child Operator, maxDistinct int) *Distinct {
	if maxDistinct <= 0 {
		maxDistinct = DefaultMaxDistinct
	}
	return &Distinct{child: child, maxDistinct: maxDistinct}
}

// Init initialises the operator and resets the deduplication state.
func (op *Distinct) Init(ctx context.Context) error {
	op.ctx = ctx
	op.seen = make(map[uint64][]Row)
	return op.child.Init(ctx)
}

// Next pulls rows from the child and emits the first occurrence of each unique
// row. Duplicate rows are silently discarded. Returns ErrDistinctMemoryExceeded
// if more than maxDistinct distinct rows are encountered.
func (op *Distinct) Next(out *Row) (bool, error) {
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

		h := expr.HashRow(*out)
		bucket := op.seen[h]

		// Check for collision-safe equality within the bucket.
		if rowInBucket(*out, bucket) {
			continue // duplicate — discard
		}

		// New distinct row.
		if len(op.seen) >= op.maxDistinct && len(bucket) == 0 {
			// New hash key (new distinct row) would exceed the cap.
			return false, ErrDistinctMemoryExceeded
		}
		if len(bucket) == 0 && countDistinctKeys(op.seen) >= op.maxDistinct {
			return false, ErrDistinctMemoryExceeded
		}

		// Copy and store the row.
		cp := make(Row, len(*out))
		copy(cp, *out)
		op.seen[h] = append(bucket, cp)
		*out = cp
		return true, nil
	}
}

// Close closes the child operator and releases internal state.
func (op *Distinct) Close() error {
	op.seen = nil
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// rowInBucket returns true iff row equals any entry in bucket (full equality
// check for collision resolution).
func rowInBucket(row Row, bucket []Row) bool {
	for _, seen := range bucket {
		if rowsEqual(seen, row) {
			return true
		}
	}
	return false
}

// countDistinctKeys returns the number of distinct hash keys in the seen map.
// Used to enforce the memory cap without double-counting collision chains.
func countDistinctKeys(seen map[uint64][]Row) int {
	return len(seen)
}
