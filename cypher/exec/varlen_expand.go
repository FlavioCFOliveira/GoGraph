package exec

// varlen_expand.go — VarLengthExpand operator (task-261, task-393).
//
// VarLengthExpand performs level-synchronous BFS from each input source node,
// bounded by [minHops..maxHops], and emits one row per path whose hop count is
// in [minHops..maxHops].
//
// # Algorithm choice (why BFS, not DFS)
//
// For bounded variable-length expansion in Cypher (-[*minHops..maxHops]->) the
// task is to enumerate every simple path (under relationship-isomorphism) whose
// length falls in the requested window. The candidate algorithms are:
//
//   - DFS with a per-path edge bitset. Same worst-case asymptotic complexity
//     O(b^d) where b is the average branching factor and d is maxHops, but
//     emission order is depth-first and harder to bound by depth.
//   - BFS level-synchronous expansion. Same asymptotic complexity, but emission
//     follows BFS order — natural fit for a maxHops cap, and enables clean
//     pre-emption between levels (cancellation check at each frontier).
//   - Bidirectional BFS. Only useful when both endpoints are fixed (i.e. for
//     shortestPath); variable-length expansion has an unbound destination, so
//     no bidirectional variant applies.
//
// BFS is chosen because (a) cancellation/observability are simpler at frontier
// boundaries, (b) the level-synchronous frontier has a smooth memory profile
// proportional to the branching at a single level rather than the full
// depth-first stack, and (c) it matches how graph DBs typically document
// variable-length matching (see Neo4j ExpandInto / ExpandIntoVarLength and
// Robinson/Webber/Eifrem, "Graph Databases", 2nd ed., chapter on traversal).
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
// positions; for larger CSRs the bitset grows dynamically. The bitset is
// copied per branch (copy-on-write) so distinct path suffixes never alias.
//
// # Read/write separation in the BFS loop
//
// runBFS reads the current frontier (op.queue) while expansion appends new
// path states to a separate slice (op.nextQueue). The two are swapped at the
// end of the level. This is mandatory: writing into the same backing array
// that drives the read loop would silently overwrite already-staged frontier
// entries — a slice-aliasing hazard fixed in task-393.
//
// # Safety cap
//
// If the total number of edge traversals across all paths (reset per input row)
// exceeds maxEdgesTraversed (default 1,000,000), the operator returns
// [ErrVarLenCapExceeded]. This prevents runaway queries on dense graphs.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and at the top of each
// runBFS iteration over the current frontier.
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

	// revToFwd maps a reverse-CSR edge position to its corresponding
	// forward-CSR edge position. Used by the relationship-uniqueness
	// bitset to recognise that a reverse traversal of the same physical
	// edge is NOT a distinct edge. Built lazily in Init for DirBoth
	// traversals only. Entry ^uint64(0) means "unresolved" (e.g.
	// out-of-range vertex IDs); callers fall back to the synthetic
	// reverse absPos in that rare case.
	revToFwd []uint64

	// BFS state for the current input row. Two slices are kept and ping-ponged
	// per BFS level: `queue` is read by runBFS while extensions are appended to
	// `nextQueue`. After each level the two are swapped. This avoids the
	// slice-aliasing hazard that arises if the same backing array is both read
	// and written in the same call.
	queue        []pathState // current BFS frontier (read)
	nextQueue    []pathState // next BFS frontier (write target during expansion)
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
	op.nextQueue = op.nextQueue[:0]
	op.results = op.results[:0]
	op.resultIdx = 0
	op.inputRow = nil
	op.inputEOS = false
	op.edgesVisited = 0
	// Precompute a reverse-edge-position → forward-edge-position mapping
	// so the relationship-uniqueness bitset can dedupe the same physical
	// edge across direction. For each reverse edge (b←a) in revEdges,
	// scan a's forward adjacency for the matching destination b. Without
	// this, DirBoth VLE traversal can use the same edge twice (once
	// forward, once reverse encoding) and produce duplicated paths
	// (Match9 [3]/[4]).
	if op.dir == DirBoth && op.revEdges != nil {
		op.revToFwd = make([]uint64, len(op.revEdges))
		for revUid := uint64(0); revUid+1 < uint64(len(op.revVerts)); revUid++ {
			start, end := op.revVerts[revUid], op.revVerts[revUid+1]
			for revPos := start; revPos < end; revPos++ {
				fwdSrc := uint64(op.revEdges[revPos])
				// Find the forward position p such that fwdEdges[p] = revUid
				// inside fwdSrc's adjacency range. The reverse CSR is the
				// transpose of the forward CSR, so each reverse entry has
				// exactly one forward counterpart.
				if fwdSrc+1 >= uint64(len(op.fwdVerts)) {
					op.revToFwd[revPos] = ^uint64(0) // unresolved
					continue
				}
				fStart, fEnd := op.fwdVerts[fwdSrc], op.fwdVerts[fwdSrc+1]
				fwdPos := ^uint64(0)
				for fp := fStart; fp < fEnd; fp++ {
					if uint64(op.fwdEdges[fp]) == revUid {
						fwdPos = fp
						break
					}
				}
				op.revToFwd[revPos] = fwdPos
			}
		}
	}
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
		op.nextQueue = op.nextQueue[:0]
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

