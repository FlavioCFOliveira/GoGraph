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
// # Db-hits
//
// The O(1) read is a real ZERO for the db-hits column and the fallback walk is
// not, so the figure this operator reports depends on which path Init took. That
// is why it implements [storageAccessCounter] rather than claiming the
// type-level [noStorageAccess] marker, which cannot express a per-path answer —
// see [AllNodesCountScan.storageAccesses] (rmp #2777).
//
// # Bounded resources
//
// No goroutines, no channels, no per-node allocation. The count is read once in
// Init; Next emits a single row from a fixed backing buffer. Since rmp #2777 the
// struct also carries one int64 of db-hits accounting, written once per Init and
// only on the fallback.
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
	g     nodeWalker
	ctx   context.Context //nolint:containedctx // stored for the per-Next ctx check
	buf   [1]expr.Value   // fixed backing buffer — zero-alloc per Next
	count int64
	// walked is the db-hits figure: the node references the FALLBACK walk has
	// consumed over this operator's whole lifetime. It stays zero for as long as
	// the O(1) counter answers, and that zero is the honest report — see
	// [AllNodesCountScan.storageAccesses].
	walked  int64
	emitted bool
}

// NewAllNodesCountScan creates an AllNodesCountScan over g.
func NewAllNodesCountScan(g nodeWalker) *AllNodesCountScan {
	return &AllNodesCountScan{g: g}
}

// Init reads the live-node count once. It prefers the O(1) direct counter when g
// supports it and otherwise falls back to a single WalkNodeIDs count pass — both
// yield the same tombstone-excluded live count.
//
// The fallback charges the node references it consumed to the db-hits figure; the
// O(1) path charges nothing, because it read nothing. See
// [AllNodesCountScan.storageAccesses].
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
	// The walk read n node references, and it read them whether or not it ran to
	// completion, so the charge is made before the cancellation check rather than
	// after it. ACCUMULATED, never assigned: [storageAccessCounter] specifies a
	// whole-lifetime figure, so a re-driving parent that takes the fallback twice
	// is charged for both walks.
	op.walked += n
	if cancelled {
		return ctx.Err()
	}
	op.count = n
	return nil
}

// storageAccesses reports the node references this leaf actually read. It
// implements the storageAccessCounter marker in profile.go, so PROFILE renders a
// MEASURED figure here instead of the "?" this operator printed until rmp #2777.
//
// The figure is PATH-AWARE, and that is the whole reason the operator can be
// counted at all:
//
//   - On the O(1) path — [liveNodeCounter] answers — it is ZERO, and the zero is
//     a measurement. Init reads one maintained counter and not a single node
//     reference, so nothing happened that this column counts.
//   - On the FALLBACK — the counter declines and Init walks WalkNodeIDs — it is
//     one per node id the walk consumed. That is the same figure the equivalent
//     [AllNodesScan] reports for the same work, since that leaf's [StorageRecordScan]
//     contract charges one node reference per emitted row and a bare scan emits one
//     row per live node. The two are asserted EQUAL on the same graph by
//     cypher.TestProfileDbHits_CountStoreLeaves, so this figure is checked against
//     an independent oracle rather than against itself.
//
// # Why not a type-level zero
//
// [noStorageAccess] is a claim about the OPERATOR, and this operator's answer
// depends on which path Init took, so the marker cannot state it: it would
// publish 0 for a walk of every live node. The 2026-09-03 honesty audit judged a
// flat 0 defensible for the count-store leaves; rmp #2760 classified them UNKNOWN
// on the strength of the uncounted fallback, and rmp #2777 settled it by measuring
// that fallback rather than reasoning about it.
//
// The fallback is NOT hypothetical. [github.com/FlavioCFOliveira/GoGraph/graph/lpg.ReadView.LiveNodeCountExact]
// declines whenever a node-life record is invisible to the reader's snapshot, so a
// plain `MATCH (n) RETURN count(*)` takes the walk whenever ANY other transaction
// holds an uncommitted create or delete — an ordinary state under concurrency, not
// a corner. MEASURED on a 64-node graph with one uncommitted `CREATE (x:N)` open in
// another transaction: the walk ran and consumed 64 node ids, against 0 on the same
// query over the same graph with the substrate drained (rmp #2777).
//
// # Cost when PROFILE is off
//
// One integer add per Init, on the fallback only. n is the counter the walk
// already maintains in order to produce the count itself, so this is the first
// shape [storageAccessCounter]'s admission rule allows — the counter ALREADY
// EXISTS for the operator's own reasons — and there is no per-record increment
// anywhere. The fast path adds nothing at all.
//
// Init does NOT reset it, which is [storageAccessCounter]'s documented contract.
func (op *AllNodesCountScan) storageAccesses() int64 { return op.walked }

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
