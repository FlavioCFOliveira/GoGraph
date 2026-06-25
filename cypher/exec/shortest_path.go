package exec

// shortest_path.go — ShortestPath and AllShortestPaths operators
// (task-262, doc refresh in task-393, flat-list emission in rmp #1692).
//
// ShortestPath uses BFS to find a single shortest path between a source and
// destination node. It emits one row containing the path as a flat alternating
// [expr.ListValue] (or [expr.Null] if no path exists), appended to the input
// row.
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
//   - srcCol: the source NodeID (expr.IntegerValue or expr.NodeValue).
//   - dstCol: the destination NodeID (expr.IntegerValue or expr.NodeValue).
//
// # Output schema and per-instance relationship typing (rmp #1692)
//
// Instead of emitting an empty-typed [expr.PathValue], both operators emit the
// SAME flat alternating list encoding that [VarLengthExpand] emits, at stride
// [VLEHopStride]:
//
//	[srcNodeID, fwdPos0, dstNode0, dir0, fwdPos1, dstNode1, dir1, …]
//
// where fwdPosH is the FORWARD-CSR position of hop H's physical edge
// (handle-disambiguated via [buildRevToFwd], so a reverse hop over a multigraph
// parallel edge records the SAME forward position as the matching forward hop)
// and dirH is [VLEDirForward] or [VLEDirReverse]. The cypher-package relationship
// hydrator ([resolveHopRel], reached through the path variable's pathVarMeta
// registration) then reports each hop's OWN per-instance type and properties.
// For a zero-length path (src == dst) the list is [srcNodeID] (1 element).
//
// ShortestPath:     inputRow... || pathList (or Null when unreachable).
// AllShortestPaths: one row per shortest path: inputRow... || pathList.
//
// # Relationship-type filter
//
// Both operators accept an EdgeType gate plus an EdgeTypeFilter presence-set of
// forward edge positions (the same shape [VarLengthExpand] and [Expand] use).
// A forward expansion is gated by its absolute (forward) position; a reverse
// expansion is gated by the resolved forward position. The test is MEMBERSHIP,
// not equality, so a disjunction `[:T1|T2*]` accepts an edge of either type.
//
// # Hop bounds
//
// When the wrapped pattern carries a minimum/maximum hop bound, MinHops bounds
// the shortest acceptable length (a zero-length src==dst path is reported only
// when MinHops == 0) and MaxHops caps the search depth. openCypher restricts the
// lower bound of a shortestPath pattern to 0 or 1; the operator nonetheless
// honours any MinHops/MaxHops it is given.
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

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// shortestNoMaxHops is the sentinel MaxHops value meaning "no upper bound".
const shortestNoMaxHops = 0

// spPredEntry records one predecessor edge during BFS. rawPos is the position
// within the expansion's own CSR (forward CSR for a forward expansion, reverse
// CSR for a reverse expansion); fwd marks which CSR it indexes. The forward
// position used for per-instance hydration and the type filter is resolved from
// (rawPos, fwd) at reconstruction time via [resolveFwdPos].
type spPredEntry struct {
	parent uint64
	rawPos uint64
	fwd    bool
}

// aspPredEntry records one predecessor edge during all-shortest-paths BFS.
type aspPredEntry = spPredEntry // same shape; alias for clarity at use-sites

// hop is a reconstructed path hop: the destination node plus the
// handle-disambiguated forward edge position and the traversal direction.
type hop struct {
	dstID    uint64
	fwdPos   uint64
	reversed bool
}

// ─────────────────────────────────────────────────────────────────────────────
// ShortestPath
// ─────────────────────────────────────────────────────────────────────────────

// ShortestPath is a Volcano pipeline operator that, for each input row, finds
// a single shortest path from srcCol to dstCol using BFS and emits one output
// row containing the flat alternating path list (or Null if unreachable).
//
// ShortestPath is NOT safe for concurrent use.
type ShortestPath struct {
	input  Operator
	fwd    csrAdjacency
	rev    csrAdjacency
	dir    Direction
	srcCol int
	dstCol int

	// edgeType is the "a type filter was requested" gate; edgeTypeFilter is the
	// presence-set of forward edge positions whose edge carries an accepted
	// type (membership, not equality). Both nil/"" means no type filter.
	edgeType       string
	edgeTypeFilter map[uint64]string

	// minHops / maxHops bound the accepted path length. maxHops ==
	// [shortestNoMaxHops] means unbounded.
	minHops int
	maxHops int

	// optional controls the no-path behaviour: when true (OPTIONAL MATCH) a
	// pair with no path emits one row with a Null path; when false (MATCH) the
	// row is dropped (no output for that input row). See the openCypher
	// MATCH/OPTIONAL MATCH contract.
	optional bool

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// CSR snapshots.
	fwdVerts   []uint64
	fwdEdges   []graph.NodeID
	fwdHandles []uint64
	revVerts   []uint64
	revEdges   []graph.NodeID
	revHandles []uint64
	revToFwd   []uint64

	outBuf []expr.Value
}