// seedQueue enqueues all one-hop neighbours of srcID into the BFS frontier
// (op.queue), which at this point is empty. It returns an error if the safety
// cap is exceeded during seeding.
func (op *VarLengthExpand) seedQueue(srcID uint64) error {
	// Forward edges.
	if op.dir != DirIn {
		if err := op.enqueueEdges(srcID, true, nil, &op.queue); err != nil {
			return err
		}
	}
	// Reverse edges.
	if op.dir != DirOut && op.revVerts != nil {
		if err := op.enqueueEdges(srcID, false, nil, &op.queue); err != nil {
			return err
		}
	}
	return nil
}

// runBFS pops items from op.queue, expands them, and collects results in
// op.results. It processes one frontier level at a time and returns after the
// frontier is fully consumed; the caller decides whether to drain more results
// or to invoke runBFS again to consume the next level.
//
// Read/write separation. The current frontier (`op.queue`) is iterated in this
// call while new extensions are appended to `op.nextQueue`. At the end of the
// call the two slices are swapped, so on the next call `op.queue` carries the
// new frontier and `op.nextQueue` is reset to length 0 for the level that
// follows. This split is mandatory: writing into the same backing array that
// drives the loop would cause already-staged items to be silently overwritten
// by appends from later iterations (slice-aliasing hazard).
func (op *VarLengthExpand) runBFS() error {
	// Ensure nextQueue starts empty for this level.
	op.nextQueue = op.nextQueue[:0]

	for i := range op.queue {
		if err := op.ctx.Err(); err != nil {
			return err
		}
		ps := op.queue[i]
		// If this path is within the emit window, record it.
		if ps.hops >= op.minHops && ps.hops <= op.maxHops {
			// pathState shares its backing path slice with the queue entry; the
			// slice is read-only after construction in enqueueEdges, so the copy
			// is safe.
			op.results = append(op.results, ps)
		}
		// Expand further if we can go deeper.
		if ps.hops < op.maxHops {
			if err := op.expandPath(ps); err != nil {
				return err
			}
		}
	}

	// Swap: nextQueue (writes from this level) becomes the new frontier; the old
	// frontier slice is reused as the write target for the next level.
	op.queue, op.nextQueue = op.nextQueue, op.queue[:0]
	return nil
}

// expandPath enqueues all one-hop extensions of ps into op.nextQueue (the
// write-target for the level following the current one).
func (op *VarLengthExpand) expandPath(ps pathState) error {
	if op.dir != DirIn {
		if err := op.enqueueEdges(ps.srcNode, true, &ps, &op.nextQueue); err != nil {
			return err
		}
	}
	if op.dir != DirOut && op.revVerts != nil {
		if err := op.enqueueEdges(ps.srcNode, false, &ps, &op.nextQueue); err != nil {
			return err
		}
	}
	return nil
}

