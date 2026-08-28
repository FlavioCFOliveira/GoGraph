package exec

// top.go — Top operator (Sort+Limit fused, min-heap).
//
// Top collects at most N rows using a bounded max-heap (worst-row-at-top), so
// that each incoming row is compared against the current worst and replaces it
// if it is better. After consuming the child it drains the heap in sorted order.
//
// Complexity: O(M log N) comparisons, O(N) space — significantly cheaper than
// Sort+Limit when M >> N (M = total input rows, N = limit). Since #2652 each
// heap entry carries its materialised sort keys, so key EVALUATIONS are Θ(M)
// rather than Θ(M log N) and no comparison allocates; see
// [Top.consumeAndFinish].
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
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
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

	// Runtime state.
	ctx context.Context //nolint:containedctx // stored for per-Next ctx check
	h   *topHeap

	keys   []SortKey
	result []Row // sorted result after heap drain

	n       int
	emitIdx int
	built   bool
}

// NewTop creates a Top operator.
//
//   - child: the upstream operator to consume.
//   - keys: ORDER BY specification. Must not be empty.
//   - n: number of rows to return. Must be ≥ 0; n == 0 yields an empty result
//     while still draining the child (ORDER BY … LIMIT 0, see #1801).
func NewTop(child Operator, keys []SortKey, n int) (*Top, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("exec: Top requires at least one SortKey")
	}
	if n < 0 {
		return nil, fmt.Errorf("exec: Top n must be ≥ 0, got %d", n)
	}
	return &Top{child: child, keys: keys, n: n}, nil
}

