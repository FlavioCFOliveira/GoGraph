package cypher

// projection_fusion.go — one shared [expr.RowContext] per input row for a whole
// projection body (rmp #2658).
//
// # The cost this removes
//
// [buildIRProjection] compiles one independent eval closure per projection item,
// and every closure that lands on the general path builds its OWN row binding
// context (evalRowPooled → populateRowCtx). `RETURN r.a, r.b` therefore resolved
// the SAME relationship twice per row: two mapper resolutions, two
// [relStoredInverted] orientation decisions, two by-handle routing decisions —
// once per column, per row. rmp #2388 made each of those rebuilds cheap; it did
// not make them happen once.
//
// Neither peer pays that cost, and both converged on the same rule
// independently: Memgraph's PropertyLookupEvaluationModeVisitor caches a symbol's
// properties iff property_lookup_counts_by_symbol[sym] > 1, and Neo4j's
// InsertCachedProperties rewrites to CachedProperty iff usageCount > 1 ||
// canGetFromIndex. Both scope the cache strictly to ONE ROW. This file is that
// rule, expressed as a shared context rather than as a per-property cache: a
// content-keyed cache with no per-row boundary is unsound here, because the same
// identity triple can denote a DIFFERENT edge after a delete-and-create inside
// one statement.
//
// # What is fused, and what is deliberately not
//
// Only the items that build a context today are fused. Every fast path in
// [buildIRProjection] — the bare-column lookups, the Variable upgrade, the
// edge/path/VLE reconstruction closures — builds no context and keeps building
// none: fusing them would ADD a context where there was none. Fusion is also
// skipped below two general-path items, because one item's shared context is one
// item's own context.
//
// # Materialisation level: the union, and why fusion can decline
//
// One shared context carries ONE materialisation decision per variable, so the
// level must be the UNION of what every fused item asked for
// ([unionNodeScalarUses]): more eager is always sound, less never is. The union is
// PER VARIABLE, not per body, so a variable no item needs whole stays lazy even
// when another variable in the same row is eager.
//
// That still leaves one case where the union would be sound but WORSE: a body that
// mixes a field extractor with a scalar read of the SAME variable
// (`RETURN type(r) AS t, r.p_str AS s`). rmp #2388 pinned the lazy gate as per
// EXPRESSION, not per query, precisely so the second item there stays lazy;
// collapsing both onto one context would make `r` eager for both. [fusionClass]
// is what keeps that promise: fusion is installed only when the union leaves
// EVERY fused item on the materialisation class it would have had on its own, and
// declines otherwise, falling back to today's per-item contexts. So the win is
// taken where it is free and never paid for with a regression elsewhere.
//
// It is also a correctness net, and one worth naming: had the class check not been
// there, `RETURN keys(n) AS k, n.a AS a` would have fused onto a union carrying
// both needsKeyNames and a value key. [buildPartialNodeValueForFn] handles that
// combination correctly (it loads the real map when both are present), so the
// answer would have stayed right — but only because that function anticipated the
// combination. The class check means fusion never depends on it having done so.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// projFusionDisabled turns projection fusion off for the plans built while it is
// set. It exists for MEASUREMENT and FALSIFICATION, not for tuning: it is what
// lets one process run both arms of an interleaved A/B over the same binary (so
// the counters below are present in both, and the comparison is not an artefact of
// instrumenting only one side), and it is what lets a test prove a fused oracle
// FAILS on the unfused build.
//
// The sense is inverted — disabled rather than enabled — so the zero value is the
// production setting and no init is needed.
//
// It is read at PLAN BUILD time, so a flip does not reach a plan already in the
// cache: a caller that flips it must use a fresh [Engine] or call
// [Engine.ClearPlanCache].
var projFusionDisabled atomic.Bool

// projFusionCountersOn gates the two counters below.
//
// They are gated rather than unconditional because both sit on a per-ROW path
// that the morsel-parallel tier drives from several worker goroutines at once, so
// an unconditional atomic add would put a contended cache line in the hot loop
// this task exists to speed up — and would then be measured as part of the result.
// Off, each site costs one relaxed load and a branch that predicts perfectly.
//
// Counting is a whole-process property, so a test that enables it must not run in
// parallel with one that reads it.
var projFusionCountersOn atomic.Bool

// projRowCtxBuildCount counts the row binding contexts built for a PROJECTION
// body — one per fused row, or one per general-path ITEM per row when the body is
// not fused. It is the direct oracle for "the context is built once per row": for
// `RETURN r.a, r.b` it must move by 1 per row fused and by 2 per row unfused.
//
// It counts only the projection path. The Filter-predicate and ORDER BY callers of
// evalRowPooled are deliberately excluded, so a query carrying a WHERE clause does
// not blur the projection's own count.
var projRowCtxBuildCount atomic.Uint64

