package exec

// sort.go — Sort operator (in-memory, pipeline breaker).
//
// Sort consumes all rows from its child, sorts them in-place by one or more
// [SortKey] columns, and then emits the sorted rows one at a time.
//
// # Sort-key evaluation
//
// Each key is materialised ONCE per row before the sort begins
// (decorate-sort-undecorate, #2652), so the comparator reads precomputed values
// and allocates nothing. Key evaluations are therefore Θ(n), not Θ(n log n); see
// [Sort.sortDecorated] for the measurement that motivated it and for the
// stability argument. [sortseam.KeyDecorationDisabled] restores the legacy
// per-comparison evaluation for the differential test and the profiler's A/B.
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
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
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
	// Eval is an optional expression evaluator. When non-nil the sort key
	// value is obtained by calling Eval(row) rather than reading row[ColIdx].
	// This supports ORDER BY expressions that are not direct projection
	// output columns (e.g. ORDER BY n.age after RETURN n).
	//
	// Eval MUST be a pure function of the row it is given: since #2652 [Sort]
	// and [Top] call it exactly ONCE per row and compare the value they stored,
	// so a value that changes between calls no longer changes the ordering.
	// (Before #2652 it was called from inside the comparators, where an impure
	// evaluator made the comparator inconsistent and the resulting order
	// arbitrary — the new contract is strictly more defined, not less.)
	//
	// The value Eval returns is RETAINED for the whole span of the sort, so it
	// must not borrow storage that the caller recycles per row: no pooled
	// RowContext map, no pooled lazy node, no partially materialised entity.
	//
	// An error is treated as NULL; see [sortKeyValue].
	Eval func(Row) (expr.Value, error)
	// ColIdx is the zero-based index of the column within each Row.
	// Ignored when Eval is non-nil.
	ColIdx int
	// Ascending controls the sort direction. true = ASC, false = DESC.
	Ascending bool
}

// ─────────────────────────────────────────────────────────────────────────────
// Sort
// ─────────────────────────────────────────────────────────────────────────────

// Sort is a blocking Volcano operator that collects all rows from its child,
// sorts them by the specified [SortKey] sequence, and emits them in order.
//
// Sort is NOT safe for concurrent use.
type Sort struct {
	child Operator

	// Runtime state.
	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	keys   []SortKey
	budget byteBudget // estimated-byte cap on the buffered rows (#1841)
	rows   []Row

	// Decoration buffers (#2652). keyBuf holds the materialised sort keys as a
	// FLAT slice of len(rows)*len(keys) values, the key j of row r at
	// keyBuf[r*len(keys)+j]; permBuf holds the index permutation that is sorted
	// against them. Flat rather than [][]expr.Value because it is one allocation
	// instead of n+1 and the comparator reads two contiguous blocks instead of
	// chasing two pointers.
	//
	// Both are retained across executions and re-sliced, the same reuse [Sort.Init]
	// already performs on rows. The site that benefits is an operator RE-INITIALISED
	// in place: [Apply] calls Init on its inner arm once per OUTER row, so a Sort
	// beneath an Apply pays the buffer allocation on the first outer row and none
	// thereafter. A cached plan is NOT that site — the plan cache stores the
	// ir.LogicalPlan, and the physical operator tree is rebuilt on every execution,
	// so a re-executed cached query gets a fresh Sort whose buffers start nil.
	//
	// keyBuf is NOT charged against budget, which estimates the retained size of
	// the ROWS only. It is bounded by maxRows all the same: at most one
	// [expr.Value] per row per key, cleared as soon as the sort completes and
	// released entirely by [Sort.Close].
	keyBuf  []expr.Value
	permBuf []int

	maxRows int
	emitIdx int
	sorted  bool
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
	op.keyBuf = nil
	op.permBuf = nil
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

	// The control is read ONCE per blocking phase, never per row.
	if sortseam.KeyDecorationDisabled() {
		// Legacy arm: evaluate both operands of every comparison. Retained as
		// the differential test's control and as the profiler's A/B arm; see
		// [sortseam].
		sort.SliceStable(op.rows, func(i, j int) bool {
			return op.rowLess(op.rows[i], op.rows[j])
		})
		return nil
	}
	op.sortDecorated()
	return nil
}

