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

	// pathPred, when non-nil, is a whole-path predicate that references the path
	// variable (#1786). The operator then runs an EXHAUSTIVE search returning the
	// shortest path that SATISFIES the predicate, rather than the unconstrained
	// shortest path. It is called with the candidate's full output row (the input
	// row followed by the candidate path-list column).
	pathPred func(Row) (bool, error)

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

// WithPathPredicate fuses a whole-path predicate onto the operator (#1786). The
// operator then returns the shortest path that SATISFIES pred (an exhaustive
// search), instead of the unconstrained shortest path. pred is called with the
// candidate's full output row. It returns op for chaining.
func (op *ShortestPath) WithPathPredicate(pred func(Row) (bool, error)) *ShortestPath {
	op.pathPred = pred
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

		var (
			path  expr.Value
			found bool
		)
		if op.pathPred != nil {
			path, found, err = op.exhaustiveShortestPath(srcID, dstID, inputRow)
		} else {
			path, found, err = op.bfsShortestPath(srcID, dstID)
		}
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
		// minHops >= 1 forbids the zero-length path: search for the shortest
		// non-trivial cycle back to src (a self-loop, or a longer closed
		// trail). The plain BFS below pre-marks src visited at level 0, so it
		// can never take the closing edge back to src — a dedicated
		// cycle-aware search is required (#1779).
		return op.bfsShortestCycle(src)
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

// exhParc is one directed traversal arc enumerated during an exhaustive
// path search: the neighbour reached, the per-edge stable handle (for
// relationship-uniqueness), and the hop encoding fields (handle-disambiguated
// forward position + traversal orientation) used to hydrate the per-instance
// relationship.
type exhParc struct {
	neighbour uint64
	handle    uint64
	fwdPos    uint64
	reversed  bool
}

// exhArcs enumerates every traversal arc out of node honouring op.dir: forward
// arcs unless DirIn, reverse arcs when DirIn or DirBoth. Arcs are
// de-duplicated by stable handle so an undirected edge present in both CSRs is
// considered once per node.
func (op *ShortestPath) exhArcs(node uint64) []exhParc {
	var out []exhParc
	seen := map[uint64]struct{}{}
	scan := func(isFwd bool) {
		verts, edges := op.revVerts, op.revEdges
		if isFwd {
			verts, edges = op.fwdVerts, op.fwdEdges
		}
		if node+1 >= uint64(len(verts)) {
			return
		}
		for pos := verts[node]; pos < verts[node+1]; pos++ {
			if !op.passesTypeFilter(pos, isFwd) {
				continue
			}
			h := op.relHandle(pos, isFwd)
			if _, dup := seen[h]; dup {
				continue
			}
			seen[h] = struct{}{}
			fwdPos, reversed := op.resolveFwdPos(spPredEntry{rawPos: pos, fwd: isFwd})
			out = append(out, exhParc{neighbour: uint64(edges[pos]), handle: h, fwdPos: fwdPos, reversed: reversed})
		}
	}
	if op.dir != DirIn {
		scan(true)
	}
	if op.dir != DirOut && op.revVerts != nil {
		scan(false)
	}
	return out
}

// exhaustiveShortestPath returns the SHORTEST src->dst path that SATISFIES the
// fused whole-path predicate (#1786). It enumerates relationship-unique
// (edge-simple) paths in non-decreasing length order via a frontier of partial
// paths, builds each candidate's flat path-list, assembles the candidate output
// row (inputRow followed by the path column) and evaluates op.pathPred; the
// FIRST candidate the predicate accepts — necessarily of minimum satisfying
// length — is returned. The search is bounded by op.maxHops and honours
// op.ctx (#1780): the enumeration of satisfying paths can be exponential, so a
// deadline or cancellation interrupts it.
func (op *ShortestPath) exhaustiveShortestPath(src, dst uint64, inputRow Row) (expr.Value, bool, error) {
	// A zero-length path is a candidate only when src == dst and minHops == 0.
	if src == dst && op.minHops == 0 {
		cand := expr.ListValue{expr.IntegerValue(int64(src))}
		okPred, err := op.testCandidate(inputRow, cand)
		if err != nil {
			return nil, false, err
		}
		if okPred {
			return cand, true, nil
		}
	}

	type partial struct {
		node    uint64
		hops    []hop
		handles []uint64
	}
	frontier := []partial{{node: src}}

	const ctxCheckEvery = 1024
	iter := 0
	for level := 1; len(frontier) > 0; level++ {
		if op.maxHops != shortestNoMaxHops && level > op.maxHops {
			break
		}
		var next []partial
		for _, pp := range frontier {
			for _, arc := range op.exhArcs(pp.node) {
				iter++
				if iter&(ctxCheckEvery-1) == 0 {
					if err := op.ctx.Err(); err != nil {
						return nil, false, err
					}
				}
				if containsRel(pp.handles, arc.handle) {
					continue // relationship-uniqueness
				}
				nh := make([]hop, len(pp.hops)+1)
				copy(nh, pp.hops)
				nh[len(pp.hops)] = hop{dstID: arc.neighbour, fwdPos: arc.fwdPos, reversed: arc.reversed}
				nhandles := make([]uint64, len(pp.handles)+1)
				copy(nhandles, pp.handles)
				nhandles[len(pp.handles)] = arc.handle
				if arc.neighbour == dst && level >= op.minHops {
					cand := buildHopList(src, nh)
					okPred, err := op.testCandidate(inputRow, cand)
					if err != nil {
						return nil, false, err
					}
					if okPred {
						return cand, true, nil
					}
				}
				next = append(next, partial{node: arc.neighbour, hops: nh, handles: nhandles})
			}
		}
		frontier = next
	}
	return expr.Null, false, nil
}

// testCandidate assembles the candidate output row (inputRow followed by the
// candidate path-list) and evaluates the fused whole-path predicate against it.
func (op *ShortestPath) testCandidate(inputRow Row, cand expr.ListValue) (bool, error) {
	row := make(Row, len(inputRow)+1)
	copy(row, inputRow)
	row[len(inputRow)] = cand
	return op.pathPred(row)
}

// bfsShortestCycle finds the shortest non-trivial closed trail from src back to
// src (hop count >= 1), honouring relationship-uniqueness.
//
// Two algorithms are used depending on traversal direction (#1779):
//
//   - DirOut (the strictly forward `(s)-[*1..]->(s)` form): a node-keyed
//     single-predecessor BFS that treats any edge whose head is src as a
//     closing edge (see [ShortestPath.bfsShortestCycleForward]). This is sound
//     for forward-only traversal because a forward arc m->src is a distinct
//     relationship from the forward prefix arcs (per-arc identity), and the
//     prefix walk rejects any handle reuse.
//
//   - DirBoth / DirIn (the undirected `(s)-[*1..]-(s)` or reverse form): when
//     an undirected/anti-parallel edge is one handle traversable both ways, the
//     node-keyed decomposition is UNSOUND — the closing edge {m,src} can be the
//     same handle as the shortest-prefix edge {src,m}, so it would miss the true
//     shortest edge-simple cycle (the a-b-c-a triangle trap). The Itai & Rodeh
//     1978 branch-collision algorithm is used instead (see
//     [ShortestPath.bfsShortestCycleBranch]).
func (op *ShortestPath) bfsShortestCycle(src uint64) (expr.Value, bool, error) {
	if op.dir == DirBoth {
		return op.bfsShortestCycleBranch(src)
	}
	return op.bfsShortestCycleForward(src)
}

// bfsShortestCycleForward is the forward-only (DirOut) shortest-cycle search:
// the shortest cycle length is L* = min over in-neighbours m of src of
// (dist(src, m) + 1). A shortest src->m path followed by the closing edge
// m->src is an optimal witness; for L* >= 3 the witness is automatically
// edge-simple (a shortest path from src is node-simple and never re-enters
// src). The prefix-reuse guard ([ShortestPath.prefixReusesEdge]) rejects the
// L* == 2 case where the closing arc would reuse the opening arc's handle. A
// self-loop src->src is the L* == 1 case, captured at seeding.
func (op *ShortestPath) bfsShortestCycleForward(src uint64) (expr.Value, bool, error) {
	// pred[nodeID] = the single predecessor entry that first discovered nodeID
	// (excluding src, which is the cycle anchor and never a discovery target).
	pred := make(map[uint64]spPredEntry)

	// closing records the edge that returns to src and the node it left from.
	var (
		closingParent uint64
		closingPos    uint64
		closingFwd    bool
		found         bool
	)

	// expandFromCycle scans node's forward/reverse edges. A neighbour == src is
	// the closing hop; any other unvisited neighbour is a normal discovery.
	// The closing edge is rejected if its physical relationship is already on
	// the src->node prefix (relationship-uniqueness). The prefix is a shortest
	// path from src so it is node-simple; the only feasible reuse is an
	// undirected/anti-parallel edge re-traversed in the opposite direction
	// (which shares its handle), but the prefix walk covers the general case.
	expandFromCycle := func(node uint64, _ int) []uint64 {
		var next []uint64
		scan := func(isFwd bool) {
			verts, edges := op.revVerts, op.revEdges
			if isFwd {
				verts, edges = op.fwdVerts, op.fwdEdges
			}
			if node+1 >= uint64(len(verts)) {
				return
			}
			for pos := verts[node]; pos < verts[node+1]; pos++ {
				if !op.passesTypeFilter(pos, isFwd) {
					continue
				}
				neighbour := uint64(edges[pos])
				if neighbour == src {
					// Closing edge back to src. The first edge-simple closing
					// edge found at this (minimum) BFS level wins.
					if found {
						continue
					}
					if op.prefixReusesEdge(pred, src, node, op.relHandle(pos, isFwd)) {
						continue
					}
					closingParent = node
					closingPos = pos
					closingFwd = isFwd
					found = true
					continue
				}
				if _, visited := pred[neighbour]; visited {
					continue
				}
				pred[neighbour] = spPredEntry{parent: node, rawPos: pos, fwd: isFwd}
				next = append(next, neighbour)
			}
		}
		if op.dir != DirIn {
			scan(true)
		}
		if op.dir != DirOut && op.revVerts != nil {
			scan(false)
		}
		return next
	}

	// Level-1 seeding from src. A self-loop src->src closes the cycle at length 1.
	queue := expandFromCycle(src, 1)
	level := 1

	for !found && len(queue) > 0 {
		if err := op.ctx.Err(); err != nil {
			return nil, false, err
		}
		level++
		if op.maxHops != shortestNoMaxHops && level > op.maxHops {
			break
		}
		var next []uint64
		for _, node := range queue {
			next = append(next, expandFromCycle(node, level)...)
		}
		queue = next
	}

	if !found {
		return expr.Null, false, nil
	}
	if level < op.minHops {
		return expr.Null, false, nil
	}

	// Reconstruct: walk pred from closingParent back to src (the src->...->m
	// prefix), then append the closing edge m->src.
	var hops []hop
	cur := closingParent
	for cur != src {
		entry := pred[cur]
		fwdPos, reversed := op.resolveFwdPos(entry)
		hops = append(hops, hop{dstID: cur, fwdPos: fwdPos, reversed: reversed})
		cur = entry.parent
	}
	for i, j := 0, len(hops)-1; i < j; i, j = i+1, j-1 {
		hops[i], hops[j] = hops[j], hops[i]
	}
	// Closing hop m->src.
	closeFwd, closeRev := op.resolveFwdPos(spPredEntry{parent: closingParent, rawPos: closingPos, fwd: closingFwd})
	hops = append(hops, hop{dstID: src, fwdPos: closeFwd, reversed: closeRev})
	return buildHopList(src, hops), true, nil
}

// srcBranchSentinel is the branch tag of the cycle anchor src itself. It must
// not equal any real edge handle so that a non-tree arc incident on src (a
// distinct parallel edge or a self-loop's closing comparison) always satisfies
// the branch-inequality test. Real handles begin at 1, so the maximum uint64 is
// a safe reserved value.
const srcBranchSentinel = ^uint64(0)

// scanArc is one examined arc out of a node during the branch-collision search:
// the neighbour reached, the per-edge stable handle, and the
// handle-disambiguated forward position / traversal direction used for
// per-instance hydration. fwdPos is ALWAYS a forward-CSR position (already
// resolved through [ShortestPath.resolveFwdPos]); reversed is the traversal
// orientation. Because fwdPos is pre-resolved, a hop built directly from a
// scanArc never re-runs reverse→forward resolution (the lossy round-trip that a
// prior prototype got wrong).
type scanArc struct {
	neighbour uint64
	handle    uint64
	fwdPos    uint64
	reversed  bool
}

// branchPred is a BFS-tree predecessor in the branch-collision search. It keeps
// the parent node and the FULLY-RESOLVED arc that discovered the child, so
// reconstruction reads the forward position and orientation directly without
// any reverse→forward remapping.
type branchPred struct {
	parent uint64
	arc    scanArc
}

// hopForTraversal resolves the hop that traverses the edge identified by handle
// h from node `from` to node `to`, in that traversal direction. It is the
// hydration-correct primitive for the undirected shortest-cycle reconstruction:
// the emitted (fwdPos, reversed) pair always points the relationship hydrator at
// the edge's TRUE stored direction, so the per-instance type and properties
// resolve regardless of which of an undirected edge's two symmetric forward arcs
// was traversed during the search.
//
//   - When the edge is STORED from->to, a forward-CSR arc from->to carries h:
//     emit reversed=false with that forward position; the hydrator's storage
//     pair (from,to) then matches the by-handle store.
//   - Otherwise the edge is STORED to->from (an undirected reverse hop or a
//     directed edge traversed against its arrow): the forward-CSR arc to->from
//     carries h. Emit reversed=true with THAT position; the hydrator swaps its
//     storage pair to (to,from), matching the by-handle store, and renders the
//     hop as a reverse traversal.
//
// When no handle column is present (a simple directed snapshot) the per-slot
// handle falls back to the forward position itself, which is still unique per
// arc, so the lookup remains correct.
func (op *ShortestPath) hopForTraversal(from, to, h uint64) hop {
	// Forward arc from->to carrying h (edge stored in traversal direction).
	if from+1 < uint64(len(op.fwdVerts)) {
		for pos := op.fwdVerts[from]; pos < op.fwdVerts[from+1]; pos++ {
			if uint64(op.fwdEdges[pos]) == to && op.relHandle(pos, true) == h {
				return hop{dstID: to, fwdPos: pos, reversed: false}
			}
		}
	}
	// Forward arc to->from carrying h (edge stored against traversal direction).
	if to+1 < uint64(len(op.fwdVerts)) {
		for pos := op.fwdVerts[to]; pos < op.fwdVerts[to+1]; pos++ {
			if uint64(op.fwdEdges[pos]) == from && op.relHandle(pos, true) == h {
				return hop{dstID: to, fwdPos: pos, reversed: true}
			}
		}
	}
	// Fallback (should not occur for a handle taken from the live CSR): emit a
	// forward hop with no resolvable position so hydration degrades to the
	// declared type rather than panicking.
	return hop{dstID: to, fwdPos: h, reversed: false}
}

// branchArcs enumerates every distinct undirected arc out of node for the
// DirBoth shortest-cycle search. It scans both the forward and the reverse CSR
// (so a directed graph queried with `-[*1..]-` sees both orientations) and
// de-duplicates by stable handle, because an UNDIRECTED graph stores each edge
// as two forward-CSR arcs that share one handle — and the reverse CSR mirrors
// them — so the same physical edge would otherwise be visited up to four times
// from one node. Each returned arc carries the handle-disambiguated forward
// position and traversal direction so the emitted hop hydrates the correct
// per-instance relationship type and properties.
func (op *ShortestPath) branchArcs(node uint64, seen map[uint64]struct{}) []scanArc {
	var out []scanArc
	scan := func(isFwd bool) {
		verts, edges := op.revVerts, op.revEdges
		if isFwd {
			verts, edges = op.fwdVerts, op.fwdEdges
		}
		if node+1 >= uint64(len(verts)) {
			return
		}
		for pos := verts[node]; pos < verts[node+1]; pos++ {
			if !op.passesTypeFilter(pos, isFwd) {
				continue
			}
			h := op.relHandle(pos, isFwd)
			if _, dup := seen[h]; dup {
				continue
			}
			seen[h] = struct{}{}
			fwdPos, reversed := op.resolveFwdPos(spPredEntry{rawPos: pos, fwd: isFwd})
			out = append(out, scanArc{
				neighbour: uint64(edges[pos]),
				handle:    h,
				fwdPos:    fwdPos,
				reversed:  reversed,
			})
		}
	}
	scan(true)
	if op.revVerts != nil {
		scan(false)
	}
	return out
}

// bfsShortestCycleBranch is the UNDIRECTED (DirBoth) shortest-cycle search,
// implementing the Itai & Rodeh (1978) branch-collision method (#1785). A
// node-keyed single-predecessor BFS is UNSOUND here because an undirected edge
// is one handle traversable both ways, so the closing edge {m,src} can be the
// SAME handle as the shortest-prefix edge {src,m} (the a-b-c-a triangle needs a
// non-shortest prefix that node-keyed BFS never explores).
//
// The method tags every node with branch[node] = the HANDLE of the first
// s-incident edge on its BFS-tree arm (keyed on the handle, not the level-1
// node: two distinct parallel s—m edges are distinct branches). Branch tags
// propagate only along tree edges, so two nodes with different tags share no
// node but src (in a BFS tree each node has one parent ⇒ one tag). A non-tree
// arc e=(u,v) with branch[u] != branch[v] therefore closes an EDGE-SIMPLE cycle
// of length dist[u]+dist[v]+1: arm s->u + e + reverse(arm v->s). The minimum
// such length, together with the length-1 self-loop and length-2
// distinct-parallel special cases at src, is the shortest cycle through src.
func (op *ShortestPath) bfsShortestCycleBranch(src uint64) (expr.Value, bool, error) {
	dist := map[uint64]int{src: 0}
	bpred := make(map[uint64]branchPred) // BFS-tree predecessor (src excluded)
	branch := make(map[uint64]uint64)    // branch tag per discovered node

	branchOf := func(node uint64) uint64 {
		if node == src {
			return srcBranchSentinel
		}
		return branch[node]
	}

	// Best witness found so far.
	var (
		found     bool
		bestLen   int
		wU, wV    uint64 // witness arm endpoints (cycle = src->u + e + reverse(v->src))
		wArc      scanArc
		selfLoop  bool    // best witness is a length-1 self-loop
		selfArc   scanArc // the self-loop arc
		dparallel bool    // best witness is a length-2 distinct-parallel pair
		dpFirst   scanArc // first src->m arc
		dpSecond  scanArc // second src->m arc (distinct handle)
	)

	consider := func(u, v uint64, arc scanArc) {
		l := dist[u] + dist[v] + 1
		if found && l >= bestLen {
			return
		}
		found, bestLen = true, l
		wU, wV, wArc = u, v, arc
		selfLoop, dparallel = false, false
	}

	// Level-1 seeding from src. A self-loop is the length-1 case; a second
	// distinct-handle arc to an already-seen neighbour m is the length-2 case.
	seedSeen := map[uint64]struct{}{}
	firstArcTo := map[uint64]scanArc{}
	var frontier []uint64
	for _, arc := range op.branchArcs(src, seedSeen) {
		if arc.neighbour == src {
			// Self-loop {src,src}: a length-1 edge-simple cycle.
			if !found || 1 < bestLen {
				found, bestLen = true, 1
				selfLoop, dparallel = true, false
				selfArc = arc
			}
			continue
		}
		if prev, ok := firstArcTo[arc.neighbour]; ok {
			// Second distinct edge src—m: a length-2 edge-simple cycle.
			if !found || 2 < bestLen {
				found, bestLen = true, 2
				dparallel, selfLoop = true, false
				dpFirst, dpSecond = prev, arc
			}
			continue
		}
		firstArcTo[arc.neighbour] = arc
		dist[arc.neighbour] = 1
		branch[arc.neighbour] = arc.handle
		bpred[arc.neighbour] = branchPred{parent: src, arc: arc}
		frontier = append(frontier, arc.neighbour)
	}

	// Level >= 1 BFS over the frontier, examining every non-tree arc for a
	// branch collision. At iteration start, frontier holds the nodes at distance
	// `level`; tree edges discover the level+1 nodes.
	for level := 1; len(frontier) > 0; level++ {
		if err := op.ctx.Err(); err != nil {
			return nil, false, err
		}
		if op.maxHops != shortestNoMaxHops && level >= op.maxHops {
			break
		}
		var next []uint64
		for _, u := range frontier {
			seen := map[uint64]struct{}{}
			for _, arc := range op.branchArcs(u, seen) {
				v := arc.neighbour
				// Skip the tree arc back to u's parent (same physical handle).
				if pe, ok := bpred[u]; ok && pe.arc.handle == arc.handle {
					continue
				}
				vDist, disc := dist[v]
				if !disc {
					// Tree edge: discover v at level+1, inheriting u's branch.
					dist[v] = level + 1
					branch[v] = branchOf(u)
					bpred[v] = branchPred{parent: u, arc: arc}
					next = append(next, v)
					continue
				}
				if vDist == 0 {
					continue // arc into src: covered by the seeding special cases
				}
				// Non-tree arc u->v with v already discovered. A branch collision
				// closes an edge-simple cycle of length dist[u]+dist[v]+1.
				if branchOf(u) != branchOf(v) {
					consider(u, v, arc)
				}
			}
		}
		// Once the next frontier sits at distance d=level+1, the shortest cycle a
		// future collision could still form is (d-1)+d+1 = 2d-1 (a node at d-1
		// colliding with one at d). Stop as soon as that cannot beat the best
		// (2d-1 >= bestLen, equivalently 2d > bestLen for integers).
		if found && 2*(level+1) > bestLen {
			break
		}
		frontier = next
	}

	if !found || bestLen < op.minHops {
		return expr.Null, false, nil
	}

	switch {
	case selfLoop:
		hops := []hop{op.hopForTraversal(src, src, selfArc.handle)}
		return buildHopList(src, hops), true, nil
	case dparallel:
		// src -> m (first arc), then m -> src (second, distinct-handle arc).
		m := dpFirst.neighbour
		hops := []hop{
			op.hopForTraversal(src, m, dpFirst.handle),
			op.hopForTraversal(m, src, dpSecond.handle),
		}
		return buildHopList(src, hops), true, nil
	default:
		return buildHopList(src, op.assembleBranchCycle(bpred, src, wU, wV, wArc)), true, nil
	}
}

// assembleBranchCycle builds the src->u arm, the joining edge u->v, and the
// reversed v->src arm into a single src-ordered hop list. The joining edge is
// emitted in its u->v orientation; the second arm is the v->src BFS-tree path
// walked toward src, so each of its arcs is emitted with its traversal
// direction flipped (the tree recorded it as parent->child, the cycle traverses
// child->parent).
func (op *ShortestPath) assembleBranchCycle(bpred map[uint64]branchPred, src, u, v uint64, join scanArc) []hop {
	// Build the ordered traversal node+handle sequence
	// src -> … -> u -> v -> … -> src, then resolve each step into a
	// hydration-correct hop via [ShortestPath.hopForTraversal] (which points the
	// hydrator at the edge's TRUE stored direction regardless of which symmetric
	// arc the search traversed).
	type step struct {
		from, to uint64
		handle   uint64
	}

	// Arm 1: src -> … -> u. Walk the BFS tree from u back to src collecting
	// (parent->child, handle), then reverse to src-forward order.
	var arm1 []step
	cur := u
	for cur != src {
		e := bpred[cur]
		arm1 = append(arm1, step{from: e.parent, to: cur, handle: e.arc.handle})
		cur = e.parent
	}
	for i, j := 0, len(arm1)-1; i < j; i, j = i+1, j-1 {
		arm1[i], arm1[j] = arm1[j], arm1[i]
	}

	steps := arm1
	// Joining step u -> v.
	steps = append(steps, step{from: u, to: v, handle: join.handle})

	// Arm 2: v -> … -> src, traversed child->parent (the BFS tree recorded each
	// arc as parent->child).
	cur = v
	for cur != src {
		e := bpred[cur]
		steps = append(steps, step{from: cur, to: e.parent, handle: e.arc.handle})
		cur = e.parent
	}

	hops := make([]hop, len(steps))
	for i, s := range steps {
		hops[i] = op.hopForTraversal(s.from, s.to, s.handle)
	}
	return hops
}

// relHandle returns the stable per-edge identity of the relationship at CSR
// position pos (forward CSR when isFwd, otherwise reverse CSR). It uses the
// per-slot stable handle when available — essential for an undirected/
// anti-parallel edge, whose two stored arcs occupy DIFFERENT CSR positions but
// share ONE handle, so a handle comparison recognises a reverse re-traversal of
// the same physical edge. When handles are absent (a simple directed snapshot
// with no parallel edges) it falls back to the handle-disambiguated forward
// position, which is unique per relationship there.
func (op *ShortestPath) relHandle(pos uint64, isFwd bool) uint64 {
	if isFwd {
		if op.fwdHandles != nil && pos < uint64(len(op.fwdHandles)) {
			return op.fwdHandles[pos]
		}
		return pos
	}
	if op.revHandles != nil && pos < uint64(len(op.revHandles)) {
		return op.revHandles[pos]
	}
	return op.resolvedFwdPosOrSelf(pos)
}

// prefixReusesEdge reports whether the relationship identified by handle h
// already appears on the shortest src->node prefix recorded in pred. The walk
// terminates at src (the cycle anchor, absent from pred). Used to enforce
// relationship-uniqueness on the closing edge of a cycle.
func (op *ShortestPath) prefixReusesEdge(pred map[uint64]spPredEntry, src, node, h uint64) bool {
	cur := node
	for cur != src {
		entry := pred[cur]
		if op.relHandle(entry.rawPos, entry.fwd) == h {
			return true
		}
		cur = entry.parent
	}
	return false
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

	// pathPred, when non-nil, is a whole-path predicate referencing the path
	// variable (#1786). The operator then runs an EXHAUSTIVE search returning ALL
	// shortest paths that SATISFY the predicate (the minimum satisfying length),
	// rather than all unconstrained shortest paths.
	pathPred func(Row) (bool, error)

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

// WithPathPredicate fuses a whole-path predicate onto the operator (#1786). The
// operator then returns ALL shortest paths that SATISFY pred (an exhaustive
// search at the minimum satisfying length), instead of all unconstrained
// shortest paths. pred is called with each candidate's full output row. It
// returns op for chaining.
func (op *AllShortestPaths) WithPathPredicate(pred func(Row) (bool, error)) *AllShortestPaths {
	op.pathPred = pred
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
// level-synchronous BFS with a multi-predecessor map. When a whole-path
// predicate is fused (#1786) it instead runs an exhaustive search returning all
// shortest SATISFYING paths.
func (op *AllShortestPaths) bfsAllShortest(src, dst uint64) ([]expr.ListValue, error) {
	if op.pathPred != nil {
		return op.exhaustiveAllShortest(src, dst, op.inputRow)
	}
	if src == dst {
		if op.minHops == 0 {
			return []expr.ListValue{{expr.IntegerValue(int64(src))}}, nil
		}
		// minHops >= 1: enumerate all shortest non-trivial cycles back to src
		// (#1779). Plain BFS cannot close the cycle (src is marked at level 0).
		if op.dir == DirBoth {
			// UNDIRECTED cycles need the branch-collision method (#1785) for the
			// same reason as the single-path case: a node-keyed search misses the
			// shortest edge-simple cycle through the single-handle reverse-edge
			// trap.
			return op.bfsAllShortestCycleBranch(src)
		}
		return op.bfsAllShortestCycle(src)
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

// exhArcs mirrors [ShortestPath.exhArcs]: every traversal arc out of node
// honouring op.dir, de-duplicated by stable handle.
func (op *AllShortestPaths) exhArcs(node uint64) []exhParc {
	var out []exhParc
	seen := map[uint64]struct{}{}
	scan := func(isFwd bool) {
		verts, edges := op.revVerts, op.revEdges
		if isFwd {
			verts, edges = op.fwdVerts, op.fwdEdges
		}
		if node+1 >= uint64(len(verts)) {
			return
		}
		for pos := verts[node]; pos < verts[node+1]; pos++ {
			if !op.passesTypeFilter(pos, isFwd) {
				continue
			}
			h := op.relHandle(pos, isFwd)
			if _, dup := seen[h]; dup {
				continue
			}
			seen[h] = struct{}{}
			fwdPos, reversed := op.resolveFwdPos(aspPredEntry{rawPos: pos, fwd: isFwd})
			out = append(out, exhParc{neighbour: uint64(edges[pos]), handle: h, fwdPos: fwdPos, reversed: reversed})
		}
	}
	if op.dir != DirIn {
		scan(true)
	}
	if op.dir != DirOut && op.revVerts != nil {
		scan(false)
	}
	return out
}

// exhaustiveAllShortest returns EVERY shortest src->dst path that SATISFIES the
// fused whole-path predicate (#1786). It enumerates relationship-unique paths in
// non-decreasing length order; once a length L* yields at least one satisfying
// path, every satisfying path of that SAME length is collected and the search
// stops (longer paths cannot be shortest). Bounded by op.maxHops and honours
// op.ctx (#1780).
func (op *AllShortestPaths) exhaustiveAllShortest(src, dst uint64, inputRow Row) ([]expr.ListValue, error) {
	var out []expr.ListValue

	if src == dst && op.minHops == 0 {
		cand := expr.ListValue{expr.IntegerValue(int64(src))}
		okPred, err := op.testCandidate(inputRow, cand)
		if err != nil {
			return nil, err
		}
		if okPred {
			return []expr.ListValue{cand}, nil
		}
	}

	type partial struct {
		node    uint64
		hops    []hop
		handles []uint64
	}
	frontier := []partial{{node: src}}
	foundLen := -1

	const ctxCheckEvery = 1024
	iter := 0
	for level := 1; len(frontier) > 0; level++ {
		if op.maxHops != shortestNoMaxHops && level > op.maxHops {
			break
		}
		if foundLen != -1 && level > foundLen {
			break // already collected every satisfying path of the minimum length
		}
		var next []partial
		for _, pp := range frontier {
			for _, arc := range op.exhArcs(pp.node) {
				iter++
				if iter&(ctxCheckEvery-1) == 0 {
					if err := op.ctx.Err(); err != nil {
						return nil, err
					}
				}
				if containsRel(pp.handles, arc.handle) {
					continue
				}
				nh := make([]hop, len(pp.hops)+1)
				copy(nh, pp.hops)
				nh[len(pp.hops)] = hop{dstID: arc.neighbour, fwdPos: arc.fwdPos, reversed: arc.reversed}
				nhandles := make([]uint64, len(pp.handles)+1)
				copy(nhandles, pp.handles)
				nhandles[len(pp.handles)] = arc.handle
				if arc.neighbour == dst && level >= op.minHops {
					cand := buildHopList(src, nh)
					okPred, err := op.testCandidate(inputRow, cand)
					if err != nil {
						return nil, err
					}
					if okPred {
						foundLen = level
						out = append(out, cand)
					}
				}
				// Only keep expanding while a shorter satisfying length is still
				// possible.
				if foundLen == -1 {
					next = append(next, partial{node: arc.neighbour, hops: nh, handles: nhandles})
				}
			}
		}
		if foundLen != -1 {
			break
		}
		frontier = next
	}
	return out, nil
}

// testCandidate assembles the candidate output row (inputRow followed by the
// candidate path-list) and evaluates the fused whole-path predicate.
func (op *AllShortestPaths) testCandidate(inputRow Row, cand expr.ListValue) (bool, error) {
	row := make(Row, len(inputRow)+1)
	copy(row, inputRow)
	row[len(inputRow)] = cand
	return op.pathPred(row)
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
		iter++
		if iter&(ctxCheckEvery-1) == 0 {
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

// bfsAllShortestCycle enumerates every shortest non-trivial closed trail from
// src back to src (hop count >= 1), honouring relationship-uniqueness. Like the
// single-cycle case (#1779), src is NOT a discovery target in the
// multi-predecessor map; every edge whose head is src is a candidate closing
// edge. The minimum closing level L* fixes the cycle length; all closing edges
// found at level L* — combined with all shortest src->m prefixes — yield the
// complete set. Each reconstructed path is checked for relationship-uniqueness
// (a per-path relationship-id set); this only ever rejects a 2-cycle that
// reuses the same physical edge, but is coded generally so the edge-simplicity
// invariant is explicit.
func (op *AllShortestPaths) bfsAllShortestCycle(src uint64) ([]expr.ListValue, error) {
	// preds[node] = all predecessor entries reaching node at its shortest BFS
	// distance from src (src excluded). dist[node] = that shortest distance.
	preds := make(map[uint64][]aspPredEntry)
	dist := make(map[uint64]int)

	// closings = the predecessor entries of src itself (edges m->src) recorded
	// at the minimum closing level. closeLevel is that level (== L*).
	var closings []aspPredEntry
	closeLevel := -1

	expand := func(node uint64, level int) []uint64 {
		var next []uint64
		scan := func(isFwd bool) {
			verts, edges := op.revVerts, op.revEdges
			if isFwd {
				verts, edges = op.fwdVerts, op.fwdEdges
			}
			if node+1 >= uint64(len(verts)) {
				return
			}
			for pos := verts[node]; pos < verts[node+1]; pos++ {
				if !op.passesTypeFilter(pos, isFwd) {
					continue
				}
				neighbour := uint64(edges[pos])
				if neighbour == src {
					// Closing edge back to src. Record every such edge found at
					// the minimum closing level (later levels are longer cycles
					// and are discarded).
					if closeLevel == -1 {
						closeLevel = level
					}
					if level == closeLevel {
						closings = append(closings, aspPredEntry{parent: node, rawPos: pos, fwd: isFwd})
					}
					continue
				}
				existingDist, visited := dist[neighbour]
				if visited && existingDist < level {
					continue
				}
				if !visited {
					dist[neighbour] = level
					next = append(next, neighbour)
				}
				preds[neighbour] = append(preds[neighbour], aspPredEntry{parent: node, rawPos: pos, fwd: isFwd})
			}
		}
		if op.dir != DirIn {
			scan(true)
		}
		if op.dir != DirOut && op.revVerts != nil {
			scan(false)
		}
		return next
	}

	// Level-1 seeding from src (a self-loop closes at length 1).
	queue := expand(src, 1)
	for level := 2; len(queue) > 0; level++ {
		if err := op.ctx.Err(); err != nil {
			return nil, err
		}
		// Stop once we have started a level beyond the closing level: no longer
		// cycle can be shortest.
		if closeLevel != -1 && level > closeLevel {
			break
		}
		if op.maxHops != shortestNoMaxHops && level > op.maxHops {
			break
		}
		var next []uint64
		for _, node := range queue {
			next = append(next, expand(node, level)...)
		}
		queue = next
	}

	if closeLevel == -1 || closeLevel < op.minHops {
		return nil, nil
	}

	return op.reconstructAllCycles(preds, src, closings)
}

// reconstructAllCycles enumerates every shortest cycle: for each closing edge
// m->src, every shortest src->m prefix is reconstructed (via the
// multi-predecessor DAG) and the closing edge appended. Paths that repeat a
// relationship id are rejected (the edge-simplicity guard). It honours
// op.ctx periodically like [reconstructAll] (#1780).
func (op *AllShortestPaths) reconstructAllCycles(preds map[uint64][]aspPredEntry, src uint64, closings []aspPredEntry) ([]expr.ListValue, error) {
	type frame struct {
		node    uint64
		partial []hop    // hops collected so far, in reverse order (src-side end → closing-side)
		usedRel []uint64 // relationship ids already on this partial (edge-simplicity)
	}

	var paths []expr.ListValue
	const ctxCheckEvery = 1024
	iter := 0

	for _, ce := range closings {
		closeFwd, closeRev := op.resolveFwdPos(ce)
		// Seed the walk at the closing edge's tail (m), with the closing hop
		// m->src already placed as the last hop. The edge-simplicity set keys on
		// the stable per-edge handle (op.relHandle) so an undirected/
		// anti-parallel edge re-traversed in the opposite direction is rejected
		// even though its two arcs occupy different CSR positions.
		stack := []frame{{
			node:    ce.parent,
			partial: []hop{{dstID: src, fwdPos: closeFwd, reversed: closeRev}},
			usedRel: []uint64{op.relHandle(ce.rawPos, ce.fwd)},
		}}
		for len(stack) > 0 {
			iter++
			if iter&(ctxCheckEvery-1) == 0 {
				if err := op.ctx.Err(); err != nil {
					return nil, err
				}
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]

			if top.node == src {
				// Reverse partial (closing-end → src) into src→...→src order.
				hops := make([]hop, len(top.partial))
				for i := range top.partial {
					hops[i] = top.partial[len(top.partial)-1-i]
				}
				paths = append(paths, buildHopList(src, hops))
				continue
			}

			for _, entry := range preds[top.node] {
				fwdPos, reversed := op.resolveFwdPos(entry)
				h := op.relHandle(entry.rawPos, entry.fwd)
				// Edge-simplicity: reject reuse of an already-traversed
				// relationship (only fires for 2-cycles in practice).
				if containsRel(top.usedRel, h) {
					continue
				}
				newPartial := make([]hop, len(top.partial)+1)
				copy(newPartial, top.partial)
				newPartial[len(top.partial)] = hop{dstID: top.node, fwdPos: fwdPos, reversed: reversed}
				newUsed := make([]uint64, len(top.usedRel)+1)
				copy(newUsed, top.usedRel)
				newUsed[len(top.usedRel)] = h
				stack = append(stack, frame{node: entry.parent, partial: newPartial, usedRel: newUsed})
			}
		}
	}
	return paths, nil
}

// branchArcs mirrors [ShortestPath.branchArcs]: every distinct undirected arc
// out of node, de-duplicated by stable handle, with each arc's
// handle-disambiguated forward position and traversal orientation resolved.
func (op *AllShortestPaths) branchArcs(node uint64, seen map[uint64]struct{}) []scanArc {
	var out []scanArc
	scan := func(isFwd bool) {
		verts, edges := op.revVerts, op.revEdges
		if isFwd {
			verts, edges = op.fwdVerts, op.fwdEdges
		}
		if node+1 >= uint64(len(verts)) {
			return
		}
		for pos := verts[node]; pos < verts[node+1]; pos++ {
			if !op.passesTypeFilter(pos, isFwd) {
				continue
			}
			h := op.relHandle(pos, isFwd)
			if _, dup := seen[h]; dup {
				continue
			}
			seen[h] = struct{}{}
			fwdPos, reversed := op.resolveFwdPos(aspPredEntry{rawPos: pos, fwd: isFwd})
			out = append(out, scanArc{neighbour: uint64(edges[pos]), handle: h, fwdPos: fwdPos, reversed: reversed})
		}
	}
	scan(true)
	if op.revVerts != nil {
		scan(false)
	}
	return out
}

// hopForTraversal mirrors [ShortestPath.hopForTraversal]: the hydration-correct
// resolution of a from->to traversal of the edge with handle h.
func (op *AllShortestPaths) hopForTraversal(from, to, h uint64) hop {
	if from+1 < uint64(len(op.fwdVerts)) {
		for pos := op.fwdVerts[from]; pos < op.fwdVerts[from+1]; pos++ {
			if uint64(op.fwdEdges[pos]) == to && op.relHandle(pos, true) == h {
				return hop{dstID: to, fwdPos: pos, reversed: false}
			}
		}
	}
	if to+1 < uint64(len(op.fwdVerts)) {
		for pos := op.fwdVerts[to]; pos < op.fwdVerts[to+1]; pos++ {
			if uint64(op.fwdEdges[pos]) == from && op.relHandle(pos, true) == h {
				return hop{dstID: to, fwdPos: pos, reversed: true}
			}
		}
	}
	return hop{dstID: to, fwdPos: h, reversed: false}
}

// cycleStep is one node+handle step of a reconstructed undirected cycle, used by
// the all-shortest enumeration before each step is resolved into a
// hydration-correct hop.
type cycleStep struct {
	from, to uint64
	handle   uint64
}

// bfsAllShortestCycleBranch enumerates EVERY shortest undirected edge-simple
// cycle through src (#1785), the all-shortest companion to
// [ShortestPath.bfsShortestCycleBranch]. It runs a multi-predecessor
// branch-collision BFS: each node records every shortest BFS-tree arm reaching
// it (apreds) and the SET of branch tags those arms carry. A non-tree arc
// e=(u,v) with a branch tag on the u-side different from one on the v-side
// witnesses a cycle of length dist[u]+dist[v]+1. The minimum witnessed length
// L* fixes the cycle length; every witness at L* is expanded into all shortest
// src->u and src->v arms whose tags differ, and each (arm1, e, reverse(arm2))
// triple is emitted iff the two arms are NODE- and EDGE-disjoint (the
// single-predecessor branch guarantee does not survive a multi-pred DAG, so
// disjointness is checked explicitly). Results are de-duplicated by the ordered
// handle sequence so a cycle produced twice by different arm routings collapses,
// while the two opposite traversal orientations of one undirected cycle — which
// have distinct ordered sequences — are correctly kept as distinct paths.
func (op *AllShortestPaths) bfsAllShortestCycleBranch(src uint64) ([]expr.ListValue, error) {
	dist := map[uint64]int{src: 0}
	apreds := map[uint64][]branchPred{}            // all shortest tree predecessors per node
	branchTags := map[uint64]map[uint64]struct{}{} // branch tag set per node

	tagsOf := func(node uint64) map[uint64]struct{} {
		if node == src {
			return map[uint64]struct{}{srcBranchSentinel: {}}
		}
		return branchTags[node]
	}
	disjointTags := func(a, b uint64) bool {
		ta, tb := tagsOf(a), tagsOf(b)
		// True iff there exists a tag in ta and a tag in tb that differ — i.e.
		// the witness can be closed by SOME pair of differently-tagged arms.
		for x := range ta {
			for y := range tb {
				if x != y {
					return true
				}
			}
		}
		return false
	}

	// Witnesses, self-loops and distinct-parallel pairs collected at their level.
	type witness struct {
		u, v uint64
		arc  scanArc
		l    int
	}
	var witnesses []witness
	type loopW struct{ arc scanArc }
	var selfLoops []loopW
	type parW struct{ first, second scanArc }
	var parallels []parW

	bestLen := -1
	consider := func(w witness) {
		if bestLen == -1 || w.l < bestLen {
			bestLen = w.l
		}
		witnesses = append(witnesses, w)
	}

	// Level-1 seeding from src.
	seedSeen := map[uint64]struct{}{}
	firstArcTo := map[uint64]scanArc{}
	var frontier []uint64
	for _, arc := range op.branchArcs(src, seedSeen) {
		if arc.neighbour == src {
			selfLoops = append(selfLoops, loopW{arc: arc})
			if bestLen == -1 || 1 < bestLen {
				bestLen = 1
			}
			continue
		}
		if prev, ok := firstArcTo[arc.neighbour]; ok {
			parallels = append(parallels, parW{first: prev, second: arc})
			if bestLen == -1 || 2 < bestLen {
				bestLen = 2
			}
			continue
		}
		firstArcTo[arc.neighbour] = arc
		dist[arc.neighbour] = 1
		branchTags[arc.neighbour] = map[uint64]struct{}{arc.handle: {}}
		apreds[arc.neighbour] = []branchPred{{parent: src, arc: arc}}
		frontier = append(frontier, arc.neighbour)
	}

	// Level >= 1 BFS, recording all shortest arms and every branch collision.
	for level := 1; len(frontier) > 0; level++ {
		if err := op.ctx.Err(); err != nil {
			return nil, err
		}
		if op.maxHops != shortestNoMaxHops && level >= op.maxHops {
			break
		}
		var next []uint64
		discovered := map[uint64]struct{}{}
		for _, u := range frontier {
			seen := map[uint64]struct{}{}
			for _, arc := range op.branchArcs(u, seen) {
				v := arc.neighbour
				vDist, disc := dist[v]
				if !disc {
					// First discovery of v at level+1.
					dist[v] = level + 1
					branchTags[v] = map[uint64]struct{}{}
					for t := range tagsOf(u) {
						branchTags[v][t] = struct{}{}
					}
					apreds[v] = []branchPred{{parent: u, arc: arc}}
					discovered[v] = struct{}{}
					next = append(next, v)
					continue
				}
				if vDist == level+1 {
					// Another shortest tree arm to v at the same level: merge.
					apreds[v] = append(apreds[v], branchPred{parent: u, arc: arc})
					for t := range tagsOf(u) {
						branchTags[v][t] = struct{}{}
					}
					continue
				}
				if v == src {
					continue // closing arc into src: covered by seeding cases
				}
				if vDist == 0 {
					continue
				}
				// Non-tree arc u->v (v at level <= u). Branch collision witness.
				if disjointTags(u, v) {
					consider(witness{u: u, v: v, arc: arc, l: dist[u] + dist[v] + 1})
				}
			}
		}
		// Stop once no shorter cycle can still appear.
		if bestLen != -1 && 2*(level+1)-1 >= bestLen {
			break
		}
		frontier = next
	}

	if bestLen == -1 || bestLen < op.minHops {
		return nil, nil
	}

	// Assemble all shortest cycles of length bestLen, de-duplicated by ordered
	// handle sequence.
	var out []expr.ListValue
	seenSeq := map[string]struct{}{}
	emit := func(steps []cycleStep) {
		key := cycleKey(steps)
		if _, dup := seenSeq[key]; dup {
			return
		}
		seenSeq[key] = struct{}{}
		hops := make([]hop, len(steps))
		for i, s := range steps {
			hops[i] = op.hopForTraversal(s.from, s.to, s.handle)
		}
		out = append(out, buildHopList(src, hops))
	}

	const ctxCheckEvery = 1024
	iter := 0
	checkCtx := func() error {
		iter++
		if iter&(ctxCheckEvery-1) == 0 {
			return op.ctx.Err()
		}
		return nil
	}

	switch bestLen {
	case 1:
		for _, sl := range selfLoops {
			if err := checkCtx(); err != nil {
				return nil, err
			}
			emit([]cycleStep{{from: src, to: src, handle: sl.arc.handle}})
		}
	case 2:
		for _, p := range parallels {
			if err := checkCtx(); err != nil {
				return nil, err
			}
			m := p.first.neighbour
			// Both orientations: src-(h1)-m-(h2)-src and src-(h2)-m-(h1)-src.
			emit([]cycleStep{{src, m, p.first.handle}, {m, src, p.second.handle}})
			emit([]cycleStep{{src, m, p.second.handle}, {m, src, p.first.handle}})
		}
	default:
		for _, w := range witnesses {
			if w.l != bestLen {
				continue
			}
			armsU := op.enumerateArms(apreds, src, w.u)
			armsV := op.enumerateArms(apreds, src, w.v)
			for _, au := range armsU {
				for _, av := range armsV {
					if err := checkCtx(); err != nil {
						return nil, err
					}
					steps, ok := assembleCycleSteps(au, av, w)
					if !ok {
						continue
					}
					emit(steps)
				}
			}
		}
	}
	return out, nil
}

// arm is one shortest src->endpoint path as an ordered node+handle sequence
// (src-forward order), plus the sets used to test cycle disjointness.
type arm struct {
	steps   []cycleStep         // src -> … -> endpoint
	nodes   map[uint64]struct{} // intermediate + endpoint nodes (src excluded)
	handles map[uint64]struct{} // edge handles on the arm
}

// enumerateArms returns every shortest src->target path through the
// multi-predecessor DAG as a node+handle sequence in src-forward order.
func (op *AllShortestPaths) enumerateArms(apreds map[uint64][]branchPred, src, target uint64) []arm {
	if target == src {
		return []arm{{nodes: map[uint64]struct{}{}, handles: map[uint64]struct{}{}}}
	}
	type frame struct {
		node  uint64
		steps []cycleStep // collected child->parent order (reversed on completion)
	}
	var arms []arm
	stack := []frame{{node: target}}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if top.node == src {
			// Reverse child->parent order into src-forward order.
			n := len(top.steps)
			steps := make([]cycleStep, n)
			nodes := make(map[uint64]struct{}, n)
			handles := make(map[uint64]struct{}, n)
			for i := 0; i < n; i++ {
				s := top.steps[n-1-i]
				steps[i] = s
				if s.to != src {
					nodes[s.to] = struct{}{}
				}
				handles[s.handle] = struct{}{}
			}
			arms = append(arms, arm{steps: steps, nodes: nodes, handles: handles})
			continue
		}
		for _, pe := range apreds[top.node] {
			ns := make([]cycleStep, len(top.steps)+1)
			copy(ns, top.steps)
			// Tree arc parent->child; the arm step is parent->child (from src side).
			ns[len(top.steps)] = cycleStep{from: pe.parent, to: top.node, handle: pe.arc.handle}
			stack = append(stack, frame{node: pe.parent, steps: ns})
		}
	}
	return arms
}

// assembleCycleSteps joins arm1 (src->u), the witness edge u->v, and the
// reverse of arm2 (src->v traversed v->src) into one ordered cycle step
// sequence, returning ok=false when the two arms are not node/edge-disjoint or
// when the joining edge is already on an arm (so the cycle would not be
// edge-simple).
func assembleCycleSteps(au, av arm, w struct {
	u, v uint64
	arc  scanArc
	l    int
}) ([]cycleStep, bool) {
	// Arms must share no intermediate node.
	for n := range au.nodes {
		if n == w.u {
			continue // u is au's endpoint, shared only there
		}
		if _, ok := av.nodes[n]; ok {
			return nil, false
		}
	}
	// v must not lie on arm1 (other than as a node strictly inside), and u must
	// not lie on arm2.
	if _, ok := au.nodes[w.v]; ok {
		return nil, false
	}
	if _, ok := av.nodes[w.u]; ok {
		return nil, false
	}
	// Arms must share no edge, and neither may contain the joining edge.
	for h := range au.handles {
		if _, ok := av.handles[h]; ok {
			return nil, false
		}
	}
	if _, ok := au.handles[w.arc.handle]; ok {
		return nil, false
	}
	if _, ok := av.handles[w.arc.handle]; ok {
		return nil, false
	}

	steps := make([]cycleStep, 0, len(au.steps)+1+len(av.steps))
	steps = append(steps, au.steps...)
	steps = append(steps, cycleStep{from: w.u, to: w.v, handle: w.arc.handle})
	// Arm 2 reversed: walk av.steps from endpoint back toward src (v -> … -> src).
	for i := len(av.steps) - 1; i >= 0; i-- {
		s := av.steps[i]
		steps = append(steps, cycleStep{from: s.to, to: s.from, handle: s.handle})
	}
	return steps, true
}

// cycleKey is the canonical de-dup key for an ordered cycle: the ordered handle
// sequence. Two opposite traversal orientations have distinct sequences and are
// kept distinct; the same oriented cycle produced by different routings collapses.
func cycleKey(steps []cycleStep) string {
	b := make([]byte, 0, len(steps)*8)
	for _, s := range steps {
		h := s.handle
		for i := 0; i < 8; i++ {
			b = append(b, byte(h))
			h >>= 8
		}
	}
	return string(b)
}

// relHandle mirrors [ShortestPath.relHandle]: the stable per-edge identity used
// for the cycle edge-simplicity set.
func (op *AllShortestPaths) relHandle(pos uint64, isFwd bool) uint64 {
	if isFwd {
		if op.fwdHandles != nil && pos < uint64(len(op.fwdHandles)) {
			return op.fwdHandles[pos]
		}
		return pos
	}
	if op.revHandles != nil && pos < uint64(len(op.revHandles)) {
		return op.revHandles[pos]
	}
	return op.resolvedFwdPosOrSelf(pos)
}

// containsRel reports whether relationship id r is already present in rs.
func containsRel(rs []uint64, r uint64) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
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