// relRowBindCount counts the CALLS to [buildRelationshipValueFromRow] — each one
// an attempt to resolve one bound relationship into a value, costing a mapper
// resolution pair, a [relStoredInverted] orientation decision and a by-handle
// routing decision. Calls, not successes: a call that refuses the row (no edge
// column, a non-integer cell) is counted too, so the counter can never
// under-report the resolutions a shape performs. It is
// the oracle for "the relationship is resolved once per row" as distinct from "the
// context is built once per row": the two counters would move together under a
// change that merely moved the context build, and separately under one that shared
// the map but re-resolved the entity.
var relRowBindCount atomic.Uint64

// projFusedItemUnboundCount counts the times a FUSED item closure ran with no
// bound context and had to fall back to building its own.
//
// It exists because that fallback is the one thing about this design that could
// silently delete the whole improvement: the closure stays correct when a driver
// forgets to bracket the row, so a regression would be invisible to every
// result-comparing test. Asserting the count is ZERO across the fused shapes is
// how the seam is proved to be reached rather than merely present.
//
// It is ungated, unlike the two counters above, because it must be readable
// without arming instrumentation and because it sits on a path that is not
// supposed to be taken at all.
//
// ONE reachable non-zero case is known and is not a defect: a
// [exec.ColumnarProject] on its CHUNK-input path runs its chunk fillers outside any
// row bracket, and such a filler falls back to the item's eval closure for a source
// cell that is not a resolvable bare NodeID. That path is column-major by
// construction — [exec.ColumnarProject.fillChunkFromChunk] is deliberately not
// bracketed — so the fallback pays exactly what it paid before fusion existed.
var projFusedItemUnboundCount atomic.Uint64

// countProjRowCtxBuild records one projection-path row-context build.
func countProjRowCtxBuild() {
	if projFusionCountersOn.Load() {
		projRowCtxBuildCount.Add(1)
	}
}

// countRelRowBind records one relationship binding materialisation.
func countRelRowBind() {
	if projFusionCountersOn.Load() {
		relRowBindCount.Add(1)
	}
}

// projFusionMinItems is the number of general-path items below which fusion is
// not installed.
//
// Two, because one item's shared context IS that item's own context: installing a
// binder for a single-item projection would add a driver bracket and a level of
// indirection to buy nothing, and would stop that projection being byte-identical
// to the build that preceded this file.
const projFusionMinItems = 2

// fusableProjItem is one general-path projection item, captured during
// [buildIRProjection]'s single pass so the fusion decision can be taken after the
// pass — when every item's analysis is known — without a second walk over the IR.
//
// use is the GATED analysis, i.e. exactly what the item's own closure would hand
// populateRowCtx: nil for a bailed-out or whole-entity item (full eager
// materialisation, unpooled fresh map), non-nil for a provably-scalar one.
type fusableProjItem struct {
	expr ast.Expression
	use  map[string]*nodeScalarUse
	idx  int
}

// projRowBinder is the [exec.RowBinder] that holds one projection body's shared
// per-row context.
//
// # Ownership
//
// Exactly one driver on exactly one goroutine, which is the contract
// [exec.RowBinder] states and [exec.Project] already declared for itself. The
// morsel-parallel tier honours it structurally rather than by convention: it
// rebuilds the entire projection subtree per worker
// ([tryBuildParallelScanProject]'s per-worker factory), so each worker's Project
// carries its own binder over its own [buildOpts] copy. Nothing about the binder
// is registered on buildOpts, so [buildOpts.forWorker] has no sixth eval-time
// field to reset.
//
// # Lifetime
//
// bound is true ONLY between BindRow and ReleaseRow. That is not bookkeeping: it
// is the enforcement. An item closure that finds it false builds its own context
// instead of evaluating against a released — possibly recycled — map. So a driver
// that fails to bracket a row loses the optimisation and cannot lose correctness.
// [projFusedItemUnboundCount] is what stops that fallback being a silent
// regression.
//
// # Why the context is built on FIRST USE, not in BindRow
//
// Because one driver brackets rows whose items usually need no context at all.
// [exec.ColumnarProject]'s row-input arm calls the item closures only as the
// fallback for a cell its unboxed filler cannot take, so building in BindRow would
// have charged every columnar row for a context nothing read — a straight
// regression on a path this change is not even trying to improve. Building on
// first use makes an unused bracket cost two field writes.
type projRowBinder struct {
	bopts  *buildOpts
	g      *lpg.ReadView[string, float64]
	pooled *pooledRowCtx
	ctx    expr.RowContext
	use    map[string]*nodeScalarUse
	rs     rowSchema
	bound  bool
}

