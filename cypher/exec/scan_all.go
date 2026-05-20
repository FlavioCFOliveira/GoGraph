package exec

// scan_all.go — AllNodesScan operator (task-236).
//
// AllNodesScan iterates every node interned in an lpg.Graph's Mapper and
// emits one Row per NodeID.  It uses Mapper.Walk so that only nodes that are
// actually known to the graph (i.e. that have been AddNode'd or AddEdge'd) are
// visited, which matches the "live nodes" acceptance criterion.
//
// # Zero-alloc contract
//
// After the first call to Init the operator reuses a fixed [1]expr.Value
// backing array for every Next call; no per-row heap allocation occurs once
// the internal nodeIDs slice has been collected.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and additionally every
// 4096 rows in the drain loop that collects node IDs during Init.

import (
	"context"
	"fmt"

	"gograph/cypher/expr"
	"gograph/graph"
)

// nodeWalker is the minimal interface that AllNodesScan requires from the
// graph — just the ability to walk every interned NodeID.  lpg.Graph exposes
// this through AdjList().Mapper().Walk; callers pass a thin closure adapter.
//
// Using an interface (rather than a concrete *lpg.Graph[…]) keeps the
// operator independent of the LPG generic instantiation while remaining
// testable with simple stubs.
type nodeWalker interface {
	// WalkNodeIDs calls fn with every NodeID currently in the graph.
	// fn must return true to continue iteration or false to stop early.
	WalkNodeIDs(fn func(graph.NodeID) bool)
}

// AllNodesScan is a Volcano leaf operator that produces one Row per node in
// the graph.  Each Row has a single column: an [expr.IntegerValue] holding the
// node's [graph.NodeID] cast to int64.
//
// AllNodesScan is NOT safe for concurrent use.
type AllNodesScan struct {
	g       nodeWalker
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
	nodeIDs []graph.NodeID  // collected during Init; owned by this operator
	pos     int             // current iteration cursor
	buf     [1]expr.Value   // fixed backing buffer — zero-alloc per Next
}

// NewAllNodesScan creates an AllNodesScan over g.
func NewAllNodesScan(g nodeWalker) *AllNodesScan {
	return &AllNodesScan{g: g}
}

// Init collects all NodeIDs from the graph into an internal slice.  The
// collection itself honours ctx cancellation every 4096 nodes.
func (op *AllNodesScan) Init(ctx context.Context) error {
	op.ctx = ctx
	op.pos = 0
	op.nodeIDs = op.nodeIDs[:0] // reuse backing array across re-inits

	var count int
	var cancelled bool
	op.g.WalkNodeIDs(func(id graph.NodeID) bool {
		// Check cancellation every 4096 nodes.
		if count%4096 == 0 {
			if ctx.Err() != nil {
				cancelled = true
				return false
			}
		}
		op.nodeIDs = append(op.nodeIDs, id)
		count++
		return true
	})

	if cancelled {
		return fmt.Errorf("exec: AllNodesScan init cancelled: %w", ctx.Err())
	}
	return nil
}

// Next writes the next NodeID into out and returns (true, nil), or returns
// (false, nil) at end-of-stream.  ctx.Err() is checked on every call.
func (op *AllNodesScan) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.pos >= len(op.nodeIDs) {
		return false, nil
	}
	op.buf[0] = expr.IntegerValue(op.nodeIDs[op.pos])
	*out = op.buf[:]
	op.pos++
	return true, nil
}

// Close releases resources.  The collected nodeIDs slice is retained (but its
// length zeroed) to allow reuse if Init is called again.
func (op *AllNodesScan) Close() error {
	op.pos = len(op.nodeIDs) // mark as exhausted
	return nil
}
