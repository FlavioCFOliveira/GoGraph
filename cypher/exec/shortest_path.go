package exec

// shortest_path.go — ShortestPath and AllShortestPaths operators
// (task-262, doc refresh in task-393).
//
// ShortestPath uses BFS to find a single shortest path between a source and
// destination node. It emits one row containing a [expr.PathValue] (or
// [expr.Null] if no path exists), appended to the input row.
//
// AllShortestPaths uses level-synchronous BFS with a multi-predecessor map to
// find all paths of minimum length. It emits one row per shortest path found
// (or zero rows if no path exists).
//
// Both operators enforce relationship-uniqueness within each path (no edge
// repeated). BFS naturally avoids revisiting nodes, ensuring each node appears
// at most once per path in the shortest-path case.
//
// # Algorithm choice (why single-direction BFS, not bidirectional or Dijkstra)
//
// Cypher's shortestPath() / allShortestPaths() operates on the unweighted edge
// count (semantically: fewest hops), so the optimal algorithm is BFS. The
// alternatives considered were:
//
//   - Bidirectional BFS — halves the expected exploration cost when both
//     endpoints are fixed and the graph is reasonably regular, but doubles
//     implementation and test surface and confers no asymptotic improvement.
//     Forward-only BFS is kept until a measured benchmark shows the doubling
//     pays off in this codebase.
//   - Dijkstra / A* — only beneficial for weighted shortest paths or with an
//     admissible heuristic; neither applies to unweighted Cypher semantics.
//
// For the all-shortest-paths variant, the standard solution is level-
// synchronous BFS with a multi-predecessor map: at each BFS level we record
// every predecessor edge that reaches a node at the current distance; once dst
// is first found, the predecessor DAG is traversed in reverse to enumerate
// every shortest path. This is the textbook approach (see e.g. CLRS chap. 22
// and graph-DB documentation on allShortestPaths).
//
// # Input schema
//
// Each input row must carry:
//   - srcCol: the source NodeID (expr.IntegerValue).
//   - dstCol: the destination NodeID (expr.IntegerValue).
//
// # Output schema
//
// ShortestPath:    inputRow... || PathValue (or Null when unreachable).
// AllShortestPaths: one row per shortest path: inputRow... || PathValue.
//
// # NodeValue / RelationshipValue construction
//
// The emitted PathValue contains NodeValue{ID: nodeID} and
// RelationshipValue{ID: edgePos, StartID: src, EndID: dst} with empty labels
// and properties. Callers that need full properties must hydrate the path.
//
// # Concurrency
//
// ShortestPath and AllShortestPaths are NOT safe for concurrent use.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and inside BFS loops.

import (
	"context"

	"gograph/cypher/expr"
	"gograph/graph"
)

// spPredEntry records one predecessor edge during BFS.
type spPredEntry struct {
	parent  uint64
	edgePos uint64
}

// aspPredEntry records one predecessor edge during all-shortest-paths BFS.
type aspPredEntry = spPredEntry // same shape; alias for clarity at use-sites

// ─────────────────────────────────────────────────────────────────────────────
// ShortestPath
// ─────────────────────────────────────────────────────────────────────────────

// ShortestPath is a Volcano pipeline operator that, for each input row, finds
// a single shortest path from srcCol to dstCol using BFS and emits one output
// row containing the path (or Null if unreachable).
//
// ShortestPath is NOT safe for concurrent use.
type ShortestPath struct {
	input  Operator
	fwd    csrAdjacency
	rev    csrAdjacency
	dir    Direction
	srcCol int
	dstCol int

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// CSR snapshots.
	fwdVerts []uint64
	fwdEdges []graph.NodeID
	revVerts []uint64
	revEdges []graph.NodeID

	outBuf []expr.Value
}

