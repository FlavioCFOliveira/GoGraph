package exec

// top.go — Top operator (Sort+Limit fused, bounded max-heap).
//
// Top emits the n smallest rows of its child under a [SortKey] sequence, in
// order. It is the bounded counterpart of [Sort]: where Sort retains every input
// row, Top retains at most n (transiently 2n, see "Bound handling" below), so a
// query that orders a million rows to show ten does not buffer a million.
//
// # Equivalence to Sort
//
//	Top(n) emits exactly the first n rows Sort emits, in exactly that order.
//
// That is the operator's whole contract and it is stronger than "the n best
// rows": ties included, position for position. It has to be, because the planner
// fuses ORDER BY … SKIP s LIMIT k into Skip(s) over Top(s+k) (#2509), which hands
// the caller rows [s, s+k) of Top's output. Two orderings that agree as SETS but
// disagree in position would produce two different pages for one query, silently.
// TestTopEqualsStableSortPrefix is the oracle; the arrival ordinal on
// [topEntry] is what makes the property hold.
//
// # Bound handling
//
// Rows are ACCUMULATED, not pushed into a heap one at a time, until the buffer
// reaches 2n; only then are the best n selected and a max-heap built over them,
// after which each further row is compared against the heap's worst entry and
// replaces it when it is better. This is PostgreSQL's tuned bounded-sort
// heuristic (tuplesort.c: puttuple_common defers make_bounded_heap until
// memtupcount reaches bound * 2), and it exists because a bounded heap is
// EXPENSIVE relative to a plain sort when n approaches the input size M. Measured
// on the #352 audit fixture (120 000 rows, evaluator-backed key), the previous
// heap-from-the-first-row shape cost 6.45x a plain Sort at n == M and 4.54x at
// n == M/2; with accumulation those shapes never build a heap at all and now
// cost 1.01x and 1.05x a Sort respectively. See
// docs/benchmarks/top-fusion-pagination-2026-08-29.md.
//
// The heapify is deferred one row further still: crossing the threshold selects
// the best n but builds no heap, because a heap is only worth its cost if
// ANOTHER row arrives to be tested against it. A threshold that lands on the last
// input row would otherwise scramble an order that had just been computed and pay
// a second full sort to put it back.
//
// # Memory bounds
//
// Like [Sort], Top bounds its buffer in BOTH dimensions: a row count (maxRows,
// default [DefaultMaxSortRows]) and an optional estimated-byte ceiling
// ([Top.WithByteBudget]). Both are load-bearing rather than defensive. The fused
// bound n is s+k, and s and k may come from PARAMETERS, so a hostile $skip asks
// this operator to retain an arbitrary number of rows; without the caps that is
// an out-of-memory kill instead of a typed [ErrSortMemoryExceeded]. The byte
// budget is charged on RETAINED bytes — an evicted row is credited back — since a
// cumulative total would grow with the input and trip exactly where an unbounded
// Sort trips.
//
// # NULL ordering
//
// Uses the same comparator as [Sort]: NULLs last in ASC, first in DESC.
//
// # Concurrency
//
// Top is NOT safe for concurrent use.

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
)

// ─────────────────────────────────────────────────────────────────────────────
// Top
// ─────────────────────────────────────────────────────────────────────────────

// Top is a blocking Volcano operator that emits the n smallest rows (per the
// given sort keys) from its child, using a bounded buffer.
//
// Top is NOT safe for concurrent use.
type Top struct {
	child Operator

	// Runtime state.
	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	keys   []SortKey
	budget byteBudget // estimated RETAINED-byte cap on the buffer (#2509)
	h      topHeap

	// rows is the ACCUMULATION buffer and, at the end of the blocking phase, the
	// emission buffer. During accumulation it holds one copied row per buffered
	// arrival, in arrival order — byte for byte the representation [Sort] uses,
	// which is the point: while Top is accumulating it IS a Sort, and it must not
	// cost more than one. A [topEntry] is 64 bytes against a Row header's 24, so
	// buffering entries here instead measured +24% allocated bytes on a fused
	// bound close to the input size, where the operator never builds a heap at
	// all. Entries are materialised only if and when the heap is actually needed.
	rows []Row

	// Decoration buffers, retained across executions exactly as [Sort] retains
	// its own. kvBuf is the flat key arena, holding the k materialised sort keys
	// of each buffered row, block i belonging to rows[i]. It is read positionally
	// while the rows are in arrival order, and handed out as per-entry sub-slices
	// once the heap takes over.
	kvBuf   []expr.Value
	permBuf []int

	n       int
	maxRows int
	emitIdx int
	built   bool
	// usedHeap records whether the last blocking phase crossed the accumulation
	// threshold and built a heap, or answered entirely from the accumulated
	// buffer. It exists so a test can assert WHICH of the two regimes it
	// exercised: the two produce the same order by different routes, and a
	// regime test that silently ran the same path twice would prove nothing.
	usedHeap bool
}

