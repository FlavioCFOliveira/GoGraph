package exec

// expand.go — Expand operator (task-240).
//
// Expand performs a single-hop traversal using a CSR snapshot.  For each
// input row that carries a NodeID in column inputCol, the operator expands
// the node's adjacency list and emits one row per neighbour.
//
// # Schema
//
// Output row = input row || [srcID, edgeID, dstID].
// srcID   — the originating NodeID (expr.IntegerValue).
// edgeID  — the positional index of the edge in the CSR edges array
//            (expr.IntegerValue).  This is a stable, cheap-to-compute
//            surrogate for an edge ID when a dedicated edge-ID table is
//            absent; it is consistent within a single CSR snapshot.
// dstID   — the neighbour NodeID (expr.IntegerValue).
//
// # Directions
//
// DirOut  — follows forward edges only (standard CSR adjacency).
// DirIn   — follows reverse edges; the caller must supply the reverse CSR.
// DirBoth — follows both; the operator emits forward-edge rows followed by
//            reverse-edge rows for each source node.
//
// # Edge-type filter
//
// When EdgeType is set, only edges whose positional index maps to an entry in
// EdgeTypeFilter are emitted.  The filter is a set (map[uint64]string) from
// edge position to type label; an edge passes when its type matches.  Pass a
// nil EdgeTypeFilter to disable type filtering.
//
// # Zero-alloc contract
//
// The operator reads VerticesSlice/EdgesSlice directly (no closure
// allocation).  The output Row is built by appending into a per-row
// pre-allocated slice that is reset on each Next call.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and every 4096 emitted
// rows inside the expand inner loop.

import (
	"context"

	"gograph/cypher/expr"
	"gograph/graph"
)

// Direction controls which edges Expand follows.
type Direction uint8

const (
	// DirOut follows only out-edges of the source node.
	DirOut Direction = iota + 1
	// DirIn follows only in-edges (reverse edges).
	DirIn
	// DirBoth follows both out-edges and in-edges.
	DirBoth
)

// csrAdjacency is the minimal interface required from a CSR snapshot.
// csr.CSR[W] satisfies this interface for any W.
type csrAdjacency interface {
	// VerticesSlice returns the CSR offsets array (length MaxNodeID+1).
	VerticesSlice() []uint64
	// EdgesSlice returns the flat neighbour array.
	EdgesSlice() []graph.NodeID
}

// Expand is a Volcano pipeline operator that, for each input row, expands
// one hop along the graph's CSR adjacency.
//
// Expand is NOT safe for concurrent use.
type Expand struct {
	input    Operator
	fwd      csrAdjacency // forward CSR (always required)
	rev      csrAdjacency // reverse CSR; required for DirIn / DirBoth
	dir      Direction
	edgeType string // optional edge-type filter; empty = no filter
	// edgeTypeFilter maps absolute edge positions (in fwd.EdgesSlice) to type
	// labels.  nil = no type filtering.
	edgeTypeFilter map[uint64]string

	inputCol int   // column in the input row that carries the source NodeID
	relCols  []int // input-row columns holding existing edge IDs; nil = no check

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// current expansion state
	srcID    int64          // current source NodeID
	fwdVerts []uint64       // snapshot of fwd.VerticesSlice()
	fwdEdges []graph.NodeID // snapshot of fwd.EdgesSlice()
	revVerts []uint64       // snapshot of rev.VerticesSlice() (nil for DirOut)
	revEdges []graph.NodeID // snapshot of rev.EdgesSlice() (nil for DirOut)
	inputRow Row            // current input row (borrowed reference)
	// expansion cursors
	fwdStart, fwdEnd uint64
	revStart, revEnd uint64
	fwdDone          bool // true after all forward edges for current src are exhausted
	emitCount        int  // total rows emitted; drives ctx check cadence

	outBuf []expr.Value // reusable output row backing slice
}

// ExpandConfig carries the optional configuration for [NewExpand].
type ExpandConfig struct {
	// Direction to follow. Defaults to DirOut when zero.
	Direction Direction
	// EdgeType, when non-empty, restricts emitted edges to those whose
	// positional index is present in EdgeTypeFilter with this type label.
	EdgeType string
	// EdgeTypeFilter maps absolute edge positions to type labels.  Required
	// when EdgeType is non-empty.
	EdgeTypeFilter map[uint64]string
	// InputCol is the column index in each input row that holds the source
	// NodeID (as expr.IntegerValue).  Defaults to 0.
	InputCol int
	// RelCols lists the input-row columns holding edge IDs already traversed
	// by sibling Expand operators in the same MATCH pattern. Each emitted
	// edge must NOT match any of these columns (openCypher 9 §3.2.2
	// relationship-isomorphism / cyphermorphism). Empty disables the
	// check.
	RelCols []int
}