// NewShortestPath creates a ShortestPath operator.
//   - input is the upstream operator supplying (srcID, dstID) pairs.
//   - fwd is the forward CSR adjacency.
//   - rev is the reverse CSR (required for DirIn/DirBoth).
//   - dir is the traversal direction.
//   - srcCol / dstCol are the column indices in each input row for source and
//     destination node IDs.
func NewShortestPath(input Operator, fwd, rev csrAdjacency, dir Direction, srcCol, dstCol int) *ShortestPath {
	if dir == 0 {
		dir = DirOut
	}
	return &ShortestPath{
		input:  input,
		fwd:    fwd,
		rev:    rev,
		dir:    dir,
		srcCol: srcCol,
		dstCol: dstCol,
	}
}

// Init initialises the operator.
func (op *ShortestPath) Init(ctx context.Context) error {
	op.ctx = ctx
	op.fwdVerts = op.fwd.VerticesSlice()
	op.fwdEdges = op.fwd.EdgesSlice()
	if op.dir != DirOut && op.rev != nil {
		op.revVerts = op.rev.VerticesSlice()
		op.revEdges = op.rev.EdgesSlice()
	}
	return op.input.Init(ctx)
}

// Next emits one row per input row, containing the shortest path (or Null).
func (op *ShortestPath) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	var inputRow Row
	ok, err := op.input.Next(&inputRow)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	srcID, dstID, valid := op.extractEndpoints(inputRow)
	if !valid {
		op.emitRow(out, inputRow, expr.Null)
		return true, nil
	}

	path, found, err := op.bfsShortestPath(srcID, dstID)
	if err != nil {
		return false, err
	}
	if !found {
		op.emitRow(out, inputRow, expr.Null)
		return true, nil
	}
	op.emitRow(out, inputRow, path)
	return true, nil
}

// bfsShortestPath runs BFS from src to dst and returns the first path found.
// Returns (Null, false, nil) when unreachable.
func (op *ShortestPath) bfsShortestPath(src, dst uint64) (expr.Value, bool, error) {
	if src == dst {
		return expr.PathValue{Nodes: []expr.NodeValue{{ID: src}}}, true, nil
	}

	// pred[nodeID] = the single predecessor entry that first discovered nodeID.
	// The sentinel for src has parent == src.
	pred := make(map[uint64]spPredEntry)
	pred[src] = spPredEntry{parent: src, edgePos: 0}

	queue := []uint64{src}
	found := false

	for len(queue) > 0 && !found {
		if err := op.ctx.Err(); err != nil {
			return nil, false, err
		}
		var next []uint64
		for _, node := range queue {
			if op.dir != DirIn {
				ns, f := op.spExpandFwd(node, pred, dst)
				if f {
					found = true
				}
				next = append(next, ns...)
			}
			if op.dir != DirOut && op.revVerts != nil {
				ns, f := op.spExpandRev(node, pred, dst)
				if f {
					found = true
				}
				next = append(next, ns...)
			}
		}
		queue = next
	}

	if !found {
		return expr.Null, false, nil
	}
	return op.spReconstructPath(pred, src, dst), true, nil
}

// spExpandFwd explores forward edges of node, recording new discoveries in pred.
func (op *ShortestPath) spExpandFwd(node uint64, pred map[uint64]spPredEntry, dst uint64) (newNodes []uint64, found bool) {
	if node+1 >= uint64(len(op.fwdVerts)) {
		return nil, false
	}
	for pos := op.fwdVerts[node]; pos < op.fwdVerts[node+1]; pos++ {
		neighbour := uint64(op.fwdEdges[pos])
		if _, visited := pred[neighbour]; visited {
			continue
		}
		pred[neighbour] = spPredEntry{parent: node, edgePos: pos}
		newNodes = append(newNodes, neighbour)
		if neighbour == dst {
			found = true
		}
	}
	return newNodes, found
}