// NewTop creates a Top operator.
//
//   - child: the upstream operator to consume.
//   - keys: ORDER BY specification. Must not be empty.
//   - n: number of rows to return. Must be ≥ 0; n == 0 yields an empty result
//     while still draining the child (ORDER BY … LIMIT 0, see #1801).
//   - maxRows: upper bound on rows held in memory; pass 0 to use
//     [DefaultMaxSortRows]. It is a constructor parameter rather than a chained
//     option for the same reason [NewSort]'s is: n can come from a parameter, so
//     a caller who forgets the cap has an unbounded operator (#2509).
func NewTop(child Operator, keys []SortKey, n, maxRows int) (*Top, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("exec: Top requires at least one SortKey")
	}
	if n < 0 {
		return nil, fmt.Errorf("exec: Top n must be ≥ 0, got %d", n)
	}
	if maxRows <= 0 {
		maxRows = DefaultMaxSortRows
	}
	return &Top{child: child, keys: keys, n: n, maxRows: maxRows}, nil
}

// WithByteBudget bounds the estimated retained size of the buffered rows by
// maxBytes, using estimateRow for the per-row estimate. It mirrors
// [Sort.WithByteBudget] — same ceiling, same estimator, same sentinel — but the
// total it maintains is RETAINED rather than cumulative, because Top evicts. A
// non-positive maxBytes or nil estimateRow leaves the byte dimension disabled.
// Returns op for chaining and must be called before Init.
func (op *Top) WithByteBudget(maxBytes int64, estimateRow func(Row) int64) *Top {
	op.budget.set(maxBytes, estimateRow)
	return op
}

// Init initialises the operator. The blocking consume phase is deferred to the
// first Next call.
func (op *Top) Init(ctx context.Context) error {
	op.ctx = ctx
	// The control is read ONCE per execution, never per row and never per
	// comparison. decorated is false for n == 0 as well: that shape admits no
	// row at all, so materialising a key for each of the M rows it drains would
	// be pure waste.
	op.h.keys = op.keys
	op.h.decorated = !sortseam.KeyDecorationDisabled() && op.n > 0
	op.h.entries = op.h.entries[:0]
	op.rows = op.rows[:0]
	op.kvBuf = op.kvBuf[:0]
	op.built = false
	op.emitIdx = 0
	op.budget.reset()
	return op.child.Init(ctx)
}

// Next emits the next top-n row in sorted order. On the first call it consumes
// all rows from the child and finalises the buffer.
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

	if op.emitIdx >= len(op.rows) {
		return false, nil
	}

	*out = op.rows[op.emitIdx]
	op.emitIdx++
	return true, nil
}

