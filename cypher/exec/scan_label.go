package exec

// scan_label.go — NodeByLabelScan operator (task-237).
//
// NodeByLabelScan resolves a label name to a Roaring bitmap via
// label.Index.Intersect and iterates the bitmap with an IntPeekable64
// iterator.  The bitmap is materialised once during Init; Next consumes it
// one NodeID at a time with zero additional allocations.
//
// # Zero-alloc contract
//
// The IntPeekable64 iterator returned by Bitmap.Iterator does not allocate on
// each HasNext/Next call.  The Row is written into a fixed [1]expr.Value
// backing buffer.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and every 4096
// iterations.

import (
	"context"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// labelResolver resolves a string label name to a bitmap of matching NodeIDs.
// lpg.Graph satisfies this via Registry().Lookup + NodeIndex().Intersect.
type labelResolver interface {
	// ResolveLabelBitmap returns a roaring64.Bitmap of NodeIDs that carry the
	// named label, or an empty bitmap when the label is unknown.
	ResolveLabelBitmap(name string) *roaring64.Bitmap
}

// LPGLabelSource is a concrete [labelResolver] built from an lpg label
// registry and a label index.  It is the standard adapter for tests and
// production use.
type LPGLabelSource struct {
	reg *label.Index
	// lookupFn translates a label name to its uint32 ID.
	lookupFn func(name string) (uint32, bool)
}

// NewLPGLabelSource constructs a LPGLabelSource.
// lookupFn should wrap lpg.LabelRegistry.Lookup, casting LabelID to uint32.
func NewLPGLabelSource(reg *label.Index, lookupFn func(string) (uint32, bool)) *LPGLabelSource {
	return &LPGLabelSource{reg: reg, lookupFn: lookupFn}
}

// ResolveLabelBitmap implements [labelResolver].
func (s *LPGLabelSource) ResolveLabelBitmap(name string) *roaring64.Bitmap {
	id, ok := s.lookupFn(name)
	if !ok {
		return roaring64.New()
	}
	return s.reg.Intersect(id)
}

// NodeByLabelScan is a Volcano leaf operator that emits one Row per NodeID
// carrying the named label.  Each Row has a single column:
// expr.IntegerValue(nodeID).
//
// NodeByLabelScan is NOT safe for concurrent use.
type NodeByLabelScan struct {
	label    string
	src      labelResolver
	ctx      context.Context //nolint:containedctx // stored for per-Next ctx check
	iter     roaring64.IntPeekable64
	cardHint int           // bitmap cardinality captured in Init; -1 before Init
	count    int           // iteration counter for ctx check cadence
	buf      [1]expr.Value // fixed backing buffer — zero-alloc per Next
}

// NewNodeByLabelScan creates a NodeByLabelScan for the given label.
func NewNodeByLabelScan(labelName string, src labelResolver) *NodeByLabelScan {
	return &NodeByLabelScan{label: labelName, src: src, cardHint: -1}
}

// Init resolves the label to a bitmap and initialises the iterator.
func (op *NodeByLabelScan) Init(ctx context.Context) error {
	op.ctx = ctx
	op.count = 0
	bm := op.src.ResolveLabelBitmap(op.label)
	// GetCardinality is the bitmap's element count — the exact number of rows
	// this scan will emit. Capture it now (the bitmap is consumed by the
	// iterator below) so rowCountHint can report it after Init (#1720).
	op.cardHint = int(bm.GetCardinality())
	op.iter = bm.Iterator()
	return nil
}

// Next emits the next matching NodeID.  Returns (false, nil) at end-of-stream.
func (op *NodeByLabelScan) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if !op.iter.HasNext() {
		return false, nil
	}
	// Check cancellation every 4096 iterations.
	if op.count%4096 == 0 && op.count > 0 {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}
	}
	op.buf[0] = expr.IntegerValue(int64(op.iter.Next()))
	*out = op.buf[:]
	op.count++
	return true, nil
}

// Close releases resources held by the operator.
func (op *NodeByLabelScan) Close() error {
	op.iter = nil
	return nil
}

// rowCountHint reports the exact number of rows this scan will emit: the
// cardinality of the resolved label bitmap, captured in Init. It is therefore a
// valid upper bound. ok is false before Init has run (cardHint == -1). It
// satisfies [rowCountHinter] so the materialise drain can presize its backing
// slice for a label scan (#1720).
func (op *NodeByLabelScan) rowCountHint() (int, bool) {
	if op.cardHint < 0 {
		return 0, false
	}
	return op.cardHint, true
}
