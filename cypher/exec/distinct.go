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
	child Operator

	// Runtime state.
	ctx  context.Context  //nolint:containedctx // stored for per-Next ctx check
	seen map[uint64][]Row // hash → list of rows (collision chain)

	budget byteBudget // estimated-byte cap on the retained distinct rows (#1841)

	maxDistinct   int
	distinctCount int // number of distinct rows STORED across all buckets
}

// WithByteBudget bounds the estimated retained size of the stored distinct rows
// by maxBytes. It complements the maxDistinct count cap so a few large-valued
// distinct rows cannot exceed the engine's result-byte budget before the count
// cap fires (#1841). A non-positive maxBytes or nil estimateRow leaves the byte
// dimension disabled. Returns op for chaining and must be called before Init.
func (op *Distinct) WithByteBudget(maxBytes int64, estimateRow func(Row) int64) *Distinct {
	op.budget.set(maxBytes, estimateRow)
	return op
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
	op.distinctCount = 0
	op.budget.reset()
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

		h := expr.HashRowEquivalent(*out)
		bucket := op.seen[h]

		// Check for collision-safe equivalence within the bucket.
		if rowInBucket(*out, bucket) {
			continue // duplicate — discard
		}

		// New distinct row. The cap counts retained distinct ROWS, not hash
		// buckets: a bucket is a collision chain that can hold many distinct
		// rows, so keying the cap off the bucket count would let adversarial
		// inputs engineered to collide on [expr.HashRow] retain more than
		// maxDistinct rows in a single chain before tripping (a bounded-
		// resources violation). Storing this row would push the count past the
		// cap.
		if op.distinctCount >= op.maxDistinct {
			return false, ErrDistinctMemoryExceeded
		}
		if op.budget.charge(*out) {
			return false, ErrDistinctMemoryExceeded
		}

		// The copy is REQUIRED for two independent reasons and cannot be elided
		// (rmp #2702). First, cp is retained in op.seen for the operator's
		// lifetime as the comparand every later row is tested against by
		// rowInBucket, long after the producer has reused *out's backing array
		// ([Row]'s contract). Second, `*out = cp` below hands that SAME slice to
		// the consumer, so the retained comparand and the emitted row are one
		// object — a consumer that wrote through the emitted row would silently
		// corrupt the dedup table.
		//
		// Measured (rmp #2702, MemProfileRate=1, no -race): on
		// `MATCH (p:Person) RETURN DISTINCT p.salary` over 120 000 rows with a
		// unique key this fires 120 000 times — 19.81% of the query's allocs/op
		// but only 3.01% of its B/op, since each is a 16-byte one-column Row.
		// On a low-cardinality key (200 distinct of 120 000) it fires 200 times,
		// 0.055% of allocs/op.
		cp := make(Row, len(*out))
		copy(cp, *out)
		op.seen[h] = append(bucket, cp)
		op.distinctCount++
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

// rowInBucket returns true iff row is equivalent to any entry in bucket
// (full equivalence check for collision resolution, using openCypher
// CIP2016-06-14 grouping semantics).
func rowInBucket(row Row, bucket []Row) bool {
	for _, seen := range bucket {
		if rowsEqual(seen, row) {
			return true
		}
	}
	return false
}
