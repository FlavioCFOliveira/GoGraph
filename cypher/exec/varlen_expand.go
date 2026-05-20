package exec

// varlen_expand.go — VarLengthExpand operator (task-261).
//
// VarLengthExpand performs BFS from each input source node, bounded by
// [minHops..maxHops], and emits one row per path whose hop count is in
// [minHops..maxHops].
//
// # Path representation
//
// A path in progress is a []edgeStep where each edgeStep holds the edge
// position (uint64) and the destination node ID. The path is kept on a BFS
// queue of pathState values.
//
// # Edge deduplication within a single path
//
// Cypher relationship-uniqueness: no edge may appear more than once in a
// single path. Each pathState carries a compact []uint64 bitset that tracks
// which edge IDs are already on that path. Two uint64 words cover 128 edge
// positions; for larger CSRs the bitset grows dynamically.
//
// # Safety cap
//
// If the total number of edge traversals across all paths (reset per input row)
// exceeds maxEdgesTraversed (default 1,000,000), the operator returns
// [ErrVarLenCapExceeded]. This prevents runaway queries on dense graphs.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and inside the BFS inner
// loop every 4096 steps.
//
// # Concurrency
//
// VarLengthExpand is NOT safe for concurrent use.

import (
	"context"
	"errors"

	"gograph/cypher/expr"
	"gograph/graph"
)

// ErrVarLenCapExceeded is returned when a VarLengthExpand exceeds its
// configured maximum edge traversal count.
var ErrVarLenCapExceeded = errors.New("exec: variable-length expand safety cap exceeded")

// defaultMaxEdgesTraversed is the default safety cap for VarLengthExpand.
const defaultMaxEdgesTraversed = 1_000_000

// edgeStep is one hop in a BFS path.
type edgeStep struct {
	edgePos uint64 // absolute edge position in the forward CSR edges array
	dstID   uint64 // destination node ID
}

// pathState is the state of one BFS path being explored.
type pathState struct {
	hops    int        // number of hops taken so far
	srcNode uint64     // source node of the most recent hop (=current node)
	path    []edgeStep // edges taken so far (length == hops)
	visited []uint64   // bitset of edge positions used in this path
}

// VarLengthExpand is a Volcano pipeline operator that performs bounded BFS
// variable-length expansion.
//
// VarLengthExpand is NOT safe for concurrent use.
type VarLengthExpand struct {
	input    Operator
	fwd      csrAdjacency
	rev      csrAdjacency
	dir      Direction
	edgeType string
	// edgeTypeFilter maps absolute forward edge positions to type labels.
	edgeTypeFilter    map[uint64]string
	inputCol          int
	minHops           int
	maxHops           int
	maxEdgesTraversed int

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// CSR snapshots (valid after Init).
	fwdVerts []uint64
	fwdEdges []graph.NodeID
	revVerts []uint64
	revEdges []graph.NodeID

	// BFS state for the current input row.
	queue        []pathState // BFS frontier
	inputRow     Row         // current outer input row (stable copy)
	inputEOS     bool        // true after input plan exhausted
	edgesVisited int         // traversal counter for safety cap (reset per input row)

	// pending result rows: paths whose hop count is in [minHops..maxHops]
	// that have been collected during BFS but not yet emitted.
	results   []pathState
	resultIdx int

	outBuf []expr.Value
}

// VarLengthConfig carries configuration for [NewVarLengthExpand].
type VarLengthConfig struct {
	// Direction to follow. Defaults to DirOut when zero.
	Direction Direction
	// EdgeType, when non-empty, restricts expansion to edges of this type.
	EdgeType string
	// EdgeTypeFilter maps absolute edge positions to type labels.
	EdgeTypeFilter map[uint64]string
	// InputCol is the column index in each input row that holds the source
	// NodeID. Defaults to 0.
	InputCol int
	// MinHops is the minimum path length (inclusive). Must be ≥ 0.
	MinHops int
	// MaxHops is the maximum path length (inclusive). Must be ≥ MinHops.
	// Use math.MaxInt for unbounded (not recommended without a safety cap).
	MaxHops int
	// MaxEdgesTraversed is the safety cap on total edge traversals per input
	// row. Defaults to 1,000,000 when 0.
	MaxEdgesTraversed int
}

// NewVarLengthExpand creates a VarLengthExpand operator.
func NewVarLengthExpand(input Operator, fwd, rev csrAdjacency, cfg VarLengthConfig) *VarLengthExpand {
	dir := cfg.Direction
	if dir == 0 {
		dir = DirOut
	}
	capVal := cfg.MaxEdgesTraversed
	if capVal <= 0 {
		capVal = defaultMaxEdgesTraversed
	}
	return &VarLengthExpand{
		input:             input,
		fwd:               fwd,
		rev:               rev,
		dir:               dir,
		edgeType:          cfg.EdgeType,
		edgeTypeFilter:    cfg.EdgeTypeFilter,
		inputCol:          cfg.InputCol,
		minHops:           cfg.MinHops,
		maxHops:           cfg.MaxHops,
		maxEdgesTraversed: capVal,
	}
}

