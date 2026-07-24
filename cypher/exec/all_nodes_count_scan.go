package exec

// all_nodes_count_scan.go — AllNodesCountScan operator (#2113 / #2066).
//
// AllNodesCountScan is the full-node-scan counterpart of [LabelCountScan] (#2004):
// a leaf specialised for the single shape a direct count read can serve while
// staying BIT-IDENTICAL to the serial [AllNodesScan] + [EagerAggregation]
// pipeline — a group-by-less count(*) / count(<scan-var>) over a bare full-node
// scan. It emits exactly one row carrying the graph's live-node count, read from
// the maintained live-order counter in O(1), without walking a single node.
//
// # Why the direct read equals the serial count
//
// The serial pipeline for `MATCH (n) RETURN count(*)` is EagerAggregation(count)
// over AllNodesScan, whose WalkNodeIDs skips tombstones and therefore emits exactly
// one row per LIVE node — LiveOrder() of them. count(*) counts rows; count(n)
// counts non-null bindings, and every row of a bare scan binds a non-null node, so
// both equal that same live count. AllNodesCountScan reads LiveOrder() directly, so
// its single output row is bit-identical to what the serial aggregation computes.
//
// It is both the serial count pushdown BELOW the parallel-scan threshold (where the
// morsel-parallel [ParallelCountScan] declines) and the count pushdown when the
// parallel path is disabled — a full O(N) scan just to count is never necessary.
//
// # Bounded resources
//
// No goroutines, no channels, no per-node allocation. The count is read once in
// Init; Next emits a single row from a fixed backing buffer.
//
// # Concurrency contract
//
// AllNodesCountScan is NOT safe for concurrent use (the caller drives
// Init/Next/Close from a single goroutine). Init reads the live-order counter,
// which the read-path driver observes under the graph's visibility barrier, so the
// count reflects a consistent snapshot.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// liveNodeCounter is the fast path a [nodeWalker] may implement to report the exact
// number of live (tombstone-excluded) nodes a whole-graph WalkNodeIDs would yield,
// without materialising them. The live LPG walker implements it; a walker that
// cannot answer directly (ok == false, e.g. a morsel-restricted walker or a test
// stub) makes AllNodesCountScan fall back to a single WalkNodeIDs count pass, which
// yields the identical value.
type liveNodeCounter interface {
	LiveNodeCount() (int64, bool)
}

// AllNodesCountScan is a Volcano leaf operator that computes a group-by-less count
// over a bare full-node scan by reading the graph's live-node count directly. It
// emits exactly one row with a single [expr.IntegerValue] column carrying that
// count.
//
// AllNodesCountScan is NOT safe for concurrent use.
type AllNodesCountScan struct {
	g       nodeWalker
	ctx     context.Context //nolint:containedctx // stored for the per-Next ctx check
	buf     [1]expr.Value   // fixed backing buffer — zero-alloc per Next
	count   int64
	emitted bool
}

// NewAllNodesCountScan creates an AllNodesCountScan over g.
func NewAllNodesCountScan(g nodeWalker) *AllNodesCountScan {
	return &AllNodesCountScan{g: g}
}

// Init reads the live-node count once. It prefers the O(1) direct counter when g
// supports it and otherwise falls back to a single WalkNodeIDs count pass — both
// yield the same tombstone-excluded live count.
func (op *AllNodesCountScan) Init(ctx context.Context) error {
	op.ctx = ctx
	op.emitted = false
	if err := ctx.Err(); err != nil {
		return err
	}
	if lc, ok := op.g.(liveNodeCounter); ok {
		if n, ok := lc.LiveNodeCount(); ok {
			op.count = n
			return nil
		}
	}
	// Fallback: count live node IDs by a single walk. Identical result, O(N) once.
	// The walk honours ctx cancellation every 4096 nodes, matching AllNodesScan.
	var n int64
	var cancelled bool
	op.g.WalkNodeIDs(func(_ graph.NodeID) bool {
		if n%4096 == 0 && ctx.Err() != nil {
			cancelled = true
			return false
		}
		n++
		return true
	})
	if cancelled {
		return ctx.Err()
	}
	op.count = n
	return nil
}

// Next emits the single count row on its first call and reports end-of-stream
// thereafter.
func (op *AllNodesCountScan) Next(out *Row) (bool, error) {
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

// Close releases resources held by the operator. AllNodesCountScan holds none, so
// Close is a no-op and is safe to call whether or not Next was ever called.
func (op *AllNodesCountScan) Close() error { return nil }