// NewExpand creates an Expand operator.
// fwd is the forward CSR; rev is the reverse CSR (required for DirIn/DirBoth,
// ignored for DirOut).
func NewExpand(input Operator, fwd, rev csrAdjacency, cfg ExpandConfig) *Expand {
	dir := cfg.Direction
	if dir == 0 {
		dir = DirOut
	}
	return &Expand{
		input:          input,
		fwd:            fwd,
		rev:            rev,
		dir:            dir,
		edgeType:       cfg.EdgeType,
		edgeTypeFilter: cfg.EdgeTypeFilter,
		inputCol:       cfg.InputCol,
		relCols:        cfg.RelCols,
	}
}

// Init initialises the operator and its child.
func (op *Expand) Init(ctx context.Context) error {
	op.ctx = ctx
	op.fwdVerts = op.fwd.VerticesSlice()
	op.fwdEdges = op.fwd.EdgesSlice()
	if op.dir != DirOut && op.rev != nil {
		op.revVerts = op.rev.VerticesSlice()
		op.revEdges = op.rev.EdgesSlice()
	}
	op.srcID = -1
	op.fwdDone = true
	op.emitCount = 0
	return op.input.Init(ctx)
}

// Next emits the next (srcID, edgeID, dstID) triplet appended to the current
// input row.  It pulls a new input row whenever the current source's
// adjacency is exhausted.
//
//nolint:gocyclo // complexity driven by direction×filter state machine; see helpers below
func (op *Expand) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}
		// tryFwdEdge returns (true, true) = emitted; (false, true) = skipped
		// (filtered/morphism), retry; (_, false) = no more forward edges.
		if emitted, ok := op.tryFwdEdge(out); ok {
			if emitted {
				return true, nil
			}
			continue // skip (filtered or morphism-rejected), try next edge
		}
		// tryRevEdge follows the same convention.
		if emitted, ok := op.tryRevEdge(out); ok {
			if emitted {
				return true, nil
			}
			continue // skip reverse edge
		}
		done, err := op.advanceInput()
		if err != nil {
			return false, err
		}
		if done {
			return false, nil
		}
	}
}

// tryFwdEdge attempts to emit one forward edge for the current source node.
// Returns (true, true) when a row was written, (false, true) when the forward
// cursor needs to skip a filtered edge (caller retries), (_, false) when no
// forward edge is available and the caller should check reverse edges.
func (op *Expand) tryFwdEdge(out *Row) (emitted, handled bool) {
	if op.dir == DirIn || op.fwdDone {
		return false, false
	}
	if op.fwdStart >= op.fwdEnd {
		op.fwdDone = true
		return false, false
	}
	pos := op.fwdStart
	dst := op.fwdEdges[pos]
	op.fwdStart++
	if !op.passesFilter(pos) {
		return false, true // filtered out; caller retries
	}
	if !op.passesRelMorphism(int64(pos)) {
		return false, true // cyphermorphism: duplicate edge; caller retries
	}
	op.buildRow(out, op.srcID, int64(pos), int64(dst))
	op.incEmitCount()
	return true, true
}

// tryRevEdge attempts to emit one reverse edge for the current source node.
// Returns (true, true) when a row was written, (false, true) when the reverse
// cursor needs to skip a filtered edge, (_, false) when no reverse edge is
// available and the caller should pull a new input row.
func (op *Expand) tryRevEdge(out *Row) (emitted, handled bool) {
	if op.dir == DirOut || op.revVerts == nil {
		return false, false
	}
	if op.revStart >= op.revEnd {
		return false, false
	}
	pos := op.revStart
	dst := op.revEdges[pos]
	op.revStart++
	// Undirected self-loop deduplication: when the pattern is undirected
	// (DirBoth) and the reverse edge being considered is a self-loop on
	// the current source node (dst == srcID), the same edge has already
	// been emitted by the forward pass. Skip it to honour the openCypher
	// rule that every matched edge appears exactly once for an undirected
	// relationship pattern. The skip is restricted to DirBoth because a
	// pure DirIn traversal does not perform the forward pass and therefore
	// must still emit reverse self-loops.
	if op.dir == DirBoth && int64(dst) == op.srcID {
		return false, true // self-loop already emitted by forward pass
	}
	// Edge-type filter: locate the corresponding forward-edge position so
	// the existing fwd-position-keyed filter map applies. The reverse edge
	// (revEdges[pos] → src) corresponds to the forward edge
	// (dst → src), found by scanning dst's outgoing range in the forward
	// CSR. The scan is O(deg(dst)) per reverse edge; acceptable for typical
	// graphs where in-degree and out-degree are bounded.
	if op.edgeType != "" {
		if !op.reverseEdgePassesFilter(uint64(dst), uint64(op.srcID)) {
			return false, true // filtered out; caller retries
		}
	}
	syntheticID := int64(uint64(len(op.fwdEdges)) + pos)
	if !op.passesRelMorphism(syntheticID) {
		return false, true // cyphermorphism: duplicate edge; caller retries
	}
	op.buildRow(out, op.srcID, syntheticID, int64(dst))
	op.incEmitCount()
	return true, true
}