// Close closes the child operator and releases internal storage.
func (op *Top) Close() error {
	op.h.entries = nil
	op.rows = nil
	op.kvBuf = nil
	op.permBuf = nil
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// consumeAndFinish — blocking phase
// ─────────────────────────────────────────────────────────────────────────────

// consumeAndFinish drains the child and leaves op.rows holding the answer, in
// emission order.
//
// # Why keys are materialised once per row (#2652)
//
// Top used to reach the legacy comparator from three sites, each of which
// re-evaluated BOTH operands: the per-row admission test, the heap's own
// comparator, and the final result sort. For an evaluator-backed key — any ORDER
// BY expression that is not a projected column — one such call builds a fresh
// expr.RowContext map and a fresh sorted schema walk, so the cost was
// Θ(M log n) row-context builds. Each key is now materialised exactly once per
// input row and every comparison reads only the stored value.
//
// Buffered rows are decorated in one batch ([Top.decorateArrivals]) rather than
// as they arrive, and rows past the selection are decorated individually as
// candidates. The COUNT is the same either way — one evaluation per input row,
// which TestTopKeyEvalIsLinearInRows pins — but the batch form allocates the
// arena once at its final size instead of growing it by append.
//
// # Why a candidate is compared BEFORE it is copied (#2509)
//
// Past the selection the admission test needs the candidate's KEYS, not a private
// copy of its row, and it rejects almost every candidate. Copying first meant one
// Row allocation per INPUT row — 120 000 of them on the audit fixture to retain
// 110 — of which all but the admitted few were immediately garbage. The copy now
// happens only once the row has displaced something. Reading the keys from the
// child's borrowed row is sound for exactly the reason [SortKey.Eval]'s contract
// already requires: a key value must not borrow storage the caller recycles, and
// a ColIdx key reads an interface value that the copy would have shared anyway.
func (op *Top) consumeAndFinish() error {
	if op.n == 0 {
		// ORDER BY … LIMIT 0 admits nothing but must still drain the child, so
		// that every write below it runs to completion (#1801). Nothing is
		// evaluated and nothing is retained.
		return op.drainChild()
	}

	h := &op.h
	k := len(op.keys)
	thr := op.accumulationThreshold()
	op.usedHeap = false

	// scratch holds the candidate row's decorated keys. One buffer for the whole
	// run: it is read and either copied into the buffer or discarded before the
	// next row is fetched, so nothing can alias it.
	var scratch []expr.Value
	if h.decorated {
		scratch = make([]expr.Value, k)
	}

	var row Row
	iter := 0
	seq := 0
	// Three states, in order: ACCUMULATING (rows are appended in arrival order);
	// SELECTED (the threshold was crossed, the best n have been picked out into
	// heap entries, but no heap exists yet); HEAP BUILT (further rows displace
	// the worst retained row). The middle state exists because the heap is only
	// worth building if ANOTHER row actually arrives — heapifying eagerly at the
	// threshold scrambles an order that was just computed, and when the threshold
	// lands on the last input row it costs a second full sort for nothing.
	selected := false
	heapBuilt := false
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

		if !selected {
			if len(op.rows) >= op.maxRows {
				// The buffer has reached the count cap and no bound below it can
				// relieve it: n is at least this large, so there is nothing to
				// select down to. Same sentinel and same trigger point as
				// [Sort.collectAndSort].
				return ErrSortMemoryExceeded
			}
			cp := make(Row, len(row))
			copy(cp, row)
			if op.budget.charge(cp) {
				return ErrSortMemoryExceeded
			}
			op.rows = append(op.rows, cp)
			seq++
			if len(op.rows) >= thr && op.n < thr {
				// Threshold reached: select the best n, PostgreSQL's
				// make_bounded_heap minus the heapify, which is deferred to the
				// first row that needs it. op.n < thr is what makes the selection
				// meaningful; when the bound is at or above the threshold (a
				// hostile or simply enormous n) the operator stays in
				// accumulation for good and behaves exactly as Sort does, count
				// cap and byte budget included.
				op.decorateArrivals()
				op.selectBest()
				selected = true
			}
			continue
		}

		// Past the selection every arrival is a CANDIDATE, and a candidate needs
		// its keys before the admission test can reject it.
		if h.decorated {
			for j := range op.keys {
				scratch[j] = sortKeyValue(op.keys[j], row)
			}
		}

		if !heapBuilt {
			// A row arrived after the selection, so the heap earns its cost now.
			h.heapify()
			heapBuilt = true
			op.usedHeap = true
		}

		// Admit only a row strictly better than the worst retained one. A row
		// that TIES with the worst is rejected, and that is what keeps the
		// retained set equal to the stable prefix — among equals, the earliest
		// arrival is already in the buffer.
		if !h.betterThanWorst(row, scratch) {
			seq++
			continue
		}
		if err := op.replaceWorst(row, scratch, seq); err != nil {
			return err
		}
		seq++
	}

	if selected {
		op.emitFromEntries(!heapBuilt)
		return nil
	}
	op.decorateArrivals()
	op.orderArrivalsAndTruncate()
	return nil
}

// decorateArrivals materialises the sort keys of every buffered arrival into the
// flat arena, block i belonging to rows[i]. It is [Sort.sortDecorated]'s decorate
// step, and it runs at exactly the two points where the arrivals stop arriving:
// the selection threshold, and the end of a run that never reached it.
//
// Deferring it to a single pass, rather than appending each row's keys as it is
// buffered, costs the same NUMBER of evaluations — one per input row either way,
// since a row past the selection is decorated as a candidate instead — and buys
// one sized allocation in place of an append growth series. The difference is
// what separated this operator's allocated bytes from Sort's on a bound close to
// the input size.
func (op *Top) decorateArrivals() {
	if !op.h.decorated {
		return
	}
	n, k := len(op.rows), len(op.keys)
	kv := op.kvBuf
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
	op.kvBuf = kv
}