// spExpandRev explores reverse edges of node, recording new discoveries in pred.
func (op *ShortestPath) spExpandRev(node uint64, pred map[uint64]spPredEntry, dst uint64) (newNodes []uint64, found bool) {
	if node+1 >= uint64(len(op.revVerts)) {
		return nil, false
	}
	base := uint64(len(op.fwdEdges))
	for pos := op.revVerts[node]; pos < op.revVerts[node+1]; pos++ {
		neighbour := uint64(op.revEdges[pos])
		if _, visited := pred[neighbour]; visited {
			continue
		}
		pred[neighbour] = spPredEntry{parent: node, edgePos: base + pos}
		newNodes = append(newNodes, neighbour)
		if neighbour == dst {
			found = true
		}
	}
	return newNodes, found
}

// spReconstructPath builds a PathValue from the predecessor map walking dst→src.
func (op *ShortestPath) spReconstructPath(pred map[uint64]spPredEntry, src, dst uint64) expr.PathValue {
	type hop struct {
		nodeID  uint64
		edgePos uint64
	}
	var hops []hop
	cur := dst
	for cur != src {
		entry := pred[cur]
		hops = append(hops, hop{nodeID: cur, edgePos: entry.edgePos})
		cur = entry.parent
	}
	// Reverse: hops is [dst→…→src] but we need [src→…→dst].
	for i, j := 0, len(hops)-1; i < j; i, j = i+1, j-1 {
		hops[i], hops[j] = hops[j], hops[i]
	}

	nodes := make([]expr.NodeValue, len(hops)+1)
	rels := make([]expr.RelationshipValue, len(hops))
	nodes[0] = expr.NodeValue{ID: src}
	for i, h := range hops {
		nodes[i+1] = expr.NodeValue{ID: h.nodeID}
		var startID uint64
		if i == 0 {
			startID = src
		} else {
			startID = hops[i-1].nodeID
		}
		rels[i] = expr.RelationshipValue{
			ID:      h.edgePos,
			StartID: startID,
			EndID:   h.nodeID,
		}
	}
	return expr.PathValue{Nodes: nodes, Relationships: rels}
}

func (op *ShortestPath) extractEndpoints(row Row) (src, dst uint64, valid bool) {
	if op.srcCol >= len(row) || op.dstCol >= len(row) {
		return 0, 0, false
	}
	sv, ok1 := row[op.srcCol].(expr.IntegerValue)
	dv, ok2 := row[op.dstCol].(expr.IntegerValue)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return uint64(sv), uint64(dv), true
}

func (op *ShortestPath) emitRow(out *Row, inputRow Row, pathVal expr.Value) {
	need := len(inputRow) + 1
	if cap(op.outBuf) < need {
		op.outBuf = make([]expr.Value, need)
	}
	op.outBuf = op.outBuf[:need]
	copy(op.outBuf, inputRow)
	op.outBuf[len(inputRow)] = pathVal
	*out = op.outBuf
}

