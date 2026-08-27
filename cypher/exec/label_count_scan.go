package exec

// label_count_scan.go — LabelCountScan operator (#2004).
//
// LabelCountScan is a leaf operator specialised for the single shape a direct
// count read can serve while staying BIT-IDENTICAL to the serial
// NodeByLabelScan + EagerAggregation pipeline: a group-by-less count(*) /
// count(<scan-var>) over a bare single-label node scan. It emits exactly one row
// carrying the number of live nodes that carry the label, read straight from the
// label index in O(number-of-roaring-containers) — without materialising or
// iterating the per-node bitmap the serial scan walks.
//
// # Why the direct read equals the serial count
//
// The serial pipeline for `MATCH (p:Label) RETURN count(*)` is
// EagerAggregation(count) over [NodeByLabelScan]. NodeByLabelScan emits exactly
// one row per NodeID in ResolveLabelBitmap(Label), with NO liveness re-filter
// (deleted nodes are stripped from the label bitmap at delete time, so the
// bitmap holds only live nodes). Hence the serial scan produces exactly
// ResolveLabelBitmap(Label).GetCardinality() rows. count(*) counts rows;
// count(<scan-var>) counts non-null bindings, and every row of a bare label scan
// binds a non-null node — so both equal that same cardinality. LabelCountScan
// reads that cardinality directly, so its single output row is bit-identical to
// what the serial aggregation would compute.
//
// # Why this shape only
//
// The planner (tryBuildLabelCountScan) routes here only when the aggregate is a
// non-DISTINCT count with an empty argument (count(*)) or the bare scan variable
// (count(p)), there is no grouping key, and the child is a BARE
// [github.com/FlavioCFOliveira/GoGraph/cypher/ir.NodeByLabelScan] — which the IR
// translator emits only for a pure single-label pattern with no inline property
// filter and no WHERE (extra labels, property maps, and WHERE each wrap the scan
// in a Selection, so the child is then not a bare scan and the fast path
// declines). count(p.prop) counts non-null p.prop (a different result),
// count(DISTINCT p) changes the result, and any Selection between the scan and
// the aggregate changes which rows are counted; all of these decline to the
// serial path.
//
// # Bounded resources
//
// No goroutines, no channels, no per-node allocation. The count is read once in
// Init; Next emits a single row from a fixed backing buffer.
//
// # Concurrency contract
//
// LabelCountScan is NOT safe for concurrent use (the caller drives
// Init/Next/Close from a single goroutine). Init reads the label index, which is
// internally synchronised; the read-path driver holds the graph's visibility
// barrier across the whole query, so the count reflects a consistent snapshot.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// labelCounter is an optional fast path a [labelResolver] may implement to
// report a label's live node count directly, without materialising a bitmap.
// The live LPG resolver implements it (backed by the label index's O(1)
// cardinality); resolvers that do not are handled by LabelCountScan's
// bitmap-cardinality fallback.
type labelCounter interface {
	// ResolveLabelCount returns the number of live nodes carrying name. ok is
	// false when the resolver cannot answer directly (the caller then falls back
	// to ResolveLabelBitmap). An unknown label yields (0, true).
	ResolveLabelCount(name string) (int64, bool)
}

// LabelCountScan is a Volcano leaf operator that computes a group-by-less count
// over a bare single-label node scan by reading the label's live-node count
// directly. It emits exactly one row with a single [expr.IntegerValue] column
// carrying that count.
//
// LabelCountScan is NOT safe for concurrent use.
type LabelCountScan struct {
	src labelResolver
	ctx context.Context //nolint:containedctx // stored for the per-Next ctx check
	// buf is inline in the struct and is never re-allocated. It is not
	// allocation-free end to end: storing the count into it in Next is an
	// interface conversion, which boxes 8 bytes once per query for a count
	// >= 256 — see scan_label.go's "Per-row allocation" section for the same
	// mechanism paid per ROW there. Once per query is the point of this operator.
	buf     [1]expr.Value
	label   string
	count   int64
	emitted bool
}

// NewLabelCountScan creates a LabelCountScan that counts the nodes carrying
// label via src.
func NewLabelCountScan(label string, src labelResolver) *LabelCountScan {
	return &LabelCountScan{label: label, src: src}
}

// Init reads the label's live-node count once. It prefers the zero-alloc direct
// count when src supports it and otherwise falls back to the cardinality of the
// resolved bitmap — both yield the same value.
func (op *LabelCountScan) Init(ctx context.Context) error {
	op.ctx = ctx
	op.emitted = false
	if err := ctx.Err(); err != nil {
		return err
	}
	if lc, ok := op.src.(labelCounter); ok {
		if n, ok := lc.ResolveLabelCount(op.label); ok {
			op.count = n
			return nil
		}
	}
	// Fallback: materialise the bitmap and read its cardinality. Identical
	// result, one bitmap allocation instead of the zero-alloc direct count.
	op.count = int64(op.src.ResolveLabelBitmap(op.label).GetCardinality())
	return nil
}

// Next emits the single count row on its first call and reports end-of-stream
// thereafter.
func (op *LabelCountScan) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.emitted {
		return false, nil
	}
	op.emitted = true
	op.buf[0] = expr.IntegerValue(op.count)
	*out = op.buf[:]
	return true, nil
}

// Close releases resources held by the operator. LabelCountScan holds none, so
// Close is a no-op and is safe to call whether or not Next was ever called.
func (op *LabelCountScan) Close() error { return nil }