// drainChild consumes the child for its side effects alone.
func (op *Top) drainChild() error {
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
			return nil
		}
	}
}

// accumulationThreshold is the buffer size at which the operator stops
// accumulating and switches to the bounded heap: 2n, PostgreSQL's tuned
// heuristic, clamped to the row cap so the threshold can never ask for more rows
// than the operator is allowed to hold. The doubling saturates rather than
// wrapping, because n is s+k for a fused pagination plan and both may be
// parameters.
func (op *Top) accumulationThreshold() int {
	thr := math.MaxInt
	if op.n <= math.MaxInt/2 {
		thr = 2 * op.n
	}
	if thr > op.maxRows {
		thr = op.maxRows
	}
	return thr
}

// takePerm returns an identity permutation of length n, reusing the retained
// buffer.
func (op *Top) takePerm(n int) []int {
	perm := op.permBuf
	if cap(perm) < n {
		perm = make([]int, n)
	} else {
		perm = perm[:n]
	}
	for i := range perm {
		perm[i] = i
	}
	return perm
}

// sortArrivalPerm orders perm — an identity permutation over op.rows, which are
// still in ARRIVAL order — into the emission order.
//
// The sort is STABLE and the comparator reads the keys alone. That is not a
// weaker guarantee than the (keys, ordinal) total order used elsewhere, it is
// the same one reached more cheaply: perm starts ascending, so preserving the
// relative order of elements the comparator calls equal preserves ascending
// arrival index. It is exactly what [Sort.sortDecorated] does, comparator
// included, so the two operators cannot disagree about tie order by
// construction.
//
// On the decorated path the two operands are CONTIGUOUS blocks of the flat
// arena. Reaching them through a per-entry slice header instead costs a random
// 64-byte-strided load per operand and measured 22.80ms against Sort's 8.88ms on
// the 120 000-row fixture at n == M, where this is the only path taken.
func (op *Top) sortArrivalPerm(perm []int) {
	if op.h.decorated {
		k := len(op.keys)
		kv := op.kvBuf
		sort.SliceStable(perm, func(a, b int) bool {
			return keysLess(op.keys, kv[perm[a]*k:], kv[perm[b]*k:])
		})
		return
	}
	rows := op.rows
	sort.SliceStable(perm, func(a, b int) bool {
		return rowCompareForKeys(rows[perm[a]], rows[perm[b]], op.keys) < 0
	})
}

// selectBest is PostgreSQL's make_bounded_heap without the heapify: it orders
// the accumulated arrivals, materialises the best n as heap entries, and returns
// the accumulation buffer to the operator as free space.
//
// The entries are built directly from the permutation rather than by permuting
// the rows first, which is why each one's arrival ordinal is simply the index
// the permutation names.
func (op *Top) selectBest() {
	k := len(op.keys)
	perm := op.takePerm(len(op.rows))
	op.sortArrivalPerm(perm)

	ents := op.h.entries[:0]
	if cap(ents) < op.n {
		ents = make([]topEntry, 0, op.n)
	}
	// The retained set shrinks from len(rows) to n here, so the running total is
	// rebuilt from the survivors rather than decremented per discard. It can
	// only fall, so no admission decision is revisited.
	op.budget.reset()
	for j := 0; j < op.n; j++ {
		i := perm[j]
		e := topEntry{row: op.rows[i], seq: i}
		if op.h.decorated {
			e.kv = op.kvBuf[i*k : (i+1)*k : (i+1)*k]
		}
		e.bytes = op.budget.sizeOf(e.row)
		op.budget.retain(e.bytes)
		ents = append(ents, e)
	}
	op.h.entries = ents

	// Release what was not selected. The rows array keeps its capacity as the
	// emission buffer, but must not go on pinning the discarded rows; the same
	// goes for their blocks in the key arena, which stay where they are and are
	// simply blanked.
	if op.h.decorated {
		for j := op.n; j < len(perm); j++ {
			i := perm[j]
			clear(op.kvBuf[i*k : (i+1)*k])
		}
	}
	clear(op.rows)
	op.rows = op.rows[:0]
	op.permBuf = perm[:0]
}

