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

// ChunkColumnFiller extracts one projected column value from a COLUMNAR input row
// — source row srcRow of src — and appends it to column dstCol of dst, WITHOUT
// boxing the input node id (the whole point of the chunk-input path: it reads the
// raw int64 NodeID from src via [Chunk.Int64] instead of type-asserting a boxed
// [expr.Value]). Like [ColumnFiller] it MUST append EXACTLY one value to dstCol so
// the chunk stays rectangular, and falls back — byte-identically — to the
// row-at-a-time evaluation for any source row it cannot take the unboxed fast path
// on. It is built by the engine and paired 1:1 with the operator's [ColumnFiller]
// fallbacks; the two are equivalent by construction. It is used only when the
// child is a [NodeIDColumnProducer] (#1704 P3).
type ChunkColumnFiller func(src *Chunk, srcRow int, dst *Chunk, dstCol int) error

// ColumnarProject applies a list of scalar-property projections column-major.
//
// ColumnarProject is NOT safe for concurrent use.
type ColumnarProject struct {
	// chunk-input path: populated by WithChunkInput when the child is a
	// [NodeIDColumnProducer] (#1704 P3, reads raw int64 NodeID columns unboxed) or
	// by WithScalarChunkInput when the child is any [ChunkProducer] emitting
	// already-materialised scalar columns (#2045, copies cells unboxed). FillChunk
	// then pulls the child column-major and runs chunkFillers, instead of pulling
	// the child row-at-a-time via Next and boxing every cell. nil chunkFillers keeps
	// the row-input path (P2), byte-identical.
	chunkChild ChunkProducer
	*Project
	scratch      *Chunk // reused source-batch buffer for the chunk-input path
	fillers      []ColumnFiller
	chunkFillers []ChunkColumnFiller
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

// WithChunkInput switches this ColumnarProject to consume its child column-major
// (#1704 P3): the child must be a [NodeIDColumnProducer] so chunkFillers can read
// raw int64 NodeID columns unboxed, and len(chunkFillers) must equal the number
// of projection items. It returns an error otherwise, leaving the operator on the
// row-input path. Call before Init. Passing this way — rather than a constructor
// variant — keeps the P2 [NewColumnarProject] signature and its callers untouched;
// the chunk-input path is strictly additive and opt-in.
func (op *ColumnarProject) WithChunkInput(chunkFillers []ChunkColumnFiller) error {
	cp, ok := op.child.(NodeIDColumnProducer)
	if !ok {
		return fmt.Errorf("exec: ColumnarProject.WithChunkInput: child %T is not a NodeIDColumnProducer", op.child)
	}
	if len(chunkFillers) != len(op.fillers) {
		return fmt.Errorf("exec: ColumnarProject.WithChunkInput: %d chunk fillers for %d items", len(chunkFillers), len(op.fillers))
	}
	op.chunkChild = cp
	op.chunkFillers = chunkFillers
	return nil
}

// WithScalarChunkInput switches this ColumnarProject to consume its child
// column-major over ALREADY-MATERIALISED scalar columns (#2045): the child must be
// a [ChunkProducer], and each chunk filler copies a source-chunk cell into the
// output WITHOUT any graph read — the value is already materialised in the child's
// chunk (a prior columnar projection produced it under the query's visibility
// barrier). len(chunkFillers) must equal the number of projection items. It
// returns an error otherwise, leaving the operator on the row-input path.
//
// Unlike [ColumnarProject.WithChunkInput] it does NOT require a
// [NodeIDColumnProducer]: the scalar-passthrough fillers never read the live graph
// by NodeID, so the box-at-sink isolation contract that forbids a deferred
// entity-by-id read (rmp #1704 P4/P5) does not apply — every value copied here was
// captured at query time. Call before Init.
func (op *ColumnarProject) WithScalarChunkInput(chunkFillers []ChunkColumnFiller) error {
	cp, ok := op.child.(ChunkProducer)
	if !ok {
		return fmt.Errorf("exec: ColumnarProject.WithScalarChunkInput: child %T is not a ChunkProducer", op.child)
	}
	if len(chunkFillers) != len(op.fillers) {
		return fmt.Errorf("exec: ColumnarProject.WithScalarChunkInput: %d chunk fillers for %d items", len(chunkFillers), len(op.fillers))
	}
	op.chunkChild = cp
	op.chunkFillers = chunkFillers
	return nil
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
	if op.chunkFillers != nil {
		return op.fillChunkFromChunk(dst, maxRows)
	}
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

// fillChunkFromChunk is the chunk-input path (#1704 P3, #2045): it pulls up to
// maxRows source rows from the [ChunkProducer] child column-major and projects each
// through the [ChunkColumnFiller] extractors — which read the raw int64 NodeID
// column unboxed (WithChunkInput) or copy an already-materialised scalar cell
// unboxed (WithScalarChunkInput). Projection is 1:1 (never drops a row — it may
// select/reorder columns but emits one output row per source row), so one child
// pull per call suffices and no cross-call cursor is needed: a short child return
// (n < maxRows) is end-of-stream, which the drain relies on. The source batch
// buffer (op.scratch) is reused across calls and Reset before each pull. On a
// filler error the partially-appended row is not counted in the returned n,
// mirroring the row-input path so the ragged tail is never observed.
func (op *ColumnarProject) fillChunkFromChunk(dst *Chunk, maxRows int) (int, error) {
	if maxRows <= 0 {
		return 0, nil
	}
	if op.scratch == nil {
		op.scratch = op.chunkChild.NewOutputChunk(DefaultChunkCapacity)
	}
	op.scratch.Reset()
	n, err := op.chunkChild.FillChunk(op.scratch, maxRows)
	for row := 0; row < n; row++ {
		for col, fill := range op.chunkFillers {
			if fErr := fill(op.scratch, row, dst, col); fErr != nil {
				return row, fmt.Errorf("exec: ColumnarProject column %d: %w", col, fErr)
			}
		}
	}
	return n, err
}
