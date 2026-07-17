package exec

// columnar_filter.go — ColumnarFilter operator (rmp #1704 P3, task #1824).
//
// ColumnarFilter is the late-materialisation form of [Filter] for a predicate
// over a columnar (unboxed) input. It removes the second-largest read-path cost
// measured after Phase 2: boxing a node property into an [expr.Value] once per
// row purely to evaluate a WHERE predicate (e.g. `WHERE n.v >= 0`). Instead it
// pulls its child column-major, evaluates the predicate over the unboxed columns,
// and compacts the passing rows into its output chunk with no per-row box.
//
// # Reversibility
//
// ColumnarFilter is a drop-in [Operator]: driven row-at-a-time through the
// embedded [Filter]'s promoted [Filter.Next] it behaves EXACTLY as that Filter,
// pulling its child row-at-a-time and evaluating the same boxed predicate — the
// fallback for a non-columnar parent. It additionally implements [ChunkProducer]
// ([ColumnarFilter.FillChunk]), which a columnar-aware parent prefers, and
// [NodeIDColumnProducer] (its output is a passthrough of its child's scan row
// shape). The two paths are equivalent by construction: FillChunk evaluates the
// unboxed [ChunkPredicate] fast path and, for any row it cannot decide unboxed,
// boxes that one row and evaluates the SAME boxed predicate the Next path uses.
//
// # Concurrency
//
// ColumnarFilter is NOT safe for concurrent use.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// metricColumnarFilterBatch counts each columnar FillChunk batch the columnar
// filter drains — utilisation observability for the unboxed filter path, and the
// signal a test uses to confirm the path engaged rather than silently falling
// back to the boxed row path.
const metricColumnarFilterBatch = "cypher.exec.columnar_filter.batch"

// ChunkPredicate decides, WITHOUT boxing, whether source row `row` of `src`
// passes the filter predicate. It returns (keep, decided): decided=true means
// keep is authoritative (the predicate was fully evaluated over the unboxed
// columns); decided=false means the predicate shape cannot be decided unboxed for
// this row, and the caller must fall back to the boxed row predicate for a
// byte-identical result. A ChunkPredicate is built by the engine (the cypher
// package), which owns the graph and the openCypher comparison semantics, and is
// handed to [NewColumnarFilter]. It never boxes a scalar; a nil ChunkPredicate
// means "always fall back" (every row uses the boxed predicate).
type ChunkPredicate func(src *Chunk, row int) (keep, decided bool)

// ColumnarFilter applies a predicate to a columnar input and compacts the passing
// rows into a column-major output chunk.
//
// The [Filter] is embedded BY VALUE, not by pointer: a ColumnarFilter is one heap
// allocation, exactly like the plain Filter it replaces, so building one for a
// predicate whose parent turns out to consume it row-at-a-time (e.g. an
// aggregation) costs no extra allocation over the plain Filter. The columnar-only
// state (scratch batch, cursor) stays zero until the columnar FillChunk path first
// runs, so the boxed Next path never pays for it either.
//
// ColumnarFilter is NOT safe for concurrent use.
type ColumnarFilter struct {
	Filter                   // boxed fallback: promoted Next/Close, predFn, ctx
	scanChild ChunkProducer  // the columnar child (the SAME object as Filter.child)
	pred      ChunkPredicate // unboxed fast-path predicate; nil means always fall back

	scratch    *Chunk // current source batch (owned, lazily allocated; reused across FillChunk)
	scratchPos int    // cursor into scratch: index of the next row to examine
	scratchLen int    // number of rows currently held in scratch
	scanDone   bool   // true once the child returned a short batch (end-of-stream)
	boxScratch Row    // reused boxed-row buffer for the fallback predicate
}

// NewColumnarFilter creates a ColumnarFilter over a [ChunkProducer] child. predFn
// is the row-at-a-time predicate (the [Operator.Next] fallback, identical to
// [NewFilter]); pred is the parallel unboxed fast path (nil to always fall back).
// The two must be equivalent for every row pred decides — see [ChunkPredicate].
func NewColumnarFilter(child ChunkProducer, predFn FilterFn, pred ChunkPredicate) *ColumnarFilter {
	return &ColumnarFilter{
		Filter:    Filter{child: child, predFn: predFn},
		scanChild: child,
		pred:      pred,
	}
}