// emitFromEntries moves the retained entries' rows into the emission buffer and
// releases the entries and the key arena.
//
// ordered says the entries are already in emission order, which they are when
// the threshold was crossed but no heap was ever built: [Top.selectBest] left
// them sorted and nothing has touched them since. Otherwise the heap's sift
// sequence has lost the arrival order and only the recorded ordinal can restore
// it, so they are sorted by (sort keys, arrival ordinal). That relation is a
// TOTAL order — the ordinal is unique — so the cheaper unstable sort is correct
// here where the arrival-order path must use a stable one. It runs over at most
// n entries.
//
// The rows are read out THROUGH the permutation rather than by permuting the
// entries first and then copying: the emission buffer has to be filled either
// way, and filling it in permutation order makes the separate permutation pass
// redundant.
//
// The emission buffer already has capacity for at least the threshold, hence for
// n, so nothing here allocates.
func (op *Top) emitFromEntries(ordered bool) {
	ents := op.h.entries
	op.rows = op.rows[:0]

	if ordered || len(ents) < 2 {
		for i := range ents {
			op.rows = append(op.rows, ents[i].row)
		}
	} else {
		perm := op.takePerm(len(ents))
		if op.h.decorated {
			sort.Slice(perm, func(a, b int) bool {
				ea, eb := &ents[perm[a]], &ents[perm[b]]
				if c := keysCompare(op.keys, ea.kv, eb.kv); c != 0 {
					return c < 0
				}
				return ea.seq < eb.seq
			})
		} else {
			sort.Slice(perm, func(a, b int) bool {
				ea, eb := &ents[perm[a]], &ents[perm[b]]
				if c := rowCompareForKeys(ea.row, eb.row, op.keys); c != 0 {
					return c < 0
				}
				return ea.seq < eb.seq
			})
		}
		for _, i := range perm {
			op.rows = append(op.rows, ents[i].row)
		}
		op.permBuf = perm[:0]
	}

	// Nothing reads a key again, so drop the arena's references while keeping
	// its capacity for the next execution. A key produced by an evaluator — an
	// upper-cased name, a computed expression — is not otherwise reachable from
	// the row, so this is not merely tidiness.
	clear(ents)
	op.h.entries = ents[:0]
	clear(op.kvBuf)
	op.kvBuf = op.kvBuf[:0]
}

// orderArrivalsAndTruncate finishes a run that never crossed the threshold: the
// rows are still in arrival order, so they are ordered exactly as [Sort] orders
// them and then cut to the bound.
func (op *Top) orderArrivalsAndTruncate() {
	if len(op.rows) > 1 {
		perm := op.takePerm(len(op.rows))
		op.sortArrivalPerm(perm)
		permuteRows(op.rows, perm)
		op.permBuf = perm[:0]
	}
	// The arena is dead once the permutation has been applied; drop the
	// references it holds but keep its capacity for the next execution, exactly
	// as [Sort.sortDecorated] does.
	clear(op.kvBuf)
	op.kvBuf = op.kvBuf[:0]

	if len(op.rows) <= op.n {
		return
	}
	clear(op.rows[op.n:]) // do not pin the rows past the bound
	op.rows = op.rows[:op.n]
}