// NewShortestPath creates a ShortestPath operator.
//   - input is the upstream operator supplying (srcID, dstID) pairs.
//   - fwd is the forward CSR adjacency.
//   - rev is the reverse CSR (required for DirIn/DirBoth).
//   - dir is the traversal direction.
//   - srcCol / dstCol are the column indices in each input row for source and
//     destination node IDs.
//
// The returned operator has no type filter and no hop bounds; use
// [ShortestPath.WithTypeFilter] and [ShortestPath.WithHopBounds] to configure
// them.
func NewShortestPath(input Operator, fwd, rev csrAdjacency, dir Direction, srcCol, dstCol int) *ShortestPath {
	if dir == 0 {
		dir = DirOut
	}
	return &ShortestPath{
		input:   input,
		fwd:     fwd,
		rev:     rev,
		dir:     dir,
		srcCol:  srcCol,
		dstCol:  dstCol,
		minHops: 1,
		maxHops: shortestNoMaxHops,
	}
}

// WithTypeFilter restricts traversal to edges whose forward position is present
// in filter. edgeType is the non-empty "a filter was requested" gate (typically
// the pattern's first declared relationship type). It returns op for chaining.
func (op *ShortestPath) WithTypeFilter(edgeType string, filter map[uint64]string) *ShortestPath {
	op.edgeType = edgeType
	op.edgeTypeFilter = filter
	return op
}

// WithHopBounds sets the accepted path-length window. minHops is the minimum
// length (a zero-length src==dst path is reported only when minHops == 0);
// maxHops caps the BFS depth ([shortestNoMaxHops] == 0 means unbounded). It
// returns op for chaining.
func (op *ShortestPath) WithHopBounds(minHops, maxHops int) *ShortestPath {
	op.minHops = minHops
	op.maxHops = maxHops
	return op
}

// WithOptional selects the OPTIONAL MATCH no-path behaviour (emit a Null-path
// row) when optional is true; the default (false) drops the row when no path
// exists, the MATCH behaviour. It returns op for chaining.
func (op *ShortestPath) WithOptional(optional bool) *ShortestPath {
	op.optional = optional
	return op
}

// Init initialises the operator.
func (op *ShortestPath) Init(ctx context.Context) error {
	op.ctx = ctx
	op.fwdVerts = op.fwd.VerticesSlice()
	op.fwdEdges = op.fwd.EdgesSlice()
	op.fwdHandles = op.fwd.HandlesSlice()
	if op.dir != DirOut && op.rev != nil {
		op.revVerts = op.rev.VerticesSlice()
		op.revEdges = op.rev.EdgesSlice()
		op.revHandles = op.rev.HandlesSlice()
		op.revToFwd = buildRevToFwd(
			op.fwdVerts, op.fwdEdges, op.fwdHandles,
			op.revVerts, op.revEdges, op.revHandles,
		)
	}
	return op.input.Init(ctx)
}

// Next emits one row per input row, containing the shortest path. When no path
// exists the row is emitted with a Null path (OPTIONAL MATCH) or dropped (MATCH)
// depending on the operator's optional flag.
func (op *ShortestPath) Next(out *Row) (bool, error) {
	for {
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

		srcID, dstID, valid := extractEndpoints(inputRow, op.srcCol, op.dstCol)
		if !valid {
			if op.optional {
				op.emitRow(out, inputRow, expr.Null)
				return true, nil
			}
			continue
		}

		path, found, err := op.bfsShortestPath(srcID, dstID)
		if err != nil {
			return false, err
		}
		if !found {
			if op.optional {
				op.emitRow(out, inputRow, expr.Null)
				return true, nil
			}
			continue
		}
		op.emitRow(out, inputRow, path)
		return true, nil
	}
}

