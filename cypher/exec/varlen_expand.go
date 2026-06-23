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
// # Safety caps
//
// Two edge-traversal caps bound the work. The PER-INPUT-ROW cap
// (maxEdgesTraversed, default 1,000,000, reset for each source row) bounds the
// expansion from a single anchor and prevents runaway queries on dense graphs.
// The AGGREGATE PER-QUERY cap (maxTotalEdgesTraversed, default 100,000,000, NOT
// reset per row) bounds the sum across every input row, so a query that first
// produces M source rows and then expands from each cannot multiply the per-row
// budget by M. Exceeding either returns [ErrVarLenCapExceeded]. In addition, an
// omitted upper bound (-[*]-) is given a finite default hop ceiling
// ([defaultMaxUnboundedHops]) rather than math.MaxInt.
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
	"math"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ErrVarLenCapExceeded is returned when a VarLengthExpand exceeds its
// configured maximum edge traversal count — either the per-input-row cap or the
// aggregate per-query cap (see [defaultMaxEdgesTraversed] and
// [defaultMaxTotalEdgesTraversed]).
var ErrVarLenCapExceeded = errors.New("exec: variable-length expand safety cap exceeded")

// defaultMaxEdgesTraversed is the default PER-INPUT-ROW safety cap for
// VarLengthExpand. It bounds the edge traversals performed while expanding from
// a single anchor row.
const defaultMaxEdgesTraversed = 1_000_000

// defaultMaxTotalEdgesTraversed is the default AGGREGATE PER-QUERY safety cap
// for VarLengthExpand: the sum of edge traversals across every input row that
// drives the operator. The per-row cap alone does not bound a query that first
// produces M source rows and then expands a variable-length pattern from each
// (e.g. MATCH (a),(b) MATCH (a)-[*]->() ...), because the per-row counter is
// reset for every input row — the aggregate work is then up to M ×
// [defaultMaxEdgesTraversed]. This counter is NOT reset per row; once the
// running total exceeds the cap the operator returns [ErrVarLenCapExceeded]
// (#1478). The default (100,000,000) is two orders of magnitude above the
// per-row cap, so a single multi-row traversal is not rejected, yet it removes
// the unbounded multiplication an attacker could drive by inflating source
// cardinality. Every openCypher TCK variable-length pattern runs on a tiny
// graph far below this bound, so no conforming query trips it.
const defaultMaxTotalEdgesTraversed = 100_000_000

// defaultMaxUnboundedHops is the default upper hop bound applied when a
// variable-length pattern omits its upper bound (-[*]-, -[*1..]-, -[*..]-). The
// IR encodes "unbounded" as math.MaxInt (see cypher/ir/match.go); leaving the
// operator with that value means the only depth bound is the no-repeated-edge
// rule plus the edge-traversal caps. This finite default is a defence-in-depth
// hop ceiling that bounds per-path memory (the edge slice grows with the hop
// count). At 65,536 it is astronomically above any legitimate path length and
// far above the longest path any TCK graph can produce, so no conforming query
// regresses (#1478).
const defaultMaxUnboundedHops = 65_536

// VLEHopStride is the per-hop element count of the flat alternating path list
// that [VarLengthExpand] emits. The list is
//
//	[srcNodeID, fwdPos0, dstNode0, dir0, fwdPos1, dstNode1, dir1, …]
//
// so an N-hop path occupies 1 + VLEHopStride*N elements: the leading source
// node id, then one (forward edge position, destination node id, direction
// marker) triple per hop. The direction marker is [VLEDirForward] or
// [VLEDirReverse]. Readers in the cypher package (the path/relationship-list
// hydrators) and [appendExcludedFromValue] index the list with this stride; it
// is exported so those readers share the single source of truth (rmp #1685).
//
// Before #1685 the stride was 2 ((edgePos, dst) pairs) and the edge position
// was synthetic for a reverse hop, which lost both the physical-edge identity
// and the traversal direction the hydrator needs for per-instance type and
// property reporting on multigraph parallel edges.
const VLEHopStride = 3

// VLEDirForward and VLEDirReverse are the direction-marker values stored at the
// third slot of each hop triple in the [VarLengthExpand] flat path list
// (stride [VLEHopStride]). A reverse marker tells the relationship hydrator to
// swap the storage endpoints when resolving the edge's per-instance type and
// properties, and tells the path renderer to emit `<-[…]-` (rmp #1685).
const (
	VLEDirForward = 0
	VLEDirReverse = 1
)