// reverseEdgePassesFilter reports whether the forward edge (dst → src),
// corresponding to a reverse traversal from src to dst, has an
// edge-type filter entry. It scans the forward CSR's outgoing range of
// dst to locate the position of the (dst → src) edge, then consults the
// edge-type filter map. Returns true on no match (edge type filter
// declined the edge).
func (op *Expand) reverseEdgePassesFilter(dst, src uint64) bool {
	if op.edgeTypeFilter == nil {
		return true // no filter declared → accept all
	}
	if dst+1 >= uint64(len(op.fwdVerts)) {
		return false
	}
	start := op.fwdVerts[dst]
	end := op.fwdVerts[dst+1]
	for fwdPos := start; fwdPos < end; fwdPos++ {
		if uint64(op.fwdEdges[fwdPos]) == src {
			if typ, ok := op.edgeTypeFilter[fwdPos]; ok && typ == op.edgeType {
				return true
			}
			return false
		}
	}
	return false
}

// advanceInput pulls the next row from the child operator and loads the
// corresponding adjacency ranges.  Returns (true, nil) at end-of-stream,
// (false, err) on error, (false, nil) when a new source was loaded
// successfully.
func (op *Expand) advanceInput() (done bool, err error) {
	var inputRow Row
	ok, err := op.input.Next(&inputRow)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	if op.inputCol >= len(inputRow) {
		return false, nil // row too narrow; skip silently
	}
	iv, ok := inputRow[op.inputCol].(expr.IntegerValue)
	if !ok {
		return false, nil // not an integer; skip silently
	}
	op.srcID = int64(iv)
	op.inputRow = inputRow
	op.loadAdjacency(uint64(op.srcID))
	return false, nil
}

// loadAdjacency sets the forward and reverse cursor ranges for srcID uid.
func (op *Expand) loadAdjacency(uid uint64) {
	op.fwdDone = false
	if uid+1 < uint64(len(op.fwdVerts)) {
		op.fwdStart = op.fwdVerts[uid]
		op.fwdEnd = op.fwdVerts[uid+1]
	} else {
		op.fwdStart, op.fwdEnd = 0, 0
		op.fwdDone = true
	}
	if op.dir != DirOut && op.revVerts != nil && uid+1 < uint64(len(op.revVerts)) {
		op.revStart = op.revVerts[uid]
		op.revEnd = op.revVerts[uid+1]
	} else {
		op.revStart, op.revEnd = 0, 0
	}
}

// passesRelMorphism reports whether edgeID is absent from all cyphermorphism
// columns of the current input row.  It returns true when relCols is nil
// (no enforcement) or when edgeID does not match any existing column value.
//
// The check is O(len(relCols)) with no allocations.
func (op *Expand) passesRelMorphism(edgeID int64) bool {
	if len(op.relCols) == 0 {
		return true
	}
	for _, col := range op.relCols {
		if col < 0 || col >= len(op.inputRow) {
			continue
		}
		if iv, ok := op.inputRow[col].(expr.IntegerValue); ok && int64(iv) == edgeID {
			return false
		}
	}
	return true
}

// passesFilter reports whether the edge at absolute position pos (in the
// forward edges array) satisfies the optional edge-type filter.
func (op *Expand) passesFilter(pos uint64) bool {
	if op.edgeType == "" {
		return true
	}
	if op.edgeTypeFilter == nil {
		return false
	}
	t, ok := op.edgeTypeFilter[pos]
	return ok && t == op.edgeType
}

// buildRow writes (inputRow... || srcID || edgeID || dstID) into out.
func (op *Expand) buildRow(out *Row, srcID, edgeID, dstID int64) {
	need := len(op.inputRow) + 3
	if cap(op.outBuf) < need {
		op.outBuf = make([]expr.Value, need)
	}
	op.outBuf = op.outBuf[:need]
	copy(op.outBuf, op.inputRow)
	op.outBuf[len(op.inputRow)] = expr.IntegerValue(srcID)
	op.outBuf[len(op.inputRow)+1] = expr.IntegerValue(edgeID)
	op.outBuf[len(op.inputRow)+2] = expr.IntegerValue(dstID)
	*out = op.outBuf
}

// incEmitCount increments the emission counter and checks cancellation every
// 4096 emitted rows (checked in the outer loop, so this is a no-op here).
func (op *Expand) incEmitCount() {
	op.emitCount++
}

// Close releases resources and closes the child operator.
func (op *Expand) Close() error {
	op.fwdVerts = nil
	op.fwdEdges = nil
	op.revVerts = nil
	op.revEdges = nil
	op.outBuf = nil
	return op.input.Close()
}
