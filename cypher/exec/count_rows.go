package exec

// count_rows.go — CountRows operator (#2625).
//
// CountRows counts the rows its child produces and emits exactly one row
// carrying that count. It serves the single shape whose pre-projection is pure
// waste: a group-by-less, non-DISTINCT count(*) over ANY child.
//
// # Why it is identical to the serial pipeline
//
// The serial pipeline for a group-by-less count(*) is EagerAggregation(count)
// over a pre-projection whose only item, for an EMPTY aggregate argument, is the
// constant non-null sentinel expr.BoolValue(true) (see aggArgItem). CountAgg
// null-checks its input and ticks on every non-null value, so it ticks exactly
// once per child row. Counting the child's rows directly yields the same integer
// by construction, and count(*) is defined to count rows rather than non-null
// bindings, so no null semantics are lost.
//
// This is deliberately NOT extended to count(<var>), which counts non-null
// bindings and therefore must keep evaluating its argument per row.
//
// # Why it is worth an operator
//
// The pre-projection it removes builds one fresh single-column row per input
// row to carry a constant, so a count over 7 million relationships materialises
// 7 million rows purely so a counter can null-check a value that is never null.
//
// HOW MUCH THAT IS WORTH DEPENDS ENTIRELY ON THE CHILD, and the honest numbers
// are far apart. Over a bare typed expansion — MATCH ()-[:T]->() RETURN count(*),
// 80 000 edges — the pre-projection was 4.06ms of an 11.30ms query and removing
// it cut the query 4.52ms to 2.71ms, about 40%. Add an endpoint label —
// MATCH (:L)-[:T]->(:L) RETURN count(*), the same 80 000 edges — and the
// per-row Filter that checks the far endpoint's label costs about 18.2ms of
// 26.1ms while this operator's own cost is about 2.6ms.
//
// On the 7-million-edge labelled count in examples/26_social_scale_bench, an
// INTERLEAVED A/B of five runs per arm puts the latency gain at a modest +6.7%
// median for FRIEND (8.040s to 7.504s, 1.149 to 1.073 us/edge) and +2.8% for
// LIKE. The larger and far more robust effect is on VARIANCE: the arm without
// this operator spreads 52% and 59% between its fastest and slowest run, and
// the arm with it spreads 4% and 9%. Removing seven million per-row allocations
// takes the GC out of the query's tail. A single early pairing showed 35%; five
// rounds identify that as an outlier in the before arm, and it is not claimed.
//
// It is kept because it is strictly less work on every shape — worth about 40%
// on a bare expansion, a few percent and a much tighter tail on a filtered one.
//
// The unrelated leaf pushdowns ([AllNodesCountScan], [LabelCountScan],
// [ParallelCountScan]) answer a count in O(1) from a maintained counter and are
// strictly better where they apply; they only apply over a BARE node scan.
// CountRows is the general case — it still visits every row, and claims only to
// stop building rows nobody reads.
//
// # Bounded resources
//
// No goroutines, no channels, no per-row allocation: the child's row is read and
// discarded, and the single output row comes from a fixed backing buffer.
//
// # Concurrency contract
//
// CountRows is NOT safe for concurrent use (the caller drives Init/Next/Close
// from a single goroutine), matching every other Volcano operator here.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// countRowsCancelCheck is how many rows CountRows consumes between context
// checks. It matches the stride AllNodesCountScan's fallback walk uses, so a
// cancelled count aborts on the same granularity whichever path serves it.
const countRowsCancelCheck = 4096

// CountRows is a Volcano operator that counts its child's rows and emits exactly
// one row with a single [expr.IntegerValue] column carrying that count.
//
// CountRows is NOT safe for concurrent use.
type CountRows struct {
	child   Operator
	ctx     context.Context //nolint:containedctx // stored for the periodic drain check
	buf     [1]expr.Value   // fixed backing buffer — zero-alloc per Next
	count   int64
	emitted bool
	counted bool
}

// NewCountRows creates a CountRows over child.
func NewCountRows(child Operator) *CountRows {
	return &CountRows{child: child}
}

// Init resets the counter and initialises the child.
func (op *CountRows) Init(ctx context.Context) error {
	op.ctx = ctx
	op.emitted = false
	op.counted = false
	op.count = 0
	return op.child.Init(ctx)
}

// Next drains the child on its first call, then emits the single count row and
// reports end-of-stream thereafter. An empty child yields a count of 0 in one
// row, which is what a group-by-less count(*) over no matches returns.
func (op *CountRows) Next(out *Row) (bool, error) {
	if op.emitted {
		return false, nil
	}
	if !op.counted {
		if err := op.drain(); err != nil {
			return false, err
		}
		op.counted = true
	}
	op.emitted = true
	op.buf[0] = expr.IntegerValue(op.count)
	*out = op.buf[:]
	return true, nil
}

// drain consumes every child row, counting them. The row itself is never read:
// count(*) counts rows, so its contents cannot affect the answer.
//
// It checks ctx itself every countRowsCancelCheck rows rather than relying on
// the child to do it, so a cancelled count over a very large stream aborts
// promptly whatever the child pipeline happens to be.
func (op *CountRows) drain() error {
	var row Row
	for {
		if op.count%countRowsCancelCheck == 0 {
			if err := op.ctx.Err(); err != nil {
				return err
			}
		}
		ok, err := op.child.Next(&row)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		op.count++
	}
}

// Close closes the child.
func (op *CountRows) Close() error { return op.child.Close() }
