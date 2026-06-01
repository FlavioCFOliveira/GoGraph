package exec

// top.go — Top operator (Sort+Limit fused, min-heap).
//
// Top collects at most N rows using a bounded max-heap (worst-row-at-top), so
// that each incoming row is compared against the current worst and replaces it
// if it is better. After consuming the child it drains the heap in sorted order.
//
// Complexity: O(M log N) time, O(N) space — significantly cheaper than
// Sort+Limit when M >> N (M = total input rows, N = limit).
//
// # NULL ordering
//
// Uses the same comparator as [Sort]: NULLs last in ASC, first in DESC.
//
// # Concurrency
//
// Top is NOT safe for concurrent use.

import (
	"container/heap"
	"context"
	"fmt"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Top
// ─────────────────────────────────────────────────────────────────────────────

// Top is a blocking Volcano operator that emits the N smallest rows (per the
// given sort keys) from its child, using a bounded heap for O(M log N) memory
// and time.
//
// Top is NOT safe for concurrent use.
type Top struct {
	child Operator
	keys  []SortKey
	n     int

	// Runtime state.
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
	h       *topHeap
	result  []Row // sorted result after heap drain
	built   bool
	emitIdx int
}

// NewTop creates a Top operator.
//
//   - child: the upstream operator to consume.
//   - keys: ORDER BY specification. Must not be empty.
//   - n: number of rows to return. Must be ≥ 1.
func NewTop(child Operator, keys []SortKey, n int) (*Top, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("exec: Top requires at least one SortKey")
	}
	if n < 1 {
		return nil, fmt.Errorf("exec: Top n must be ≥ 1, got %d", n)
	}
	return &Top{child: child, keys: keys, n: n}, nil
}

// Init initialises the operator. The blocking consume phase is deferred to the
// first Next call.
func (op *Top) Init(ctx context.Context) error {
	op.ctx = ctx
	op.h = &topHeap{keys: op.keys}
	op.result = nil
	op.built = false
	op.emitIdx = 0
	return op.child.Init(ctx)
}

// Next emits the next top-N row in sorted order. On the first call it consumes
// all rows from the child and finalises the heap.
func (op *Top) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	if !op.built {
		if err := op.consumeAndFinish(); err != nil {
			return false, err
		}
		op.built = true
	}

	if op.emitIdx >= len(op.result) {
		return false, nil
	}

	*out = op.result[op.emitIdx]
	op.emitIdx++
	return true, nil
}

// Close closes the child operator and releases internal storage.
func (op *Top) Close() error {
	op.h = nil
	op.result = nil
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// consumeAndFinish — blocking phase
// ─────────────────────────────────────────────────────────────────────────────

func (op *Top) consumeAndFinish() error {
	h := op.h
	heap.Init(h)

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

		cp := make(Row, len(row))
		copy(cp, row)

		if h.Len() < op.n {
			heap.Push(h, cp)
		} else if h.Len() > 0 && rowLessForKeys(cp, h.rows[0], op.keys) {
			// cp is better than the current worst — replace.
			h.rows[0] = cp
			heap.Fix(h, 0)
		}
	}

	// Drain heap into result in sorted order (smallest to largest).
	op.result = make([]Row, h.Len())
	for i := len(op.result) - 1; i >= 0; i-- {
		op.result[i] = heap.Pop(h).(Row) //nolint:forcetypeassert // heap invariant
	}
	// After draining in reverse pop order the slice is already sorted because
	// we filled from the back. Verify by sorting once more (stable, low cost
	// because the slice is nearly-sorted from the heap drain).
	sort.SliceStable(op.result, func(i, j int) bool {
		return rowLessForKeys(op.result[i], op.result[j], op.keys)
	})
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// topHeap — max-heap keyed by the sort order (worst row at root)
// ─────────────────────────────────────────────────────────────────────────────

// topHeap is a max-heap: the root is the "worst" row by the given sort order.
// When the heap is full, a newly arriving row that is "better" than the root
// replaces it.
type topHeap struct {
	rows []Row
	keys []SortKey
}

func (h *topHeap) Len() int { return len(h.rows) }

// Less returns true when i should be above j in the max-heap, i.e. when row i
// is "worse" (sorts later) than row j.
func (h *topHeap) Less(i, j int) bool {
	return rowLessForKeys(h.rows[j], h.rows[i], h.keys) // reversed: worst at root
}

func (h *topHeap) Swap(i, j int) { h.rows[i], h.rows[j] = h.rows[j], h.rows[i] }

func (h *topHeap) Push(x any) {
	h.rows = append(h.rows, x.(Row)) //nolint:forcetypeassert // heap contract
}

func (h *topHeap) Pop() any {
	old := h.rows
	n := len(old)
	x := old[n-1]
	old[n-1] = nil // zero for GC
	h.rows = old[:n-1]
	return x
}

// ─────────────────────────────────────────────────────────────────────────────
// rowLessForKeys — shared comparator for Sort and Top
// ─────────────────────────────────────────────────────────────────────────────

// rowLessForKeys returns true iff row a should appear before row b according to
// the given key sequence. It applies the same NULL ordering as [Sort.rowLess].
func rowLessForKeys(a, b Row, keys []SortKey) bool {
	for _, key := range keys {
		av := sortKeyValue(key, a)
		bv := sortKeyValue(key, b)

		c := expr.Compare(av, bv)
		if !key.Ascending {
			c = -c
		}

		if c < 0 {
			return true
		}
		if c > 0 {
			return false
		}
	}
	return false
}
