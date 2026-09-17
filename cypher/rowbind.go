package cypher

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// This file holds the PLAN-TIME resolution of the per-row binding ladder that
// [populateRowCtx] runs — the ladder itself stays in api.go beside the helpers it
// calls.
//
// # What it replaces
//
// populateRowCtx used to answer, for EVERY variable of EVERY row, up to seven
// name-keyed map questions: is this name a chained path, a VLE path, a VLE
// relationship list, a relationship variable, an aggregate scalar column, a
// projection-alias scalar column, and does the expression reference it at all.
// Every one of those is fixed for the whole execution — they are properties of
// the PLAN — yet each was re-asked per row with a string key, so the answer cost
// a hash and a probe rather than a load.
//
// Measured on examples/26_social_scale_bench at its own default scale (50 000
// users) at commit f62a3c83, with -nodefraction=0: those seven lines accounted
// for 9.84 s of a 100.39 s profile, and runtime.mapaccess2_faststr reached from
// populateRowCtx alone was 9.04 s — 63.62 % of every mapaccess2_faststr sample in
// the process. See docs/benchmarks/ for the raw artefacts.
//
// # Why the resolution is LAZY rather than taken when the walk is frozen
//
// It cannot be taken in [newRowSchema]. The maps this reads are still being
// WRITTEN after a walk is frozen: the Apply / hash-join rebase
// ([shiftApplyMetaColumns] and the inline form in buildOperator's *ir.Apply case)
// re-writes every edgeVarMeta / pathVarChain / pathVarMeta / vleRelMeta entry the
// inner subtree registered, shifting its columns by the outer width, AFTER that
// subtree — and the row schemas its closures captured — has been built. That
// rebase exists because Match8 [3] returned NULL without it, so it is not
// optional and it cannot be reordered.
//
// Deferring to the first ROW is what makes the resolution sound, and the argument
// is exact rather than approximate: the per-row read and the resolution consult
// THE SAME map, and every write to those maps is inside a builder
// ([buildOperator], [buildIRProjection], [tryBuildHashJoin],
// [tryBuildIndexNestedLoopJoin] and their callees), all of which complete before
// any operator produces a row. The two builders that DO run at execution time —
// the morsel-parallel scan/project factory and the parallel pre-aggregation
// factory — build against [buildOpts.forWorker], which nils every one of these
// maps so each worker populates its own; and subquery bodies build against
// [buildOpts.forSubquery], which carries none of them. So no map this resolves
// from can change between the first row and the last, and the resolved answer is
// therefore the answer the per-row probe WOULD have returned, for every row.
//
// # The name-collision exposure, and why freezing does not widen it
//
// Four of these maps are keyed by variable NAME and are query-scoped, so two
// binders of one name collide and the later registration overwrites the earlier
// (rmp #2864 found this in edgeVarMeta, where it returned one row where two were
// required). That collapse happens in the MAP, at build time, before any row
// exists — so the per-row probe was already reading the last writer's value on
// every row of every binder. Resolving from the same map at the first row
// inherits that behaviour exactly: it neither introduces the collision nor cures
// it. The cure, where one is needed, stays where it was — the direction fact is
// demoted to [relDirUnresolved] by [demoteRelDirOnDisagreement] at registration
// time, so a disagreeing pair is never asserted in the first place.
//
// # Concurrency
//
// A plan is resolved at most once, under a [sync.Once], and is READ-ONLY
// afterwards. That is what makes it safe for the closure holding it to be called
// from several goroutines at once — which the parallel scan/project tier and the
// parallel hash join both do.

// bindKind is the set of plan-time facts that hold for ONE variable of a row.
//
// It is a SET, not an enumeration, because the ladder is not a disjunction: a
// name can be registered in more than one of these maps, and the original code
// fell THROUGH from an entity kind whose reconstruction failed to the scalar
// kinds below it. Collapsing the ladder to a single "kind" would silently drop
// that fall-through.
type bindKind uint8

const (
	// bindPathChain — bopts.pathVarChain holds the name.
	bindPathChain bindKind = 1 << iota
	// bindPathVLE — bopts.pathVarMeta holds the name.
	bindPathVLE
	// bindVLERel — bopts.vleRelMeta holds the name.
	bindVLERel
	// bindEdge — bopts.edgeVarMeta holds the name.
	bindEdge
	// bindScalarCol — bopts.scalarCols holds the name.
	bindScalarCol
	// bindProjAliasScalarCol — bopts.projAliasScalarCols holds the name.
	bindProjAliasScalarCol
	// bindScalarUsed — the closure's scalarUse analysis names this variable.
	// Only ever set on a GATED plan (scalarUse != nil); on an ungated plan the
	// per-row path never asks the question, so recording an answer would be a
	// fact nothing reads.
	bindScalarUsed
)

// bindEntityKinds is the subset whose reconstruction can FAIL and fall through to
// the scalar kinds below it. Testing the mask once skips four branches for the
// overwhelmingly common plain-node variable.
const bindEntityKinds = bindPathChain | bindPathVLE | bindVLERel | bindEdge

// bindScalarPassThrough is the subset whose row cell is forwarded unchanged. The
// two are distinct sets at BUILD time — the colliding-alias guard in
// buildIRProjection reads scalarCols and not projAliasScalarCols — but the per-row
// action they select is the same one, so the row path tests them together.
const bindScalarPassThrough = bindScalarCol | bindProjAliasScalarCol