// BindRow opens the shared-context window for row. The context itself is built by
// the first fused item that actually reads one (see [projRowBinder.contextFor]).
func (b *projRowBinder) BindRow(_ exec.Row) { b.bound = true }

// ReleaseRow closes the window and returns the pooled unit, if one was drawn.
func (b *projRowBinder) ReleaseRow() {
	b.bound = false
	b.ctx = nil
	if b.pooled != nil {
		releaseRowCtx(b.pooled)
		b.pooled = nil
	}
}

// contextFor returns the shared context for the current row, building it on the
// first call within one BindRow/ReleaseRow window and returning the same map on
// every later call in that window. row is the row the driver bound, which is the
// row every item of that bracket is handed.
//
// The pooled unit is drawn on exactly the condition evalRowPooled draws one: a
// non-nil (gated) analysis, which [analyseNodeScalarUse] produces only for
// expressions that cannot retain the context. A nil analysis takes the historical
// fresh-map eager path, unpooled — and sharing an unpooled map is safe even for
// the expression kinds whose evaluators may retain it, because nothing recycles
// it and no evaluator writes into a [expr.RowContext] (rmp #2653 moved the last
// smuggled key out of it).
func (b *projRowBinder) contextFor(row exec.Row) expr.RowContext {
	if b.ctx != nil {
		return b.ctx
	}
	countProjRowCtxBuild()
	if b.use == nil {
		b.ctx = make(expr.RowContext, b.rs.width)
		populateRowCtx(b.ctx, row, b.rs.walk, b.g, b.bopts, nil, nil)
		return b.ctx
	}
	b.pooled = acquireRowCtx(b.rs.width)
	b.ctx = b.pooled.ctx
	populateRowCtx(b.ctx, row, b.rs.walk, b.g, b.bopts, b.use, b.pooled)
	return b.ctx
}

// fusionClass is the materialisation class [populateRowCtx] selects for one bound
// variable from its [nodeScalarUse]. Fusion is admitted only when the union leaves
// every item on the class it had alone, so this function must mirror that
// selection exactly — it is the one place the two are allowed to know about each
// other.
type fusionClass uint8

const (
	// fusionClassEager — full eager materialisation: a nil use, or needsWholeNode.
	// The value may escape whole into a result row.
	fusionClassEager fusionClass = iota
	// fusionClassPartialFn — a concrete partial entity built for a field extractor
	// (id/type/labels/keys/startNode/endNode), which type-switches on a concrete
	// value and so cannot be served lazily.
	fusionClassPartialFn
	// fusionClassLazy — on-demand scalar reads through a lazy entity value.
	fusionClassLazy
)

// classOf reports the materialisation class of use, mirroring the branch order in
// [populateRowCtx] / [upgradeNodeIDToValuePartial] / [relLazyRoute].
func classOf(use *nodeScalarUse) fusionClass {
	switch {
	case use == nil || use.needsWholeNode:
		return fusionClassEager
	case use.hasScalarFnNeed():
		return fusionClassPartialFn
	default:
		return fusionClassLazy
	}
}