// sortDecorated orders op.rows by decorate-sort-undecorate (#2652).
//
// # Why
//
// [Sort.rowLess] calls [sortKeyValue] for BOTH operands of EVERY comparison, and
// for a key that carries an evaluator (any ORDER BY expression that is not a
// projected column) each of those calls builds a fresh expr.RowContext map and a
// fresh sorted schema walk. sort.SliceStable performs Θ(n log n) comparisons, so
// the row-context builds were Θ(n log n) as well: ~4 million of them to order
// 120 000 rows, a 34x amplification over the n the work actually needs. A heap
// profile of the reproduction attributed 79.29% of ALL allocated objects to that
// one path (newSchemaWalk 26.60% + buildRowCtxWithUse 26.37% + populateRowCtx
// 26.32%), every sample of it beneath Sort.collectAndSort.
//
// Materialising each key once per row makes the evaluations Θ(n) and leaves the
// comparator reading two contiguous blocks of already-computed values, which
// allocates nothing. Both reference engines do the same: Memgraph collects the
// keys into a std::vector<TypedValue> before ranges::sort
// (plan/operator.cpp OrderBy), and Neo4j projects the key into a slot at plan
// time so its comparator is a raw slot read.
//
// # Stability
//
// The emitted order is IDENTICAL to sorting op.rows directly with
// sort.SliceStable, ties included, and not merely equivalent:
//
//   - permBuf is initialised to 0,1,…,n-1, i.e. STRICTLY ASCENDING in the row
//     index it names;
//   - sort.SliceStable preserves the relative order of elements its comparator
//     reports equal, so two rows with equal keys keep the relative order their
//     permBuf entries had, which is ascending row index — the collection order;
//   - the comparator is a pure function of the decorated key values, and the
//     decorated values are exactly what [sortKeyValue] would have returned for
//     the same row, so it reports the same verdict for every pair that
//     [Sort.rowLess] would.
//
// TestSortDecorationStabilityMatchesLegacy pins this rather than leaving it to
// the argument above.
//
// # Non-determinism
//
// A key whose evaluator is not a pure function of the row (it does not exist
// today; every evaluator [irSortKeys] compiles reads only the row, the frozen
// schema and the pinned graph view) would be observed ONCE per row here instead
// of once per comparison. That is strictly more defined than the legacy path,
// under which an unstable key makes sort.SliceStable's comparator inconsistent
// and the resulting order arbitrary.
func (op *Sort) sortDecorated() {
	n := len(op.rows)
	if n < 2 {
		return // nothing to order, and nothing to decorate it with
	}
	k := len(op.keys)

	// DECORATE — one evaluation per row per key.
	kv := op.keyBuf
	if cap(kv) < n*k {
		kv = make([]expr.Value, n*k)
	} else {
		kv = kv[:n*k]
	}
	for r, row := range op.rows {
		base := r * k
		for j := range op.keys {
			kv[base+j] = sortKeyValue(op.keys[j], row)
		}
	}

	// SORT — an index permutation, comparator reading only decorated values.
	perm := op.permBuf
	if cap(perm) < n {
		perm = make([]int, n)
	} else {
		perm = perm[:n]
	}
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(a, b int) bool {
		return keysLess(op.keys, kv[perm[a]*k:], kv[perm[b]*k:])
	})

	// UNDECORATE — apply the permutation to the rows.
	permuteRows(op.rows, perm)

	// Drop the references the decoration took on the key values while the rows
	// are still being emitted, then keep both buffers' capacity for the next
	// execution. clear compiles to a memclr, so this is not a per-value loop.
	clear(kv)
	op.keyBuf = kv[:0]
	op.permBuf = perm[:0]
}