// Init initialises the operator.
func (op *VarLengthExpand) Init(ctx context.Context) error {
	op.ctx = ctx
	op.fwdVerts = op.fwd.VerticesSlice()
	op.fwdEdges = op.fwd.EdgesSlice()
	if op.dir != DirOut && op.rev != nil {
		op.revVerts = op.rev.VerticesSlice()
		op.revEdges = op.rev.EdgesSlice()
	}
	op.queue = op.queue[:0]
	op.results = op.results[:0]
	op.resultIdx = 0
	op.inputRow = nil
	op.inputEOS = false
	op.edgesVisited = 0
	return op.input.Init(ctx)
}

// Next emits the next (inputRow... || pathEdgesAsListValue || dstNodeID) row.
// The path is encoded as a [expr.ListValue] of edge positions (IntegerValues),
// followed by the destination node ID as an IntegerValue.
func (op *VarLengthExpand) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// If we have pending results from the current BFS, emit the next one.
		if op.resultIdx < len(op.results) {
			ps := op.results[op.resultIdx]
			op.resultIdx++
			op.buildRow(out, op.inputRow, ps)
			return true, nil
		}

		// Advance BFS for the current input row.
		if len(op.queue) > 0 {
			if err := op.runBFS(); err != nil {
				return false, err
			}
			if op.resultIdx < len(op.results) {
				ps := op.results[op.resultIdx]
				op.resultIdx++
				op.buildRow(out, op.inputRow, ps)
				return true, nil
			}
			continue
		}

		if op.inputEOS {
			return false, nil
		}

		// Pull next input row.
		var inputRow Row
		ok, err := op.input.Next(&inputRow)
		if err != nil {
			return false, err
		}
		if !ok {
			op.inputEOS = true
			return false, nil
		}

		// Stable snapshot.
		cp := make(Row, len(inputRow))
		copy(cp, inputRow)
		op.inputRow = cp

		// Seed BFS from the source node.
		if op.inputCol >= len(cp) {
			continue // row too narrow; skip
		}
		iv, ok2 := cp[op.inputCol].(expr.IntegerValue)
		if !ok2 {
			continue // not an integer; skip
		}
		srcID := uint64(iv)

		// Reset per-source state.
		op.queue = op.queue[:0]
		op.results = op.results[:0]
		op.resultIdx = 0
		op.edgesVisited = 0

		// If minHops == 0, the source node itself is a valid result.
		if op.minHops == 0 {
			op.results = append(op.results, pathState{
				hops:    0,
				srcNode: srcID,
				path:    nil,
				visited: nil,
			})
		}

		// Seed BFS queue with each neighbour of srcID (hop 1).
		if op.maxHops > 0 {
			if err := op.seedQueue(srcID); err != nil {
				return false, err
			}
		}
	}
}

// seedQueue enqueues all one-hop neighbours of srcID into the BFS queue.
// It returns an error if the safety cap is exceeded during seeding.
func (op *VarLengthExpand) seedQueue(srcID uint64) error {
	// Forward edges.
	if op.dir != DirIn {
		if err := op.enqueueEdges(srcID, true, nil); err != nil {
			return err
		}
	}
	// Reverse edges.
	if op.dir != DirOut && op.revVerts != nil {
		if err := op.enqueueEdges(srcID, false, nil); err != nil {
			return err
		}
	}
	return nil
}

// runBFS pops items from op.queue, expands them, and collects results in
// op.results. It processes one frontier level at a time (returning after the
// first result is collected, or when the frontier is empty).
// For simplicity (and to respect cancellation), we process the full queue in
// one call, populating op.results, and return.
func (op *VarLengthExpand) runBFS() error {
	// Swap queue into a local snapshot and clear op.queue for the next level.
	work := op.queue
	op.queue = op.queue[:0]

	for i := range work {
		if err := op.ctx.Err(); err != nil {
			return err
		}
		ps := work[i]
		// If this path is within the emit window, record it.
		if ps.hops >= op.minHops && ps.hops <= op.maxHops {
			// Copy pathState for the result (path slice is already stable).
			op.results = append(op.results, ps)
		}
		// Expand further if we can go deeper.
		if ps.hops < op.maxHops {
			if err := op.expandPath(ps); err != nil {
				return err
			}
		}
	}
	return nil
}

