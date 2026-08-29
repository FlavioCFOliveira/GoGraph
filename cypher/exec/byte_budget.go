package exec

// byte_budget.go — shared estimated-byte accounting for the pipeline-breaking
// operators (Sort, Distinct, Eager, EagerAggregation, HashJoin).
//
// Every breaker already bounds the COUNT of rows/groups it retains. That count
// cap alone does not bound PEAK MEMORY: a handful of rows carrying large values
// (e.g. a 9-million-element list per row, each under the per-eval element cap)
// can hold tens of gigabytes while the row count stays far below the cap. The
// engine's aggregate-byte budget (EngineOptions.MaxResultBytes) is charged only
// at the drain (Result.materialize), which pulls from the TOP of the plan and so
// runs strictly AFTER a breaker child has finished buffering — it therefore
// cannot bound a breaker's own transient buffer. This helper threads the same
// byte budget into each breaker so buffering stops (with the breaker's typed
// memory-cap sentinel) once its retained estimate exceeds the budget (#1841).
//
// The estimate reuses the engine's own per-row size function (cypher/api.go
// estimateRowSize), injected so the breaker's accounting matches the drain's.
// A non-positive maxBytes or a nil estimateRow disables the byte dimension, in
// which case the breaker behaves exactly as before (bounded only by its count
// cap) and its result multiset is unchanged.

// byteBudget accumulates the estimated retained size of a breaker's buffer and
// reports when it crosses a configured ceiling. Its zero value is a disabled
// budget. It is NOT safe for concurrent use; each breaker owns one and drives it
// from its single-goroutine collect/drain loop.
//
// # Cumulative versus retained
//
// For every breaker that only ever GROWS its buffer — Sort, Distinct, Eager,
// EagerAggregation, HashJoin — the cumulative sum of what it charged and the
// size of what it currently retains are the same number, and [byteBudget.charge]
// alone expresses both. [Top] is the exception: it EVICTS, replacing the worst
// retained row with a better arrival, so a purely cumulative total would grow
// with the INPUT rather than with the buffer and would trip at exactly the point
// an unbounded Sort does — defeating the purpose of bounding the operator at all
// (#2509). The [byteBudget.retain] / [byteBudget.release] pair expresses the
// retained quantity for that case; charge is retain(sizeOf(row)) and the two
// spellings share one running total, so a breaker may use either but must not
// mix them.
type byteBudget struct {
	estimateRow func(Row) int64
	maxBytes    int64
	used        int64
}

// set configures the budget. A non-positive maxBytes or a nil estimateRow
// leaves the byte dimension disabled.
func (b *byteBudget) set(maxBytes int64, estimateRow func(Row) int64) {
	b.maxBytes = maxBytes
	b.estimateRow = estimateRow
}

// reset zeroes the running total; call it when a breaker (re)initialises.
func (b *byteBudget) reset() { b.used = 0 }

// charge adds the estimated size of row to the running total and reports whether
// the budget is now exceeded. When the budget is disabled it is a no-op that
// returns false, so a breaker never rejects a row that fits within its count cap
// unless a finite byte budget was configured.
func (b *byteBudget) charge(row Row) bool {
	if !b.active() {
		return false
	}
	b.used += b.estimateRow(row)
	return b.used > b.maxBytes
}

// active reports whether the byte dimension is configured. A disabled budget
// never rejects anything, so the breaker stays bounded by its count cap alone.
func (b *byteBudget) active() bool { return b.maxBytes > 0 && b.estimateRow != nil }

// sizeOf returns the estimated size of row, or 0 when the budget is disabled so
// the caller can store the result unconditionally without a branch of its own.
// The value is meant to be REMEMBERED by an evicting breaker and handed back to
// [byteBudget.release]: re-estimating the row at eviction time would be exact
// only while its values keep the same shape, and storing the number removes that
// assumption along with a second walk over the row's columns.
func (b *byteBudget) sizeOf(row Row) int64 {
	if !b.active() {
		return 0
	}
	return b.estimateRow(row)
}

// retain adds a size obtained from [byteBudget.sizeOf] to the running total and
// reports whether the budget is now exceeded.
func (b *byteBudget) retain(size int64) bool {
	if !b.active() {
		return false
	}
	b.used += size
	return b.used > b.maxBytes
}

// release subtracts a size previously passed to [byteBudget.retain], for a row
// the breaker no longer holds. Sizes are remembered per retained row rather than
// recomputed, so the total returns to exactly the value it had before that row
// was admitted.
func (b *byteBudget) release(size int64) {
	if !b.active() {
		return
	}
	b.used -= size
}
