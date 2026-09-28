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
// # When the resolution is taken
//
// A plan is resolved once the subtree whose rows it reads is complete, and no
// earlier: at the next SCOPE BOUNDARY of the build, or at its first row when no
// boundary follows it. Every plan made is logged on [buildOpts.pendingBindPlans],
// and [buildOpts.flushBindPlans] resolves the log at each boundary:
//
//   - the end of a plain-Apply inner arm — the *ir.Apply case of buildOperator,
//     [tryBuildHashJoin] and [tryBuildIndexNestedLoopJoin] — before the arm's
//     metadata columns are rebased by the outer width;
//   - the end of a Projection or EagerAggregation, before the facts of the names
//     that leave scope are dropped ([buildOpts.endScope]);
//   - the end of a SemiApply, AntiSemiApply, RollUpApply or FOREACH body, and of
//     a UNION branch, before the facts of the enclosing scope are restored
//     ([buildOpts.restoreScopeFacts]).
//
// It cannot be taken EARLIER, in [newRowSchema], because a plan is made while the
// subtree it sits in is still being built. It must not be taken LATER, because
// each boundary rewrites the maps it resolves from for the benefit of the
// operators built after it, never before it:
//
//   - The rebase shifts the metadata an inner arm registered so that operators
//     ABOVE the join address it in the combined outer||inner row. A plan made
//     INSIDE the arm runs on inner-only rows, so resolving it after the rebase
//     reconstructed its relationships from the wrong slot:
//     `WITH 1 AS one MATCH (a)-[r {s: 'a'}]->(b)` matched nothing (rmp #2907).
//     The rebase itself is not optional — Match8 [3] returned NULL without it —
//     it is simply for the other side.
//   - Four of these maps are keyed by variable NAME, and one query may bind the
//     same name in two scopes: WITH ends the scope of every variable it does not
//     project, and a later clause may introduce the name afresh (openCypher 9,
//     WITH). A plan resolved after the later binding read the later variable's
//     facts: `MATCH (x:N {k: 'a'}) WITH 1 AS one … UNWIND [1] AS x` tagged the
//     earlier x as a scalar, so the `x.k = 'a'` filter below the WITH stopped
//     reading x as a node and matched nothing (rmp #2906). In the other
//     direction a fact that outlived its scope was inherited by the next binder
//     of the name — `MATCH ()-[x]->() WITH 1 AS one UNWIND [1] AS x RETURN x`
//     reconstructed the integer 1 as a relationship and returned null. The facts
//     are therefore scoped exactly as the schema is: dropped where their names
//     leave scope, restored where the schema is restored, and read by each plan
//     while they still describe its own binding.
//
// rmp #2864 found the collision between the two branches of a UNION, in
// edgeVarMeta: both branches bound `r` with opposite directions, the later
// registration overwrote the earlier, and one row came back where two were
// required. Each UNION branch is now a scope of its own, so that shape no longer
// collides. A re-registration of one name inside ONE scope would still
// overwrite; for the relationship direction it stays guarded where the #2864 cure
// put it: the direction fact is demoted to [relDirUnresolved] by
// [demoteRelDirOnDisagreement] at registration time, so a disagreeing pair is
// never asserted in the first place.
//
// The two builders that run at execution time — the morsel-parallel scan/project
// factory and the parallel pre-aggregation factory — build against
// [buildOpts.forWorker], which nils every one of these maps and the log, so each
// worker populates and resolves its own; subquery bodies build against
// [buildOpts.forSubquery], which carries none of them.
//
// # Concurrency
//
// A plan is resolved at most once, under a [sync.Once], and is READ-ONLY
// afterwards. It is normally resolved on the building goroutine, at a boundary;
// a plan no boundary follows is resolved by its first row. That is what makes it safe for the closure holding it to be called
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
// plan and logs it on bopts. It resolves NOTHING: the subtree it sits in is still
// being built when this is called, so the next scope boundary — or, failing one,
// the first row — resolves it (see the file comment).
//
// rs, bopts, g and scalarUse are exactly the four values the closure used to
// carry separately and hand to populateRowCtx on every row.
func newRowBindPlan(rs rowSchema, bopts *buildOpts, g *lpg.ReadView[string, float64], scalarUse map[string]*nodeScalarUse) *rowBindPlan {
	p := &rowBindPlan{rs: rs, bopts: bopts, g: g, scalarUse: scalarUse, gated: scalarUse != nil}
	p.resolveOnce = p.resolve
	if bopts != nil {
		bopts.pendingBindPlans = append(bopts.pendingBindPlans, p)
	}
	return p
}

// flushBindPlans resolves every [rowBindPlan] made since the last flush and
// empties the log. It is called at every scope boundary of the build — see the
// file comment for the list and for why each one must resolve BEFORE it rewrites
// the maps. A nil receiver is a no-op.
func (b *buildOpts) flushBindPlans() {
	if b == nil {
		return
	}
	for _, p := range b.pendingBindPlans {
		p.resolved()
	}
	clear(b.pendingBindPlans)
	b.pendingBindPlans = b.pendingBindPlans[:0]
}