// unionNodeScalarUses merges the per-item gated analyses into the ONE analysis a
// shared context can carry, or reports that no union is admissible.
//
// The result is (nil, true) — a fully eager shared context — when EVERY item is
// already fully eager, which is a pure win: one eager map per row in place of one
// per item, with no item's level changed. It is (nil, false) when the items
// DISAGREE (some eager, some scalar), because the only sound union is then the
// eager one and that would demote the scalar items.
//
// Otherwise every item carries a non-nil analysis and the merge is per variable:
// value keys and presence keys unioned, every need flag OR-ed. The merged
// [nodeScalarUse] values are FRESH — the inputs come from [nodeScalarUseMemo],
// whose entries are shared across every execution of a cached plan and are
// immutable by contract ([TestNodeScalarUseMemoValueIsNotMutated]); mutating one
// here would corrupt every later execution.
func unionNodeScalarUses(items []fusableProjItem) (map[string]*nodeScalarUse, bool) {
	eager, scalar := 0, 0
	for i := range items {
		if items[i].use == nil {
			eager++
		} else {
			scalar++
		}
	}
	switch {
	case scalar == 0:
		return nil, true // every item is already eager: share one eager context
	case eager > 0:
		return nil, false // mixed: the sound union would demote the scalar items
	}

	out := make(map[string]*nodeScalarUse, len(items[0].use))
	for i := range items {
		for name, u := range items[i].use {
			m, ok := out[name]
			if !ok {
				m = &nodeScalarUse{
					keys:         make(map[string]struct{}, len(u.keys)),
					presenceKeys: make(map[string]struct{}, len(u.presenceKeys)),
				}
				out[name] = m
			}
			for k := range u.keys {
				m.keys[k] = struct{}{}
			}
			for k := range u.presenceKeys {
				m.presenceKeys[k] = struct{}{}
			}
			m.needsLabels = m.needsLabels || u.needsLabels
			m.needsIDOnly = m.needsIDOnly || u.needsIDOnly
			m.needsType = m.needsType || u.needsType
			m.needsEndpoint = m.needsEndpoint || u.needsEndpoint
			m.needsLabelList = m.needsLabelList || u.needsLabelList
			m.needsKeyNames = m.needsKeyNames || u.needsKeyNames
			m.needsWholeNode = m.needsWholeNode || u.needsWholeNode
		}
	}
	// C1 reconciliation, on the SAME terms [analyseNodeScalarUse] applies it: a key
	// that is a value use in ANY item is value-needed for the shared context, so it
	// leaves presenceKeys. Then re-intern the presence table, because the key set it
	// is indexed by is the merged one and the per-item tables cannot answer for it.
	for _, m := range out {
		for k := range m.presenceKeys {
			if _, alsoValue := m.keys[k]; alsoValue {
				delete(m.presenceKeys, k)
			}
		}
		m.internPresenceMaps()
	}
	return out, true
}

// fusionPreservesEveryItem reports whether sharing union would leave every item on
// the materialisation class and the presence-key treatment it has on its own.
//
// Two things can differ, and both are performance rather than correctness:
//
//   - CLASS. A field extractor over `r` in one item forces a concrete partial
//     value, which would demote a lazy scalar read of `r` in another item. This is
//     the rmp #2388 per-expression promise, and it is the reason this function
//     exists.
//   - PRESENCE PROMOTION. A key read only as `IS NULL` in one item but for its
//     value in another becomes value-needed under C1, replacing a kind-gated
//     storage presence check with a value read.
//
// A nil union (the all-eager case) preserves every item by construction: each item
// was already eager.
func fusionPreservesEveryItem(union map[string]*nodeScalarUse, items []fusableProjItem) bool {
	if union == nil {
		return true
	}
	for i := range items {
		for name, u := range items[i].use {
			m, ok := union[name]
			if !ok {
				return false // unreachable: the union is built from these very maps
			}
			if classOf(m) != classOf(u) {
				return false
			}
			for k := range u.presenceKeys {
				if _, stillPresence := m.presenceKeys[k]; !stillPresence {
					return false
				}
			}
		}
	}
	return true
}

// newProjRowBinder decides whether the general-path items of one projection body
// can share a context and, when they can, returns the binder and the replacement
// eval closures to install. It returns (nil, nil) to leave the body exactly as the
// single pass built it.
//
// rs is the shared schema snapshot the per-item closures were built against; every
// general-path item in one body resolves against the same input layout, because
// buildIRProjection does not touch the schema map until after the pass.
func newProjRowBinder(
	items []fusableProjItem,
	rs rowSchema,
	g *lpg.ReadView[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) (*projRowBinder, []func(exec.Row) (expr.Value, error)) {
	if len(items) < projFusionMinItems || projFusionDisabled.Load() {
		return nil, nil
	}
	union, ok := unionNodeScalarUses(items)
	if !ok || !fusionPreservesEveryItem(union, items) {
		return nil, nil
	}
	b := &projRowBinder{bopts: bopts, g: g, rs: rs, use: union}
	fns := make([]func(exec.Row) (expr.Value, error), len(items))
	for i := range items {
		capturedExpr := items[i].expr
		capturedUse := items[i].use
		fns[i] = func(row exec.Row) (expr.Value, error) {
			if b.bound {
				return evalRow(bopts, capturedExpr, b.contextFor(row), params, reg)
			}
			// No bound context: a driver did not bracket this row. Correct, and
			// counted, because the correctness is exactly what would hide the lost
			// optimisation. See [projFusedItemUnboundCount].
			projFusedItemUnboundCount.Add(1)
			countProjRowCtxBuild()
			return evalRowPooled(bopts, capturedExpr, row, rs, g, params, reg, capturedUse)
		}
	}
	return b, fns
}
