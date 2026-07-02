package exec

// sort.go — Sort operator (in-memory, pipeline breaker).
//
// Sort consumes all rows from its child, sorts them in-place by one or more
// [SortKey] columns, and then emits the sorted rows one at a time.
//
// # NULL ordering
//
// Per openCypher 9 specification:
//   - ASC order:  NULLs sort LAST  (after all non-null values).
//   - DESC order: NULLs sort FIRST (before all non-null values).
//
// This is handled by [expr.Compare] in combination with the key direction.
//
// # Memory cap
//
// The number of collected rows is bounded by maxRows (default 10 000 000).
// Exceeding the cap returns [ErrSortMemoryExceeded].
//
// # Concurrency
//
// Sort is NOT safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// DefaultMaxSortRows is the default upper bound on rows that Sort holds in
// memory.
const DefaultMaxSortRows = 10_000_000

// ErrSortMemoryExceeded is returned when Sort collects more than maxRows rows.
var ErrSortMemoryExceeded = errors.New("exec: sort memory cap exceeded")

// ─────────────────────────────────────────────────────────────────────────────
// SortKey
// ─────────────────────────────────────────────────────────────────────────────

// SortKey describes a single ORDER BY column.
type SortKey struct {
	// ColIdx is the zero-based index of the column within each Row.
	// Ignored when Eval is non-nil.
	ColIdx int
	// Ascending controls the sort direction. true = ASC, false = DESC.
	Ascending bool
	// Eval is an optional expression evaluator. When non-nil the sort key
	// value is obtained by calling Eval(row) rather than reading row[ColIdx].
	// This supports ORDER BY expressions that are not direct projection
	// output columns (e.g. ORDER BY n.age after RETURN n).
	Eval func(Row) (expr.Value, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Sort
// ─────────────────────────────────────────────────────────────────────────────

// Sort is a blocking Volcano operator that collects all rows from its child,
// sorts them by the specified [SortKey] sequence, and emits them in order.
//
// Sort is NOT safe for concurrent use.
type Sort struct {
	child   Operator
	keys    []SortKey
	maxRows int
	budget  byteBudget // estimated-byte cap on the buffered rows (#1841)

	// Runtime state.
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
	rows    []Row
	sorted  bool
	emitIdx int
}

// WithByteBudget bounds the estimated retained size of the buffered rows by
// maxBytes, using estimateRow for the per-row estimate. It complements the
// maxRows count cap so a few large-valued rows cannot exceed the engine's
// result-byte budget before the count cap fires (#1841). A non-positive maxBytes
// or nil estimateRow leaves the byte dimension disabled. Returns op for chaining
// and must be called before Init.
func (op *Sort) WithByteBudget(maxBytes int64, estimateRow func(Row) int64) *Sort {
	op.budget.set(maxBytes, estimateRow)
	return op
}

// NewSort creates a Sort operator.
//
//   - child: the upstream operator to consume.
//   - keys: ORDER BY specification. Must not be empty.
//   - maxRows: upper bound on rows held in memory; pass 0 to use DefaultMaxSortRows.
func NewSort(child Operator, keys []SortKey, maxRows int) (*Sort, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("exec: Sort requires at least one SortKey")
	}
	if maxRows <= 0 {
		maxRows = DefaultMaxSortRows
	}
	return &Sort{
		child:   child,
		keys:    keys,
		maxRows: maxRows,
	}, nil
}

// Init initialises the operator. The blocking collect+sort phase is deferred
// to the first Next call.
func (op *Sort) Init(ctx context.Context) error {
	op.ctx = ctx
	op.rows = op.rows[:0] // reuse slice backing if already allocated
	op.sorted = false
	op.emitIdx = 0
	op.budget.reset()
	return op.child.Init(ctx)
}

// Next emits the next sorted row. On the first call it collects and sorts all
// rows from the child (pipeline breaker). Subsequent calls step through the
// sorted slice.
func (op *Sort) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	if !op.sorted {
		if err := op.collectAndSort(); err != nil {
			return false, err
		}
		op.sorted = true
	}

	if op.emitIdx >= len(op.rows) {
		return false, nil
	}

	*out = op.rows[op.emitIdx]
	op.emitIdx++
	return true, nil
}

// Close closes the child operator and releases internal storage.
func (op *Sort) Close() error {
	op.rows = nil
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// collectAndSort — blocking phase
// ─────────────────────────────────────────────────────────────────────────────

func (op *Sort) collectAndSort() error {
	op.rows = op.rows[:0]

	var row Row
	iter := 0
	for {
		if iter%4096 == 0 {
			if err := op.ctx.Err(); err != nil {
				return err
			}
		}
		iter++

		ok, err := op.child.Next(&row)
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		if len(op.rows) >= op.maxRows {
			return ErrSortMemoryExceeded
		}
		if op.budget.charge(row) {
			return ErrSortMemoryExceeded
		}

		// Copy the row before appending — the operator contract allows reuse of
		// the backing slice across Next calls.
		cp := make(Row, len(row))
		copy(cp, row)
		op.rows = append(op.rows, cp)
	}

	sort.SliceStable(op.rows, func(i, j int) bool {
		return op.rowLess(op.rows[i], op.rows[j])
	})
	return nil
}

// rowLess is the comparator for sort.SliceStable. It iterates over the key
// sequence and returns true iff row i should appear before row j.
//
// NULL ordering:
//   - ASC:  NULL > everything → sort last.
//   - DESC: NULL < everything → sort first.
//
// [expr.Compare] already places NULL last (+1 vs any non-null); for DESC we
// negate the comparison result, which naturally puts NULL first.
func (op *Sort) rowLess(a, b Row) bool {
	for _, key := range op.keys {
		av := sortKeyValue(key, a)
		bv := sortKeyValue(key, b)

		c := expr.Compare(av, bv)
		if !key.Ascending {
			// Reverse: DESC. NULL sort first because Compare returns +1 for
			// NULL vs non-null; negating gives -1, i.e. NULL < non-null.
			c = -c
		}

		if c < 0 {
			return true
		}
		if c > 0 {
			return false
		}
		// c == 0: tie-break with next key.
	}
	return false // equal
}

// sortKeyValue extracts the sort key value from a row. When key.Eval is set
// it calls the evaluator; otherwise it reads row[key.ColIdx]. Evaluation
// errors are treated as NULL so sort order remains defined under any runtime
// fault.
func sortKeyValue(key SortKey, row Row) expr.Value {
	if key.Eval != nil {
		v, err := key.Eval(row)
		if err != nil {
			return expr.Null
		}
		return v
	}
	if key.ColIdx < len(row) {
		return row[key.ColIdx]
	}
	return expr.Null
}
