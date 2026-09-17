package cypher

// rel_stored_dir.go — the STORED ORIENTATION of a bound relationship, resolved
// at PLAN time where the plan already knows it (rmp #2864).
//
// # The cost this removes
//
// [exec.Expand] emits (src, edge, dst) in TRAVERSAL order and keeps no
// direction flag, so [relStoredInverted] had to recover the stored orientation
// from the graph itself, once per row, for every bound relationship. On
// examples/26_social_scale_bench at its default 50 000 users that recovery was
// 20.11s of a 118.05s CPU profile (17.04%), and 19.59s of it was two
// ReadView.HasEdge topology probes that both concluded "stored forward" —
// because the benchmark binds only `-[r:FRIEND]->` and `-[r]->`, every hop
// DirOut, so the probe was computing a plan-time constant per row.
//
// # The bit the plan already holds
//
// A hop's TRAVERSAL direction decides the stored orientation outright for two
// of the three directions, because [exec.Expand] walks a different adjacency
// for each:
//
//   - DirOut walks the FORWARD adjacency only ([exec.Expand.advanceFwdEdge]
//     returns edgeNone for DirIn), and a forward slot of src to dst IS the
//     stored edge (src -> dst). Never inverted.
//   - DirIn walks the REVERSE adjacency only ([exec.Expand.advanceRevEdge]
//     returns edgeNone for DirOut), and a reverse slot of src to dst is the
//     stored edge (dst -> src). Always inverted. This holds for a RECIPROCAL
//     pair too, and is strictly stronger than what the topology ladder could
//     conclude there: the reverse adjacency cannot contain the forward member
//     of the pair, so no probe is needed to tell the two apart.
//   - DirBoth walks BOTH, so the orientation is genuinely per row and stays on
//     [relStoredInverted]. That is rmp #2504's reciprocal-pair surface.
//
// The direction is read from the [ir.Expand] node at the registration site, NOT
// from the query text: [mirrorAnchorSite] re-roots a written-forward hop onto
// its other endpoint as a NEW ir.Expand carrying [reverseSingleEdgeDir], so a
// query reading `-[r]->` can legitimately execute as DirIn.
//
// # Why a disagreeing re-registration falls back rather than deciding
//
// bopts.edgeVarMeta is keyed by variable NAME, is query-scoped, and is read PER
// ROW by [populateRowCtx] — not captured per plan node. Two Expand hops in ONE
// plan can therefore bind the SAME name, and the second registration overwrites
// the first; every row of both hops then reads whatever the build wrote LAST.
// `MATCH (n)-[r:T]->(m) RETURN r UNION ALL MATCH (n)<-[r:T]-(m) RETURN r` is
// exactly that shape, and the two hops disagree about the direction.
//
// This is not theoretical: asserting the last-written direction there was
// measured to return the WRONG relationship for the first branch (a reciprocal
// pair with distinct per-direction properties returned one row where two are
// required — see TestRelStoredDir_CollisionFallsBackToLadder, which fails on a
// build that omits the demotion below).
//
// So the direction is asserted only when EVERY hop that binds the name agrees
// on it. [demoteRelDirOnDisagreement] enforces that, and the disagreeing case
// falls through to the per-row ladder — SLOW, never wrong. The rule is monotone:
// [relDirUnresolved] differs from every resolved direction, so once a name has
// been demoted no later registration can promote it back.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// relDirUnresolved is the [edgeVarInfo.dir] sentinel meaning "the plan did not
// resolve this relationship variable's stored orientation" — the zero value, so
// an edgeVarInfo built without setting dir (a hand-built test row, a future
// producer that forgets) falls through to [relStoredInverted] and is SLOW rather
// than WRONG. That is the whole reason the sentinel is the zero value and the
// resolved directions are not.
const relDirUnresolved = exec.Direction(0)

// relDirPlanDisabled turns the plan-time orientation off for the plans built
// while it is set, leaving every bound relationship on the [relStoredInverted]
// ladder.
//
// It exists for MEASUREMENT and FALSIFICATION, not for tuning, and mirrors
// [projFusionDisabled]: it is what lets one process run both arms of an
// interleaved A/B over the same binary — so the counters below are present in
// both arms and the comparison is not an artefact of instrumenting one side —
// and it is what lets a differential test prove the two arms agree row for row.
//
// The sense is inverted, disabled rather than enabled, so the zero value is the
// production setting and no init is needed.
//
// It is read at PLAN BUILD time, so a flip does not reach a plan already in the
// cache: a caller that flips it must use a fresh [Engine] or call
// [Engine.ClearPlanCache].
var relDirPlanDisabled atomic.Bool

// relDirCountersOn gates the two counters below.
//
// They are gated rather than unconditional for the same reason
// [projFusionCountersOn] is: both sites sit on a per-ROW path that the
// morsel-parallel tier drives from several worker goroutines at once, so an
// unconditional atomic add would put a contended cache line in the hot loop this
// task exists to speed up — and would then be measured as part of the result.
// Off, each site costs one relaxed load and a branch that predicts perfectly.
//
// Counting is a whole-process property, so a test that enables it must not run
// in parallel with one that reads it.
var relDirCountersOn atomic.Bool

// relDirPlanCount counts the orientation decisions answered by the PLAN — the
// resolved-direction arms of [relStoredInvertedForHop]. It is the white-box
// oracle for "the new path ran", which a result-comparing test cannot supply:
// the change is result-identical by construction, so nothing about the answers
// distinguishes the two arms.
var relDirPlanCount atomic.Uint64

