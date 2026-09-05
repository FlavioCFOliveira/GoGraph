// Package exec implements the Volcano-style executor for the Cypher query
// engine. It defines the [Operator] interface, the two batch layouts an operator
// can emit — the row-at-a-time [Row] and the column-major [Chunk] — and the
// pipeline driver [Drain].
//
// # Data model
//
// A [Row] is a slice of [expr.Value]: one tuple, one boxed value per column, in
// the operator's output-schema order. It is the layout every [Operator] speaks
// through [Operator.Next], and the only one an operator is required to support.
//
// A [Chunk] is the column-major (struct-of-arrays) execution batch an operator
// emits when it additionally implements [ChunkProducer]. It holds scalar columns
// in contiguous, unboxed, typed backings with a packed validity bitmap, and boxes
// only at the sink, so a columnar-aware consumer pays no per-cell interface box.
// [ChunkProducer] is an optional capability discovered by type assertion: an
// operator that does not implement it is drained row-at-a-time exactly as before.
// The [Chunk] documentation names the operators that produce and consume one.
//
// # Concurrency
//
// Neither layout is safe for concurrent use, and neither is an [Operator]: each
// goroutine in a parallel pipeline segment owns its own operator tree and its own
// batches.
package exec

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Row
// ─────────────────────────────────────────────────────────────────────────────

// Row is a single tuple in the pipeline: a slice of [expr.Value] whose positions
// correspond to the operator's output schema.
//
// The row [Operator.Next] writes into out is valid only until the next call to
// Next on that operator: a producer may reuse the same backing slice across
// calls. A consumer that retains rows past the call that produced them therefore
// copies them — as [Drain], and the pipeline breakers that buffer their input
// ([Eager], [Distinct], [Sort], [Top], [HashJoin]) all do.
type Row []expr.Value