// bindMeta carries the metadata a resolved entity binding needs, taken by VALUE
// from its map at resolution time.
//
// It is behind a pointer on [boundVar] and shared by no one: only a variable that
// is actually registered in one of the four entity maps allocates one, so the
// walk a plain scan carries stays 48 bytes per variable instead of ~200. The four
// live in one struct rather than four fields because the kinds are disjoint in
// every plan shape observed, so a second allocation would buy nothing.
type bindMeta struct {
	chain  pathChainInfo
	path   pathVarInfo
	vleRel vleRelInfo
	edge   edgeVarInfo
}

// boundVar is one variable of the row, with every plan-time question about it
// already answered: its column, the set of maps that hold its name, the metadata
// those maps carried, and its entry in the closure's scalar-use analysis.
//
// name survives because the RowContext is still a map keyed by name — binding it
// positionally is rmp #2876, and is deliberately not attempted here.
type boundVar struct {
	name string
	// meta is nil unless kind&bindEntityKinds != 0.
	meta *bindMeta
	// use is scalarUse[name] on a gated plan, and nil on an ungated one. It is
	// the value, not merely the membership: the membership is kind&bindScalarUsed,
	// and the two differ, because a key may be present with a nil value.
	use  *nodeScalarUse
	col  int
	kind bindKind
}

// rowBindPlan is one row-context-building closure's binding ladder, resolved once
// and then read per row. See the file comment for why the resolution is deferred
// and why that is sound.
//
// # Concurrency
//
// Safe for concurrent use by any number of goroutines once constructed. Every
// field but vars is written only by [newRowBindPlan]; vars is written once inside
// the [sync.Once] and read-only afterwards, so the Once's happens-before edge
// covers every reader.
type rowBindPlan struct {
	rs        rowSchema
	bopts     *buildOpts
	g         *lpg.ReadView[string, float64]
	scalarUse map[string]*nodeScalarUse
	vars      []boundVar
	// resolveOnce is the bound method value handed to once.Do, bound ONCE at
	// construction. `once.Do(p.resolve)` would build a fresh method value on
	// every call, and because sync.Once.Do passes it to the non-inlinable
	// doSlow, escape analysis heap-allocates it whether or not the slow path is
	// taken — an allocation per ROW on the hottest path in the engine.
	// TestRowBindPlan_ResolvedIsAllocationFree pins that this stays true.
	resolveOnce func()
	once        sync.Once
	// gated mirrors scalarUse != nil, hoisted out of the per-variable loop.
	gated bool
}

// newRowBindPlan freezes the inputs of one row-context-building closure into a
// plan. It resolves NOTHING: the maps it will read are still being written when
// this is called (see the file comment), so the first row resolves them.
//
// rs, bopts, g and scalarUse are exactly the four values the closure used to
// carry separately and hand to populateRowCtx on every row.
func newRowBindPlan(rs rowSchema, bopts *buildOpts, g *lpg.ReadView[string, float64], scalarUse map[string]*nodeScalarUse) *rowBindPlan {
	p := &rowBindPlan{rs: rs, bopts: bopts, g: g, scalarUse: scalarUse, gated: scalarUse != nil}
	p.resolveOnce = p.resolve
	return p
}

// resolved returns the resolved walk, resolving it on the first call.
//
// It is the ONLY route to [rowBindPlan.vars], and [populateRowCtx] is its only
// caller, which is what keeps the resolution off the build path: a build-time
// caller would freeze the pre-rebase state of the maps.
func (p *rowBindPlan) resolved() []boundVar {
	p.once.Do(p.resolveOnce)
	return p.vars
}

// resolve answers, for every variable of the walk, every name-keyed question the
// per-row path used to ask. Runs at most once, inside the Once.
func (p *rowBindPlan) resolve() {
	walk := p.rs.walk
	vars := make([]boundVar, len(walk))
	b := p.bopts
	for i, w := range walk {
		v := boundVar{name: w.name, col: w.col}
		var meta bindMeta
		if b != nil {
			// The nil-map guards are kept: a nil map answers every lookup, but
			// the guard is what the original per-row path tested first and it
			// costs nothing here.
			if b.pathVarChain != nil {
				if info, ok := b.pathVarChain[w.name]; ok {
					v.kind |= bindPathChain
					meta.chain = info
				}
			}
			if b.pathVarMeta != nil {
				if info, ok := b.pathVarMeta[w.name]; ok {
					v.kind |= bindPathVLE
					meta.path = info
				}
			}
			if b.vleRelMeta != nil {
				if info, ok := b.vleRelMeta[w.name]; ok {
					v.kind |= bindVLERel
					meta.vleRel = info
				}
			}
			if b.edgeVarMeta != nil {
				if info, ok := b.edgeVarMeta[w.name]; ok {
					v.kind |= bindEdge
					meta.edge = info
				}
			}
			if b.scalarCols != nil {
				if _, ok := b.scalarCols[w.name]; ok {
					v.kind |= bindScalarCol
				}
			}
			if b.projAliasScalarCols != nil {
				if _, ok := b.projAliasScalarCols[w.name]; ok {
					v.kind |= bindProjAliasScalarCol
				}
			}
		}
		if p.scalarUse != nil {
			if use, ok := p.scalarUse[w.name]; ok {
				v.kind |= bindScalarUsed
				v.use = use
			}
		}
		if v.kind&bindEntityKinds != 0 {
			m := meta
			v.meta = &m
		}
		vars[i] = v
	}
	p.vars = vars
}