// relDirLadderCount counts the orientation decisions that still reached
// [relStoredInverted]. Together with [relDirPlanCount] it partitions every
// decision, so a shape's two counters must sum to its relationship-resolution
// count and a fallback can never hide as a plan answer.
var relDirLadderCount atomic.Uint64

// countRelDirPlan records one orientation answered from the plan.
func countRelDirPlan() {
	if relDirCountersOn.Load() {
		relDirPlanCount.Add(1)
	}
}

// relDirColumnCount counts the orientation decisions answered by an undirected
// hop's per-row COLUMN. It is a third bucket rather than part of
// [relDirPlanCount] because the two halves of rmp #2864 are separate claims and
// each is measured on its own: a shape's three counters partition every
// decision it takes.
var relDirColumnCount atomic.Uint64

// countRelDirColumn records one orientation answered from the emitted column.
func countRelDirColumn() {
	if relDirCountersOn.Load() {
		relDirColumnCount.Add(1)
	}
}

// countRelDirLadder records one orientation answered by [relStoredInverted].
func countRelDirLadder() {
	if relDirCountersOn.Load() {
		relDirLadderCount.Add(1)
	}
}

// resolveHopStoredDir maps a hop's traversal direction to the value recorded in
// [edgeVarInfo.dir]: the direction itself when it decides the stored orientation
// outright, and [relDirUnresolved] otherwise (DirBoth, an unset direction, or
// the measurement switch being set).
//
// It is called at PLAN BUILD time, once per hop.
func resolveHopStoredDir(d exec.Direction) exec.Direction {
	if relDirPlanDisabled.Load() {
		return relDirUnresolved
	}
	switch d {
	case exec.DirOut, exec.DirIn:
		return d
	default:
		return relDirUnresolved
	}
}

// demoteRelDirOnDisagreement returns info with its resolved direction cleared
// when prev — an entry already registered for the SAME relationship variable
// name in this build — resolved to a different one.
//
// See this file's header for why one name can be registered twice and why the
// last writer's direction cannot simply be trusted.
func demoteRelDirOnDisagreement(info, prev edgeVarInfo) edgeVarInfo {
	if prev.dir != info.dir {
		info.dir = relDirUnresolved
	}
	// The COLUMN coordinate needs the same treatment and for the same reason:
	// the surviving entry serves the rows of BOTH hops, so a coordinate only
	// one of them emits is a coordinate the other's rows would read from
	// whatever happens to sit there. The BoolValue assertion would refuse an
	// integer cell, but it cannot tell one hop's direction column from
	// another's, so a disagreeing pair is demoted rather than assumed.
	if prev.dirCol != info.dirCol {
		info.dirCol = -1
	}
	return info
}

// relDirColDisabled turns the undirected hop's fourth stored-direction COLUMN
// off for the plans built while it is set, leaving an undirected hop on the
// [relStoredInverted] ladder. It is the second measurement seam, independent of
// [relDirPlanDisabled] so the two halves of rmp #2864 can be measured apart:
// the plan-time half answers a DIRECTED hop, and this column answers an
// UNDIRECTED one, and each has to justify itself on its own numbers.
//
// Read at PLAN BUILD time, like [relDirPlanDisabled].
var relDirColDisabled atomic.Bool

// relDirColumnAdmits reports whether a hop of this direction should emit the
// per-row stored-direction column. Only DirBoth does: it is the one direction
// whose stored orientation is not a property of the hop.
func relDirColumnAdmits(d exec.Direction) bool {
	return d == exec.DirBoth && !relDirColDisabled.Load()
}

// relStoredDirFromRow reads the stored orientation an undirected hop recorded
// in its fourth column, or (false, false) when this row carries none it can
// trust.
//
// The type assertion is the SENTINEL, and it is the reason the cell is a
// BoolValue. edgeVarMeta's coordinates describe the Expand-emitted row shape
// only; after a projection the same slot belongs to another variable, and every
// other column of that row holds a node id or an edge handle — an
// [expr.IntegerValue]. A stale coordinate therefore fails the assertion and
// falls back to the ladder, rather than reading a node id's low bit as a
// direction. Slow, never wrong.
func relStoredDirFromRow(row exec.Row, dirCol int) (inverted, ok bool) {
	if dirCol < 0 || dirCol >= len(row) {
		return false, false
	}
	bv, isBool := row[dirCol].(expr.BoolValue)
	if !isBool {
		return false, false
	}
	return bool(bv), true
}

// relStoredInvertedForHop reports whether the bound relationship is stored as
// (dstID -> srcID) rather than in the row's traversal orientation, answering
// from [edgeVarInfo.dir] when the plan resolved it and deferring to
// [relStoredInverted] when it did not.
//
// The two arms are ANSWER-IDENTICAL, not merely usually-equal; that is what
// TestRelStoredDir_DifferentialAgainstLadder pins over a corpus, and what the
// full openCypher TCK pins over 3897 scenarios, since a wrong answer here moves
// a rendered StartID/EndID.
func relStoredInvertedForHop(
	row exec.Row,
	meta edgeVarInfo,
	g *lpg.ReadView[string, float64],
	srcID, dstID graph.NodeID,
	srcKey, dstKey string,
	handle uint64,
) bool {
	switch meta.dir {
	case exec.DirOut:
		countRelDirPlan()
		return false
	case exec.DirIn:
		countRelDirPlan()
		return true
	}
	if inverted, ok := relStoredDirFromRow(row, meta.dirCol); ok {
		countRelDirColumn()
		return inverted
	}
	countRelDirLadder()
	return relStoredInverted(g, srcID, dstID, srcKey, dstKey, handle)
}
