package exec

// columnar_project.go — ColumnarProject operator (rmp #1704 P2, task #1823).
//
// ColumnarProject is the late-materialisation form of [Project] for a projection
// whose every item is a plain scalar-property access on a bound node. It removes
// the dominant read-path cost: boxing every projected scalar into an [expr.Value]
// once per row during projection (`RETURN n.v`). Instead it writes the projected
// scalars into a typed, column-major [Chunk] and lets a columnar-aware sink box
// only at the API boundary.
//
// # Reversibility
//
// ColumnarProject is a drop-in [Operator]: driven row-at-a-time through
// [ColumnarProject.Next] it behaves EXACTLY as the [Project] it embeds, boxing
// per row — the fallback for any shape or consumer not (yet) columnar. It
// additionally implements [ChunkProducer] ([ColumnarProject.FillChunk]), which a
// columnar-aware sink prefers. The two paths are equivalent by construction: Next
// evaluates the same [ProjectionItem] closures, while FillChunk runs the parallel
// [ColumnFiller] extractors that avoid the per-row box and fall back to those same
// closures for any row they cannot take the unboxed fast path on.
//
// # Concurrency
//
// ColumnarProject is NOT safe for concurrent use.

import (
	"fmt"
)

// ColumnFiller extracts one projected column value for the current input row and
// appends it to column col of dst, WITHOUT boxing a plain scalar into an
// [expr.Value] — the whole point of the columnar projection path. It is built by
// the engine (the cypher package), which owns the graph and the property-value
// classification, and handed to [NewColumnarProject].
//
// A ColumnFiller MUST append EXACTLY one value to dst column col — typed via
// [Chunk.PutInt64]/[Chunk.PutFloat64]/[Chunk.PutString]/[Chunk.PutBool], boxed
// via [Chunk.PutValue], or NULL via [Chunk.PutNull] — so the chunk stays
// rectangular. A filler that cannot take the unboxed fast path for the current
// row (a cell that is not a resolvable node, or a value that must retain special
// decoding such as a temporal) falls back to the row-at-a-time evaluation and
// appends the resulting boxed value via [Chunk.PutValue], keeping the result
// byte-identical to the row-at-a-time path.
type ColumnFiller func(row Row, dst *Chunk, col int) error

// ColumnarProject applies a list of scalar-property projections column-major.
//
// ColumnarProject is NOT safe for concurrent use.
type ColumnarProject struct {
	*Project
	fillers []ColumnFiller
}

// NewColumnarProject creates a ColumnarProject. items are the row-at-a-time
// projection items (the [Operator.Next] fallback, identical to [NewProject]);
// fillers are the parallel columnar extractors, one per item in the same order.
// len(fillers) must equal len(items).
func NewColumnarProject(child Operator, items []ProjectionItem, fillers []ColumnFiller) (*ColumnarProject, error) {
	if len(fillers) != len(items) {
		return nil, fmt.Errorf("exec: NewColumnarProject: %d fillers for %d items", len(fillers), len(items))
	}
	p, err := NewProject(child, items)
	if err != nil {
		return nil, err
	}
	return &ColumnarProject{Project: p, fillers: fillers}, nil
}

// NewOutputChunk returns a [Chunk] sized for this operator's output: one dynamic
// column per projection item, so each column's kind is decided by the values the
// fillers produce (a property column's kind is not known until the values are
// read). It implements [ChunkProducer].
func (op *ColumnarProject) NewOutputChunk(capacity int) *Chunk {
	return NewDynamicChunk(capacity, len(op.fillers))
}

// FillChunk pulls up to maxRows input rows from the child and appends each as one
// column-major row into dst via the [ColumnFiller] extractors, returning the
// number of complete rows appended (0 at end-of-stream). dst is the caller-owned
// sink chunk, filled incrementally across calls; box-at-sink happens later at the
// API boundary. It honours context cancellation before pulling each row. It
// implements [ChunkProducer].
//
// On a filler error the partially-appended row is left in dst but is not counted
// in the returned n; the caller (the result-materialisation drain) records the
// error and serves no rows, so the ragged tail is never observed.
func (op *ColumnarProject) FillChunk(dst *Chunk, maxRows int) (int, error) {
	n := 0
	for n < maxRows {
		if err := op.ctx.Err(); err != nil {
			return n, err
		}
		ok, err := op.child.Next(&op.inputRow)
		if err != nil {
			return n, err
		}
		if !ok {
			break
		}
		for col, fill := range op.fillers {
			if err := fill(op.inputRow, dst, col); err != nil {
				return n, fmt.Errorf("exec: ColumnarProject column %d: %w", col, err)
			}
		}
		n++
	}
	return n, nil
}