// bfsShortestPath runs BFS from src to dst and returns the first path found as
// a flat alternating list. Returns (Null, false, nil) when unreachable.
func (op *ShortestPath) bfsShortestPath(src, dst uint64) (expr.Value, bool, error) {
	if src == dst {
		if op.minHops == 0 {
			return expr.ListValue{expr.IntegerValue(int64(src))}, true, nil
		}
		// minHops >= 1 forbids the zero-length path; fall through to search a
		// genuine cycle back to src.
	}

	// pred[nodeID] = the single predecessor entry that first discovered nodeID.
	// The sentinel for src has parent == src.
	pred := make(map[uint64]spPredEntry)
	pred[src] = spPredEntry{parent: src}

	queue := []uint64{src}
	found := false
	level := 0

	for len(queue) > 0 && !found {
		if err := op.ctx.Err(); err != nil {
			return nil, false, err
		}
		level++
		if op.maxHops != shortestNoMaxHops && level > op.maxHops {
			break
		}
		var next []uint64
		for _, node := range queue {
			if op.dir != DirIn {
				ns, f := op.spExpand(node, pred, dst, true)
				if f {
					found = true
				}
				next = append(next, ns...)
			}
			if op.dir != DirOut && op.revVerts != nil {
				ns, f := op.spExpand(node, pred, dst, false)
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
	if level < op.minHops {
		return expr.Null, false, nil
	}
	return op.reconstructList(pred, src, dst), true, nil
}

// spExpand explores edges of node, recording new discoveries in pred. isFwd
// selects forward vs reverse adjacency. found reports whether dst was reached.
func (op *ShortestPath) spExpand(node uint64, pred map[uint64]spPredEntry, dst uint64, isFwd bool) (newNodes []uint64, found bool) {
	verts, edges := op.revVerts, op.revEdges
	if isFwd {
		verts, edges = op.fwdVerts, op.fwdEdges
	}
	if node+1 >= uint64(len(verts)) {
		return nil, false
	}
	for pos := verts[node]; pos < verts[node+1]; pos++ {
		if !op.passesTypeFilter(pos, isFwd) {
			continue
		}
		neighbour := uint64(edges[pos])
		if _, visited := pred[neighbour]; visited {
			continue
		}
		pred[neighbour] = spPredEntry{parent: node, rawPos: pos, fwd: isFwd}
		newNodes = append(newNodes, neighbour)
		if neighbour == dst {
			found = true
		}
	}
	return newNodes, found
}

// reconstructList walks the predecessor map dst→src and builds the flat
// alternating path list [srcID, fwdPos0, dst0, dir0, …] (stride [VLEHopStride]).
func (op *ShortestPath) reconstructList(pred map[uint64]spPredEntry, src, dst uint64) expr.ListValue {
	var hops []hop
	cur := dst
	for cur != src {
		entry := pred[cur]
		fwdPos, reversed := op.resolveFwdPos(entry)
		hops = append(hops, hop{dstID: cur, fwdPos: fwdPos, reversed: reversed})
		cur = entry.parent
	}
	// hops is [dst→…→src]; reverse to [src→…→dst].
	for i, j := 0, len(hops)-1; i < j; i, j = i+1, j-1 {
		hops[i], hops[j] = hops[j], hops[i]
	}
	return buildHopList(src, hops)
}

func (op *ShortestPath) emitRow(out *Row, inputRow Row, pathVal expr.Value) {
	*out = appendValue(&op.outBuf, inputRow, pathVal)
}

// passesTypeFilter reports whether the edge at CSR position pos (in the forward
// CSR when isFwd, otherwise the reverse CSR) is accepted by the relationship-
// type filter. When no filter is configured all edges pass.
func (op *ShortestPath) passesTypeFilter(pos uint64, isFwd bool) bool {
	if op.edgeType == "" {
		return true
	}
	fwdPos := pos
	if !isFwd {
		fwdPos = op.resolvedFwdPosOrSelf(pos)
		// When the reverse slot has no resolvable forward counterpart we cannot
		// type-check it; keep it permissive (mirrors VLE's fwdAbsPos==absPos
		// fallback for the rare out-of-range case).
		if fwdPos == pos {
			return true
		}
	}
	_, ok := op.edgeTypeFilter[fwdPos]
	return ok
}

// resolveFwdPos returns the handle-disambiguated forward edge position and the
// reversed flag for a predecessor entry, used both for the type filter and for
// per-instance hydration.
func (op *ShortestPath) resolveFwdPos(e spPredEntry) (fwdPos uint64, reversed bool) {
	if e.fwd {
		return e.rawPos, false
	}
	return op.resolvedFwdPosOrSelf(e.rawPos), true
}

// resolvedFwdPosOrSelf maps a reverse-CSR position to its forward counterpart,
// falling back to the reverse position itself when unresolved.
func (op *ShortestPath) resolvedFwdPosOrSelf(revPos uint64) uint64 {
	if op.revToFwd != nil && revPos < uint64(len(op.revToFwd)) {
		if mapped := op.revToFwd[revPos]; mapped != unresolvedFwdPos {
			return mapped
		}
	}
	return revPos
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
// row per path, each carrying the flat alternating path list (stride
// [VLEHopStride]).
//
// AllShortestPaths is NOT safe for concurrent use.
type AllShortestPaths struct {
	input  Operator
	fwd    csrAdjacency
	rev    csrAdjacency
	dir    Direction
	srcCol int
	dstCol int

	edgeType       string
	edgeTypeFilter map[uint64]string
	minHops        int
	maxHops        int
	optional       bool

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// CSR snapshots.
	fwdVerts   []uint64
	fwdEdges   []graph.NodeID
	fwdHandles []uint64
	revVerts   []uint64
	revEdges   []graph.NodeID
	revHandles []uint64
	revToFwd   []uint64

	// per-input-row state
	inputRow    Row
	inputEOS    bool
	pending     []expr.ListValue // collected paths from last BFS
	pendingIdx  int
	pendingNull bool // emit one Null-path row (OPTIONAL MATCH, no path found)

	outBuf []expr.Value
}

// NewAllShortestPaths creates an AllShortestPaths operator. Like
// [NewShortestPath] it starts with no type filter and minHops == 1; configure
// via [AllShortestPaths.WithTypeFilter] and [AllShortestPaths.WithHopBounds].
func NewAllShortestPaths(input Operator, fwd, rev csrAdjacency, dir Direction, srcCol, dstCol int) *AllShortestPaths {
	if dir == 0 {
		dir = DirOut
	}
	return &AllShortestPaths{
		input:   input,
		fwd:     fwd,
		rev:     rev,
		dir:     dir,
		srcCol:  srcCol,
		dstCol:  dstCol,
		minHops: 1,
		maxHops: shortestNoMaxHops,
	}
}

// WithTypeFilter restricts traversal to edges whose forward position is present
// in filter. It returns op for chaining.
func (op *AllShortestPaths) WithTypeFilter(edgeType string, filter map[uint64]string) *AllShortestPaths {
	op.edgeType = edgeType
	op.edgeTypeFilter = filter
	return op
}

// WithHopBounds sets the accepted path-length window. It returns op for
// chaining.
func (op *AllShortestPaths) WithHopBounds(minHops, maxHops int) *AllShortestPaths {
	op.minHops = minHops
	op.maxHops = maxHops
	return op
}

// WithOptional selects the OPTIONAL MATCH no-path behaviour (emit a single
// Null-path row) when optional is true; the default (false) drops the row. It
// returns op for chaining.
func (op *AllShortestPaths) WithOptional(optional bool) *AllShortestPaths {
	op.optional = optional
	return op
}

// Init initialises the operator.
func (op *AllShortestPaths) Init(ctx context.Context) error {
	op.ctx = ctx
	op.fwdVerts = op.fwd.VerticesSlice()
	op.fwdEdges = op.fwd.EdgesSlice()
	op.fwdHandles = op.fwd.HandlesSlice()
	if op.dir != DirOut && op.rev != nil {
		op.revVerts = op.rev.VerticesSlice()
		op.revEdges = op.rev.EdgesSlice()
		op.revHandles = op.rev.HandlesSlice()
		op.revToFwd = buildRevToFwd(
			op.fwdVerts, op.fwdEdges, op.fwdHandles,
			op.revVerts, op.revEdges, op.revHandles,
		)
	}
	op.pending = op.pending[:0]
	op.pendingIdx = 0
	op.pendingNull = false
	op.inputRow = nil
	op.inputEOS = false
	return op.input.Init(ctx)
}

// Next emits one row per shortest path per input row. With no path: one
// Null-path row (OPTIONAL MATCH) or no row (MATCH).
func (op *AllShortestPaths) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// Emit pending paths for the current input row.
		if op.pendingIdx < len(op.pending) {
			path := op.pending[op.pendingIdx]
			op.pendingIdx++
			*out = appendValue(&op.outBuf, op.inputRow, path)
			return true, nil
		}
		// Emit the single Null-path row for an OPTIONAL MATCH with no path.
		if op.pendingNull {
			op.pendingNull = false
			*out = appendValue(&op.outBuf, op.inputRow, expr.Null)
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

		srcID, dstID, valid := extractEndpoints(cp, op.srcCol, op.dstCol)
		if !valid {
			if op.optional {
				op.pendingNull = true
			}
			continue
		}

		paths, err := op.bfsAllShortest(srcID, dstID)
		if err != nil {
			return false, err
		}
		op.pending = paths
		op.pendingIdx = 0
		if len(paths) == 0 && op.optional {
			op.pendingNull = true
		}
	}
}

// bfsAllShortest finds all shortest paths from src to dst using
// level-synchronous BFS with a multi-predecessor map.
func (op *AllShortestPaths) bfsAllShortest(src, dst uint64) ([]expr.ListValue, error) {
	if src == dst && op.minHops == 0 {
		return []expr.ListValue{{expr.IntegerValue(int64(src))}}, nil
	}

	// preds[nodeID] = all predecessor entries that reach nodeID at the shortest
	// BFS distance. dist[nodeID] = the BFS level at which nodeID was first
	// reached.
	preds := make(map[uint64][]aspPredEntry)
	dist := make(map[uint64]int)
	dist[src] = 0

	queue := []uint64{src}
	found := false
	foundLevel := -1

	for level := 1; len(queue) > 0; level++ {
		if err := op.ctx.Err(); err != nil {
			return nil, err
		}
		if found && level > foundLevel {
			break
		}
		if op.maxHops != shortestNoMaxHops && level > op.maxHops {
			break
		}

		var next []uint64
		for _, node := range queue {
			if op.dir != DirIn {
				ns, f := op.aspExpand(node, dist, preds, level, dst, true)
				if f {
					found = true
					foundLevel = level
				}
				next = append(next, ns...)
			}
			if op.dir != DirOut && op.revVerts != nil {
				ns, f := op.aspExpand(node, dist, preds, level, dst, false)
				if f {
					found = true
					foundLevel = level
				}
				next = append(next, ns...)
			}
		}
		queue = next
	}

	if !found || foundLevel < op.minHops {
		return nil, nil
	}

	return op.reconstructAll(preds, src, dst)
}

// aspExpand expands edges of node at the given BFS level. isFwd selects forward
// vs reverse adjacency.
func (op *AllShortestPaths) aspExpand(node uint64, dist map[uint64]int, preds map[uint64][]aspPredEntry, level int, dst uint64, isFwd bool) (newNodes []uint64, found bool) {
	verts, edges := op.revVerts, op.revEdges
	if isFwd {
		verts, edges = op.fwdVerts, op.fwdEdges
	}
	if node+1 >= uint64(len(verts)) {
		return nil, false
	}
	for pos := verts[node]; pos < verts[node+1]; pos++ {
		if !op.passesTypeFilter(pos, isFwd) {
			continue
		}
		neighbour := uint64(edges[pos])
		existingDist, visited := dist[neighbour]
		if visited && existingDist < level {
			continue // reached at a shorter distance; not on any shortest path through here
		}
		if !visited {
			dist[neighbour] = level
			newNodes = append(newNodes, neighbour)
		}
		preds[neighbour] = append(preds[neighbour], aspPredEntry{parent: node, rawPos: pos, fwd: isFwd})
		if neighbour == dst {
			found = true
		}
	}
	return newNodes, found
}

// reconstructAll reconstructs all shortest paths via a stack walk over the
// predecessor map, walking backwards from dst to src. The number of shortest
// paths can be exponential in a dense/layered graph, so the walk checks
// op.ctx periodically and aborts with the context error rather than running
// unbounded — openCypher requires returning ALL shortest paths, but a deadline
// or cancellation must still interrupt the enumeration (#1780).
func (op *AllShortestPaths) reconstructAll(preds map[uint64][]aspPredEntry, src, dst uint64) ([]expr.ListValue, error) {
	type frame struct {
		node    uint64
		partial []hop // hops collected so far, in reverse order (dst→src)
	}

	var paths []expr.ListValue
	stack := []frame{{node: dst}}

	const ctxCheckEvery = 1024
	iter := 0
	for len(stack) > 0 {
		if iter++; iter&(ctxCheckEvery-1) == 0 {
			if err := op.ctx.Err(); err != nil {
				return nil, err
			}
		}
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if top.node == src {
			// Reverse partial (dst→src) into src→dst order.
			hops := make([]hop, len(top.partial))
			for i := range top.partial {
				hops[i] = top.partial[len(top.partial)-1-i]
			}
			paths = append(paths, buildHopList(src, hops))
			continue
		}

		for _, entry := range preds[top.node] {
			fwdPos, reversed := op.resolveFwdPos(entry)
			newPartial := make([]hop, len(top.partial)+1)
			copy(newPartial, top.partial)
			newPartial[len(top.partial)] = hop{dstID: top.node, fwdPos: fwdPos, reversed: reversed}
			stack = append(stack, frame{node: entry.parent, partial: newPartial})
		}
	}
	return paths, nil
}

// passesTypeFilter mirrors [ShortestPath.passesTypeFilter].
func (op *AllShortestPaths) passesTypeFilter(pos uint64, isFwd bool) bool {
	if op.edgeType == "" {
		return true
	}
	fwdPos := pos
	if !isFwd {
		fwdPos = op.resolvedFwdPosOrSelf(pos)
		if fwdPos == pos {
			return true
		}
	}
	_, ok := op.edgeTypeFilter[fwdPos]
	return ok
}

// resolveFwdPos mirrors [ShortestPath.resolveFwdPos].
func (op *AllShortestPaths) resolveFwdPos(e aspPredEntry) (fwdPos uint64, reversed bool) {
	if e.fwd {
		return e.rawPos, false
	}
	return op.resolvedFwdPosOrSelf(e.rawPos), true
}

// resolvedFwdPosOrSelf mirrors [ShortestPath.resolvedFwdPosOrSelf].
func (op *AllShortestPaths) resolvedFwdPosOrSelf(revPos uint64) uint64 {
	if op.revToFwd != nil && revPos < uint64(len(op.revToFwd)) {
		if mapped := op.revToFwd[revPos]; mapped != unresolvedFwdPos {
			return mapped
		}
	}
	return revPos
}

// Close closes the input operator.
func (op *AllShortestPaths) Close() error {
	op.pending = nil
	op.outBuf = nil
	return op.input.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────

// extractEndpoints reads the source and destination NodeIDs from a row. Both
// the raw IntegerValue (in-pipeline encoding) and an upgraded NodeValue (after
// a WITH projection) are accepted, mirroring [VarLengthExpand]'s source-column
// handling.
func extractEndpoints(row Row, srcCol, dstCol int) (src, dst uint64, valid bool) {
	if srcCol >= len(row) || dstCol >= len(row) {
		return 0, 0, false
	}
	s, ok1 := nodeIDFromValue(row[srcCol])
	d, ok2 := nodeIDFromValue(row[dstCol])
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return uint64(s), uint64(d), true
}

// buildHopList builds the flat alternating list
// [srcID, fwdPos0, dst0, dir0, …] (stride [VLEHopStride]) from a src node and
// its src→dst-ordered hops. A zero-hop path yields [srcID] (1 element).
func buildHopList(src uint64, hops []hop) expr.ListValue {
	lv := make(expr.ListValue, 1+VLEHopStride*len(hops))
	lv[0] = expr.IntegerValue(int64(src))
	for i, h := range hops {
		dir := VLEDirForward
		if h.reversed {
			dir = VLEDirReverse
		}
		lv[1+VLEHopStride*i] = expr.IntegerValue(int64(h.fwdPos))
		lv[2+VLEHopStride*i] = expr.IntegerValue(int64(h.dstID))
		lv[3+VLEHopStride*i] = expr.IntegerValue(int64(dir))
	}
	return lv
}

// appendValue writes inputRow followed by val into the reusable buffer *buf and
// returns the resulting row slice.
func appendValue(buf *[]expr.Value, inputRow Row, val expr.Value) Row {
	need := len(inputRow) + 1
	if cap(*buf) < need {
		*buf = make([]expr.Value, need)
	}
	*buf = (*buf)[:need]
	copy(*buf, inputRow)
	(*buf)[len(inputRow)] = val
	return *buf
}