// Init initialises the operator. The blocking consume phase is deferred to the
// first Next call.
func (op *Top) Init(ctx context.Context) error {
	op.ctx = ctx
	// The control is read ONCE per execution, never per row and never per
	// comparison. decorated is false for n == 0 as well: that shape admits no
	// row at all, so materialising a key for each of the M rows it drains would
	// be pure waste (see [Top.consumeAndFinish]).
	op.h = &topHeap{
		keys:      op.keys,
		decorated: !sortseam.KeyDecorationDisabled() && op.n > 0,
	}
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

// consumeAndFinish drains the child and finalises the bounded heap.
//
// # Decoration (#2652)
//
// Top reached [rowLessForKeys] from THREE sites, each of which re-evaluated both
// operands: the per-row admission test against the heap root, the heap's own
// Less (so every sift of every replacement re-evaluated the keys of every node
// it touched), and the final result sort. For an evaluator-backed key — any
// ORDER BY expression that is not a projected column — one such call builds a
// fresh expr.RowContext map and a fresh sorted schema walk, so the cost was
// Θ(M log N) row-context builds for M input rows and a limit of N.
//
// Every heap entry now CARRIES its keys, materialised exactly once when the row
// is admitted, and every comparison reads only those precomputed values. Key
// evaluations drop to one per input row.
//
// # Why the emitted order is unchanged
//
// The decorated values are exactly what [sortKeyValue] returns for the same row,
// so [keysLess] returns the same verdict for every pair [rowLessForKeys] would.
// Heap operations are deterministic functions of the comparator's VERDICTS
// alone, so the sequence of sifts, the drain order, and therefore the tie order
// are all identical. The final stable sort is kept, driven by the decorated
// keys, rather than dropped on the argument that the drain already leaves the
// slice ordered.
func (op *Top) consumeAndFinish() error {
	h := op.h
	heap.Init(h)
	k := len(op.keys)

	// scratch holds the candidate row's decorated keys. One buffer for the whole
	// run: it is read and either copied into the heap or discarded before the
	// next row is fetched, so no entry can alias it.
	var scratch []expr.Value
	if h.decorated {
		scratch = make([]expr.Value, k)
	}

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

		if !h.decorated {
			// Legacy arm — see [sortseam]. Also the n == 0 arm, which admits
			// nothing and must therefore evaluate nothing.
			if h.Len() < op.n {
				heap.Push(h, topEntry{row: cp})
			} else if h.Len() > 0 && rowLessForKeys(cp, h.entries[0].row, op.keys) {
				h.entries[0].row = cp
				heap.Fix(h, 0)
			}
			continue
		}

		for j := range op.keys {
			scratch[j] = sortKeyValue(op.keys[j], cp)
		}
		switch {
		case h.Len() < op.n:
			// Filling the heap: this is the only site that allocates a key
			// block, and it runs at most min(M, n) times.
			kv := make([]expr.Value, k)
			copy(kv, scratch)
			heap.Push(h, topEntry{row: cp, kv: kv})
		case h.Len() > 0 && keysLess(op.keys, scratch, h.entries[0].kv):
			// cp is better than the current worst — replace, reusing the
			// displaced entry's key block so a replacement allocates nothing.
			e := &h.entries[0]
			e.row = cp
			copy(e.kv, scratch)
			heap.Fix(h, 0)
		}
	}

	// Drain heap into ascending order (smallest to largest) by filling from the
	// back, then re-sort stably. Entries are drained rather than bare rows so
	// the final sort can read the decorated keys instead of re-evaluating them.
	ents := make([]topEntry, h.Len())
	for i := len(ents) - 1; i >= 0; i-- {
		ents[i] = heap.Pop(h).(topEntry) //nolint:forcetypeassert // heap invariant
	}
	sort.SliceStable(ents, func(i, j int) bool {
		if h.decorated {
			return keysLess(op.keys, ents[i].kv, ents[j].kv)
		}
		return rowLessForKeys(ents[i].row, ents[j].row, op.keys)
	})
	op.result = make([]Row, len(ents))
	for i := range ents {
		op.result[i] = ents[i].row
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// topHeap — max-heap keyed by the sort order (worst row at root)
// ─────────────────────────────────────────────────────────────────────────────

// topEntry is one heap slot: a retained row together with the sort keys
// materialised for it (#2652). kv has len(topHeap.keys) values on the decorated
// path and is nil on the legacy path, where the comparator re-evaluates instead.
type topEntry struct {
	row Row
	kv  []expr.Value
}

// topHeap is a max-heap: the root is the "worst" entry by the given sort order.
// When the heap is full, a newly arriving row that is "better" than the root
// replaces it.
type topHeap struct {
	entries []topEntry
	keys    []SortKey
	// decorated selects which comparator Less uses. Set once by [Top.Init] from
	// [sortseam.KeyDecorationDisabled] and never written afterwards, so a
	// half-decorated heap is not representable.
	decorated bool
}

func (h *topHeap) Len() int { return len(h.entries) }

// Less returns true when i should be above j in the max-heap, i.e. when entry i
// is "worse" (sorts later) than entry j.
func (h *topHeap) Less(i, j int) bool {
	if h.decorated {
		// reversed: worst at root.
		return keysLess(h.keys, h.entries[j].kv, h.entries[i].kv)
	}
	return rowLessForKeys(h.entries[j].row, h.entries[i].row, h.keys)
}

func (h *topHeap) Swap(i, j int) { h.entries[i], h.entries[j] = h.entries[j], h.entries[i] }

func (h *topHeap) Push(x any) {
	h.entries = append(h.entries, x.(topEntry)) //nolint:forcetypeassert // heap contract
}

func (h *topHeap) Pop() any {
	old := h.entries
	n := len(old)
	x := old[n-1]
	old[n-1] = topEntry{} // zero for GC
	h.entries = old[:n-1]
	return x
}

// ─────────────────────────────────────────────────────────────────────────────
// rowLessForKeys — shared comparator for Sort and Top
// ─────────────────────────────────────────────────────────────────────────────

// rowLessForKeys is the LEGACY comparator: it evaluates both operands of every
// comparison through [sortKeyValue]. Since #2652 it is reached only when
// [sortseam.KeyDecorationDisabled] selects the control arm, and on the n == 0
// shape that admits nothing; the production path compares decorated keys through
// [keysLess]. It is kept because it is the definition the decorated path must
// agree with, and because the differential test needs a real arm to compare
// against rather than a golden file.
//
// It returns true iff row a should appear before row b according to the given key
// sequence, and applies the same NULL ordering as [Sort.rowLess].
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