// edgeStep is one hop in a BFS path.
//
// fwdPos is the FORWARD-CSR position of the physical edge this hop traverses —
// the handle-disambiguated per-instance position, NOT a synthetic reverse
// position. Both a forward hop and a reverse hop over the same physical edge
// record the SAME fwdPos, so the hop is decodable by the relationship hydrator
// ([cypher].edgeHandleAtFwdPos), which keys off a forward position to recover
// the stable per-edge handle (rmp #1685). reversed marks whether this hop was
// taken against storage direction (an undirected/reverse traversal of a
// stored src->dst edge): the path renderer needs it to emit `<-[…]-`, and the
// hydrator needs it to swap the storage endpoints when resolving the edge's
// per-instance type and properties.
//
// Before #1685 this field held the absolute position (forward position for a
// forward hop, but a SYNTHETIC base+pos for a reverse hop) which overloaded
// physical-edge identity and direction into one integer the hydrator could not
// decode — so a reverse hop over a multigraph parallel edge collapsed onto the
// first forward slot's type and the coalesced per-pair property union.
type edgeStep struct {
	fwdPos   uint64 // forward-CSR position of the physical edge (handle-disambiguated)
	dstID    uint64 // destination node ID
	reversed bool   // true when traversed against storage direction (reverse/undirected hop)
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
	edgeTypeFilter         map[uint64]string
	inputCol               int
	minHops                int
	maxHops                int
	maxEdgesTraversed      int // per-input-row cap
	maxTotalEdgesTraversed int // aggregate per-query cap (NOT reset per row)
	excludedRelCols        []int

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// CSR snapshots (valid after Init).
	fwdVerts   []uint64
	fwdEdges   []graph.NodeID
	fwdHandles []uint64 // forward per-slot stable handles (nil unless a multigraph snapshot)
	revVerts   []uint64
	revEdges   []graph.NodeID
	revHandles []uint64 // reverse per-slot stable handles (nil for DirOut / non-multigraph)

	// revToFwd maps a reverse-CSR edge position to its corresponding
	// forward-CSR edge position. Used by the relationship-uniqueness
	// bitset to recognise that a reverse traversal of the same physical
	// edge is NOT a distinct edge, and (rmp #1685) to record the FORWARD
	// position on each reverse hop so the relationship hydrator can recover
	// the edge's stable handle and report its per-instance type/properties.
	// Built lazily in Init for ANY reverse-traversing direction — DirIn as
	// well as DirBoth (rmp #1689/D2), since a pure-reverse hop needs the same
	// forward position for per-instance hydration and type filtering. Entry
	// ^uint64(0) means "unresolved" (e.g. out-of-range vertex IDs); callers
	// fall back to the synthetic reverse absPos in that rare case.
	//
	// For PARALLEL edges (a multigraph dst->src pair), the mapping is keyed
	// on the stable per-edge handle when both CSRs carry handles
	// ([csr.BuildReverse] keeps one handle per logical edge across both
	// directions), so the k-th reverse slot pairs with the forward slot of
	// the SAME physical edge — delete-stable and instance-exact. Without
	// handles it falls back to a positional-ordinal pairing (the n-th
	// reverse dst->src slot pairs with the n-th forward src->dst... actually
	// dst->src slot). The handle path mirrors [Expand.lookupFwdEdgePosByHandle]
	// (the single-hop #1634 fix); the ordinal fallback mirrors its pre-#1634
	// behaviour for non-multigraph snapshots.
	revToFwd []uint64

	// BFS state for the current input row. Two slices are kept and ping-ponged
	// per BFS level: `queue` is read by runBFS while extensions are appended to
	// `nextQueue`. After each level the two are swapped. This avoids the
	// slice-aliasing hazard that arises if the same backing array is both read
	// and written in the same call.
	queue             []pathState // current BFS frontier (read)
	nextQueue         []pathState // next BFS frontier (write target during expansion)
	inputRow          Row         // current outer input row (stable copy)
	inputEOS          bool        // true after input plan exhausted
	edgesVisited      int         // traversal counter for the per-row safety cap (reset per input row)
	totalEdgesVisited int         // traversal counter for the aggregate per-query cap (reset once per Init)

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
	// MaxEdgesTraversed is the safety cap on edge traversals per input row.
	// Defaults to [defaultMaxEdgesTraversed] (1,000,000) when 0.
	MaxEdgesTraversed int
	// MaxTotalEdgesTraversed is the aggregate safety cap on edge traversals
	// across all input rows for the whole query — it is NOT reset per row, so it
	// bounds the M × (per-row cost) multiplication that the per-row cap alone
	// cannot (#1478). Defaults to [defaultMaxTotalEdgesTraversed] (100,000,000)
	// when 0.
	MaxTotalEdgesTraversed int
	// ExcludedRelCols lists column indices in the input row holding edge
	// identifiers (IntegerValue or RelationshipValue) that must not be
	// traversed inside this VLE step. Implements the openCypher
	// no-repeated-relationships rule across distinct rel patterns within
	// the same MATCH (e.g. `MATCH ()-[r:EDGE]-() MATCH (n)-[*0..1]-()-[r]
	// -()-[*0..1]-(m)` — the two variable-length steps must not reuse the
	// edge bound to `r`). The visited bitset is pre-populated with each
	// listed column's edge position at BFS seed time.
	ExcludedRelCols []int
}

