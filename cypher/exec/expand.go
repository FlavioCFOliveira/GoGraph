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

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
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
	// HandlesSlice returns the per-slot stable edge handles parallel to
	// EdgesSlice, or nil when the snapshot carries no handles (a
	// non-multigraph never built via AddEdgeH). The forward and reverse
	// CSRs of the same graph carry the SAME handle for a given logical
	// edge, which is what lets the reverse traversal recover per-instance
	// edge identity across parallel edges (rmp #1634).
	HandlesSlice() []uint64
}

// Expand is a Volcano pipeline operator that, for each input row, expands
// one hop along the graph's CSR adjacency.
//
// Expand is NOT safe for concurrent use.
type Expand struct {
	input Operator
	fwd   csrAdjacency // forward CSR (always required)
	rev   csrAdjacency // reverse CSR; required for DirIn / DirBoth

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// edgeTypeFilter maps absolute edge positions (in fwd.EdgesSlice) to type
	// labels.  nil = no type filtering.
	edgeTypeFilter map[uint64]string
	multiplicity   func(srcID, dstID uint64) int64 // per-edge CREATE multiplicity; nil = single-row emit

	edgeType string // optional edge-type filter; empty = no filter

	relCols    []int          // input-row columns holding existing edge IDs; nil = no check
	fwdVerts   []uint64       // snapshot of fwd.VerticesSlice()
	fwdEdges   []graph.NodeID // snapshot of fwd.EdgesSlice()
	fwdHandles []uint64       // snapshot of fwd.HandlesSlice() (nil unless multigraph)
	revVerts   []uint64       // snapshot of rev.VerticesSlice() (nil for DirOut)
	revEdges   []graph.NodeID // snapshot of rev.EdgesSlice() (nil for DirOut)
	revHandles []uint64       // snapshot of rev.HandlesSlice() (nil for DirOut / non-multigraph)
	inputRow   Row            // current input row (borrowed reference)
	// Pending state for emitting an edge N times when its
	// CREATE-multiplicity is greater than 1 (Merge5 [21]). The full row
	// is cached and re-emitted; pendingRemaining counts the extra
	// emissions left after the first one.
	pendingRow Row
	outBuf     []expr.Value // reusable output row backing slice

	// Columnar (ChunkProducer) state — used only on the FillChunk path, driven
	// through the [columnarExpand] wrapper. Zero in row mode, so the row-mode
	// Next path pays for none of it. See [Expand.fillChunk].
	chunkChild ChunkProducer // child presented column-major (nil in row mode)
	cScratch   *Chunk        // current child batch (owned, reused across FillChunk)

	inputCol int // column in the input row that carries the source NodeID
	// current expansion state
	srcID int64 // current source NodeID
	// expansion cursors
	fwdStart, fwdEnd uint64
	revStart, revEnd uint64
	emitCount        int // total rows emitted; drives ctx check cadence
	pendingRemaining int64

	// Columnar fan-out cursor and multiplicity re-emit state (FillChunk path).
	cScratchLen    int   // rows currently held in cScratch
	cRow           int   // index of the current input row within cScratch (outer cursor)
	cPendSrc       int64 // cached (src,edge,dst) triplet re-emitted for CREATE multiplicity
	cPendEdge      int64
	cPendDst       int64
	cPendRemaining int64 // extra chunk re-emissions of the cached triplet still owed

	dir       Direction
	fwdDone   bool // true after all forward edges for current src are exhausted
	chunkMode bool // true while the FillChunk (columnar) path drives the operator
	cScanDone bool // true once the child ChunkProducer is exhausted (a 0-row pull)
}