// Close closes the input operator.
func (op *ShortestPath) Close() error {
	op.outBuf = nil
	return op.input.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// AllShortestPaths
// ─────────────────────────────────────────────────────────────────────────────

// AllShortestPaths is a Volcano pipeline operator that, for each input row,
// finds all paths of minimum length from srcCol to dstCol and emits one output
// row per path.
//
// AllShortestPaths is NOT safe for concurrent use.
type AllShortestPaths struct {
	input  Operator
	fwd    csrAdjacency
	rev    csrAdjacency
	dir    Direction
	srcCol int
	dstCol int

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// CSR snapshots.
	fwdVerts []uint64
	fwdEdges []graph.NodeID
	revVerts []uint64
	revEdges []graph.NodeID

	// per-input-row state
	inputRow   Row
	inputEOS   bool
	pending    []expr.PathValue // collected paths from last BFS
	pendingIdx int

	outBuf []expr.Value
}

// NewAllShortestPaths creates an AllShortestPaths operator.
func NewAllShortestPaths(input Operator, fwd, rev csrAdjacency, dir Direction, srcCol, dstCol int) *AllShortestPaths {
	if dir == 0 {
		dir = DirOut
	}
	return &AllShortestPaths{
		input:  input,
		fwd:    fwd,
		rev:    rev,
		dir:    dir,
		srcCol: srcCol,
		dstCol: dstCol,
	}
}

// Init initialises the operator.
func (op *AllShortestPaths) Init(ctx context.Context) error {
	op.ctx = ctx
	op.fwdVerts = op.fwd.VerticesSlice()
	op.fwdEdges = op.fwd.EdgesSlice()
	if op.dir != DirOut && op.rev != nil {
		op.revVerts = op.rev.VerticesSlice()
		op.revEdges = op.rev.EdgesSlice()
	}
	op.pending = op.pending[:0]
	op.pendingIdx = 0
	op.inputRow = nil
	op.inputEOS = false
	return op.input.Init(ctx)
}

// Next emits one row per shortest path per input row.
func (op *AllShortestPaths) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// Emit pending paths for the current input row.
		if op.pendingIdx < len(op.pending) {
			path := op.pending[op.pendingIdx]
			op.pendingIdx++
			op.emitRow(out, op.inputRow, path)
			return true, nil
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

		cp := make(Row, len(inputRow))
		copy(cp, inputRow)
		op.inputRow = cp

		srcID, dstID, valid := op.extractEndpoints(cp)
		if !valid {
			continue
		}

		paths, err := op.bfsAllShortest(srcID, dstID)
		if err != nil {
			return false, err
		}
		op.pending = paths
		op.pendingIdx = 0
	}
}

// bfsAllShortest finds all shortest paths from src to dst using
// level-synchronous BFS with a multi-predecessor map.
func (op *AllShortestPaths) bfsAllShortest(src, dst uint64) ([]expr.PathValue, error) {
	if src == dst {
		return []expr.PathValue{{Nodes: []expr.NodeValue{{ID: src}}}}, nil
	}

	// preds[nodeID] = all (parent, edgePos) pairs that reach nodeID at the
	// shortest BFS distance. Multiple entries are allowed for the same node
	// (from different parents at the same level).
	preds := make(map[uint64][]aspPredEntry)
	// dist[nodeID] = BFS level at which nodeID was first reached.
	dist := make(map[uint64]int)
	dist[src] = 0

	queue := []uint64{src}
	found := false
	foundLevel := -1

	for level := 1; len(queue) > 0; level++ {
		if err := op.ctx.Err(); err != nil {
			return nil, err
		}
		// Stop expanding beyond the level where dst was first found.
		if found && level > foundLevel {
			break
		}

		var next []uint64
		for _, node := range queue {
			if op.dir != DirIn {
				ns, f := op.aspExpandFwd(node, dist, preds, level, dst)
				if f {
					found = true
					foundLevel = level
				}
				next = append(next, ns...)
			}
			if op.dir != DirOut && op.revVerts != nil {
				ns, f := op.aspExpandRev(node, dist, preds, level, dst)
				if f {
					found = true
					foundLevel = level
				}
				next = append(next, ns...)
			}
		}
		queue = next
	}

	if !found {
		return nil, nil
	}

	return op.aspReconstructAll(preds, src, dst), nil
}

// aspExpandFwd expands forward edges of node at the given BFS level.
func (op *AllShortestPaths) aspExpandFwd(node uint64, dist map[uint64]int, preds map[uint64][]aspPredEntry, level int, dst uint64) (newNodes []uint64, found bool) {
	if node+1 >= uint64(len(op.fwdVerts)) {
		return nil, false
	}
	for pos := op.fwdVerts[node]; pos < op.fwdVerts[node+1]; pos++ {
		neighbour := uint64(op.fwdEdges[pos])
		existingDist, visited := dist[neighbour]
		if visited && existingDist < level {
			continue // reached at a shorter distance; not on any shortest path through here
		}
		if !visited {
			dist[neighbour] = level
			newNodes = append(newNodes, neighbour)
		}
		preds[neighbour] = append(preds[neighbour], aspPredEntry{parent: node, edgePos: pos})
		if neighbour == dst {
			found = true
		}
	}
	return newNodes, found
}