// keysLess compares two DECORATED key blocks under the given key sequence,
// applying the same direction and NULL ordering as [Sort.rowLess]: [expr.Compare]
// already places NULL after every non-null value, and negating for DESC puts it
// first. a and b must each hold at least len(keys) values.
//
// It performs NO allocation — it reads two interface values already stored in the
// decoration buffer and hands them to [expr.Compare], which boxes nothing. That
// is asserted, not assumed, by TestSortComparatorIsAllocationFree.
//
// Shared with [Top] so the two operators cannot drift in their ordering.
func keysLess(keys []SortKey, a, b []expr.Value) bool {
	for j := range keys {
		c := expr.Compare(a[j], b[j])
		if !keys[j].Ascending {
			c = -c
		}
		if c < 0 {
			return true
		}
		if c > 0 {
			return false
		}
		// c == 0: tie-break with the next key.
	}
	return false // equal under every key
}

// keysCompare is the THREE-WAY form of [keysLess] over the same decorated key
// blocks: it returns a negative number when a sorts before b, a positive number
// when it sorts after, and 0 when the two are equal under every key.
//
// Both spellings exist because they serve different callers and one of them is
// hot:
//
//   - [Sort] needs only a boolean and performs Θ(n log n) of them, so keysLess is
//     kept as a direct loop rather than a wrapper around this function. A wrapper
//     would add one non-inlinable call per comparison (keysLess contains a loop
//     and is not inlined either), which at the audit fixture's 120 000 rows is
//     roughly two million extra calls on the operator's hot path.
//   - [Top] orders by (keys, arrival ordinal), so it must distinguish "before"
//     from "equal" in ONE pass. Expressing that with keysLess costs a SECOND full
//     comparison on every tie, and Top's whole reason for existing is inputs
//     where ties are dense.
//
// TestKeysCompareAgreesWithKeysLess pins the two against each other so they
// cannot drift.
func keysCompare(keys []SortKey, a, b []expr.Value) int {
	for j := range keys {
		c := expr.Compare(a[j], b[j])
		if !keys[j].Ascending {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// permuteRows reorders s in place so that s[i] ends up holding the element that
// perm[i] named on entry. It CONSUMES perm, using each entry's self-reference as
// the visited mark, and so needs no auxiliary storage at all.
//
// It is generic over the element type because [Sort] permutes rows while [Top]
// permutes heap entries, and a second copy of a cycle-following permutation is a
// second place for an off-by-one to hide.
//
// The alternative — filling a fresh slice of the same length and assigning it to
// op.rows — is shorter, but it costs one slice header per row.
// BenchmarkPermuteRowsInPlace against BenchmarkPermuteRowsBuffered measures the
// difference at the audit fixture's 120 000 rows: 0 B/op and 0 allocs/op against
// 2 883 584 B/op and 1 alloc/op. That 2.88 MB is larger than either buffer the
// decoration does keep (keyBuf at one key per row is 1.92 MB, permBuf 0.96 MB),
// and it is paid on EVERY execution, because assigning a fresh slice to op.rows
// discards the backing array that [Sort.Init] reuses and documents. Cycle
// following avoids both and needs no auxiliary storage at all.
func permuteRows[T any](s []T, perm []int) {
	for i := range perm {
		if perm[i] == i {
			continue // already home, or already placed by an earlier cycle
		}
		held := s[i]
		j := i
		for {
			src := perm[j]
			perm[j] = j // mark placed
			if src == i {
				s[j] = held // the cycle closes on the element we lifted out
				break
			}
			s[j] = s[src]
			j = src
		}
	}
}

// rowLess is the LEGACY comparator: it evaluates both operands of every
// comparison through [sortKeyValue]. Since #2652 it is reached only when
// [sortseam.KeyDecorationDisabled] selects the control arm; the production path
// is [Sort.sortDecorated], whose comparator is [keysLess]. It is kept because it
// is the definition the decorated path must agree with, and because the
// differential test needs a real arm to compare against rather than a golden
// file.
//
// It iterates over the key sequence and returns true iff row i should appear
// before row j.
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
