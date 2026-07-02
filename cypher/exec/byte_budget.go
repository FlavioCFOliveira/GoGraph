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
type byteBudget struct {
	maxBytes    int64
	estimateRow func(Row) int64
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
	if b.maxBytes <= 0 || b.estimateRow == nil {
		return false
	}
	b.used += b.estimateRow(row)
	return b.used > b.maxBytes
}