// NewVarLengthExpand creates a VarLengthExpand operator. cfg is read-only and
// taken by pointer to avoid copying the configuration struct on this hot path.
func NewVarLengthExpand(input Operator, fwd, rev csrAdjacency, cfg *VarLengthConfig) *VarLengthExpand {
	dir := cfg.Direction
	if dir == 0 {
		dir = DirOut
	}
	capVal := cfg.MaxEdgesTraversed
	if capVal <= 0 {
		capVal = defaultMaxEdgesTraversed
	}
	totalCap := cfg.MaxTotalEdgesTraversed
	if totalCap <= 0 {
		totalCap = defaultMaxTotalEdgesTraversed
	}
	// Apply a finite default hop ceiling when the pattern is unbounded
	// (MaxHops == math.MaxInt, as the IR encodes -[*]-, -[*1..]-, -[*..]-).
	// This bounds per-path memory and adds defence-in-depth on top of the
	// edge-traversal caps without affecting any bounded pattern (#1478).
	maxHops := cfg.MaxHops
	if maxHops == math.MaxInt {
		maxHops = defaultMaxUnboundedHops
	}
	return &VarLengthExpand{
		input:                  input,
		fwd:                    fwd,
		rev:                    rev,
		dir:                    dir,
		edgeType:               cfg.EdgeType,
		edgeTypeFilter:         cfg.EdgeTypeFilter,
		inputCol:               cfg.InputCol,
		minHops:                cfg.MinHops,
		maxHops:                maxHops,
		maxEdgesTraversed:      capVal,
		maxTotalEdgesTraversed: totalCap,
		excludedRelCols:        append([]int(nil), cfg.ExcludedRelCols...),
	}
}

// Init initialises the operator.
func (op *VarLengthExpand) Init(ctx context.Context) error {
	op.ctx = ctx
	op.fwdVerts = op.fwd.VerticesSlice()
	op.fwdEdges = op.fwd.EdgesSlice()
	op.fwdHandles = op.fwd.HandlesSlice()
	if op.dir != DirOut && op.rev != nil {
		op.revVerts = op.rev.VerticesSlice()
		op.revEdges = op.rev.EdgesSlice()
		op.revHandles = op.rev.HandlesSlice()
	}
	op.queue = op.queue[:0]
	op.nextQueue = op.nextQueue[:0]
	op.results = op.results[:0]
	op.resultIdx = 0
	op.inputRow = nil
	op.inputEOS = false
	op.edgesVisited = 0
	// The aggregate per-query counter is reset exactly once per operator run
	// (here in Init), NEVER at the per-input-row reset below, so it accumulates
	// across every source row (#1478).
	op.totalEdgesVisited = 0
	// Precompute a reverse-edge-position → forward-edge-position mapping
	// so the relationship-uniqueness bitset can dedupe the same physical
	// edge across direction. For each reverse edge (b←a) in revEdges,
	// scan a's forward adjacency for the matching destination b. Without
	// this, DirBoth VLE traversal can use the same edge twice (once
	// forward, once reverse encoding) and produce duplicated paths
	// (Match9 [3]/[4]).
	//
	// Built for ANY direction that traverses reverse edges (DirIn and
	// DirBoth), not DirBoth alone: a pure-reverse hop (DirIn, the `<-[*]-`
	// pattern) also needs each reverse slot mapped to its handle-disambiguated
	// FORWARD position so the relationship hydrator recovers the edge's stable
	// handle and reports its per-instance type and properties — without it a
	// DirIn hop over a multigraph parallel edge recorded a synthetic reverse
	// position and collapsed onto the coalesced per-pair type (rmp #1689/D2).
	if op.dir != DirOut && op.revEdges != nil {
		op.buildRevToFwd()
	}
	return op.input.Init(ctx)
}

