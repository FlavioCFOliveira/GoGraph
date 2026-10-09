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
// # Streaming (rmp #3081)
//
// When the walker also implements [nodeIDStreamer] and reports that it streams
// — the read path at a pinned MVCC snapshot — Init collects nothing: Next and
// FillChunk pull ids in batches of at most [allNodesScanBatch] into one
// operator-owned buffer, so the scan holds O(batch) ids instead of O(V). The
// emitted ids and their order are exactly those the materialising path emits,
// because the streamer resolves every id's existence as of the snapshot, which
// does not move while the statement runs. Every other walker (a write path's
// own view, a morsel, a test stub) keeps the Init-time materialisation.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and additionally every
// 4096 rows in the drain loop that collects node IDs during Init.

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
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

// nodeIDStreamer is the optional batch-pull extension of [nodeWalker]. A walker
// implements it when the set of ids it yields is fixed for the whole statement,
// so ids may be produced lazily rather than collected in Init.
type nodeIDStreamer interface {
	// StreamsNodeIDs reports whether NextNodeIDs may replace WalkNodeIDs.
	StreamsNodeIDs() bool
	// NextNodeIDs refills dst (len 0, cap > 0) with the next ids in WalkNodeIDs
	// order, advancing cur; an empty result means the walk is exhausted.
	NextNodeIDs(cur *graph.MapperCursor, dst []graph.NodeID) []graph.NodeID
	// NodeIDUpperBound is an upper bound on the ids a whole walk yields.
	NodeIDUpperBound() int
}

// allNodesScanBatch is the streamed scan's batch size in ids (4 KiB): large
// enough that the per-batch shard lock and visibility checks amortise, small
// enough that the buffer stays in L1.
const allNodesScanBatch = 512

// AllNodesScan is a Volcano leaf operator that produces one Row per node in
// the graph.  Each Row has a single column: an [expr.IntegerValue] holding the
// node's [graph.NodeID] cast to int64.
//
// AllNodesScan is NOT safe for concurrent use.
type AllNodesScan struct {
	g       nodeWalker
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
	buf     [1]expr.Value   // fixed backing buffer — zero-alloc per Next
	nodeIDs []graph.NodeID  // collected during Init, or the current streamed batch
	pos     int             // current iteration cursor

	// Streaming state (rmp #3081); stream is nil on the materialising path.
	stream   nodeIDStreamer
	cur      graph.MapperCursor
	batch    []graph.NodeID // operator-owned batch buffer, allocated once
	bound    int            // NodeIDUpperBound at Init, for rowCountHint
	streamed bool           // set once the stream is exhausted or closed
}

// NewAllNodesScan creates an AllNodesScan over g.
func NewAllNodesScan(g nodeWalker) *AllNodesScan {
	return &AllNodesScan{g: g}
}

// Init collects all NodeIDs from the graph into an internal slice, or, for a
// streaming walker, only positions the stream at its start. The collection
// itself honours ctx cancellation every 4096 nodes.
func (op *AllNodesScan) Init(ctx context.Context) error {
	op.ctx = ctx
	op.pos = 0
	if s, ok := op.g.(nodeIDStreamer); ok && s.StreamsNodeIDs() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("exec: AllNodesScan init cancelled: %w", err)
		}
		if op.batch == nil {
			op.batch = make([]graph.NodeID, 0, allNodesScanBatch)
		}
		op.stream = s
		op.cur = graph.MapperCursor{}
		op.nodeIDs = op.batch[:0]
		op.bound = s.NodeIDUpperBound()
		op.streamed = false
		return nil
	}
	op.stream = nil
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
	if op.pos >= len(op.nodeIDs) && !op.refill() {
		return false, nil
	}
	op.buf[0] = expr.IntegerValue(op.nodeIDs[op.pos])
	*out = op.buf[:]
	op.pos++
	return true, nil
}

// refill pulls the next streamed batch into op.nodeIDs and reports whether it
// holds any id. On the materialising path it reports false: Init collected
// everything there is.
func (op *AllNodesScan) refill() bool {
	if op.stream == nil || op.streamed {
		return false
	}
	op.nodeIDs = op.stream.NextNodeIDs(&op.cur, op.batch[:0])
	op.pos = 0
	if len(op.nodeIDs) == 0 {
		op.streamed = true
		return false
	}
	return true
}

// Close releases resources.  The collected nodeIDs slice is retained (but its
// length zeroed) to allow reuse if Init is called again.
func (op *AllNodesScan) Close() error {
	op.pos = len(op.nodeIDs) // mark as exhausted
	op.streamed = true
	return nil
}

// rowCountHint reports the exact number of rows this scan will emit: one per
// collected NodeID. Init has already walked the graph and filled op.nodeIDs by
// the time a consumer queries the hint, so the bound is exact (and therefore a
// valid upper bound). It satisfies [rowCountHinter] so the materialise drain can
// presize its backing slice for a full-node scan (#1720).
func (op *AllNodesScan) rowCountHint() (int, bool) {
	if op.stream != nil {
		// Streamed: nothing is collected yet, so report the interned-id count
		// taken at Init — still a true upper bound (see nodeIDStreamer).
		return op.bound, true
	}
	return len(op.nodeIDs), true
}

// NewOutputChunk returns a [Chunk] with a single static integer column that
// AllNodesScan fills with unboxed NodeIDs. It implements [ChunkProducer] (#1704
// P3): a columnar-aware parent drains the scan column-major, avoiding the
// per-row [expr.Value] box [AllNodesScan.Next] pays.
func (op *AllNodesScan) NewOutputChunk(capacity int) *Chunk {
	return NewChunk(capacity, expr.KindInteger)
}

// FillChunk appends up to maxRows more NodeIDs, as unboxed int64, into column 0
// of dst and returns the number appended (0 at end-of-stream). It is the
// column-major counterpart of [AllNodesScan.Next]: the SAME nodeIDs in the SAME
// order (advancing the shared op.pos cursor), but written to a typed column with
// no per-row heap box. Only one of Next/FillChunk drives a given query — the
// result sink picks the columnar drain or the row drain once — so sharing op.pos
// between them is sound. It honours context cancellation. It implements
// [ChunkProducer].
func (op *AllNodesScan) FillChunk(dst *Chunk, maxRows int) (int, error) {
	if err := op.ctx.Err(); err != nil {
		return 0, err
	}
	n := 0
	for n < maxRows {
		if op.pos >= len(op.nodeIDs) && !op.refill() {
			break
		}
		if n&4095 == 0 && n > 0 {
			if err := op.ctx.Err(); err != nil {
				return n, err
			}
		}
		dst.AppendInt64(0, int64(op.nodeIDs[op.pos]))
		op.pos++
		n++
	}
	return n, nil
}

// nodeIDColumnProducer marks AllNodesScan as a [NodeIDColumnProducer]: column 0
// of its output chunk carries the raw int64 NodeID of the scanned node variable.
func (op *AllNodesScan) nodeIDColumnProducer() {}