// expandPath enqueues all one-hop extensions of ps into op.queue.
func (op *VarLengthExpand) expandPath(ps pathState) error {
	if op.dir != DirIn {
		if err := op.enqueueEdges(ps.srcNode, true, &ps); err != nil {
			return err
		}
	}
	if op.dir != DirOut && op.revVerts != nil {
		if err := op.enqueueEdges(ps.srcNode, false, &ps); err != nil {
			return err
		}
	}
	return nil
}

// enqueueEdges enqueues all qualifying edges from node uid into op.queue.
// isFwd selects forward vs reverse edges. parent is nil only for the seed call.
func (op *VarLengthExpand) enqueueEdges(uid uint64, isFwd bool, parent *pathState) error {
	var (
		verts []uint64
		edges []graph.NodeID
		base  uint64 // edge position base (0 for fwd, len(fwdEdges) for rev synthetic IDs)
	)
	if isFwd {
		verts = op.fwdVerts
		edges = op.fwdEdges
		base = 0
	} else {
		verts = op.revVerts
		edges = op.revEdges
		base = uint64(len(op.fwdEdges))
	}

	if uid+1 >= uint64(len(verts)) {
		return nil
	}
	start, end := verts[uid], verts[uid+1]

	for pos := start; pos < end; pos++ {
		op.edgesVisited++
		if op.edgesVisited > op.maxEdgesTraversed {
			return ErrVarLenCapExceeded
		}

		absPos := base + pos
		dst := uint64(edges[pos])

		// Edge-type filter (forward only; reverse edges skip type filter).
		if isFwd && op.edgeType != "" {
			t, ok := op.edgeTypeFilter[absPos]
			if !ok || t != op.edgeType {
				continue
			}
		}

		// Relationship-uniqueness: skip if this edge is already on the path.
		if parent != nil && bitsetContains(parent.visited, absPos) {
			continue
		}

		// Build new path state.
		var newPath []edgeStep
		var newVisited []uint64
		hops := 1
		if parent != nil {
			hops = parent.hops + 1
			newPath = make([]edgeStep, len(parent.path)+1)
			copy(newPath, parent.path)
			newPath[len(parent.path)] = edgeStep{edgePos: absPos, dstID: dst}
			newVisited = bitsetAdd(parent.visited, absPos)
		} else {
			newPath = []edgeStep{{edgePos: absPos, dstID: dst}}
			newVisited = bitsetAdd(nil, absPos)
		}

		op.queue = append(op.queue, pathState{
			hops:    hops,
			srcNode: dst,
			path:    newPath,
			visited: newVisited,
		})
	}
	return nil
}

// buildRow writes (inputRow... || pathList || dstID) into out.
// pathList is a ListValue of IntegerValues (edge positions).
// dstID is the terminal node ID.
func (op *VarLengthExpand) buildRow(out *Row, inputRow Row, ps pathState) {
	// Encode path as a list of edge positions.
	pathList := make(expr.ListValue, len(ps.path))
	for i, step := range ps.path {
		pathList[i] = expr.IntegerValue(int64(step.edgePos))
	}

	var dstID expr.Value
	if len(ps.path) == 0 {
		// hop-0: source is the destination.
		if op.inputCol < len(inputRow) {
			dstID = inputRow[op.inputCol]
		} else {
			dstID = expr.Null
		}
	} else {
		dstID = expr.IntegerValue(int64(ps.path[len(ps.path)-1].dstID))
	}

	need := len(inputRow) + 2
	if cap(op.outBuf) < need {
		op.outBuf = make([]expr.Value, need)
	}
	op.outBuf = op.outBuf[:need]
	copy(op.outBuf, inputRow)
	op.outBuf[len(inputRow)] = pathList
	op.outBuf[len(inputRow)+1] = dstID
	*out = op.outBuf
}

// Close releases resources.
func (op *VarLengthExpand) Close() error {
	op.queue = nil
	op.results = nil
	op.outBuf = nil
	op.inputRow = nil
	return op.input.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Compact bitset helpers (inline, no alloc for ≤128 edges)
// ─────────────────────────────────────────────────────────────────────────────

// bitsetContains reports whether pos is set in bs.
func bitsetContains(bs []uint64, pos uint64) bool {
	word := pos / 64
	bit := pos % 64
	if word >= uint64(len(bs)) {
		return false
	}
	return bs[word]>>bit&1 == 1
}

// bitsetAdd returns a new bitset with pos set. The original slice is not
// modified (copy-on-write semantics so each path branch has its own copy).
func bitsetAdd(bs []uint64, pos uint64) []uint64 {
	word := pos / 64
	bit := pos % 64
	need := int(word) + 1
	newBs := make([]uint64, max(len(bs), need))
	copy(newBs, bs)
	newBs[word] |= 1 << bit
	return newBs
}