// scopeFacts is the name-keyed per-variable state of one scope: the three
// scalar-column sets and the four entity-metadata maps [rowBindPlan.resolve] and
// the builders read. It is what [buildOpts.snapshotScopeFacts] captures and
// [buildOpts.restoreScopeFacts] puts back.
type scopeFacts struct {
	scalarCols          map[string]struct{}
	projAliasScalarCols map[string]struct{}
	aggKeyScalarCols    map[string]struct{}
	edgeVarMeta         map[string]edgeVarInfo
	pathVarMeta         map[string]pathVarInfo
	pathVarChain        map[string]pathChainInfo
	vleRelMeta          map[string]vleRelInfo
}

// snapshotScopeFacts copies the facts of the current scope. It is taken before
// the build of a body whose bindings are private to it — a SemiApply,
// AntiSemiApply or RollUpApply inner plan, a FOREACH body, a UNION branch — so
// [buildOpts.restoreScopeFacts] can put the enclosing scope back afterwards, as
// the schema is put back. A nil receiver yields the zero snapshot.
func (b *buildOpts) snapshotScopeFacts() scopeFacts {
	if b == nil {
		return scopeFacts{}
	}
	return scopeFacts{
		scalarCols:          copyMetaMap(b.scalarCols),
		projAliasScalarCols: copyMetaMap(b.projAliasScalarCols),
		aggKeyScalarCols:    copyMetaMap(b.aggKeyScalarCols),
		edgeVarMeta:         copyMetaMap(b.edgeVarMeta),
		pathVarMeta:         copyMetaMap(b.pathVarMeta),
		pathVarChain:        copyMetaMap(b.pathVarChain),
		vleRelMeta:          copyMetaMap(b.vleRelMeta),
	}
}

// restoreScopeFacts resolves the plans the private body made, then puts the facts
// back as s captured them: a body's own bindings are dropped, and a fact of the
// enclosing scope the body shadowed or dropped at one of its own boundaries comes
// back. The maps are restored in place. A nil receiver is a no-op.
func (b *buildOpts) restoreScopeFacts(s scopeFacts) {
	if b == nil {
		return
	}
	b.flushBindPlans()
	b.scalarCols = restoreFactMap(b.scalarCols, s.scalarCols)
	b.projAliasScalarCols = restoreFactMap(b.projAliasScalarCols, s.projAliasScalarCols)
	b.aggKeyScalarCols = restoreFactMap(b.aggKeyScalarCols, s.aggKeyScalarCols)
	b.edgeVarMeta = restoreFactMap(b.edgeVarMeta, s.edgeVarMeta)
	b.pathVarMeta = restoreFactMap(b.pathVarMeta, s.pathVarMeta)
	b.pathVarChain = restoreFactMap(b.pathVarChain, s.pathVarChain)
	b.vleRelMeta = restoreFactMap(b.vleRelMeta, s.vleRelMeta)
}

// restoreFactMap makes dst hold exactly src's entries and returns it. dst is
// rewritten in place when it exists; a nil dst with a non-empty src yields a copy
// of src.
func restoreFactMap[V any](dst, src map[string]V) map[string]V {
	if dst == nil {
		if len(src) == 0 {
			return nil
		}
		dst = make(map[string]V, len(src))
	}
	restoreMetaMap(dst, src)
	return dst
}

// endScope closes the scope a Projection or an EagerAggregation ends: it resolves
// the plans made so far, then drops the facts of every name the boundary does not
// carry into the next scope. keep reports whether a name is carried — an item
// alias of the projection, or a grouping key or aggregate output of the
// aggregation. Dropping is what stops a later clause that introduces a name afresh
// from inheriting the facts of the earlier variable of that name (rmp #2906). A
// nil receiver is a no-op.
//
// The variable-length relationship facts are dropped for EVERY name, carried or
// not (rmp #2917). Such a fact addresses the raw hop list VarLengthExpand writes
// ([vleRelInfo.listCol]), and that list exists only in the layout below the
// boundary: a Projection or an EagerAggregation that carries the variable
// materialises it into its own column as a list of RelationshipValues, and one
// that re-binds the name (`WITH [1, 2] AS rs`) puts an unrelated value there.
// A kept fact would decode whatever the next layout holds at the old column — an
// empty list when that is the carried list itself, fabricated relationships when
// it is an integer list — so the carried value must be read from its own column,
// which is what the absence of a fact does.
func (b *buildOpts) endScope(keep func(string) bool) {
	if b == nil {
		return
	}
	b.flushBindPlans()
	dropFacts(b.scalarCols, keep)
	dropFacts(b.projAliasScalarCols, keep)
	dropFacts(b.aggKeyScalarCols, keep)
	dropFacts(b.edgeVarMeta, keep)
	dropFacts(b.pathVarMeta, keep)
	dropFacts(b.pathVarChain, keep)
	clear(b.vleRelMeta)
}

// dropFacts deletes from m every name keep does not report.
func dropFacts[V any](m map[string]V, keep func(string) bool) {
	for name := range m {
		if !keep(name) {
			delete(m, name)
		}
	}
}

// resolved returns the resolved walk, resolving it on the first call.
//
// It is the ONLY route to [rowBindPlan.vars]. [populateRowCtx] calls it per row;
// [buildOpts.flushBindPlans] calls it at the scope boundary that follows the
// plan's subtree.
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