// buildRevToFwd fills op.revToFwd, mapping each reverse-CSR edge position to the
// forward-CSR position of the SAME physical edge. A reverse slot revPos in
// revUID's adjacency encodes an edge whose stored direction is
// (fwdSrc -> revUID), where fwdSrc = revEdges[revPos]; its forward counterpart
// lives in fwdSrc's forward adjacency at the position whose destination is
// revUID. The reverse CSR is the transpose of the forward CSR, so the mapping
// is a bijection per logical edge. Entry ^uint64(0) marks "unresolved" (an
// out-of-range vertex or a missing forward counterpart); callers fall back to
// the synthetic reverse position in that rare case.
//
// The disambiguation strategy (handle-exact, with a positional-ordinal
// fallback for simple/legacy graphs) lives in the package-level [buildRevToFwd]
// free function, which [ShortestPath] and [AllShortestPaths] share so the
// per-instance mapping is computed identically across all three operators
// (rmp #1692). This method is a thin adapter over the operator's CSR snapshots.
func (op *VarLengthExpand) buildRevToFwd() {
	op.revToFwd = buildRevToFwd(
		op.fwdVerts, op.fwdEdges, op.fwdHandles,
		op.revVerts, op.revEdges, op.revHandles,
	)
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

		// Seed BFS from the source node. The input column may carry either
		// the raw IntegerValue NodeID (Expand-emitted, the in-pipeline
		// encoding) or an upgraded NodeValue (after a WITH projection has
		// run through buildRowCtx's upgrade pass). Accept both shapes so
		// patterns like `MATCH (a) WITH a AS x MATCH (x)-[*]->(...)` work
		// — the WITH stores `x` as a NodeValue in the row, and the
		// downstream VLE was silently skipping every such row.
		if op.inputCol >= len(cp) {
			continue // row too narrow; skip
		}
		var srcID uint64
		switch v := cp[op.inputCol].(type) {
		case expr.IntegerValue:
			srcID = uint64(v)
		case expr.NodeValue:
			srcID = v.ID
		default:
			continue // unsupported source-column type
		}

		// Reset per-source state.
		op.queue = op.queue[:0]
		op.nextQueue = op.nextQueue[:0]
		op.results = op.results[:0]
		op.resultIdx = 0
		op.edgesVisited = 0

		// Compute the per-row initial visited bitset from any
		// excluded-rel columns. Edge identifiers carried in those
		// columns (IntegerValue raw edge position or
		// RelationshipValue.ID) are added to the visited set so the
		// BFS cannot traverse them — closing the openCypher
		// no-repeated-relationships rule across distinct rel patterns
		// within the same MATCH (Match4 [7] / Match5 [27]).
		var initialVisited []uint64
		for _, exCol := range op.excludedRelCols {
			if exCol < 0 || exCol >= len(cp) {
				continue
			}
			initialVisited = appendExcludedFromValue(initialVisited, cp[exCol])
		}

		// If minHops == 0, the source node itself is a valid result.
		if op.minHops == 0 {
			op.results = append(op.results, pathState{
				hops:    0,
				srcNode: srcID,
				path:    nil,
				visited: initialVisited,
			})
		}

		// Seed BFS queue with each neighbour of srcID (hop 1).
		if op.maxHops > 0 {
			if err := op.seedQueueWithVisited(srcID, initialVisited); err != nil {
				return false, err
			}
		}
	}
}