// aspExpandRev expands reverse edges of node at the given BFS level.
func (op *AllShortestPaths) aspExpandRev(node uint64, dist map[uint64]int, preds map[uint64][]aspPredEntry, level int, dst uint64) (newNodes []uint64, found bool) {
	if node+1 >= uint64(len(op.revVerts)) {
		return nil, false
	}
	base := uint64(len(op.fwdEdges))
	for pos := op.revVerts[node]; pos < op.revVerts[node+1]; pos++ {
		neighbour := uint64(op.revEdges[pos])
		existingDist, visited := dist[neighbour]
		if visited && existingDist < level {
			continue
		}
		if !visited {
			dist[neighbour] = level
			newNodes = append(newNodes, neighbour)
		}
		preds[neighbour] = append(preds[neighbour], aspPredEntry{parent: node, edgePos: base + pos})
		if neighbour == dst {
			found = true
		}
	}
	return newNodes, found
}

// aspReconstructAll reconstructs all shortest paths via DFS over the
// predecessor map, walking backwards from dst to src.
func (op *AllShortestPaths) aspReconstructAll(preds map[uint64][]aspPredEntry, src, dst uint64) []expr.PathValue {
	type hopEntry struct {
		nodeID  uint64
		edgePos uint64
	}
	type frame struct {
		node    uint64
		partial []hopEntry // path hops collected so far, in reverse order (dst→src)
	}

	var paths []expr.PathValue
	stack := []frame{{node: dst}}

	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if top.node == src {
			// Reverse partial to get src→dst order.
			hops := top.partial
			nodes := make([]expr.NodeValue, len(hops)+1)
			rels := make([]expr.RelationshipValue, len(hops))
			nodes[0] = expr.NodeValue{ID: src}
			for i := range hops {
				fwdIdx := len(hops) - 1 - i // reverse index
				h := hops[fwdIdx]
				nodes[i+1] = expr.NodeValue{ID: h.nodeID}
				var startID uint64
				if i == 0 {
					startID = src
				} else {
					startID = hops[len(hops)-i].nodeID
				}
				rels[i] = expr.RelationshipValue{
					ID:      h.edgePos,
					StartID: startID,
					EndID:   h.nodeID,
				}
			}
			paths = append(paths, expr.PathValue{Nodes: nodes, Relationships: rels})
			continue
		}

		for _, entry := range preds[top.node] {
			newPartial := make([]hopEntry, len(top.partial)+1)
			copy(newPartial, top.partial)
			newPartial[len(top.partial)] = hopEntry{nodeID: top.node, edgePos: entry.edgePos}
			stack = append(stack, frame{
				node:    entry.parent,
				partial: newPartial,
			})
		}
	}
	return paths
}

func (op *AllShortestPaths) extractEndpoints(row Row) (src, dst uint64, valid bool) {
	if op.srcCol >= len(row) || op.dstCol >= len(row) {
		return 0, 0, false
	}
	sv, ok1 := row[op.srcCol].(expr.IntegerValue)
	dv, ok2 := row[op.dstCol].(expr.IntegerValue)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return uint64(sv), uint64(dv), true
}

func (op *AllShortestPaths) emitRow(out *Row, inputRow Row, path expr.PathValue) {
	need := len(inputRow) + 1
	if cap(op.outBuf) < need {
		op.outBuf = make([]expr.Value, need)
	}
	op.outBuf = op.outBuf[:need]
	copy(op.outBuf, inputRow)
	op.outBuf[len(inputRow)] = path
	*out = op.outBuf
}

// Close closes the input operator.
func (op *AllShortestPaths) Close() error {
	op.pending = nil
	op.outBuf = nil
	return op.input.Close()
}