// replaceWorst overwrites the heap root with the candidate row and restores the
// heap invariant. The displaced entry's key block is REUSED rather than
// reallocated, so a replacement allocates only the row copy.
func (op *Top) replaceWorst(row Row, scratch []expr.Value, seq int) error {
	cp := make(Row, len(row))
	copy(cp, row)
	size := op.budget.sizeOf(cp)
	e := &op.h.entries[0]
	op.budget.release(e.bytes)
	if op.budget.retain(size) {
		return ErrSortMemoryExceeded
	}
	e.row = cp
	e.bytes = size
	e.seq = seq
	if op.h.decorated {
		copy(e.kv, scratch)
	}
	op.h.siftDown(0, len(op.h.entries))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// topHeap — max-heap keyed by the sort order (worst entry at the root)
// ─────────────────────────────────────────────────────────────────────────────

// topEntry is one buffer slot.
//
// seq is the ARRIVAL ORDINAL, the implicit final sort key that makes the
// operator's order total (#2509). Without it the emitted order of rows that tie
// on every ORDER BY key is whatever the heap's sift sequence happens to leave
// behind, which is NOT the order [Sort] produces —
// TestTopTieOrderMinimalWitness shows five all-tied rows coming out 1,2,0 where
// Sort emits 0,1,2. Under a fused SKIP that is not a cosmetic difference: it
// changes which rows the page contains.
//
// bytes is the row's estimated size as charged to the byte budget, remembered so
// that evicting the entry credits back exactly what admitting it charged.
//
// kv holds the sort keys materialised for the row (#2652). It has len(keys)
// values on the decorated path and is nil on the legacy path, where the
// comparator re-evaluates instead.
type topEntry struct {
	row   Row
	kv    []expr.Value
	bytes int64
	seq   int
}

// topHeap is a max-heap: the root is the "worst" entry by the given sort order.
// When the buffer is full, a newly arriving row that is "better" than the root
// replaces it.
//
// It does NOT implement container/heap.Interface. The only operations this
// operator needs are "build" and "the root changed, restore the invariant", and
// expressing them directly over the concrete slice keeps the comparator out of an
// interface call in the sift loop and removes the per-Push/per-Pop boxing of a
// topEntry into an `any` that container/heap's signature forces — measured at two
// extra allocations per retained row.
type topHeap struct {
	entries []topEntry
	keys    []SortKey
	// decorated selects which comparator the sift uses. Set once by [Top.Init]
	// from [sortseam.KeyDecorationDisabled] and never written afterwards, so a
	// half-decorated heap is not representable.
	decorated bool
}

// worse reports whether entry a sorts AFTER entry b under (keys, arrival
// ordinal), i.e. whether a belongs closer to the root of the max-heap. The
// ordinal makes the relation a strict total order, so the heap has no ties to
// break arbitrarily and its drain order is deterministic.
func (h *topHeap) worse(a, b int) bool {
	ea, eb := &h.entries[a], &h.entries[b]
	if h.decorated {
		if c := keysCompare(h.keys, ea.kv, eb.kv); c != 0 {
			return c > 0
		}
	} else {
		if c := rowCompareForKeys(ea.row, eb.row, h.keys); c != 0 {
			return c > 0
		}
	}
	return ea.seq > eb.seq
}

// betterThanWorst reports whether a candidate row should displace the root. A
// candidate that TIES with the root is rejected: it arrived later, so under
// (keys, arrival ordinal) it is the worse of the two and the retained set stays
// equal to the stable prefix.
func (h *topHeap) betterThanWorst(row Row, scratch []expr.Value) bool {
	if len(h.entries) == 0 {
		return false
	}
	if h.decorated {
		return keysCompare(h.keys, scratch, h.entries[0].kv) < 0
	}
	return rowCompareForKeys(row, h.entries[0].row, h.keys) < 0
}

// heapify builds the max-heap invariant over every entry, bottom-up in O(len).
func (h *topHeap) heapify() {
	n := len(h.entries)
	for i := n/2 - 1; i >= 0; i-- {
		h.siftDown(i, n)
	}
}

// siftDown restores the max-heap invariant at i over entries[:n].
func (h *topHeap) siftDown(i, n int) {
	for {
		l := 2*i + 1
		if l >= n {
			return
		}
		m := l
		if r := l + 1; r < n && h.worse(r, l) {
			m = r
		}
		if !h.worse(m, i) {
			return
		}
		h.entries[i], h.entries[m] = h.entries[m], h.entries[i]
		i = m
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// rowCompareForKeys — LEGACY comparator shared by Sort's control arm and Top
// ─────────────────────────────────────────────────────────────────────────────

// rowCompareForKeys is the LEGACY comparator: it evaluates both operands of every
// comparison through [sortKeyValue]. Since #2652 it is reached only when
// [sortseam.KeyDecorationDisabled] selects the control arm; the production path
// compares decorated keys through [keysCompare]. It is kept because it is the
// definition the decorated path must agree with, and because the differential
// test needs a real arm to compare against rather than a golden file.
//
// It returns a negative number when a sorts before b, positive when after, and 0
// when the two are equal under every key, applying the same NULL ordering as
// [Sort.rowLess].
func rowCompareForKeys(a, b Row, keys []SortKey) int {
	for _, key := range keys {
		c := expr.Compare(sortKeyValue(key, a), sortKeyValue(key, b))
		if !key.Ascending {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}