// appendExcludedFromValue extracts edge identifiers from v and appends
// each to the bitset. Handles the three shapes a relationship-bearing
// row column can carry: IntegerValue (raw edge position from Expand),
// RelationshipValue (canonical projection form), and ListValue (the
// alternating-flat representation a sibling VLE emits for the path's
// edges). For the flat list the edge positions are the FORWARD positions
// at the first slot of each hop triple (stride [VLEHopStride]); keying the
// exclusion on the forward position is strictly stronger for the
// no-repeated-relationships rule, because it rejects the same physical edge
// regardless of the direction the sibling VLE traversed it (rmp #1685).
// Other types are silently ignored so the exclusion stays conservative.
func appendExcludedFromValue(visited []uint64, v expr.Value) []uint64 {
	switch t := v.(type) {
	case expr.IntegerValue:
		return bitsetAdd(visited, uint64(t))
	case expr.RelationshipValue:
		return bitsetAdd(visited, t.ID)
	case expr.ListValue:
		// VLE flat encoding:
		// [srcNode, fwdPos0, dstNode0, dir0, fwdPos1, dstNode1, dir1, …].
		// The forward edge position is at index 1+VLEHopStride*h.
		for i := 1; i < len(t); i += VLEHopStride {
			switch x := t[i].(type) {
			case expr.IntegerValue:
				visited = bitsetAdd(visited, uint64(x))
			case expr.RelationshipValue:
				visited = bitsetAdd(visited, x.ID)
			}
		}
	}
	return visited
}

