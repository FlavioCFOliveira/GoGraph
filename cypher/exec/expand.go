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
// Row mode: ctx.Err() is checked at the top of EVERY iteration of Next's loop,
// so cancellation is observed within one neighbour step.
//
// Chunk mode: fillChunk checks on entry and then whenever its per-call row
// counter is a multiple of 4096. Every caller passes maxRows <= the chunk
// capacity (4096), so in practice that is the entry check, and the latency bound
// is one chunk.
//
// This paragraph used to describe a cadence of "every 4096 emitted rows" driven
// by an emitCount field. That field was written and reset but NEVER READ, and the
// helper that incremented it carried a comment claiming it checked cancellation,
// which it did not. The code was SAFER than documented; the dead state and the
// false description are both removed (rmp #2261).

import (
	"context"
	"sync/atomic"

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

// CSRAdjacency is the minimal interface required from a CSR snapshot.
// csr.CSR[W] satisfies this interface for any W.
//
// It is exported so a query planner in another package can supply an
// [AdjacencySource] that resolves one at execution time (rmp #2317).
type CSRAdjacency interface {
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
	// src yields the adjacency to expand over, resolved in Init rather than held
	// from plan-build time. See [AdjacencySource].
	src AdjacencySource
	fwd CSRAdjacency // forward adjacency for THIS Init; resolved from src
	rev CSRAdjacency // reverse adjacency for THIS Init; nil for DirOut

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// admit is the slot-aligned relationship-type admission view for THIS Init,
	// resolved from src together with the adjacency it is keyed to. nil = no type
	// filtering was resolved. See [RelTypeAdmit] (rmp #2251); it replaced a
	// map[uint64]string probed once per CSR slot in each direction.
	admit        RelTypeAdmit
	multiplicity func(srcID, dstID uint64) int64 // per-edge CREATE multiplicity; nil = single-row emit

	edgeType string // optional edge-type filter; empty = no filter

	relCols []int // input-row columns holding existing edge IDs; nil = no check
	// intoCol, when >= 0, is the input-row column holding an ALREADY-BOUND
	// destination NodeID that this hop must land on — the openCypher
	// "expand into" case, which arises whenever a pattern closes a cycle
	// (`MATCH (a)-[:K]->(b)-[:K]->(a)`) or re-uses a bound endpoint (#2206).
	//
	// The IR translator already detects this: matchExpandStepBoundWithFrom sets
	// destRebinding and expands into a synthetic `__anon_N_to_<var>`, with an
	// equality Selection above. Without intoCol the operator therefore emits one
	// row per NEIGHBOUR of the source — building and boxing a (srcID, edgeID,
	// dstID) triplet for each — and the Selection above discards all but the one
	// that landed on the bound node. On a triangle query that is the whole
	// adjacency materialised and thrown away per input row.
	//
	// Filtering here instead costs one integer comparison per edge and emits only
	// the matches. It deliberately reuses the existing emit gate rather than
	// adding an operator, so cyphermorphism, the multiplicity queue, direction
	// handling, the edge-type filter and tombstone skipping all keep behaving
	// exactly as they do without it.
	//
	// -1 disables the filter, which is the default and the behaviour of every
	// hop whose destination is not already bound.
	intoCol int
	// intoSeek turns the expand-into FILTER above into a SEEK (rmp #2149,
	// docs/design-expand-into-symmetric-swap.md §3). The filter alone still walks
	// every slot of the source's run and pays, per slot, the edge-type filter's
	// map lookup ([Expand.passesFilter]) and the cyphermorphism check
	// ([Expand.passesRelMorphism]) — measured at 18.6% and 16.7% of a degree-64
	// closing query, against 0.98% for the destination comparison itself. Since
	// rmp #2141 the run is ordered by the total key (destination, handle), so
	// [Expand.seekIntoRuns] narrows the CURSOR to the bound destination's
	// contiguous run in O(log d + r) and those per-slot costs are paid only on
	// slots that can emit.
	//
	// The seek is ORDER-PRESERVING, not merely multiset-preserving: the slots
	// sharing a destination are contiguous and handle-ordered, so the block
	// [dstRun] returns is exactly the subsequence enumerate-and-filter would have
	// emitted, walked in the same ascending position order. Nothing above can
	// observe a difference, which is why no order-safety predicate applies here
	// (design §4).
	//
	// True by default whenever intoCol >= 0; [Expand.WithExpandIntoSeek] turns it
	// off so a differential test can compare seek against filter-only, and as an
	// operational escape hatch.
	intoSeek   bool
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
//
// The relationship-type FILTER is deliberately not here: it is keyed to the
// absolute edge positions of one particular adjacency, so it is supplied by the
// [AdjacencySource] that yields that adjacency and cannot drift from it
// (rmp #2317). It used to be a field, resolved at plan-build time alongside a
// pair that is now resolved at execution time — two lifetimes for two halves of
// one answer.
type ExpandConfig struct {
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
	// positional index is present in the [AdjacencySource]'s filter with this
	// type label.
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

// AdjacencySource yields the adjacency a traversal expands over, RESOLVED AT THE
// MOMENT IT IS CALLED rather than when the plan was built (rmp #2317).
//
// # Why a source and not a pair
//
// A relationship traversal used to receive two prebuilt CSRs, materialised while
// the operator tree was being assembled — before any row executed. That froze the
// topology to an instant chosen before the statement began, and it is why a later
// clause of a statement could not observe an earlier edge CREATE or edge DELETE
// while it observed every node write: the node side reads live stores, the edge
// side read a frozen array.
//
// Both reference engines resolve relationships at execution time against the
// transaction's own view — Memgraph's Expand::ExpandCursor::InitEdges goes through
// vertex.OutEdges with a storage::View, and Neo4j's query context resolves
// relationships per row rather than from a plan-time structure.
//
// The type filter is part of the source, not a separate config field, because it
// is KEYED to the adjacency it was built against. Resolving one without the other
// would apply a filter built for one topology to a different one.
//
// It is called from [Expand.Init], which runs once per outer row under Apply, so
// the traversal follows the writes its own statement has made.
type AdjacencySource func() (fwd, rev CSRAdjacency, admit RelTypeAdmit)

// IntersectAdjacencySource is [AdjacencySource] for the fused cyclic expand, which
// filters TWO legs and therefore needs two type filters keyed to the one adjacency
// it resolves.
type IntersectAdjacencySource func() (fwd, rev CSRAdjacency, midAdmit, endAdmit RelTypeAdmit)

// StaticIntersectAdjacency is [StaticAdjacency] for an [IntersectAdjacencySource],
// and carries the same warning: a production plan must not use it.
func StaticIntersectAdjacency(fwd, rev CSRAdjacency, midFilter, endFilter map[uint64]string) IntersectAdjacencySource {
	return func() (CSRAdjacency, CSRAdjacency, RelTypeAdmit, RelTypeAdmit) {
		return fwd, rev,
			relTypeAdmitFromPositions(fwd, rev, midFilter),
			relTypeAdmitFromPositions(fwd, rev, endFilter)
	}
}

// StaticAdjacency is an [AdjacencySource] over a fixed pair, for callers that
// genuinely hold one — an offline traversal, or a test that builds its own CSR.
// A production query plan must NOT use it: the pair it closes over is exactly the
// plan-build materialisation this type exists to remove.
func StaticAdjacency(fwd, rev CSRAdjacency, edgeTypeFilter map[uint64]string) AdjacencySource {
	return func() (CSRAdjacency, CSRAdjacency, RelTypeAdmit) {
		return fwd, rev, relTypeAdmitFromPositions(fwd, rev, edgeTypeFilter)
	}
}

// NewExpand creates an Expand operator.
//
// src yields the forward and reverse adjacency (the reverse is required for
// DirIn/DirBoth and ignored for DirOut) together with the type filter keyed to
// them. It is consulted in [Expand.Init], not here.
func NewExpand(input Operator, src AdjacencySource, cfg ExpandConfig) *Expand {
	dir := cfg.Direction
	if dir == 0 {
		dir = DirOut
	}
	return &Expand{
		input:        input,
		src:          src,
		dir:          dir,
		edgeType:     cfg.EdgeType,
		inputCol:     cfg.InputCol,
		relCols:      cfg.RelCols,
		multiplicity: cfg.MultiplicityFn,
		// Expand-into is off unless the planner opts in via WithExpandInto (#2206).
		intoCol: -1,
		// The seek is on by default, so opting into expand-into gets the O(log d)
		// access path unless a caller explicitly asks for filter-only (#2149).
		intoSeek: true,
	}
}

// Init initialises the operator and its child.
func (op *Expand) Init(ctx context.Context) error {
	op.ctx = ctx
	// RESOLVE THE ADJACENCY NOW, not when the plan was built (rmp #2317). Init runs
	// once per outer row under Apply, so a traversal in a later clause of a
	// statement sees the edges its own earlier clauses created or deleted.
	op.fwd, op.rev, op.admit = op.src()
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
	// RESET THE REVERSE CURSOR AND THE PENDING QUEUE TOO — Init means START OVER,
	// and Init runs once per OUTER ROW under a correlated Apply, so anything a
	// previous run left behind is republished into the next one.
	//
	// The forward cursor is neutralised by fwdDone above. Neither of these two was
	// neutralised by anything: [Expand.advanceRevEdge] gates on revStart < revEnd
	// alone, and [Expand.Next] emits from the pending queue and consults the reverse
	// cursor BEFORE it pulls its first input row. A run that inherited either one
	// emitted the PREVIOUS source's rows and attributed them to the next source,
	// with op.srcID still -1 and op.inputRow still the previous run's.
	//
	// A residue is the NORMAL case, not an exotic one: an EXISTS / SemiApply stops
	// at the first match and a LIMIT at the n-th, so any run not drained to
	// exhaustion leaves one. Measured at commit 35990293 on four nodes and two arcs
	// both into b: `MATCH (a:P) WHERE EXISTS { MATCH (a)<-[r]-(x) RETURN x }
	// RETURN a.id` returned b AND c, where c has no incoming arc at all.
	//
	// The TYPED form of that query was accidentally CORRECT, which is why this
	// survived: the reverse type test recovered each slot's forward position from
	// (dst, src), was handed uint64(op.srcID) — 2^64-1 — as src, could never find a
	// counterpart for it, and rejected the slot. Answering the reverse slot from the
	// type column instead (rmp #2251) removed the accident along with the cost.
	//
	// Pinned by TestExpand_ReInitResetsReverseCursor and
	// TestExpand_ReInitResetsMultiplicityQueue here, and at engine level by
	// TestExpandReInit_ExistsReverseDoesNotLeakPriorSource; all three fail against
	// the operator as it stood at 35990293.
	op.revStart, op.revEnd = 0, 0
	op.pendingRemaining, op.pendingRow = 0, nil
	op.inputRow = nil
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
		if !op.dstMatchesInto(dst) {
			return false, true // expand-into: not the bound destination — skip
		}
		op.buildRow(out, src, edge, dst)
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
		if !op.dstMatchesInto(dst) {
			return false, true // expand-into: not the bound destination — skip
		}
		op.buildRow(out, src, edge, dst)
		return true, true
	}
}

// dstMatchesInto reports whether dst is the already-bound destination this hop must
// land on, or true when no destination is bound (intoCol < 0), which is the common case.
//
// A row whose intoCol cell is not a resolvable bare NodeID — a NULL, or a boxed
// entity a projection put there — cannot be compared unboxed, so the filter admits the
// edge and lets the equality Selection above decide. That keeps the operator's result a
// SUPERSET of the correct one in every case it cannot decide, which is what makes the
// filter safe to apply before the Selection rather than instead of it (#2206).
func (op *Expand) dstMatchesInto(dst int64) bool {
	if op.intoCol < 0 || op.intoCol >= len(op.inputRow) {
		return true
	}
	want, ok := op.inputRow[op.intoCol].(expr.IntegerValue)
	if !ok {
		return true // not a bare NodeID: defer to the boxed predicate above
	}
	return int64(want) == dst
}

// WithExpandInto binds this hop's destination to an already-bound input column, so the
// operator emits only edges landing on that node instead of one row per neighbour
// (#2206). col < 0 disables it. Returns op for chaining; call before Init.
func (op *Expand) WithExpandInto(col int) *Expand {
	op.intoCol = col
	return op
}

// WithExpandIntoSeek enables or disables the O(log d) SEEK for an expand-into hop
// (#2149). It has no effect unless [Expand.WithExpandInto] has bound a column.
//
// Disabling it keeps the expand-into FILTER and returns the operator to walking
// the whole neighbour run, which is the pre-#2149 behaviour and the "off" arm of
// the differential test that proves the two agree row for row. Returns op for
// chaining; call before Init.
func (op *Expand) WithExpandIntoSeek(enabled bool) *Expand {
	op.intoSeek = enabled
	return op
}

// ExpandIntoSeekCount reports how many times an expand-into hop has narrowed its
// FORWARD cursor to a bound destination's run since process start. It is a
// diagnostic seam for tests that must prove the seek actually FIRED — an EXPLAIN
// line proves the plan, this proves the access path — and for operational
// observability. Process-global and monotonic; callers snapshot it before and
// after a query rather than resetting it.
func ExpandIntoSeekCount() uint64 { return expandIntoSeekCount.Load() }

// ExpandIntoSeekReverseCount reports the same for the REVERSE cursor, which a
// DirIn or DirBoth closing hop narrows.
//
// It is counted separately because the two are independently defeatable and a
// result comparison cannot tell them apart: dropping the reverse narrowing makes
// the operator fall back to walking the whole in-edge range, which is SLOWER but
// returns exactly the same rows in the same order. Without its own counter that
// regression is invisible to every differential test — verified by injecting it.
func ExpandIntoSeekReverseCount() uint64 { return expandIntoSeekRevCount.Load() }

// expandIntoSeekCount and expandIntoSeekRevCount back the two accessors above.
var (
	expandIntoSeekCount    atomic.Uint64
	expandIntoSeekRevCount atomic.Uint64
)

// boundIntoDst returns the already-bound destination NodeID this hop must land on
// and whether it could be resolved unboxed.
//
// It reads the CURRENT input, mode-aware: the borrowed input row in row mode, and
// the current chunk cell in columnar mode. The mode split matters — inputRow is
// never populated on the FillChunk path, so before #2149 [Expand.dstMatchesInto]
// hit its length guard there and the #2206 expand-into filter was silently INERT
// under columnar execution. Results were unaffected (the equality Selection above
// still decided) but the optimisation was absent; reading the chunk cell restores
// it and lets the seek engage on both paths.
//
// It resolves ONLY a bare NodeID, exactly the case [Expand.dstMatchesInto] is
// willing to decide. A NULL, or an entity a projection boxed into the cell, yields
// ok=false and the caller keeps the full range so the Selection above stays the
// source of truth. Deliberately NOT resolving a boxed NodeValue keeps the seek's
// decision boundary IDENTICAL to the shipped filter's, which is what makes the
// narrower cursor provably result-identical rather than dependent on how the
// Selection compares a node to an integer.
func (op *Expand) boundIntoDst() (uint64, bool) {
	if op.intoCol < 0 {
		return 0, false
	}
	if op.chunkMode {
		if op.cScratch == nil || op.intoCol >= op.cScratch.NumCols() ||
			op.cRow < 0 || op.cRow >= op.cScratchLen {
			return 0, false
		}
		if op.cScratch.IsInt64Column(op.intoCol) {
			v, valid := op.cScratch.Int64(op.intoCol, op.cRow)
			if !valid || v < 0 {
				return 0, false
			}
			return uint64(v), true
		}
		if iv, ok := op.cScratch.BoxCell(op.intoCol, op.cRow).(expr.IntegerValue); ok && iv >= 0 {
			return uint64(iv), true
		}
		return 0, false
	}
	if op.intoCol >= len(op.inputRow) {
		return 0, false
	}
	if iv, ok := op.inputRow[op.intoCol].(expr.IntegerValue); ok && iv >= 0 {
		return uint64(iv), true
	}
	return 0, false
}

// seekIntoRuns narrows the cursors loaded by [Expand.loadAdjacency] from the
// source's whole neighbour run to just the bound destination's contiguous run,
// turning a Θ(d) walk into an O(log d + r) seek (#2149, design §3.2).
//
// It is a no-op — leaving the full range in place — whenever the seek is disabled,
// no destination is bound, or the bound cell is not a resolvable bare NodeID. In
// every such case the operator's output stays a SUPERSET of the correct result and
// the equality Selection above decides, which is what makes this incapable of
// being a regression.
//
// Both directions are seekable: the forward CSR's run is (destination, handle)-
// ordered unconditionally since #2141, and the reverse CSR's run is
// (source, handle)-ordered by construction — csr.BuildReverse scatters with the
// source ascending over already-handle-ordered forward runs. That reverse
// invariant is implicit, so graph/csr pins it with a permanent test; a seek here
// depends on it.
func (op *Expand) seekIntoRuns() {
	if !op.intoSeek || op.intoCol < 0 {
		return
	}
	dst, ok := op.boundIntoDst()
	if !ok {
		return
	}
	if op.dir != DirIn {
		op.fwdStart, op.fwdEnd = dstRun(op.fwdEdges, op.fwdStart, op.fwdEnd, dst)
		if op.fwdStart >= op.fwdEnd {
			op.fwdDone = true
		}
		expandIntoSeekCount.Add(1)
	}
	if op.dir != DirOut && op.revEdges != nil {
		op.revStart, op.revEnd = dstRun(op.revEdges, op.revStart, op.revEnd, dst)
		expandIntoSeekRevCount.Add(1)
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
	edgeID := op.emittedEdgeID(op.fwdHandles, pos)
	if !op.passesRelMorphism(edgeID) {
		return 0, 0, 0, edgeSkip // cyphermorphism: duplicate edge; caller retries
	}
	return op.srcID, edgeID, int64(d), edgeEmit
}

// emittedEdgeID is the relationship identity this operator emits for the slot at
// pos of the given handle column (rmp #2317).
//
// # It is the stable handle, not the position
//
// A position indexes ONE adjacency. Since the adjacency is now resolved per Init
// rather than once at plan-build time, a position emitted by one Init can name a
// different edge in the next — and a bound relationship must not change identity
// because its statement wrote something. The handle is minted per slot, is carried
// verbatim across the compaction that shifts positions, and csr.BuildReverse gives
// the SAME handle to both directions of one logical edge, which is what
// cyphermorphism needs.
//
// The absent-column fallback returns the position. Every adjacency built from a
// graph carries handles since rmp #2317 stage 2a, so this reaches only a CSR
// assembled directly from arrays — an offline or test shape. Such an identity is
// NOT stable across a rebuild, which is exactly why the column is mandatory
// everywhere else.
func (op *Expand) emittedEdgeID(handles []uint64, pos uint64) int64 {
	if pos < uint64(len(handles)) {
		return int64(handles[pos])
	}
	return int64(pos)
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
		if !op.reverseEdgePassesFilter(uint64(d), uint64(op.srcID), pos) {
			return 0, 0, 0, edgeSkip // filtered out; caller retries
		}
	}
	// Canonical edge ID (rmp #2317): the stable handle, which csr.BuildReverse
	// gives to BOTH directions of one logical edge, so cyphermorphism observes the
	// same id whichever way the edge was traversed. That is required by openCypher 9
	// §3.2.2: an undirected `(:Label2)--()` step following a forward hop must reject
	// the same edge matched in reverse.
	//
	// # This replaced a forward-position lookup, and the lookup is why it had to
	//
	// The id used to be the forward-CSR POSITION, which meant the reverse hop had to
	// go and FIND the corresponding forward slot — by scanning dst's outgoing range,
	// and in a multigraph by matching the handle that travelled with the reverse slot,
	// because otherwise parallel (dst → src) edges all collapsed onto the first
	// forward position and reported one merged relationship type (rmp #1634). Both
	// scans existed only to recover an identity the handle already carries, so
	// emitting the handle removes them: O(deg(dst)) per reverse edge becomes O(1).
	//
	// The absent-column fallback keeps a handle-less CSR working, with the same
	// caveat [Expand.emittedEdgeID] documents; there the reverse hop still needs the
	// forward position to produce an id the forward hop would agree with.
	var edgeID int64
	if pos < uint64(len(op.revHandles)) {
		edgeID = int64(op.revHandles[pos])
	} else if fwdPos, hasFwd := op.lookupFwdEdgePos(uint64(d), uint64(op.srcID)); hasFwd {
		edgeID = int64(fwdPos)
	} else {
		edgeID = int64(uint64(len(op.fwdEdges)) + pos)
	}
	if !op.passesRelMorphism(edgeID) {
		return 0, 0, 0, edgeSkip // cyphermorphism: duplicate edge; caller retries
	}
	return op.srcID, edgeID, int64(d), edgeEmit
}

// reverseEdgePassesFilter reports whether the reverse-CSR slot revPos — the
// traversal from src back to dst, whose forward edge is (dst → src) — is of a
// type the query's edge-type filter admits. The filter is keyed by FORWARD
// position, so the reverse slot must be mapped to its own forward slot before it
// can be tested.
//
// # It must resolve the INSTANCE, not the pair (rmp #2250)
//
// Resolving the pair by lower bound alone returns the FIRST forward slot with
// this destination, and that verdict was then applied to every parallel edge of
// the pair. With `(a)-[:T1]->(b)` and `(a)-[:T2]->(b)` both present, a reverse
// `()<-[r:T1]-()` counted BOTH slots and `()<-[r:T2]-()` counted NEITHER — a
// silent over-count on one type and a silent zero on every other, while the
// forward direction of the identical data was correct. That is the same class of
// defect [revTypeAdmitSet] was built for on the shortestPath side (#2236): a
// reverse-side type test that resolves the position first is either slow or
// permissive, and permissive is indistinguishable from correct until someone
// counts.
//
// The reverse slot carries the handle that identifies its own instance, and
// since rmp #2141/#2142 the forward run for a pair is contiguous, so the exact
// sibling is found by [matchFwdByHandle] in O(log d + r) over that run — no
// O(V+E) table, which matters because Init runs once per outer row here.
//
// The lower bound remains the fallback for a CSR carrying no handles (a
// non-multigraph or a legacy snapshot). There a pair occupies a single slot, so
// the first match IS the instance and the two agree.
func (op *Expand) reverseEdgePassesFilter(dst, src, revPos uint64) bool {
	if !op.admit.Active() {
		return true // no filter declared → accept all
	}
	// The column answers the reverse slot DIRECTLY when the pair's transpose was
	// established, which is the whole point of rmp #2251: one indexed load and one
	// bit test, with no forward-position recovery at all. `known` false is an
	// ABSENCE of information, never an admission — the recovery below then runs
	// exactly as it did before the column existed.
	if admitted, known := op.admit.Rev(revPos); known {
		return admitted
	}
	if dst+1 >= uint64(len(op.fwdVerts)) {
		return false
	}
	fStart, fEnd := op.fwdVerts[dst], op.fwdVerts[dst+1]
	if op.handlesUsable() && revPos < uint64(len(op.revHandles)) {
		if fp := matchFwdByHandle(
			op.fwdEdges, op.fwdHandles, fStart, fEnd, src, op.revHandles[revPos],
		); fp != unresolvedFwdPos {
			return op.admit.Fwd(fp)
		}
		// Unresolved: this slot carries a handle no forward sibling of the pair
		// has, which a consistent CSR pair cannot produce. Fall through to the
		// pair-level answer rather than silently dropping the edge.
	}
	fwdPos, ok := firstDstPos(op.fwdEdges, fStart, fEnd, src)
	if !ok {
		return false
	}
	return op.admit.Fwd(fwdPos)
}

// handlesUsable reports whether both handle columns are present and long enough
// to index alongside their edge columns, which is the precondition every
// by-handle disambiguation in this operator shares.
func (op *Expand) handlesUsable() bool {
	return op.fwdHandles != nil && op.revHandles != nil &&
		len(op.fwdHandles) >= len(op.fwdEdges) &&
		len(op.revHandles) >= len(op.revEdges)
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
	// Expand-into (#2149): narrow the cursors just loaded to the bound
	// destination's run. This is the ONE place a source's range is set — from
	// advanceInput in row mode and advanceInputChunk in columnar mode — so the seek
	// applies to both paths while every downstream behaviour (the edge-type filter,
	// cyphermorphism, CREATE-multiplicity re-emission, DirBoth ordering and its
	// self-loop dedup, the cancellation cadence) is inherited unchanged.
	op.seekIntoRuns()
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
// The admission view is the slot-aligned type column keyed to THIS Init's
// adjacency, masked by the pattern's accepted types (rmp #2251), so the test is
// one indexed load and one bit test. A nil view — no filter resolved while a type
// WAS requested — rejects, exactly as an absent key in the position-keyed map it
// replaced did.
//
// This is correct for both single-type (`[r:KNOWS]`) and multi-type
// (`[r:KNOWS|HATES]`) patterns. Pre-fix the predicate compared the
// looked-up type against a single op.edgeType label, which silently
// excluded edges of every accepted type other than the first.
func (op *Expand) passesFilter(pos uint64) bool {
	if op.edgeType == "" {
		return true
	}
	return op.admit.Fwd(pos)
}

// lookupFwdEdgePos returns the forward-CSR position of the edge
// (src → dst), or (0, false) when no such edge exists.
//
// Since rmp #2317 it serves only the HANDLE-LESS fallback in the reverse emit path:
// where the adjacency carries no handle column there is no orientation-free identity,
// so the reverse hop still has to recover a forward position for cyphermorphism to
// see one id for both directions of an undirected edge.
func (op *Expand) lookupFwdEdgePos(src, dst uint64) (uint64, bool) {
	if src+1 >= uint64(len(op.fwdVerts)) {
		return 0, false
	}
	// O(log d) since rmp #2141/#2142: the run is destination-ordered, and the
	// lower bound returns the FIRST slot with this destination — the same slot
	// the prior linear scan returned for parallel edges.
	return firstDstPos(op.fwdEdges, op.fwdVerts[src], op.fwdVerts[src+1], dst)
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
//
// The template is built at capacity 1 — not at capacity — because only its SCHEMA
// is read ([Chunk.NumCols] and [Chunk.ColKind], both fixed at construction) before
// it is discarded; capacity 1 gives it the minimal backing, matching
// [NewColumnarHashJoin]'s templates. It must not be 0: [NewChunk] and
// [NewDynamicChunk] map a capacity < 1 to [DefaultChunkCapacity], which would
// silently restore the full allocation. The returned chunk — the one that actually
// carries rows — keeps the caller's capacity.
func (op *Expand) columnarOutputChunk(capacity int) *Chunk {
	template := op.chunkChild.NewOutputChunk(1)
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