// enqueueEdges appends all qualifying edges from node uid into *target.
// isFwd selects forward vs reverse edges. parent is nil only for the seed call.
// The caller chooses the write target so that the BFS read frontier (op.queue)
// is never aliased with the write target during a runBFS pass.
func (op *VarLengthExpand) enqueueEdges(uid uint64, isFwd bool, parent *pathState, target *[]pathState) error {
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

		// Undirected self-loop deduplication: on a DirBoth traversal a
		// reverse self-loop at the current node has already been enqueued
		// by the forward pass under a different absolute position
		// (synthetic reverse positions use base = len(fwdEdges)). The
		// relationship-uniqueness bitset cannot detect this aliasing
		// because it keys on the absolute position. Skip reverse self-
		// loops to keep each matched edge from spawning two BFS branches.
		if !isFwd && op.dir == DirBoth && dst == uid {
			continue
		}

		// On undirected (DirBoth) traversal, alias the reverse-edge
		// synthetic position to its forward counterpart so the
		// relationship-uniqueness bitset rejects a path that uses the
		// SAME physical edge twice (once forward, once reverse) — see
		// Match9 [3]/[4]. Without this aliasing the BFS would emit
		// `(a)-[REL1]->(b)-[REL1 reverse]->(a)` as a length-2 path that
		// happens to "reuse" the only edge between a and b.
		fwdAbsPos := absPos
		if !isFwd && op.dir == DirBoth && op.revToFwd != nil && pos < uint64(len(op.revToFwd)) {
			if mapped := op.revToFwd[pos]; mapped != ^uint64(0) {
				fwdAbsPos = mapped
			}
		}

		// Edge-type filter (forward only; reverse edges skip type filter).
		if isFwd && op.edgeType != "" {
			t, ok := op.edgeTypeFilter[absPos]
			if !ok || t != op.edgeType {
				continue
			}
		}
		// Edge-type filter for reverse edges: look up by the forward
		// counterpart position. Without this, reverse traversal would
		// emit any-type edges even when the pattern declared a type
		// filter, leading Match9 [3]/[4] to enumerate paths through
		// edges that should have been filtered out.
		if !isFwd && op.edgeType != "" && op.dir == DirBoth && fwdAbsPos != absPos {
			t, ok := op.edgeTypeFilter[fwdAbsPos]
			if !ok || t != op.edgeType {
				continue
			}
		}

		// Relationship-uniqueness: skip if this edge is already on the path.
		// Key the bitset on the FORWARD position so a reverse-direction
		// traversal of the same edge is recognised as the same edge.
		if parent != nil && bitsetContains(parent.visited, fwdAbsPos) {
			continue
		}

		// Build new path state. The path step stores the absPos for
		// rendering (so forward/reverse direction is preserved) but the
		// visited bitset uses fwdAbsPos so a later traversal of the
		// same edge in the opposite direction is rejected.
		var newPath []edgeStep
		var newVisited []uint64
		hops := 1
		if parent != nil {
			hops = parent.hops + 1
			newPath = make([]edgeStep, len(parent.path)+1)
			copy(newPath, parent.path)
			newPath[len(parent.path)] = edgeStep{edgePos: absPos, dstID: dst}
			newVisited = bitsetAdd(parent.visited, fwdAbsPos)
		} else {
			newPath = []edgeStep{{edgePos: absPos, dstID: dst}}
			newVisited = bitsetAdd(nil, fwdAbsPos)
		}

		*target = append(*target, pathState{
			hops:    hops,
			srcNode: dst,
			path:    newPath,
			visited: newVisited,
		})
	}
	return nil
}

// buildRow writes (inputRow... || pathList || dstID) into out.
//
// pathList is a flat alternating ListValue encoding the full path:
//
//	[srcNodeID, edgePos0, dstNode0, edgePos1, dstNode1, ..., dstNodeN]
//
// For an N-hop path the list has 1 + 2*N elements (srcNode, then N pairs of
// (edgePos, dstNode)). For a zero-hop path the list is [srcNodeID] (1 element).
//
// dstID is the terminal node ID (same as the last dstNode in pathList, or
// srcNodeID for zero-hop).
func (op *VarLengthExpand) buildRow(out *Row, inputRow Row, ps pathState) {
	var srcID expr.Value
	if op.inputCol < len(inputRow) {
		srcID = inputRow[op.inputCol]
	} else {
		srcID = expr.Null
	}

	// Build flat alternating list: [srcID, edgePos0, dst0, edgePos1, dst1, ...].
	pathList := make(expr.ListValue, 1+2*len(ps.path))
	pathList[0] = srcID
	for i, step := range ps.path {
		pathList[1+2*i] = expr.IntegerValue(int64(step.edgePos))
		pathList[2+2*i] = expr.IntegerValue(int64(step.dstID))
	}

	var dstID expr.Value
	if len(ps.path) == 0 {
		dstID = srcID
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
	op.nextQueue = nil
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