// Init initialises the embedded [Filter] (and, through it, the child) and resets
// the columnar cursor. The scratch batch is allocated lazily on the first
// [ColumnarFilter.FillChunk] call, so a ColumnarFilter driven only through Next
// (a non-columnar parent) allocates nothing beyond the plain Filter.
func (op *ColumnarFilter) Init(ctx context.Context) error {
	if err := op.Filter.Init(ctx); err != nil {
		return err
	}
	op.scratchPos = 0
	op.scratchLen = 0
	op.scanDone = false
	if op.scratch != nil {
		op.scratch.Reset()
	}
	return nil
}

// NewOutputChunk returns a [Chunk] shaped like the child's output: ColumnarFilter
// is a row-preserving passthrough (it drops rows, never changes the column
// layout), so its output schema equals its child's. It implements
// [ChunkProducer].
func (op *ColumnarFilter) NewOutputChunk(capacity int) *Chunk {
	return op.scanChild.NewOutputChunk(capacity)
}

// FillChunk appends up to maxRows PASSING rows into dst (column-major) and returns
// the number appended, 0 at end-of-stream. It implements [ChunkProducer].
//
// It pulls the child in FULL batches into an owned scratch chunk and evaluates the
// predicate over each source row: the unboxed [ChunkPredicate] fast path when it
// can decide, otherwise a one-row box through the SAME boxed predicate the Next
// path uses (byte-identical). Passing rows are compacted into dst via
// [Chunk.AppendRowFrom] with no per-row box.
//
// The scratch cursor (scratchPos) PERSISTS across calls: a single child pull can
// yield more survivors than the remaining dst capacity, so filling stops mid-batch
// and resumes here on the next call rather than re-pulling (which would drop or
// duplicate rows). Consequently a short return (n < maxRows) means the CHILD is
// exhausted — never merely that one internal pull was selective — which the drain
// relies on to detect end-of-stream (#1704 P3).
func (op *ColumnarFilter) FillChunk(dst *Chunk, maxRows int) (int, error) {
	metrics.IncCounter(metricColumnarFilterBatch, 1)
	if op.scratch == nil {
		op.scratch = op.scanChild.NewOutputChunk(DefaultChunkCapacity)
	}
	appended := 0
	for appended < maxRows {
		if op.scratchPos >= op.scratchLen {
			if op.scanDone {
				return appended, nil
			}
			if err := op.ctx.Err(); err != nil {
				return appended, err
			}
			op.scratch.Reset()
			op.scratchPos = 0
			n, err := op.scanChild.FillChunk(op.scratch, op.scratch.Cap())
			if err != nil {
				return appended, err
			}
			op.scratchLen = n
			if n < op.scratch.Cap() {
				op.scanDone = true // a short child batch is end-of-stream
			}
			if n == 0 {
				return appended, nil
			}
		}
		row := op.scratchPos
		op.scratchPos++

		keep, decided := false, false
		if op.pred != nil {
			keep, decided = op.pred(op.scratch, row)
		}
		if !decided {
			// Row the fast path cannot decide unboxed: box it once and evaluate the
			// SAME boxed predicate as [Filter.Next], for a byte-identical decision.
			op.boxScratch = op.scratch.BoxRow(row, op.boxScratch)
			v, err := op.predFn(op.boxScratch)
			if err != nil {
				return appended, err
			}
			keep = expr.IsTruthy(v)
		}
		if keep {
			dst.AppendRowFrom(op.scratch, row)
			appended++
		}
	}
	return appended, nil
}

// nodeIDColumnProducer marks ColumnarFilter as a [NodeIDColumnProducer]: it is a
// column-preserving passthrough over a scan, so a node-variable column in its
// output still carries the raw int64 NodeID the scan emitted.
func (op *ColumnarFilter) nodeIDColumnProducer() {}