// seedQueueWithVisited enqueues all one-hop neighbours of srcID into the BFS
// frontier (op.queue), which at this point is empty. It returns an error if the
// safety cap is exceeded during seeding. It pre-loads the per-path visited
// bitset from an excluded-edge set (sibling bound rel vars in the same MATCH
// pattern). The seeded paths' visited bitset inherits the initial set so the
// corresponding edges are unreachable.
func (op *VarLengthExpand) seedQueueWithVisited(srcID uint64, initialVisited []uint64) error {
	// Build a synthetic parent state when initialVisited is non-empty so
	// enqueueEdges' bitsetContains check rejects excluded edges at hop 1
	// already.
	var parent *pathState
	if len(initialVisited) > 0 {
		parent = &pathState{visited: initialVisited}
	}
	// Forward edges.
	if op.dir != DirIn {
		if err := op.enqueueEdges(srcID, true, parent, &op.queue); err != nil {
			return err
		}
	}
	// Reverse edges.
	if op.dir != DirOut && op.revVerts != nil {
		if err := op.enqueueEdges(srcID, false, parent, &op.queue); err != nil {
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
		op.totalEdgesVisited++
		// Per-input-row cap and aggregate per-query cap. The latter is not reset
		// per row, so it bounds the M × (per-row cost) multiplication that an
		// attacker could otherwise drive by inflating source cardinality (#1478).
		if op.edgesVisited > op.maxEdgesTraversed || op.totalEdgesVisited > op.maxTotalEdgesTraversed {
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

		// Alias the reverse-edge synthetic position to its forward
		// counterpart. On undirected (DirBoth) traversal this lets the
		// relationship-uniqueness bitset reject a path that uses the SAME
		// physical edge twice (once forward, once reverse) — see Match9
		// [3]/[4]; without it the BFS would emit `(a)-[REL1]->(b)-[REL1
		// reverse]->(a)` as a length-2 path that "reuses" the only edge
		// between a and b. On pure-reverse traversal (DirIn) the same remap
		// is what lets the relationship hydrator recover the edge's stable
		// handle from a forward position and report its per-instance type and
		// properties (rmp #1689/D2) — so the alias runs for ANY direction that
		// traverses reverse edges, not DirBoth alone.
		fwdAbsPos := absPos
		if !isFwd && op.dir != DirOut && op.revToFwd != nil && pos < uint64(len(op.revToFwd)) {
			if mapped := op.revToFwd[pos]; mapped != unresolvedFwdPos {
				fwdAbsPos = mapped
			}
		}

		// Edge-type filter (forward only; reverse edges skip type filter).
		// MEMBERSHIP, not equality: op.edgeTypeFilter is a presence-SET built
		// by [buildEdgeTypeFilter] holding exactly the positions whose edge
		// carries one of the pattern's declared types, so a relationship-type
		// disjunction -[:A|B*]- accepts an edge of EITHER type. Comparing the
		// looked-up label against the single op.edgeType (= RelTypes[0]) here
		// silently dropped every edge of a non-first declared type, even on a
		// simple graph (rmp #1688/D3); the presence test mirrors
		// [Expand.passesTypeFilter]. op.edgeType stays as the "a filter was
		// requested" gate, set in lockstep with op.edgeTypeFilter.
		if isFwd && op.edgeType != "" {
			if _, ok := op.edgeTypeFilter[absPos]; !ok {
				continue
			}
		}
		// Edge-type filter for reverse edges: look up by the forward
		// counterpart position. Without this, reverse traversal would
		// emit any-type edges even when the pattern declared a type
		// filter, leading Match9 [3]/[4] to enumerate paths through
		// edges that should have been filtered out. Same membership test
		// as the forward branch (rmp #1688/D3). Applies to ANY reverse-
		// traversing direction (DirIn pure-reverse as well as DirBoth) so
		// `(a)<-[:T*]-(b)` filters on type rather than traversing every
		// incoming edge regardless of type (rmp #1689/D2). The fwdAbsPos !=
		// absPos guard keeps the unresolved-remap fallback (a rare
		// out-of-range vertex) permissive, matching the prior DirBoth path.
		if !isFwd && op.edgeType != "" && op.dir != DirOut && fwdAbsPos != absPos {
			if _, ok := op.edgeTypeFilter[fwdAbsPos]; !ok {
				continue
			}
		}

		// Relationship-uniqueness: skip if this edge is already on the path.
		// Key the bitset on the FORWARD position so a reverse-direction
		// traversal of the same edge is recognised as the same edge.
		if parent != nil && bitsetContains(parent.visited, fwdAbsPos) {
			continue
		}

		// Build new path state. The path step records the FORWARD position
		// (fwdAbsPos) of the physical edge plus a direction marker
		// (reversed). The relationship hydrator recovers the edge's stable
		// handle from this forward position to report its per-instance type
		// and properties, and the renderer uses reversed to emit `<-[…]-`
		// for an undirected/reverse hop (rmp #1685). The visited bitset
		// keys on the same fwdAbsPos so a later traversal of the same edge
		// in the opposite direction is still rejected by
		// relationship-uniqueness.
		reversed := !isFwd
		var newPath []edgeStep
		var newVisited []uint64
		hops := 1
		if parent != nil {
			hops = parent.hops + 1
			newPath = make([]edgeStep, len(parent.path)+1)
			copy(newPath, parent.path)
			newPath[len(parent.path)] = edgeStep{fwdPos: fwdAbsPos, dstID: dst, reversed: reversed}
			newVisited = bitsetAdd(parent.visited, fwdAbsPos)
		} else {
			newPath = []edgeStep{{fwdPos: fwdAbsPos, dstID: dst, reversed: reversed}}
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
// pathList is a flat alternating ListValue encoding the full path at stride
// [VLEHopStride]:
//
//	[srcNodeID, fwdPos0, dstNode0, dir0, fwdPos1, dstNode1, dir1, ..., dstNodeN, dirN]
//
// For an N-hop path the list has 1 + VLEHopStride*N elements (srcNode, then N
// triples of (forward edge position, dstNode, direction marker)). For a
// zero-hop path the list is [srcNodeID] (1 element). fwdPosH is the FORWARD-CSR
// position of hop H's physical edge (handle-disambiguated) and dirH is
// [VLEDirForward] or [VLEDirReverse] (rmp #1685).
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

	// Build flat alternating list:
	// [srcID, fwdPos0, dst0, dir0, fwdPos1, dst1, dir1, ...].
	pathList := make(expr.ListValue, 1+VLEHopStride*len(ps.path))
	pathList[0] = srcID
	for i, step := range ps.path {
		dir := VLEDirForward
		if step.reversed {
			dir = VLEDirReverse
		}
		pathList[1+VLEHopStride*i] = expr.IntegerValue(int64(step.fwdPos))
		pathList[2+VLEHopStride*i] = expr.IntegerValue(int64(step.dstID))
		pathList[3+VLEHopStride*i] = expr.IntegerValue(int64(dir))
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
