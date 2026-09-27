package exec

// live_adjacency.go — per-source forward expansion over a write transaction's own
// view, instead of a whole-graph CSR pair (rmp #2883).
//
// # Why this exists
//
// An [AdjacencySource] normally yields a whole-graph forward/reverse CSR pair. A
// write transaction's view must not share the engine's cached pair (rmp #2446), so
// every Init of a traversal inside a write transaction used to BUILD one: O(V+E)
// plus the whole-graph relationship-type column, for a statement that may touch a
// single node's neighbours. Measured on examples 31 and 36 at 127c012b, that build
// was 73% of one example's CPU and 60% of the other's allocation.
//
// It is also NOT the same thing as reading the adjacency per source. The pair is
// resolved once per Init, so it freezes the topology at that instant against the
// writes the same statement makes while the traversal is still open — a CREATE or
// DELETE above it in the pipeline. A per-source read done later would observe
// those writes. The implementation therefore journals, before every such write,
// the Init-instant state of what it is about to change (cypher's liveTopoLog), and
// serves a run from the journal when the statement has written it since Init.
//
// A forward traversal needs one source's outgoing run at a time. The versioned
// adjacency already answers that exactly at the transaction's instant, so the
// source can hand the operator that run instead of the whole graph.
//
// # Why forward only
//
// The only in-edge structure the adjacency keeps is a PRESENT-state index with no
// version information, and write transactions run concurrently, so an incoming
// walk cannot be answered at a transaction's instant from it: an edge another
// transaction deleted after this one began would be missing. The whole-graph
// build derives incoming edges by transposing the versioned forward entries, which
// is why every traversal that reads incoming edges keeps it.
//
// See the type documentation below for the contract.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// liveOutAdjacency is the per-source forward adjacency protocol.
//
// # The contract, and why it is unexported
//
// liveOutAdjacency is detected by type assertion on the forward adjacency an
// [AdjacencySource] returns. It is unexported deliberately: it is a protocol
// between this package and the cypher planner, not a surface for callers, and an
// implementation must satisfy the parity rules below that nothing outside the two
// packages could be expected to honour.
//
// An implementation MUST make every run it returns identical to the run the
// whole-graph build ([csr.BuildFromAdjListAsOf] with the view's liveness filter,
// then its type column) would have given the same source AT THE INSTANT
// [liveOutAdjacency.LiveBegin] returned, even when the statement has written
// topology since:
//
//   - the arcs the liveness filter keeps, and no others;
//   - ordered by (destination, handle), stably, as [csr.OrderRuns] orders a run;
//   - the per-slot relationship-type codes the column would hold for them;
//   - handleCol true exactly when the source is live and its entry carries a
//     handle column, which is the whole-graph build's anyHandles criterion.
//
// Its VerticesSlice, EdgesSlice and HandlesSlice return nil: an operator that does
// not recognise the live protocol sees an empty graph rather than a wrong one, and
// the planner hands a live source only to the operators that do recognise it.
type liveOutAdjacency interface {
	CSRAdjacency

	// LiveBegin names the instant an Init reads at and returns the encoded
	// relationship-type codes the pattern accepts, resolved at that instant.
	LiveBegin() (instant uint64, accept []uint32)

	// LiveOutRun returns src's outgoing run at instant, appended to the three
	// buffers (which may be nil) and returned so the caller can keep their
	// capacity. extra holds the second and later type codes of the rare slot
	// carrying more than one, keyed by index into the run, or nil.
	LiveOutRun(
		src graph.NodeID, instant uint64,
		dsts []graph.NodeID, handles []uint64, codes []uint32,
	) (outDsts []graph.NodeID, outHandles []uint64, outCodes []uint32, extra map[uint64][]uint32, handleCol bool)

	// LiveFallback returns the whole-graph forward adjacency and its type
	// admission at instant, for an operator that met a slot with no stable
	// handle before it had observed any handle column. See [liveEdgeIdentity].
	LiveFallback(instant uint64) (fwd CSRAdjacency, admit RelTypeAdmit)
}

// liveOutRunServed counts runs a traversal took from a [liveOutAdjacency], and
// liveFallbacks counts the operators that had to switch to a whole-graph
// adjacency mid-Init. Process-global and monotonic; tests bracket a drive.
var (
	liveOutRunServed atomic.Uint64
	liveFallbacks    atomic.Uint64
)

// LiveOutRunServedCount reports how many forward runs have been served from a
// transaction's live adjacency rather than from a whole-graph CSR (rmp #2883).
// Process-global and monotonic; bracket a drive to read a delta.
func LiveOutRunServedCount() uint64 { return liveOutRunServed.Load() }

// LiveFallbackCount reports how many live traversals switched to a whole-graph
// adjacency because a relationship without a stable handle had to be identified
// the way the whole-graph build identifies it (rmp #2883). Process-global and
// monotonic; bracket a drive to read a delta.
func LiveFallbackCount() uint64 { return liveFallbacks.Load() }

// liveEdgeIdentity decides how the slots of a run without a handle column are
// identified, which is the one place per-source serving cannot compute on its own
// what the whole-graph build computes globally.
//
// # The whole-graph rule it must reproduce
//
// The build allocates a handle column when ANY live source's entry carries one.
// With the column, a slot from a handle-less entry holds handle 0, and 0 is the
// identity [Expand.emittedEdgeID] emits for it. Without the column — a graph with
// no handle anywhere — the identity is the slot's absolute position, which only a
// whole-graph adjacency has.
//
// # Why the decision is exact
//
// Once this Init has read any live source whose entry carries a handle column,
// the whole-graph build at the same instant would have allocated one, so identity
// 0 is exactly what it would emit: handleSeen true answers "use zeros". Before
// that, the answer is unknown, so the operator switches to [liveOutAdjacency.LiveFallback].
//
// That switch cannot mix two identity schemes within one Init. A run with slots
// and no handle column triggers it the first time one is met; every slot served
// before it therefore came from a run WITH a handle column, which would have set
// handleSeen. So when the switch happens no relationship identity has been emitted
// from this Init at all — the argument both [Expand] and [VarLengthExpand] rely on.
//
// It returns the handle column the caller must use, or ok=false to switch.
func liveEdgeIdentity(nDsts int, handles []uint64, handleCol, handleSeen bool, zeros *[]uint64) ([]uint64, bool) {
	if handleCol || nDsts == 0 {
		return handles, true
	}
	if !handleSeen {
		return nil, false
	}
	if cap(*zeros) < nDsts {
		*zeros = make([]uint64, nDsts)
	}
	z := (*zeros)[:nDsts]
	clear(z)
	return z, true
}