// ExpandConfig carries the optional configuration for [NewExpand].
type ExpandConfig struct {
	// EdgeTypeFilter maps absolute edge positions to type labels.  Required
	// when EdgeType is non-empty.
	EdgeTypeFilter map[uint64]string
	// MultiplicityFn returns the Cypher CREATE-call multiplicity recorded
	// for the directed edge (srcID, dstID). When the returned count is N >
	// 1, the operator emits the corresponding output row N times in a row,
	// reflecting the openCypher rule that `MATCH ()-[r]->()` enumerates
	// each CREATE call separately even when the underlying simple-graph
	// storage collapsed them to one entry (Merge5 [21]). A nil fn (or
	// returning 0 / 1) disables the multiplicity emit and behaves like a
	// plain single-row Expand.
	MultiplicityFn func(srcID, dstID uint64) int64
	// EdgeType, when non-empty, restricts emitted edges to those whose
	// positional index is present in EdgeTypeFilter with this type label.
	EdgeType string
	// RelCols lists the input-row columns holding edge IDs already traversed
	// by sibling Expand operators in the same MATCH pattern. Each emitted
	// edge must NOT match any of these columns (openCypher 9 §3.2.2
	// relationship-isomorphism / cyphermorphism). Empty disables the
	// check.
	RelCols []int
	// InputCol is the column index in each input row that holds the source
	// NodeID (as expr.IntegerValue).  Defaults to 0.
	InputCol int
	// Direction to follow. Defaults to DirOut when zero.
	Direction Direction
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
		multiplicity:   cfg.MultiplicityFn,
	}
}

// Init initialises the operator and its child.
func (op *Expand) Init(ctx context.Context) error {
	op.ctx = ctx
	op.fwdVerts = op.fwd.VerticesSlice()
	op.fwdEdges = op.fwd.EdgesSlice()
	op.fwdHandles = op.fwd.HandlesSlice()
	if op.dir != DirOut && op.rev != nil {
		op.revVerts = op.rev.VerticesSlice()
		op.revEdges = op.rev.EdgesSlice()
		op.revHandles = op.rev.HandlesSlice()
	}
	op.srcID = -1
	op.fwdDone = true
	op.emitCount = 0
	// Reset the columnar fan-out cursor so a re-Init (pooled/re-run operator)
	// re-pulls from the start. cRow = -1 makes the first advanceInputChunk step
	// to row 0 (or trigger the first child batch pull). These are inert on the
	// row-mode Next path.
	op.cRow = -1
	op.cScratchLen = 0
	op.cScanDone = false
	op.cPendRemaining = 0
	if op.cScratch != nil {
		op.cScratch.Reset()
	}
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
		if op.pendingRemaining > 0 {
			need := len(op.pendingRow)
			if cap(op.outBuf) < need {
				op.outBuf = make([]expr.Value, need)
			}
			op.outBuf = op.outBuf[:need]
			copy(op.outBuf, op.pendingRow)
			*out = op.outBuf
			op.pendingRemaining--
			return true, nil
		}
		// tryFwdEdge returns (true, true) = emitted; (false, true) = skipped
		// (filtered/morphism), retry; (_, false) = no more forward edges.
		if emitted, ok := op.tryFwdEdge(out); ok {
			if emitted {
				op.maybeQueueMultiplicity(*out)
				return true, nil
			}
			continue // skip (filtered or morphism-rejected), try next edge
		}
		// tryRevEdge follows the same convention.
		if emitted, ok := op.tryRevEdge(out); ok {
			if emitted {
				op.maybeQueueMultiplicity(*out)
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

// maybeQueueMultiplicity inspects the just-emitted row's (srcID, dstID)
// pair against the configured MultiplicityFn and, when the recorded
// CREATE count is greater than 1, stages the remaining copies for
// repeated emission via the pending-row slot. The cached row is a
// fresh copy of the buffer so subsequent buildRow calls (which reuse
// outBuf) do not corrupt the queued data.
func (op *Expand) maybeQueueMultiplicity(emitted Row) {
	if op.multiplicity == nil || len(emitted) < 3 {
		return
	}
	srcVal, dstVal := uint64(0), uint64(0)
	if iv, ok := emitted[len(emitted)-3].(expr.IntegerValue); ok {
		srcVal = uint64(iv)
	}
	if iv, ok := emitted[len(emitted)-1].(expr.IntegerValue); ok {
		dstVal = uint64(iv)
	}
	mult := op.multiplicity(srcVal, dstVal)
	if mult <= 1 {
		return
	}
	cp := make(Row, len(emitted))
	copy(cp, emitted)
	op.pendingRow = cp
	op.pendingRemaining = mult - 1
}

// edgeStatus is the outcome of advancing the forward or reverse edge cursor by
// one step. It is the mode-agnostic result the row-mode ([Expand.tryFwdEdge]/
// [Expand.tryRevEdge]) and columnar ([Expand.fillOneChunkRow]) emit paths share,
// so the direction/filter/morphism/multigraph decision has exactly ONE
// implementation.
type edgeStatus uint8

const (
	edgeNone edgeStatus = iota // no edge available in this direction; move on
	edgeSkip                   // an edge was consumed but filtered/morphism-rejected; retry
	edgeEmit                   // an edge is ready to emit; the returned triplet is valid
)

// tryFwdEdge attempts to emit one forward edge for the current source node.
// Returns (true, true) when a row was written, (false, true) when the forward
// cursor needs to skip a filtered edge (caller retries), (_, false) when no
// forward edge is available and the caller should check reverse edges.
func (op *Expand) tryFwdEdge(out *Row) (emitted, handled bool) {
	src, edge, dst, st := op.advanceFwdEdge()
	switch st {
	case edgeNone:
		return false, false
	case edgeSkip:
		return false, true
	default: // edgeEmit
		op.buildRow(out, src, edge, dst)
		op.incEmitCount()
		return true, true
	}
}

// tryRevEdge attempts to emit one reverse edge for the current source node.
// Returns (true, true) when a row was written, (false, true) when the reverse
// cursor needs to skip a filtered edge, (_, false) when no reverse edge is
// available and the caller should pull a new input row.
func (op *Expand) tryRevEdge(out *Row) (emitted, handled bool) {
	src, edge, dst, st := op.advanceRevEdge()
	switch st {
	case edgeNone:
		return false, false
	case edgeSkip:
		return false, true
	default: // edgeEmit
		op.buildRow(out, src, edge, dst)
		op.incEmitCount()
		return true, true
	}
}

// advanceFwdEdge consumes at most one forward edge from the current source's
// cursor and decides its fate WITHOUT emitting. It returns the (srcID, edgeID,
// dstID) triplet and a status: edgeEmit (triplet valid), edgeSkip (an edge was
// consumed but filtered/morphism-rejected — caller retries), or edgeNone (no
// forward edge remains). It is the single source of truth for forward-edge
// semantics shared by [Expand.tryFwdEdge] (row mode) and the columnar path.
func (op *Expand) advanceFwdEdge() (src, edge, dst int64, st edgeStatus) {
	if op.dir == DirIn || op.fwdDone {
		return 0, 0, 0, edgeNone
	}
	if op.fwdStart >= op.fwdEnd {
		op.fwdDone = true
		return 0, 0, 0, edgeNone
	}
	pos := op.fwdStart
	d := op.fwdEdges[pos]
	op.fwdStart++
	if !op.passesFilter(pos) {
		return 0, 0, 0, edgeSkip // filtered out; caller retries
	}
	if !op.passesRelMorphism(int64(pos)) {
		return 0, 0, 0, edgeSkip // cyphermorphism: duplicate edge; caller retries
	}
	return op.srcID, int64(pos), int64(d), edgeEmit
}

// advanceRevEdge consumes at most one reverse edge from the current source's
// cursor and decides its fate WITHOUT emitting, following the same three-status
// convention as [Expand.advanceFwdEdge]. It is the single source of truth for
// reverse-edge semantics — undirected self-loop dedup, the reverse edge-type
// filter, the multigraph per-instance forward-position recovery, and the
// canonical edge ID for cyphermorphism — shared by [Expand.tryRevEdge] (row
// mode) and the columnar path.
func (op *Expand) advanceRevEdge() (src, edge, dst int64, st edgeStatus) {
	if op.dir == DirOut || op.revVerts == nil {
		return 0, 0, 0, edgeNone
	}
	if op.revStart >= op.revEnd {
		return 0, 0, 0, edgeNone
	}
	pos := op.revStart
	d := op.revEdges[pos]
	op.revStart++
	// Undirected self-loop deduplication: when the pattern is undirected
	// (DirBoth) and the reverse edge being considered is a self-loop on
	// the current source node (dst == srcID), the same edge has already
	// been emitted by the forward pass. Skip it to honour the openCypher
	// rule that every matched edge appears exactly once for an undirected
	// relationship pattern. The skip is restricted to DirBoth because a
	// pure DirIn traversal does not perform the forward pass and therefore
	// must still emit reverse self-loops.
	if op.dir == DirBoth && int64(d) == op.srcID {
		return 0, 0, 0, edgeSkip // self-loop already emitted by forward pass
	}
	// Edge-type filter: locate the corresponding forward-edge position so
	// the existing fwd-position-keyed filter map applies. The reverse edge
	// (revEdges[pos] → src) corresponds to the forward edge
	// (dst → src), found by scanning dst's outgoing range in the forward
	// CSR. The scan is O(deg(dst)) per reverse edge; acceptable for typical
	// graphs where in-degree and out-degree are bounded.
	if op.edgeType != "" {
		if !op.reverseEdgePassesFilter(uint64(d), uint64(op.srcID)) {
			return 0, 0, 0, edgeSkip // filtered out; caller retries
		}
	}
	// Canonical edge ID: prefer the forward-edge position when the
	// (dst → src) edge exists in the forward CSR, so cyphermorphism
	// observes the SAME id for the forward and reverse traversals of the
	// same edge. This is required for openCypher 9 §3.2.2: an undirected
	// `(:Label2)--()` step that follows a previous forward hop must
	// reject the same edge being matched in the reverse direction.
	var fwdPos uint64
	var hasFwd bool
	if op.revHandles != nil && op.fwdHandles != nil {
		// Multigraph: recover the SPECIFIC forward edge instance by
		// matching the stable handle that travelled with this reverse
		// slot (csr.BuildReverse keeps one handle per logical edge across
		// both directions). Without this, parallel edges (dst -> src)
		// would all collapse onto the first forward position and report a
		// single merged relationship type on the reverse hop (rmp #1634).
		fwdPos, hasFwd = op.lookupFwdEdgePosByHandle(uint64(d), uint64(op.srcID), op.revHandles[pos])
	} else {
		fwdPos, hasFwd = op.lookupFwdEdgePos(uint64(d), uint64(op.srcID))
	}
	var edgeID int64
	if hasFwd {
		edgeID = int64(fwdPos)
	} else {
		edgeID = int64(uint64(len(op.fwdEdges)) + pos)
	}
	if !op.passesRelMorphism(edgeID) {
		return 0, 0, 0, edgeSkip // cyphermorphism: duplicate edge; caller retries
	}
	return op.srcID, edgeID, int64(d), edgeEmit
}

// lookupFwdEdgePos returns the forward-CSR position of the edge
// (src → dst), or (0, false) when no such edge exists. Used by the
// reverse-traversal emit path so the cyphermorphism check observes the
// same edge ID for forward and reverse traversals of an undirected edge.
func (op *Expand) lookupFwdEdgePos(src, dst uint64) (uint64, bool) {
	if src+1 >= uint64(len(op.fwdVerts)) {
		return 0, false
	}
	start := op.fwdVerts[src]
	end := op.fwdVerts[src+1]
	for pos := start; pos < end; pos++ {
		if uint64(op.fwdEdges[pos]) == dst {
			return pos, true
		}
	}
	return 0, false
}

// lookupFwdEdgePosByHandle returns the forward-CSR position of the edge
// (src -> dst) whose stable handle equals handle, or (0, false) when no
// such edge exists. Unlike [lookupFwdEdgePos] it disambiguates parallel
// edges — multiple src -> dst slots — by their per-instance handle, so a
// reverse traversal recovers the exact forward edge instance it came
// from rather than always the first (rmp #1634). The caller guarantees
// op.fwdHandles is non-nil (a multigraph snapshot).
func (op *Expand) lookupFwdEdgePosByHandle(src, dst, handle uint64) (uint64, bool) {
	if src+1 >= uint64(len(op.fwdVerts)) {
		return 0, false
	}
	start := op.fwdVerts[src]
	end := op.fwdVerts[src+1]
	for pos := start; pos < end; pos++ {
		if uint64(op.fwdEdges[pos]) == dst && op.fwdHandles[pos] == handle {
			return pos, true
		}
	}
	return 0, false
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
			// Membership in the filter map is sufficient — the map only
			// contains edges of accepted types (multi-type [r:A|B] support).
			if _, ok := op.edgeTypeFilter[fwdPos]; ok {
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
	// The source column may carry either the raw NodeID (the canonical
	// in-pipeline encoding emitted by NodeScan/Expand) or a full
	// NodeValue produced by a projection alias (e.g. `WITH a` followed
	// by `MATCH (a)-->(b)`). Accept either form so cross-clause forwarding
	// of node variables through a projection does not silently drop the
	// expansion.
	switch v := inputRow[op.inputCol].(type) {
	case expr.IntegerValue:
		op.srcID = int64(v)
	case expr.NodeValue:
		op.srcID = int64(v.ID)
	default:
		return false, nil // not a node-typed value; skip silently
	}
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
	if op.chunkMode {
		return op.passesRelMorphismChunk(edgeID)
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
//
// The filter map is built by api.go::buildEdgeTypeFilter to contain only
// edge positions whose type is in the accepted set, so membership in the
// map is sufficient: when EdgeType is non-empty (any filter was requested),
// pos must appear in the filter; otherwise everything passes.
//
// This is correct for both single-type (`[r:KNOWS]`) and multi-type
// (`[r:KNOWS|HATES]`) patterns. Pre-fix the predicate compared the
// looked-up type against a single op.edgeType label, which silently
// excluded edges of every accepted type other than the first.
func (op *Expand) passesFilter(pos uint64) bool {
	if op.edgeType == "" {
		return true
	}
	if op.edgeTypeFilter == nil {
		return false
	}
	_, ok := op.edgeTypeFilter[pos]
	return ok
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
	op.cScratch = nil
	return op.input.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Columnar (ChunkProducer) path
// ─────────────────────────────────────────────────────────────────────────────
//
// columnarExpand presents an [Expand] as a [ChunkProducer]/[NodeIDColumnProducer]
// so a columnar-aware parent (a [ColumnarFilter] over the far-node property, or a
// columnar aggregation over the traversal output) drains it column-major, keeping
// the chunk chain unbroken across the traversal (design docs/columnar-deepening-
// design.md §4). The traversal itself is NOT vectorized — a single-hop pointer
// chase is not scan-heavy — so this removes nothing on the expansion; it is output
// plumbing that lets the O(edges) rows Expand emits flow into an unboxed filter /
// aggregation above.
//
// The output chunk is the input passthrough columns (whatever the child emitted,
// copied cell-for-cell) followed by three int64 columns srcID, edgeID, dstID. The
// dstID/srcID columns are raw NodeIDs, so a columnar predicate can read the far
// node's NodeID unboxed to fetch a property — hence [NodeIDColumnProducer].
//
// Only the planner builds it, and ONLY when the child is itself a [ChunkProducer]
// (design §6.2): a plain [Expand] over a row-mode child stays row-mode (its Next is
// the byte-identical fallback). This is why the [ChunkProducer] methods live on
// this distinct wrapper rather than on [Expand] — a plain Expand must NOT advertise
// [ChunkProducer], or a generic columnar sink would drain it with no unbroken chain
// below and no win.
//
// columnarExpand is NOT safe for concurrent use.
type columnarExpand struct {
	*Expand
}

// NewColumnarExpand presents exp as a [ChunkProducer] when exp's child is itself a
// [ChunkProducer] (so [Expand.fillChunk] can pull it column-major and the chunk
// chain stays unbroken). It returns (wrapper, true) on success or (nil, false) when
// the child is row-mode, in which case the caller keeps the plain row-mode exp. The
// returned wrapper also implements [NodeIDColumnProducer].
func NewColumnarExpand(exp *Expand) (ChunkProducer, bool) {
	cp, ok := exp.input.(ChunkProducer)
	if !ok {
		return nil, false
	}
	exp.chunkChild = cp
	return columnarExpand{exp}, true
}

// NewOutputChunk returns a [Chunk] whose columns mirror the child's output columns
// (the passthrough) followed by three int64 columns srcID, edgeID, dstID. It
// implements [ChunkProducer].
func (c columnarExpand) NewOutputChunk(capacity int) *Chunk { return c.columnarOutputChunk(capacity) }

// FillChunk appends up to maxRows more (passthrough || srcID, edgeID, dstID) rows
// into dst column-major and returns the number appended (0 at end-of-stream). It
// implements [ChunkProducer].
func (c columnarExpand) FillChunk(dst *Chunk, maxRows int) (int, error) {
	return c.fillChunk(dst, maxRows)
}

// nodeIDColumnProducer marks columnarExpand as a [NodeIDColumnProducer]: the
// passthrough node columns and the srcID/dstID columns carry raw int64 NodeIDs.
func (c columnarExpand) nodeIDColumnProducer() {}

// columnarOutputChunk builds the output chunk schema: the child's per-column kinds
// (the passthrough), then three int64 columns for srcID, edgeID, dstID. The
// passthrough kinds are read from a fresh child template so a scalar column stays
// unboxed; a non-scalar (boxed) child column stays boxed, byte-identically.
func (op *Expand) columnarOutputChunk(capacity int) *Chunk {
	template := op.chunkChild.NewOutputChunk(capacity)
	p := template.NumCols()
	kinds := make([]expr.Kind, p+3)
	for j := 0; j < p; j++ {
		kinds[j] = template.ColKind(j)
	}
	kinds[p] = expr.KindInteger   // srcID
	kinds[p+1] = expr.KindInteger // edgeID
	kinds[p+2] = expr.KindInteger // dstID
	return NewChunk(capacity, kinds...)
}

// fillChunk is the column-major counterpart of [Expand.Next]: it appends up to
// maxRows output rows into dst and returns the number appended (0 at
// end-of-stream). It preserves EXACTLY what Next guarantees — DirOut/DirIn/DirBoth,
// the edge-type filter, edge multiplicity, cyphermorphism, and multigraph
// per-instance relationship typing on reverse hops — by reusing the same
// [Expand.advanceFwdEdge]/[Expand.advanceRevEdge] decision helpers Next uses.
//
// The two-level fan-out cursor (current input-row index cRow within the child
// batch, plus the per-source edge cursors) PERSISTS across calls: filling stops
// mid-input-row when dst reaches maxRows and resumes on the next call (the DuckDB
// partially-consumed-input pattern, one level deeper than [ColumnarFilter]'s
// scratchPos). A short return (n < maxRows) therefore means the CHILD is exhausted,
// which the drain relies on for end-of-stream.
func (op *Expand) fillChunk(dst *Chunk, maxRows int) (int, error) {
	op.chunkMode = true
	n := 0
	for n < maxRows {
		if n&4095 == 0 {
			if err := op.ctx.Err(); err != nil {
				return n, err
			}
		}
		appended, done, err := op.fillOneChunkRow(dst)
		if err != nil {
			return n, err
		}
		if done {
			return n, nil
		}
		if appended {
			n++
		}
	}
	return n, nil
}

// fillOneChunkRow advances the columnar state machine until it writes exactly one
// output row into dst (appended=true) or reaches end-of-stream (done=true). It
// mirrors the body of the [Expand.Next] outer loop, appending column-major instead
// of building a boxed [Row]: it drains pending CREATE-multiplicity re-emissions
// first, then a forward edge, then a reverse edge, and advances to the next input
// row when the current source's edges are exhausted.
func (op *Expand) fillOneChunkRow(dst *Chunk) (appended, done bool, err error) {
	for {
		if op.cPendRemaining > 0 {
			op.appendChunkRow(dst, op.cRow, op.cPendSrc, op.cPendEdge, op.cPendDst)
			op.cPendRemaining--
			return true, false, nil
		}
		if src, edge, d, st := op.advanceFwdEdge(); st != edgeNone {
			if st == edgeEmit {
				op.appendChunkRow(dst, op.cRow, src, edge, d)
				op.maybeQueueMultiplicityChunk(src, edge, d)
				return true, false, nil
			}
			continue // skipped (filtered / morphism-rejected)
		}
		if src, edge, d, st := op.advanceRevEdge(); st != edgeNone {
			if st == edgeEmit {
				op.appendChunkRow(dst, op.cRow, src, edge, d)
				op.maybeQueueMultiplicityChunk(src, edge, d)
				return true, false, nil
			}
			continue // skipped reverse edge
		}
		eos, aerr := op.advanceInputChunk()
		if aerr != nil {
			return false, false, aerr
		}
		if eos {
			return false, true, nil
		}
	}
}

// advanceInputChunk moves the fan-out cursor to the next usable input row,
// pulling a fresh child batch when the current one is drained, and loads that
// row's adjacency ranges. It returns eos=true at end-of-stream. Narrow rows and
// rows whose source column is not a node id are skipped, mirroring the row-mode
// [Expand.advanceInput] "skip silently" behaviour.
func (op *Expand) advanceInputChunk() (eos bool, err error) {
	for {
		op.cRow++
		if op.cRow >= op.cScratchLen {
			if op.cScanDone {
				return true, nil
			}
			if err := op.ctx.Err(); err != nil {
				return false, err
			}
			if op.cScratch == nil {
				op.cScratch = op.chunkChild.NewOutputChunk(DefaultChunkCapacity)
			} else {
				op.cScratch.Reset()
			}
			nrows, ferr := op.chunkChild.FillChunk(op.cScratch, op.cScratch.Cap())
			if ferr != nil {
				return false, ferr
			}
			op.cScratchLen = nrows
			op.cRow = 0
			if nrows == 0 {
				op.cScanDone = true
				return true, nil
			}
		}
		sid, ok := op.chunkSrcID(op.cRow)
		if !ok {
			continue // narrow / non-node / null source: skip this row
		}
		op.srcID = sid
		op.loadAdjacency(uint64(sid))
		return false, nil
	}
}

// chunkSrcID reads the source NodeID from the input column of cScratch row r,
// mirroring [Expand.advanceInput]'s acceptance of either a raw NodeID or a
// [expr.NodeValue] produced by a projection alias. A NULL, a narrow row, or a
// non-node value returns ok=false so the caller skips the row.
func (op *Expand) chunkSrcID(r int) (int64, bool) {
	if op.inputCol < 0 || op.inputCol >= op.cScratch.NumCols() {
		return 0, false
	}
	if op.cScratch.IsInt64Column(op.inputCol) {
		v, valid := op.cScratch.Int64(op.inputCol, r)
		if !valid {
			return 0, false
		}
		return v, true
	}
	switch v := op.cScratch.BoxCell(op.inputCol, r).(type) {
	case expr.IntegerValue:
		return int64(v), true
	case expr.NodeValue:
		return int64(v.ID), true
	default:
		return 0, false
	}
}

// appendChunkRow appends one output row into dst: the passthrough columns copied
// cell-for-cell (unboxed for a scalar) from cScratch row srcRow, then the three
// int64 columns srcID, edgeID, dstID. All p+3 columns advance by one, keeping dst
// rectangular.
func (op *Expand) appendChunkRow(dst *Chunk, srcRow int, src, edge, dstID int64) {
	p := op.cScratch.NumCols()
	for j := 0; j < p; j++ {
		op.cScratch.CopyCellTo(j, srcRow, dst, j)
	}
	dst.AppendInt64(p, src)
	dst.AppendInt64(p+1, edge)
	dst.AppendInt64(p+2, dstID)
}

// maybeQueueMultiplicityChunk mirrors [Expand.maybeQueueMultiplicity] for the
// columnar path: when the CREATE-multiplicity recorded for (src, dst) is greater
// than one, it caches the triplet and stages the remaining copies for re-emission
// (the passthrough is re-read from the still-current cScratch row cRow, which does
// not advance while re-emissions are owed).
func (op *Expand) maybeQueueMultiplicityChunk(src, edge, dst int64) {
	if op.multiplicity == nil {
		return
	}
	mult := op.multiplicity(uint64(src), uint64(dst))
	if mult <= 1 {
		return
	}
	op.cPendSrc, op.cPendEdge, op.cPendDst = src, edge, dst
	op.cPendRemaining = mult - 1
}

// passesRelMorphismChunk is the columnar counterpart of the row-mode
// cyphermorphism check in [Expand.passesRelMorphism]: it rejects edgeID when it
// already appears in any sibling relationship column of the current cScratch input
// row cRow. A sibling edge column is an int64 column (a prior Expand's edgeID); a
// non-int64 column is boxed once and inspected, byte-identically to the row path.
func (op *Expand) passesRelMorphismChunk(edgeID int64) bool {
	for _, col := range op.relCols {
		if col < 0 || col >= op.cScratch.NumCols() {
			continue
		}
		if op.cScratch.IsInt64Column(col) {
			if v, valid := op.cScratch.Int64(col, op.cRow); valid && v == edgeID {
				return false
			}
			continue
		}
		if iv, ok := op.cScratch.BoxCell(col, op.cRow).(expr.IntegerValue); ok && int64(iv) == edgeID {
			return false
		}
	}
	return true
}
