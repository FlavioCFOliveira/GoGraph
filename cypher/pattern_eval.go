package cypher

// pattern_eval.go — runtime implementation of [expr.PatternEvaluator] for
// existential pattern predicates in WHERE clauses (task-961).
//
// # Overview
//
// Pattern predicates such as WHERE (a)-[:T]->(b) are existential checks: they
// evaluate to true iff at least one path matching the pattern exists in the
// graph given the current row bindings. They are NOT graph matches; they
// produce a boolean, not additional rows.
//
// # Algorithm
//
// For each outer row the evaluator:
//
//  1. Collects the start-node anchor from the bound variable in RowContext
//     (or treats the node as unbound, meaning "any node").
//  2. Walks the PathElement linked list hop by hop.
//  3. At each hop it follows edges in the declared direction (outgoing,
//     incoming, or undirected) and filters each relationship by type and by
//     its own property map (if given). A relationship variable already bound
//     on the row restricts a fixed hop to that one relationship.
//  4. For variable-length hops it enumerates relationship-isomorphic paths
//     within the declared min/max depth (see pattern_eval_varlen.go).
//  5. After all hops, checks that the final node satisfies the end-node
//     pattern (labels + properties + bound variable).
//  6. Returns BoolValue(true) on the first complete match found.
//
// # Concurrency
//
// patternEvaluator is NOT safe for concurrent use. Each Engine.Run call
// constructs its own instance. The underlying LPG graph is safe for concurrent
// reads, so concurrent engine calls on the same graph are safe.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	lpg "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// patternEvaluator implements [expr.PatternEvaluator] using the live LPG
// graph. All edge traversal is performed via the adjacency-list API so no CSR
// snapshot is required.
type patternEvaluator struct {
	g *lpg.ReadView[string, float64]
	// maxCollectItems bounds the size of the result list a single
	// [ast.PatternComprehension] may build, sharing the buffering-aggregator
	// budget so the cap is consistent and configurable through the same knob
	// (EngineOptions.MaxCollectItems). It is the already-resolved value: a
	// positive number is an active ceiling and zero disables the cap (the
	// explicit opt-out). See [resolvePatternCompBudget].
	maxCollectItems int

	// labelledHop caches the labelled single-hop verdict per pattern occurrence
	// (rmp #2235), so the recogniser runs once per pattern rather than once per
	// outer row. Lazily created: most patterns never reach the recogniser.
	labelledHop map[*ast.PathPattern]*labelledHopShape

	// constNodeProps caches, per node-pattern occurrence, a property map whose
	// every value is a literal or a parameter ([constantPropMap]), so
	// [patternEvaluator.resolveNodeProps] evaluates and allocates it once per
	// query run rather than once per outer row. Lazily created.
	constNodeProps map[*ast.NodePattern]*nodePropValues

	// sameHopRefs caches, per pattern occurrence, which of its property maps
	// read a variable the same element or hop binds ([sameHopRefsOf]; rmp #2916),
	// so the reference walk runs once per query run rather than once per outer
	// row. A nil entry records a pattern with no such map. Lazily created.
	sameHopRefs map[*ast.PathPattern]*sameHopRefs

	// params is the enclosing query's fully-resolved parameter map (rmp #2507).
	//
	// [patternEvaluator.checkNodePattern] evaluates an inline property map itself
	// rather than through a planned Filter, and it used to do so with a nil
	// parameter map. That silently broke the ordinary spelling of a pattern
	// predicate: [parser.StripLiterals] hoists a string literal inside a WHERE onto
	// an auto-parameter, so `WHERE (a)-[:KNOWS]->(:Person {name:'B'})` reached this
	// matcher as `{name: $«auto_1»}`, the reference evaluated to NULL, the equality
	// was not truthy, and the predicate rejected every row. A numeric literal is
	// never hoisted, which is why `{age:40}` had always worked and the defect stayed
	// invisible.
	params map[string]expr.Value

	// subEval dispatches an EXISTS { … } / COUNT { … } written inside a pattern
	// comprehension's WHERE or projection (rmp #2507). It was hard-coded nil at
	// both [expr.EvalWith] call sites in [patternEvaluator.EvalPatternComp], so
	// such a subquery answered false / 0 instead of being evaluated.
	subEval expr.SubqueryEvaluator

	// reg is the function registry the node and relationship property maps
	// [patternEvaluator.resolveStep] and [patternEvaluator.resolveNodeProps]
	// evaluate are called with. The build scaffolds set it to the query's registry
	// ([readBuildScaffold.init], [writeEvalScaffold.init]), because a bare pattern
	// predicate reaches [patternEvaluator.EvalPattern] with none: without it
	// `WHERE (a)-[:R {w: toInteger(x)}]->()` failed with "no function registry"
	// (rmp #2913). [patternEvaluator.EvalPatternComp] installs the comprehension's
	// own registry for its duration.
	reg expr.FunctionRegistry

	// adjacencyCountsDisabled forbids both adjacency-answered rewrites this
	// evaluator can take — the degree count behind `size([ … ])`
	// ([patternEvaluator.CountPatternComp]) and the labelled single hop behind
	// `WHERE (a)-[:K]->(:P)` ([patternEvaluator.matchLabelledHop]) — so each shape
	// enumerates instead. It is set from
	// [EngineOptions.DisableAdjacencyCountRewrites].
	//
	// The polarity is NEGATIVE for the reason [bind] is a setter: the several test
	// call sites of [newPatternEvaluator] pass no Engine, and the zero value must
	// leave them exactly as they were.
	adjacencyCountsDisabled bool

	// relTypeChecks counts [patternEvaluator.edgeMatchesRel] calls. Each call is
	// a versioned pair lookup (HasEdge or EdgeLabels at this view's instant), so
	// the count is the cost unit the bound-end guard in
	// [patternEvaluator.matchOutgoing] and [patternEvaluator.matchIncoming] exists
	// to bound (rmp #2894); the regression test reads it. A plain counter: the
	// evaluator is single-goroutine by contract.
	relTypeChecks uint64

	// Variable-length search state (rmp #2898; see pattern_eval_varlen.go).
	//
	// varLenRowTraversals and varLenTotalTraversals count the relationship slots
	// the exact variable-length search has read in the current top-level
	// evaluation and in the whole query, against the limits MATCH's
	// [exec.VarLengthExpand] applies; evalNesting tells a top-level evaluation,
	// which resets the per-row count and incomingCache, from a nested one.
	// varLenBufs holds one reusable candidate buffer per live search frame, and
	// varLenFrame is the number of live frames.
	varLenRowTraversals   int
	varLenTotalTraversals int
	evalNesting           int
	varLenFrame           int
	varLenBufs            [][]candidateHop
	incomingCache         map[graph.NodeID][]incomingSlot
}

// bind attaches the enclosing query's parameter map and subquery evaluator. It is
// a setter rather than a constructor argument so that the several test call sites
// of [newPatternEvaluator] — which exercise traversal and the element budget, and
// need neither — stay unchanged.
func (pe *patternEvaluator) bind(params map[string]expr.Value, subEval expr.SubqueryEvaluator) {
	pe.params = params
	pe.subEval = subEval
}

// newPatternEvaluator constructs the evaluator for one query run. The
// maxCollectItems argument carries the Engine's per-query element budget using
// the EngineOptions.MaxCollectItems encoding (0 → DefaultMaxCollectItems, <0 →
// no cap, >0 → that exact budget); it is resolved once here so the hot append
// path compares against a single non-negative ceiling.
func newPatternEvaluator(g *lpg.ReadView[string, float64], maxCollectItems int) *patternEvaluator {
	pe := &patternEvaluator{}
	pe.init(g, maxCollectItems)
	return pe
}

// init sets up an evaluator IN PLACE, so that a caller which already owns
// storage for one — [readBuildScaffold] — can initialise it without a second
// heap allocation. [newPatternEvaluator] is this plus the allocation.
func (pe *patternEvaluator) init(g *lpg.ReadView[string, float64], maxCollectItems int) {
	pe.g = g
	pe.maxCollectItems = resolvePatternCompBudget(maxCollectItems)
}

// resolvePatternCompBudget maps the EngineOptions.MaxCollectItems encoding to
// the resolved ceiling stored on patternEvaluator, matching the resolution
// buildEagerAggregation applies to the buffering aggregators (#1294):
//
//   - 0  → unset; apply the finite [funcs.DefaultMaxCollectItems]
//   - <0 → the explicit opt-out; 0 disables the cap entirely
//   - >0 → an active budget, used verbatim
func resolvePatternCompBudget(maxCollectItems int) int {
	switch {
	case maxCollectItems < 0:
		return 0 // opt-out: 0 disables the cap
	case maxCollectItems > 0:
		return maxCollectItems
	default:
		return funcs.DefaultMaxCollectItems
	}
}

// EvalPattern implements [expr.PatternEvaluator].
func (pe *patternEvaluator) EvalPattern(ctx context.Context, pp *ast.PathPattern, row expr.RowContext, _ map[string]expr.Value) (expr.Value, error) {
	if pe.g == nil || pp == nil || pp.Head == nil {
		return expr.BoolValue(false), nil
	}
	pe.beginEval()
	defer pe.endEval()
	// Labelled single hop (#2235): `WHERE (a)-[:K]->(:P)` asks only whether ONE
	// qualifying neighbour exists, which is one adjacency walk that stops at the
	// first match — not an enumeration of every candidate hop.
	if found, ok := pe.matchLabelledHop(pp, row); ok {
		return expr.BoolValue(found), nil
	}
	found, err := pe.matchPattern(ctx, pp, row)
	if err != nil {
		return nil, err
	}
	return expr.BoolValue(found), nil
}

// EvalPatternComp implements the list-producing variant of
// [expr.PatternEvaluator] for [ast.PatternComprehension] expressions.
// It enumerates every match of pc.Pattern given the bindings in row,
// evaluates pc.Predicate (when present) and pc.Projection per match,
// and returns the collected list value. Fixed and variable-length hops, in any
// number and direction, are enumerated under relationship isomorphism.
func (pe *patternEvaluator) EvalPatternComp(ctx context.Context, pc *ast.PatternComprehension, row expr.RowContext, params map[string]expr.Value, reg expr.FunctionRegistry) (expr.Value, error) {
	if pe.g == nil || pc == nil || pc.Pattern == nil || pc.Pattern.Head == nil {
		return expr.ListValue{}, nil
	}
	pe.beginEval()
	defer pe.endEval()
	prevReg := pe.reg
	pe.reg = reg
	defer func() { pe.reg = prevReg }()
	pp := pc.Pattern
	results := expr.ListValue{}
	appended := 0
	err := pe.enumeratePatternMatches(ctx, pp, row, func(innerRow expr.RowContext) error {
		// Honour cancellation per appended result, not only per start-node /
		// candidate-hop: a comprehension over a supernode anchor enumerates a
		// match per neighbour, so a huge result list must be abortable
		// mid-build. The 4096 stride (firing at appended == 0) matches the
		// ctx-check cadence the exec pipeline breakers use.
		if appended%4096 == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		if pc.Predicate != nil {
			// pe.subEval, not nil: a comprehension's WHERE may hold an
			// EXISTS { … } / COUNT { … }, which without an evaluator answered
			// false / 0 rather than being evaluated at all (rmp #2507).
			pv, perr := expr.EvalWith(ctx, pc.Predicate, innerRow, params, reg, pe.subEval, pe)
			if perr != nil {
				return perr
			}
			if !expr.IsTruthy(pv) {
				return nil
			}
		}
		var projVal = expr.Null
		if pc.Projection != nil {
			v, perr := expr.EvalWith(ctx, pc.Projection, innerRow, params, reg, pe.subEval, pe)
			if perr != nil {
				return perr
			}
			projVal = v
		}
		// Bound the result list with the same element budget as collect()
		// (#1294): an anchor with a very high degree would otherwise grow this
		// list without limit, exhausting memory while the visibility barrier is
		// held. A zero budget is the explicit opt-out (no cap).
		if pe.maxCollectItems > 0 && appended >= pe.maxCollectItems {
			return funcs.ErrCollectItemsExceeded
		}
		results = append(results, projVal)
		appended++
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// enumeratePatternMatches walks pp and invokes cb once per complete
// match, passing an extended RowContext that binds every named variable
// in pp (node / relationship variables) to its matched value, recursing
// through each successive step. A variable-length hop binds its relationship
// variable to the list of relationships it crossed (rmp #2898).
func (pe *patternEvaluator) enumeratePatternMatches(ctx context.Context, pp *ast.PathPattern, row expr.RowContext, cb func(expr.RowContext) error) error {
	adj := pe.g.AdjList()
	mapper := adj.Mapper()

	startNode := pp.Head.Node
	var startIDs []graph.NodeID
	if startNode != nil && startNode.Variable != nil {
		varName := *startNode.Variable
		if v, ok := row[varName]; ok {
			id, resolved := nodeIDFromValue(v, mapper)
			if !resolved {
				return nil
			}
			startIDs = []graph.NodeID{id}
		} else {
			startIDs = allNodeIDs(mapper)
		}
	} else {
		startIDs = allNodeIDs(mapper)
	}

	refs := pe.sameHopRefsOf(pp)
	lateStart := refs != nil && refs.start
	var startProps *nodePropValues
	if !lateStart {
		var ok bool
		var err error
		if startProps, ok, err = pe.resolveNodeProps(ctx, startNode, row); err != nil || !ok {
			return err
		}
	}
	steps := collectSteps(pp.Head)
	var late []bool
	if refs != nil {
		late = refs.hops
	}
	used := newRelPath(steps)
	for _, sid := range startIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if lateStart {
			// The start node's map reads the start node itself: bind it first
			// (rmp #2916).
			base := cloneRow(row)
			base[*startNode.Variable] = nodeValueForID(pe.g, sid)
			props, ok, err := pe.resolveNodeProps(ctx, startNode, base)
			if err != nil {
				return err
			}
			if !ok || !pe.checkStartNode(startNode, props, sid, row) {
				continue
			}
			if err := pe.enumerateSteps(ctx, sid, steps, late, base, used, cb); err != nil {
				return err
			}
			continue
		}
		if !pe.checkStartNode(startNode, startProps, sid, row) {
			continue
		}
		base := cloneRow(row)
		if startNode != nil && startNode.Variable != nil {
			base[*startNode.Variable] = nodeValueForID(pe.g, sid)
		}
		if err := pe.enumerateSteps(ctx, sid, steps, late, base, used, cb); err != nil {
			return err
		}
	}
	return nil
}

// enumerateSteps recursively walks the remaining hop list, extending
// the running RowContext with each hop's bindings. When the list is
// empty the callback is invoked with the accumulated row.
//
// used holds the relationships the path has crossed so far (see [relPath]); a
// candidate already on it is skipped, which is openCypher's relationship
// isomorphism (rmp #2895).
//
// late is aligned with steps and marks each hop whose property maps read the
// hop's own relationship or end node ([sameHopRefs]); nil marks none. It is a
// parallel slice rather than a [step] field so that step stays four words: a
// one-hop step slice then fits the stack buffer the compiler gives a small
// non-escaping make in [patternEvaluator.matchPattern].
func (pe *patternEvaluator) enumerateSteps(ctx context.Context, srcID graph.NodeID, steps []step, late []bool, row expr.RowContext, used relPath, cb func(expr.RowContext) error) error {
	if len(steps) == 0 {
		return cb(row)
	}
	s := steps[0]
	isLate := len(late) > 0 && late[0]
	var restLate []bool
	if len(late) > 0 {
		restLate = late[1:]
	}
	// A late hop's maps read the hop's own variables, so they are resolved per
	// candidate, once those are bound ([patternEvaluator.lateHopQualifies]); until
	// then s carries none and the candidates are filtered by type and label only.
	if s.hasProps() && !isLate {
		var ok bool
		var err error
		if s, ok, err = pe.resolveStep(ctx, s, row); err != nil || !ok {
			return err
		}
	}
	remaining := steps[1:]
	if s.isVarLen() {
		return pe.enumerateVarLen(ctx, srcID, s, isLate, remaining, restLate, row, used, cb)
	}
	if s.hasRelVar() {
		if c, bound, found := pe.boundRelHop(srcID, s, row); bound {
			inst := c.instance()
			dstID := c.traversalDst()
			if !found || relUsed(used, inst) || !pe.checkEndNode(s, dstID, row) {
				return nil
			}
			next := cloneRow(row)
			if s.node != nil && s.node.Variable != nil {
				next[*s.node.Variable] = nodeValueForID(pe.g, dstID)
			}
			if isLate {
				if ok, err := pe.lateHopQualifies(ctx, s, c, dstID, next); err != nil || !ok {
					return err
				}
			}
			return pe.enumerateSteps(ctx, dstID, remaining, restLate, next, pushRel(used, inst), cb)
		}
	}

	mapper := pe.g.AdjList().Mapper()
	srcKey, ok := mapper.Resolve(srcID)
	if !ok {
		return nil
	}
	dir := ast.RelDirectionOutgoing
	if s.rel != nil {
		dir = s.rel.Direction
	}

	var candidates []candidateHop
	switch dir {
	case ast.RelDirectionOutgoing:
		candidates = pe.collectOutgoingCandidates(srcID, srcKey, s)
	case ast.RelDirectionIncoming:
		// A self-loop IS an incoming edge and must be enumerated here.
		in, err := pe.collectIncomingCandidates(ctx, srcID, srcKey, s, true)
		if err != nil {
			return err
		}
		candidates = in
	default:
		// Undirected: the outgoing collector has already emitted every
		// self-loop, and openCypher matches each relationship of an
		// undirected pattern exactly ONCE, so the incoming leg must not
		// emit it a second time.
		in, err := pe.collectIncomingCandidates(ctx, srcID, srcKey, s, false)
		if err != nil {
			return err
		}
		candidates = append(pe.collectOutgoingCandidates(srcID, srcKey, s), in...)
	}
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		inst := c.instance()
		if relUsed(used, inst) {
			continue
		}
		// Advance to the TRAVERSAL destination, not to the hop's stored dstID.
		// The two coincide on a forward leg but are opposite on the reverse leg
		// of an incoming / undirected hop, where the anchor is the stored dstID
		// and the neighbour we are walking to is the stored srcID (rmp #2505).
		dstID := c.traversalDst()
		if !pe.checkEndNode(s, dstID, row) {
			continue
		}
		next := cloneRow(row)
		if s.node != nil && s.node.Variable != nil {
			next[*s.node.Variable] = nodeValueForID(pe.g, dstID)
		}
		if s.rel != nil && s.rel.Variable != nil {
			next[*s.rel.Variable] = relValueFromHop(pe.g, c, s.rel)
		}
		if isLate {
			ok, err := pe.lateHopQualifies(ctx, s, c, dstID, next)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
		}
		if err := pe.enumerateSteps(ctx, dstID, remaining, restLate, next, pushRel(used, inst), cb); err != nil {
			return err
		}
	}
	return nil
}

// lateHopQualifies evaluates the property maps of the late hop s against next —
// the row with the hop's relationship and end node bound — and reports whether
// the relationship c crosses and its end node dstID satisfy them (rmp #2916). s
// is unresolved: the caller has applied the type, label and bound-variable
// filters only.
func (pe *patternEvaluator) lateHopQualifies(ctx context.Context, s step, c candidateHop, dstID graph.NodeID, next expr.RowContext) (bool, error) {
	rs, ok, err := pe.resolveStep(ctx, s, next)
	if err != nil || !ok {
		return false, err
	}
	if !pe.slotPropsMatch(c.srcID, c.dstID, c.srcKey, c.dstKey, c.handle, rs.props) {
		return false, nil
	}
	return rs.nodeProps == nil || pe.checkNodePattern(rs.node, rs.nodeProps, dstID), nil
}

// candidateHop describes one (rel, dst) traversal candidate found by
// enumerateSteps. handle is the stable per-edge handle stamped on the specific
// adjacency slot this candidate came from (0 when the graph carries no handles,
// e.g. a simple-graph or pre-handle storage). It lets [relValueFromHop] report
// the type of THIS parallel instance rather than the whole pair's deterministic
// pick, so an untyped `[r]` over a multi-type parallel pair enumerates each
// instance's own type(r) (rmp #2017).
//
// # Orientation contract
//
// srcID/srcKey and dstID/dstKey are ALWAYS recorded in STORAGE order — the
// edge's CREATE direction — never in traversal order, and forward says which
// way the traversal crossed it. Storage order is what the by-handle type and
// property stores are keyed by, and what startNode/endNode must report (the
// #2504 contract), so recording it here lets [relValueFromHop] read all three
// off the hop with no orientation guesswork. The price is that consumers which
// need the node the traversal ADVANCES to must ask for it: use
// [candidateHop.traversalDst], never dstID (rmp #2505).
//
// slot is the position of this relationship in srcID's adjacency entry, so
// (srcID, slot) is its identity in the view — see [relInstance].
type candidateHop struct {
	srcKey, dstKey string
	srcID, dstID   graph.NodeID
	handle         uint64
	slot           int
	forward        bool
}

// instance returns the identity of the relationship this hop crosses.
func (c candidateHop) instance() relInstance { return relInstance{src: c.srcID, slot: c.slot} }

// traversalDst returns the node this hop advances the traversal to. Because the
// hop records STORAGE order, that is dstID on a forward leg and srcID on the
// reverse leg of an incoming / undirected hop — where dstID is the anchor the
// traversal came FROM. Reading dstID directly on a reverse leg re-binds the
// anchor and leaves the walk standing still, which is precisely the defect
// rmp #2505 fixed.
func (c candidateHop) traversalDst() graph.NodeID {
	if c.forward {
		return c.dstID
	}
	return c.srcID
}

func (pe *patternEvaluator) collectOutgoingCandidates(srcID graph.NodeID, srcKey string, s step) []candidateHop {
	mapper := pe.g.AdjList().Mapper()
	// EntryView resolves every column of the entry AT THIS VIEW'S INSTANT, and
	// from one atomically-published entry, so the handle column is both
	// snapshot-correct and guaranteed the same length as the neighbour column
	// (rmp #2294). handles is nil when the graph stores no handles; a missing
	// handle degrades to 0, which relValueFromHop resolves via the per-pair
	// fallback.
	view := pe.g.EntryView(srcID)
	nbs, handles := view.Neighbours, view.Handles
	out := make([]candidateHop, 0, len(nbs))
	for i, dstID := range nbs {
		dstKey, ok := mapper.Resolve(dstID)
		if !ok {
			continue
		}
		if !pe.edgeMatchesRel(srcKey, dstKey, s.rel) {
			continue
		}
		var handle uint64
		if i < len(handles) {
			handle = handles[i]
		}
		if !pe.slotQualifies(srcID, dstID, srcKey, dstKey, handle, s) {
			continue
		}
		out = append(out, candidateHop{srcID: srcID, dstID: dstID, srcKey: srcKey, dstKey: dstKey, handle: handle, slot: i, forward: true})
	}
	return out
}

// slotMatchesRelType narrows the per-PAIR verdict of [edgeMatchesRel] to THIS
// parallel slot. edgeMatchesRel answers an existence question — does the pair
// carry at least one edge of an accepted type — which is exactly right for the
// existential WHERE predicate but too weak for enumeration: over a multi-type
// parallel pair such as (a)-[:KNOWS]->(b) / (a)-[:LIKES]->(b) it admits BOTH
// slots for a `[r:KNOWS]` hop, so the comprehension emitted one row per parallel
// edge where the MATCH baseline emits one per edge of the requested type
// (rmp #2505).
//
// The narrowing applies only where it is decidable: with no type filter every
// slot qualifies, and a slot whose handle carries no per-instance label record
// (handle 0 on simple-graph / pre-handle storage, or a Go-API edge that stamped
// a handle without recording a type) cannot be distinguished from its siblings,
// so the per-pair verdict already reached stands. Both fallbacks preserve the
// pre-#2505 behaviour exactly for graphs that have no per-instance types to
// disagree about.
func (pe *patternEvaluator) slotMatchesRelType(srcID, dstID graph.NodeID, handle uint64, rel *ast.RelationshipPattern) bool {
	if rel == nil || len(rel.Types) == 0 || handle == 0 {
		return true
	}
	instanceLabels := pe.g.EdgeLabelsByHandleID(srcID, dstID, handle)
	if len(instanceLabels) == 0 {
		return true
	}
	for _, label := range instanceLabels {
		for _, t := range rel.Types {
			if label == t {
				return true
			}
		}
	}
	return false
}

// collectIncomingCandidates enumerates every adjacency slot pointing AT dstID.
//
// includeSelfLoop decides whether dstID's own loops count. A pure incoming hop
// `(n)<-[r]-(x)` matches a self-loop — the MATCH baseline does, so the
// comprehension must — but the undirected composition must not re-emit a loop
// the outgoing collector has already produced, because openCypher matches each
// relationship of an undirected pattern exactly once (rmp #2505).
//
// One candidate is emitted PER parallel slot pointing at dstID (not just the
// first), each carrying its own handle, so an untyped `[r]` incoming hop over a
// multi-type parallel pair enumerates every instance — mirroring the outgoing
// path and the primary Expand path (rmp #2017). The slots are found by
// [patternEvaluator.scanIncoming] and filtered only after its walk has returned
// (rmp #2896).
func (pe *patternEvaluator) collectIncomingCandidates(ctx context.Context, dstID graph.NodeID, dstKey string, s step, includeSelfLoop bool) ([]candidateHop, error) {
	hits, err := pe.scanIncoming(ctx, dstID, includeSelfLoop, true, nil)
	if err != nil {
		return nil, err
	}
	var out []candidateHop
	for _, h := range hits {
		if !pe.edgeMatchesRel(h.key, dstKey, s.rel) {
			continue
		}
		// The slot is stored h.id → dstID, so the per-instance type lookup uses
		// that orientation even though the traversal crosses it backwards (see
		// [candidateHop]'s orientation contract).
		if !pe.slotQualifies(h.id, dstID, h.key, dstKey, h.handle, s) {
			continue
		}
		out = append(out, candidateHop{srcID: h.id, dstID: dstID, srcKey: h.key, dstKey: dstKey, handle: h.handle, slot: h.slot, forward: false})
	}
	return out, nil
}

// incomingSlot is one adjacency slot id → dst found by
// [patternEvaluator.scanIncoming]. key is id's interned key, captured during the
// walk so that no later step has to resolve it.
type incomingSlot struct {
	key    string
	id     graph.NodeID
	handle uint64
	slot   int
}

// scanIncoming returns the adjacency slots pointing AT dstID, in
// [graph.Mapper.Walk] order: every such slot when everySlot is set, otherwise
// the first slot of each source. dstID's own loops are included only when
// includeSelf is set, and a source in skip is not read at all.
//
// # The callback only collects (rmp #2896)
//
// Walk holds each shard's read lock for the whole of that shard's iteration,
// and its contract forbids the callback from re-entering the Mapper while a
// writer may run: once a concurrent Intern queues on the shard's write lock,
// sync.RWMutex admits no new reader, so a nested read lock on the walked shard
// blocks the callback, the writer, and every later operation on the shard. The
// callers used to test the edge type inside the callback, and
// [lpg.ReadView.EdgeLabels] / [lpg.ReadView.HasEdge] look the source key up in
// the Mapper — on the very shard being walked, because that is the key's own
// shard. So the callback now reads nothing but the source's adjacency entry,
// which lives outside the Mapper, and every other test runs after Walk has
// released its last lock. The walked set, the entries read and the snapshot they
// are read at are the same as before, so what is visible is unchanged; keys are
// interned once and never change, so the captured key is the one a later
// Resolve would return.
func (pe *patternEvaluator) scanIncoming(ctx context.Context, dstID graph.NodeID, includeSelf, everySlot bool, skip map[graph.NodeID]struct{}) ([]incomingSlot, error) {
	var hits []incomingSlot
	var err error
	pe.g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if err = ctx.Err(); err != nil {
			return false
		}
		if id == dstID && !includeSelf {
			return true
		}
		if _, seen := skip[id]; seen {
			return true
		}
		view := pe.g.EntryView(id)
		for i, nb := range view.Neighbours {
			if nb != dstID {
				continue
			}
			hits = append(hits, incomingSlot{key: key, id: id, handle: handleAt(view.Handles, i), slot: i})
			if !everySlot {
				break
			}
		}
		return true
	})
	return hits, err
}

// handleAt returns slot i's stable handle, or 0 when the entry stores none.
func handleAt(handles []uint64, i int) uint64 {
	if i < len(handles) {
		return handles[i]
	}
	return 0
}

// relInstance identifies one relationship of the view: its storage-order source
// and the relationship's position in that source's adjacency entry. In a directed
// adjacency every relationship occupies exactly one slot of exactly one entry,
// and one evaluation reads every entry at one instant, so two slots are the same
// relationship exactly when their instances are equal. The identity needs no
// handle, so it holds equally on storage that stamps none.
type relInstance struct {
	src  graph.NodeID
	slot int
}

// relPath is the stack of relationships a partial match has crossed, used to
// enforce openCypher's relationship isomorphism: one relationship may fill at
// most one relationship slot of a pattern (the TCK rejects a re-used
// relationship variable with RelationshipUniquenessViolation, Match3 [29], and
// relies on the rule inside a pattern predicate, Pattern1 [10] and [18]) — rmp
// #2895.
//
// A pattern of one fixed hop cannot re-use a relationship, so [newRelPath]
// returns nil for it and the single-hop paths keep their pair-based shortcuts
// untouched. Any longer pattern gets a path pre-sized to its fixed hops plus a
// margin per variable-length hop. A variable-length hop records every
// relationship it crosses — taking a path from [ensureRelPath] when it is the
// whole pattern — so uniqueness holds between it and every other hop of the
// pattern (rmp #2898).
type relPath = []relInstance

// newRelPath returns the path to thread through a match of steps: nil when the
// pattern is a single hop (or none), else an empty path with room for every fixed
// hop and relPathVarLenReserve relationships per variable-length hop. A pattern
// that is one variable-length hop gets its path from [ensureRelPath] only when it
// needs the exact search, so the reachability fast path allocates none.
func newRelPath(steps []step) relPath {
	if len(steps) < 2 {
		return nil
	}
	n := len(steps)
	for _, s := range steps {
		if s.isVarLen() {
			n += relPathVarLenReserve
		}
	}
	return make(relPath, 0, n)
}

// ensureRelPath returns used, or a fresh tracked path when used tracks nothing:
// a variable-length hop must record the relationships it crosses even when it is
// the whole pattern.
func ensureRelPath(used relPath) relPath {
	if cap(used) == 0 {
		return make(relPath, 0, relPathVarLenReserve)
	}
	return used
}

// relPathVarLenReserve is the capacity [newRelPath] reserves per variable-length
// hop, so that paths up to this length never grow the backing array. A longer path
// grows it by append, which stays sound: each extension is consumed by its
// recursion before a sibling can overwrite it.
const relPathVarLenReserve = 8

// relUsed reports whether r is already on used. Paths are a handful of hops, so
// a linear scan beats any set.
func relUsed(used relPath, r relInstance) bool {
	for _, u := range used {
		if u == r {
			return true
		}
	}
	return false
}

// pushRel returns used with r appended, or used unchanged when uniqueness is not
// tracked (a nil path). A path sibling hops extend at the same depth shares one
// backing array, which is sound because each extension is consumed by its
// recursion before the next sibling overwrites it.
func pushRel(used relPath, r relInstance) relPath {
	if cap(used) == 0 {
		return used
	}
	return append(used, r)
}

// cloneRow returns a shallow copy of row so the callback never mutates
// the caller's map.
func cloneRow(row expr.RowContext) expr.RowContext {
	out := make(expr.RowContext, len(row)+2)
	for k, v := range row {
		out[k] = v
	}
	return out
}

// nodeValueForID materialises an expr.NodeValue for nodeID using the
// live graph's labels and properties. Returns a bare NodeValue with
// only the ID populated when the mapper cannot resolve the id.
func nodeValueForID(g *lpg.ReadView[string, float64], id graph.NodeID) expr.NodeValue {
	mapper := g.AdjList().Mapper()
	key, ok := mapper.Resolve(id)
	if !ok {
		return expr.NodeValue{ID: uint64(id)}
	}
	labels := append([]string(nil), g.NodeLabels(key)...)
	var props expr.MapValue
	if raw := g.NodeProperties(key); len(raw) > 0 {
		props = make(expr.MapValue, len(raw))
		for k, pv := range raw {
			props[k] = lpgPropToExpr(pv)
		}
	}
	return expr.NodeValue{ID: uint64(id), Labels: labels, Properties: props}
}

// relValueFromHop materialises an expr.RelationshipValue for a single hop
// produced by enumerateSteps.
//
// Every field is read in the hop's STORAGE orientation (see the
// [candidateHop] orientation contract), which is what the three in-tree
// reference materialisers — buildRelationshipValueFromRow, resolveHopRel and
// the Expand operator behind the RollUpApply route — all report:
//
//   - ID is the stable per-edge handle. Since rmp #2317 the handle IS the
//     relationship identity in both traversal directions, so the same physical
//     edge reached forwards and backwards compares equal and id(r) agrees with
//     the MATCH baseline. Leaving ID unset made id(r) report 0 for EVERY hop on
//     this route, in both directions (rmp #2505).
//   - StartID/EndID are the stored endpoints, NOT the traversal endpoints. A
//     reverse leg used to transpose them, so `(c)<-[r]-(x)` reported
//     startNode(r)=c / endNode(r)=x — the exact inverse of the orientation
//     rmp #2504 pinned and cypher/tck/features/clauses/merge/Merge5.feature
//     asserts.
//   - Properties come from the bound instance's by-handle bag when the pair has
//     one, falling back to the per-pair coalesced union otherwise. Reading them
//     under the transposed keys returned nothing at all, so every reverse hop
//     reported null properties.
func relValueFromHop(g *lpg.ReadView[string, float64], hop candidateHop, rel *ast.RelationshipPattern) expr.RelationshipValue {
	var relTypes []string
	if rel != nil {
		relTypes = rel.Types
	}
	// Resolve the type of THIS parallel instance by its stable handle when one
	// is available: [Graph.EdgeLabelsByHandleID] returns only the labels
	// recorded for hop.handle on the stored (srcID → dstID) pair, so each
	// parallel slot reports its own type. This makes an untyped `[r]` hop over a
	// multi-type parallel pair such as (a)-[:FIRST]->(b) / (a)-[:SECOND]->(b)
	// enumerate FIRST for one instance and SECOND for the other, rather than the
	// single deterministic per-pair pick both instances used to collapse onto
	// (rmp #2017). The handle store is keyed by the STORED edge direction
	// (hop.srcID → hop.dstID), which is the orientation the hop already records,
	// so no swap is involved.
	//
	// The per-pair union is the fallback for a handle-less slot (handle 0 —
	// simple-graph / pre-handle storage) or a handle that carries no per-instance
	// label: [Graph.EdgeLabels] returns the UNION of the types over all parallel
	// edges between the pair, and pickEdgeType prefers a label the pattern's type
	// filter accepts, else the deterministic alphabetically-smallest label. A
	// typed `[r:SECOND]` hop still reports SECOND via that pick even without a
	// handle (rmp #2016).
	instanceLabels := g.EdgeLabelsByHandleID(hop.srcID, hop.dstID, hop.handle)
	var typeName string
	if len(instanceLabels) > 0 {
		typeName = pickEdgeType(instanceLabels, relTypes)
	} else {
		typeName = pickEdgeType(g.EdgeLabels(hop.srcKey, hop.dstKey), relTypes)
	}
	return expr.RelationshipValue{
		ID:         hop.handle,
		StartID:    uint64(hop.srcID),
		EndID:      uint64(hop.dstID),
		Type:       typeName,
		Properties: relPropsFromHop(g, hop, len(instanceLabels) > 0),
	}
}

// relPropsFromHop resolves the property map of the ONE parallel instance the
// hop bound, mirroring the routing ladder [buildEdgeProps] applies on the
// primary Expand path so both routes report the same map for the same edge.
//
// hasByHandleLabel is the membership signal, carried in from the type
// resolution the caller has already done: Cypher CREATE/MERGE always records
// the mandatory relationship TYPE by handle, so a by-handle label entry marks a
// per-instance edge even when it holds no properties at all — which is what
// keeps a zero-property parallel edge from leaking a propertied sibling's keys.
// A non-zero handle alone is NOT sufficient, because the public Go API stamps a
// handle on the slot while writing only the per-pair store (rmp #1684).
//
// The by-handle property probe is skipped when the graph has never recorded one
// (rmp #2387): the latch being false proves the store is empty, so the probe
// could only return nil and the decision reduces to hasByHandleLabel.
func relPropsFromHop(g *lpg.ReadView[string, float64], hop candidateHop, hasByHandleLabel bool) expr.MapValue {
	if hop.handle != 0 {
		var byHandle expr.MapValue
		if hasByHandleLabel || g.AnyEdgeHandlePropertyEverWritten() {
			byHandle = edgePropsByHandleToExprMap(g, hop.srcKey, hop.dstKey, hop.handle)
		}
		if hasByHandleLabel || len(byHandle) > 0 {
			return byHandle
		}
	}
	// Per-pair fallback: stream the coalesced properties straight into the expr
	// map (M2 / #1662), dropping the transient lpg map the prior two-step build
	// allocated per hop.
	return edgePropsToExprMap(g, hop.srcKey, hop.dstKey)
}

// matchPattern returns true iff at least one path in the graph matches pp
// given the bindings in row.
func (pe *patternEvaluator) matchPattern(ctx context.Context, pp *ast.PathPattern, row expr.RowContext) (bool, error) {
	adj := pe.g.AdjList()
	mapper := adj.Mapper()

	// Resolve the start node: either bound (from RowContext) or unbound (all nodes).
	startNode := pp.Head.Node
	var startIDs []graph.NodeID
	if startNode != nil && startNode.Variable != nil {
		// Bound variable: look it up in the row.
		varName := *startNode.Variable
		if v, ok := row[varName]; ok {
			id, resolved := nodeIDFromValue(v, mapper)
			if !resolved {
				// Variable is NULL or not a node — no match.
				return false, nil
			}
			startIDs = []graph.NodeID{id}
		} else {
			// Variable not in row — treat as unbound, scan all.
			startIDs = allNodeIDs(mapper)
		}
	} else {
		// Anonymous start node: scan all nodes.
		startIDs = allNodeIDs(mapper)
	}

	// Walk the remaining hops. pp.Head is the start node; pp.Head.Next is
	// the first (rel, node) pair.
	steps := collectSteps(pp.Head)
	if len(steps) == 0 {
		// Single-node pattern — just check node labels/props for the start set.
		if startNode != nil && !pe.nodePatternFilter(startNode, row) {
			return false, nil
		}
		return len(startIDs) > 0, nil
	}

	// The existential walk never extends row, so each hop's node and
	// relationship property maps are evaluated once here, against the outer row,
	// rather than once per partial match. steps is this call's own slice, so it
	// is resolved in place.
	startProps, ok, err := pe.resolveNodeProps(ctx, startNode, row)
	if err != nil || !ok {
		return false, err
	}
	for i := range steps {
		if !steps[i].hasProps() {
			continue
		}
		s, ok, err := pe.resolveStep(ctx, steps[i], row)
		if err != nil || !ok {
			return false, err
		}
		steps[i] = s
	}
	used := newRelPath(steps)
	for _, sid := range startIDs {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !pe.checkStartNode(startNode, startProps, sid, row) {
			continue
		}
		ok, err := pe.matchSteps(ctx, sid, steps, row, used)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// step bundles a single (relationship, destination-node) hop.
//
// props is the hop's relationship property map evaluated against the row the
// hop is matched under ([patternEvaluator.resolveStep]); it is nil when the
// pattern declares none. A non-nil props makes every verdict of the hop a
// per-SLOT one, because parallel relationships between one pair may carry
// different properties (rmp #2908).
//
// nodeProps holds the values of the destination node's property map, aligned
// with its map-literal keys and evaluated against the same row
// ([patternEvaluator.resolveNodeProps]; rmp #2913); it is nil when the node
// pattern declares none.
type step struct {
	rel       *ast.RelationshipPattern
	node      *ast.NodePattern
	props     expr.MapValue
	nodeProps *nodePropValues
}

// hasProps reports whether the hop declares a relationship or a destination-node
// property map. It is the inlinable guard in front of
// [patternEvaluator.resolveStep].
func (s step) hasProps() bool {
	return (s.rel != nil && s.rel.Properties != nil) || (s.node != nil && s.node.Properties != nil)
}

// hasRelVar reports whether the hop names its relationship at all. It is the
// inlinable guard in front of [patternEvaluator.boundRelHop].
func (s step) hasRelVar() bool { return s.rel != nil && s.rel.Variable != nil }

// collectSteps builds the ordered slice of (rel, node) steps from the
// PathElement linked list, starting at el.Next (skipping the head node which
// is handled separately).
func collectSteps(head *ast.PathElement) []step {
	// Pre-sized: one allocation whatever the step size, rather than append's
	// growth sequence.
	n := 0
	for el := head.Next; el != nil; el = el.Next {
		if el.Relationship != nil {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	steps := make([]step, 0, n)
	el := head.Next
	for el != nil {
		if el.Relationship != nil {
			steps = append(steps, step{rel: el.Relationship, node: el.Node})
		}
		el = el.Next
	}
	return steps
}

// matchSteps recursively evaluates each hop in the step list starting from
// srcID, returning true when all hops produce at least one complete path.
//
// used is the [relPath] of the match so far: nil for a one-hop pattern, which
// cannot re-use a relationship, and otherwise the relationships already crossed,
// none of which a later hop may cross again (rmp #2895).
func (pe *patternEvaluator) matchSteps(ctx context.Context, srcID graph.NodeID, steps []step, row expr.RowContext, used relPath) (bool, error) {
	if len(steps) == 0 {
		return true, nil
	}
	s := steps[0]
	remaining := steps[1:]

	if s.isVarLen() {
		return pe.matchVarLen(ctx, srcID, s, remaining, row, used)
	}
	return pe.matchSingleHop(ctx, srcID, s, remaining, row, used)
}

// matchSingleHop follows a single fixed-length hop and recurses.
//
// direction × filter × recursion branches; extracted helpers bring each below 15
func (pe *patternEvaluator) matchSingleHop(ctx context.Context, srcID graph.NodeID, s step, remaining []step, row expr.RowContext, used relPath) (bool, error) {
	if !s.hasRelVar() {
		return pe.matchUnboundHop(ctx, srcID, s, remaining, row, used)
	}
	if c, bound, found := pe.boundRelHop(srcID, s, row); bound {
		inst := c.instance()
		if !found || relUsed(used, inst) || !pe.checkEndNode(s, c.traversalDst(), row) {
			return false, nil
		}
		return pe.matchSteps(ctx, c.traversalDst(), remaining, row, pushRel(used, inst))
	}
	return pe.matchUnboundHop(ctx, srcID, s, remaining, row, used)
}

// matchUnboundHop is [patternEvaluator.matchSingleHop] for a hop whose
// relationship variable, if any, is not bound on the row.
func (pe *patternEvaluator) matchUnboundHop(ctx context.Context, srcID graph.NodeID, s step, remaining []step, row expr.RowContext, used relPath) (bool, error) {
	mapper := pe.g.AdjList().Mapper()

	// Collect candidate destination node IDs based on direction.
	dir := ast.RelDirectionOutgoing // default when no direction is specified
	if s.rel != nil {
		dir = s.rel.Direction
	}

	srcKey, ok := mapper.Resolve(srcID)
	if !ok {
		return false, nil
	}

	switch dir {
	case ast.RelDirectionOutgoing:
		return pe.matchOutgoing(ctx, srcID, srcKey, s, remaining, row, used)
	case ast.RelDirectionIncoming:
		return pe.matchIncoming(ctx, srcID, srcKey, s, remaining, row, used)
	default: // undirected: check both out and in
		// A self-loop is reached by both legs under the SAME relInstance (its
		// one slot in srcID's entry), so relationship isomorphism still sees it
		// as one relationship.
		if found, err := pe.matchOutgoing(ctx, srcID, srcKey, s, remaining, row, used); err != nil || found {
			return found, err
		}
		return pe.matchIncoming(ctx, srcID, srcKey, s, remaining, row, used)
	}
}

// boundEndID resolves the bound variable of a hop's end-node pattern ONCE per
// hop evaluation, so the per-neighbour loops can reject a non-matching
// neighbour with one integer comparison instead of a versioned edge-label
// lookup (rmp #2894).
//
// bound reports whether np names a variable present in row; when it does, ok
// reports whether that value denotes a node. bound && !ok means no neighbour can
// satisfy the end node — exactly the verdict [patternEvaluator.checkEndNode]
// returns for every candidate in that case.
func (pe *patternEvaluator) boundEndID(np *ast.NodePattern, row expr.RowContext) (id graph.NodeID, bound, ok bool) {
	if np == nil || np.Variable == nil {
		return 0, false, false
	}
	v, present := row[*np.Variable]
	if !present {
		return 0, false, false
	}
	id, ok = nodeIDFromValue(v, pe.g.AdjList().Mapper())
	return id, true, ok
}

// endNodePatternOK is [patternEvaluator.checkEndNode] minus the bound-variable
// comparison, for callers that have already applied it through
// [patternEvaluator.boundEndID].
func (pe *patternEvaluator) endNodePatternOK(s step, dstID graph.NodeID) bool {
	return s.node == nil || pe.checkNodePattern(s.node, s.nodeProps, dstID)
}

// matchOutgoing iterates the outgoing neighbours of srcID and recurses for
// each neighbour that passes the edge-type and end-node filters.
//
// # Bound end node (rmp #2894)
//
// When the end node is bound — `WHERE NOT (h)-[:LINK]->(s)` with both h and s
// in scope — only slots pointing at that one node can match, so a neighbour is
// first compared by NodeID and only a matching slot pays for
// [patternEvaluator.edgeMatchesRel]'s versioned label lookup. Before this, every
// neighbour of a hub paid that lookup before the end-node check rejected it,
// which made one predicate evaluation deg(h) label lookups.
//
// The reordering is sound because both filters are pure boolean functions of
// (view, pair, row): edgeMatchesRel and the bound comparison raise no error and
// write nothing, so their conjunction is the same in either order. The
// neighbour list, edgeMatchesRel and checkNodePattern still read the same
// snapshot-bound [lpg.ReadView] as before, so visibility is unchanged.
//
// The loop also stops after the FIRST slot pointing at the bound node: every
// verdict below it — edgeMatchesRel (a per-PAIR existence question), the node
// pattern, and the recursion from that same node with the same row — is a
// function of the pair alone, so a parallel slot to the same node cannot
// answer differently. That holds only while no relationship is tracked: with a
// non-nil used the verdict also depends on WHICH slot the path crossed, so the
// multi-hop form is [patternEvaluator.matchOutgoingUnique]. Nor does it hold for
// a hop with a relationship property map, whose verdict is the slot's own
// (rmp #2908), so that hop takes the per-slot form too.
func (pe *patternEvaluator) matchOutgoing(ctx context.Context, srcID graph.NodeID, srcKey string, s step, remaining []step, row expr.RowContext, used relPath) (bool, error) {
	if used != nil || s.props != nil {
		return pe.matchOutgoingUnique(ctx, srcID, srcKey, s, remaining, row, used)
	}
	endID, endBound, endOK := pe.boundEndID(s.node, row)
	if endBound && !endOK {
		return false, nil
	}
	mapper := pe.g.AdjList().Mapper()
	neighbours := pe.g.EntryView(srcID).Neighbours
	for _, dstID := range neighbours {
		if endBound && dstID != endID {
			continue
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ok, err := pe.outgoingHopMatches(ctx, srcKey, dstID, s, remaining, row, mapper)
		if err != nil || ok {
			return ok, err
		}
		if endBound {
			return false, nil
		}
	}
	return false, nil
}

// outgoingHopMatches applies the edge-type and end-node-pattern filters to the
// slot srcKey → dstID and recurses into the remaining steps. The bound-variable
// comparison is the caller's.
func (pe *patternEvaluator) outgoingHopMatches(ctx context.Context, srcKey string, dstID graph.NodeID, s step, remaining []step, row expr.RowContext, mapper *graph.Mapper[string]) (bool, error) {
	dstKey, dstOK := mapper.Resolve(dstID)
	if !dstOK {
		return false, nil
	}
	if !pe.edgeMatchesRel(srcKey, dstKey, s.rel) {
		return false, nil
	}
	if !pe.endNodePatternOK(s, dstID) {
		return false, nil
	}
	return pe.matchSteps(ctx, dstID, remaining, row, nil)
}

// matchOutgoingUnique is [patternEvaluator.matchOutgoing] for a pattern of two
// or more hops, where relationship isomorphism applies (rmp #2895), and for a hop
// with a relationship property map (rmp #2908; used may then be nil). Each slot
// is a distinct relationship, so a slot already on used is skipped, the type and
// the property map are tested on THE SLOT ([patternEvaluator.slotQualifies]) as
// well as the type on the pair, and a parallel slot to the same node is still
// tried when the first one cannot complete the path. The bound end node is still
// compared by NodeID before any lookup.
func (pe *patternEvaluator) matchOutgoingUnique(ctx context.Context, srcID graph.NodeID, srcKey string, s step, remaining []step, row expr.RowContext, used relPath) (bool, error) {
	endID, endBound, endOK := pe.boundEndID(s.node, row)
	if endBound && !endOK {
		return false, nil
	}
	mapper := pe.g.AdjList().Mapper()
	view := pe.g.EntryView(srcID)
	for i, dstID := range view.Neighbours {
		if endBound && dstID != endID {
			continue
		}
		inst := relInstance{src: srcID, slot: i}
		if relUsed(used, inst) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		dstKey, ok := mapper.Resolve(dstID)
		if !ok {
			continue
		}
		if !pe.edgeMatchesRel(srcKey, dstKey, s.rel) || !pe.slotQualifies(srcID, dstID, srcKey, dstKey, handleAt(view.Handles, i), s) {
			continue
		}
		if !pe.endNodePatternOK(s, dstID) {
			continue
		}
		found, err := pe.matchSteps(ctx, dstID, remaining, row, pushRel(used, inst))
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

// matchIncoming scans all nodes for those that have an outgoing edge to dstID
// (= the current "source" in the traversal direction), satisfying the rel
// pattern and end-node constraints, and recurses.
//
// dstID's own self-loops are included: a self-loop is a real incoming edge, so
// `WHERE (n)<-[:R]-(:L)` must hold for a node whose only :R edge is a loop —
// which is what both MATCH and EXISTS { } already answered. Skipping self here
// made the existential predicate the lone dissenter (rmp #2505). The recursion
// still terminates on a loop because each step consumes one entry of remaining.
//
// # Bound end node (rmp #2894)
//
// When the end node is bound, the only candidate the full scan could accept is
// that node — [patternEvaluator.checkEndNode] rejects every other — so the scan
// is replaced by a read of that one node's out-slots, through the same
// snapshot-bound [lpg.ReadView.EntryView] the scan reads. The unversioned
// in-edge index is deliberately NOT used. [graph.Mapper.Walk] visits exactly the
// NodeIDs [graph.Mapper.Resolve] resolves, so a bound node the scan would never
// have visited is rejected here by the failed Resolve.
//
// # Unbound end node (rmp #2896)
//
// The scan used to be a [graph.Mapper.Walk] whose callback tested the edge type
// by key and recursed, re-entering the Mapper on the shard it held — the
// deadlock [patternEvaluator.scanIncoming] documents. It now iterates NodeIDs
// below [graph.Mapper.MaxNodeID] with no Mapper lock held, reading each entry
// through the same snapshot-bound [lpg.ReadView.EntryView], and resolves a key
// only for a slot that points at dstID. Unlike collecting the slots first, this
// keeps the early exit on the first match. Without tracked relationships the
// first slot of each source decides, exactly as for the bound end node.
func (pe *patternEvaluator) matchIncoming(ctx context.Context, dstID graph.NodeID, dstKey string, s step, remaining []step, row expr.RowContext, used relPath) (bool, error) {
	mapper := pe.g.AdjList().Mapper()
	if endID, endBound, endOK := pe.boundEndID(s.node, row); endBound {
		if !endOK {
			return false, nil
		}
		candidateKey, resolved := mapper.Resolve(endID)
		if !resolved {
			return false, nil
		}
		view := pe.g.EntryView(endID)
		for i, nb := range view.Neighbours {
			if nb != dstID {
				continue
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if used == nil && s.props == nil {
				// The first slot endID → dstID decides: every verdict below is a
				// function of the pair (see [patternEvaluator.matchOutgoing]).
				if !pe.edgeMatchesRel(candidateKey, dstKey, s.rel) || !pe.endNodePatternOK(s, endID) {
					return false, nil
				}
				return pe.matchSteps(ctx, endID, remaining, row, nil)
			}
			hit := incomingSlot{key: candidateKey, id: endID, handle: handleAt(view.Handles, i), slot: i}
			found, err := pe.incomingSlotMatches(ctx, hit, dstID, dstKey, s, remaining, row, used)
			if err != nil || found {
				return found, err
			}
		}
		return false, nil
	}
	// Every interned NodeID is below MaxNodeID, and an id that does not resolve
	// is skipped, so the loop reads the same entries a Walk would — in NodeID
	// order instead of shard order, which an existential verdict does not depend
	// on. A node interned after MaxNodeID was read is not visited; its arcs
	// would belong to a write concurrent with this read, which no ordering
	// guarantee covers.
	for id, maxID := graph.NodeID(0), mapper.MaxNodeID(); id < maxID; id++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		view := pe.g.EntryView(id)
		for i, nb := range view.Neighbours {
			if nb != dstID {
				continue
			}
			key, resolved := mapper.Resolve(id)
			if !resolved {
				break
			}
			hit := incomingSlot{key: key, id: id, handle: handleAt(view.Handles, i), slot: i}
			found, err := pe.incomingSlotMatches(ctx, hit, dstID, dstKey, s, remaining, row, used)
			if err != nil || found {
				return found, err
			}
			if used == nil && s.props == nil {
				break // the first slot id → dstID decides: every verdict is the pair's
			}
		}
	}
	return false, nil
}

// incomingSlotMatches applies the edge-type and end-node filters to the slot
// hit.id → dstID and recurses into the remaining steps from hit.id. With a
// non-nil used the slot must not already be on the path and its own type must
// qualify (rmp #2895); with a relationship property map its type and properties
// must (rmp #2908); without either the verdict is the pair's.
func (pe *patternEvaluator) incomingSlotMatches(ctx context.Context, hit incomingSlot, dstID graph.NodeID, dstKey string, s step, remaining []step, row expr.RowContext, used relPath) (bool, error) {
	inst := relInstance{src: hit.id, slot: hit.slot}
	if relUsed(used, inst) {
		return false, nil
	}
	if !pe.edgeMatchesRel(hit.key, dstKey, s.rel) {
		return false, nil
	}
	if (used != nil || s.props != nil) && !pe.slotQualifies(hit.id, dstID, hit.key, dstKey, hit.handle, s) {
		return false, nil
	}
	if !pe.checkEndNode(s, hit.id, row) {
		return false, nil
	}
	return pe.matchSteps(ctx, hit.id, remaining, row, pushRel(used, inst))
}

// edgeMatchesRel reports whether the directed edge (srcKey → dstKey) satisfies
// the relationship pattern rel. When rel is nil or has no type constraints, all
// edges match.
func (pe *patternEvaluator) edgeMatchesRel(srcKey, dstKey string, rel *ast.RelationshipPattern) bool {
	pe.relTypeChecks++
	if rel == nil || len(rel.Types) == 0 {
		// No type constraint — any edge matches (but the edge must exist AT THIS
		// VIEW'S INSTANT; ReadView.HasEdge is the versioned form, rmp #2294).
		return pe.g.HasEdge(srcKey, dstKey)
	}
	labels := pe.g.EdgeLabels(srcKey, dstKey)
	if len(labels) == 0 {
		return false
	}
	// openCypher OR semantics: the pattern matches when the pair
	// (srcKey → dstKey) carries at least one edge whose relationship type is
	// among rel.Types. [Graph.EdgeLabels] returns the UNION of the types over
	// all parallel edges between the pair, in unspecified order, so every
	// label must be tested — not only labels[0]. A multigraph pair such as
	// (a)-[:FIRST]->(b) and (a)-[:SECOND]->(b) reports both types, and a
	// `[:SECOND]` predicate must match even when SECOND is not the first entry
	// (rmp #2016: the pre-fix labels[0] check reported every non-first type as
	// non-existent).
	for _, edgeLabel := range labels {
		for _, t := range rel.Types {
			if edgeLabel == t {
				return true
			}
		}
	}
	return false
}

// resolveStep evaluates the relationship property map of s against row, the row
// the hop is matched under, and returns s carrying it (rmp #2908). ok is false
// when the map cannot be satisfied by any relationship: a map expression that
// does not evaluate to a map (a NULL parameter included). An empty map
// constrains nothing, so s is returned unchanged for it as for no map at all.
//
// Each value is evaluated once per hop evaluation, not once per candidate slot,
// so the per-slot test in [patternEvaluator.slotPropsMatch] is a plain
// comparison. A NULL value is kept: it equals nothing, so the hop matches no
// relationship, which is what MATCH answers for `[:R {w: null}]`.
func (pe *patternEvaluator) resolveStep(ctx context.Context, s step, row expr.RowContext) (step, bool, error) {
	nodeProps, ok, err := pe.resolveNodeProps(ctx, s.node, row)
	if err != nil || !ok {
		return s, false, err
	}
	s.nodeProps = nodeProps
	if s.rel == nil || s.rel.Properties == nil {
		return s, true, nil
	}
	if ml, isLit := s.rel.Properties.(*ast.MapLiteral); isLit {
		if len(ml.Keys) == 0 {
			return s, true, nil
		}
		props := make(expr.MapValue, len(ml.Keys))
		for i, k := range ml.Keys {
			v, err := expr.EvalWith(ctx, ml.Values[i], row, pe.params, pe.reg, pe.subEval, pe)
			if err != nil {
				return s, false, err
			}
			props[k] = v
		}
		s.props = props
		return s, true, nil
	}
	v, err := expr.EvalWith(ctx, s.rel.Properties, row, pe.params, pe.reg, pe.subEval, pe)
	if err != nil {
		return s, false, err
	}
	m, isMap := v.(expr.MapValue)
	if !isMap {
		return s, false, nil
	}
	if len(m) > 0 {
		s.props = m
	}
	return s, true, nil
}

// slotQualifies reports whether the ONE relationship stored in slot handle of
// srcID → dstID satisfies the hop's type filter ([patternEvaluator.slotMatchesRelType])
// and its relationship property map ([patternEvaluator.slotPropsMatch]). The
// caller has already established the per-pair type verdict.
func (pe *patternEvaluator) slotQualifies(srcID, dstID graph.NodeID, srcKey, dstKey string, handle uint64, s step) bool {
	return pe.slotMatchesRelType(srcID, dstID, handle, s.rel) && pe.slotPropsMatch(srcID, dstID, srcKey, dstKey, handle, s.props)
}

// slotPropsMatch reports whether the relationship in slot handle of the stored
// pair srcID → dstID carries every property of props with an equal value
// (rmp #2908). It is a per-INSTANCE test: two parallel relationships between one
// pair may carry different properties, and only the one the slot holds counts.
//
// The store it reads is the one [relPropsFromHop] reads for the same slot — the
// by-handle bag when the instance has one, the per-pair store otherwise — so a
// hop's property map filters exactly the properties `r.k` reports for the
// relationship the hop binds. Values are compared with openCypher equality, so
// a missing property or a NULL wanted value never matches.
func (pe *patternEvaluator) slotPropsMatch(srcID, dstID graph.NodeID, srcKey, dstKey string, handle uint64, props expr.MapValue) bool {
	if len(props) == 0 {
		return true
	}
	byHandle := false
	if handle != 0 {
		byHandle = len(pe.g.EdgeLabelsByHandleID(srcID, dstID, handle)) > 0 ||
			(pe.g.AnyEdgeHandlePropertyEverWritten() && len(pe.g.EdgePropertiesByHandle(srcKey, dstKey, handle)) > 0)
	}
	for k, want := range props {
		var (
			pv    lpg.PropertyValue
			found bool
		)
		if byHandle {
			pv, found = pe.g.EdgePropertyByHandle(srcKey, dstKey, handle, k)
		} else {
			pv, found = pe.g.GetEdgeProperty(srcKey, dstKey, k)
		}
		if !found || !expr.IsTruthy(lpgPropToExpr(pv).Equal(want)) {
			return false
		}
	}
	return true
}

// boundRelHop resolves a fixed hop whose relationship variable is already bound
// on row (rmp #2905). A bound variable denotes ONE relationship, so the hop may
// cross that relationship and no other: `WITH r … WHERE (a)-[r]->(b)` holds only
// when r itself runs from a to b, exactly as `MATCH (a)-[r]->(b)` with r bound.
//
// bound reports whether the variable is bound; when it is, found reports whether
// the relationship can fill this hop from cur, and c is it. It can when the value
// is a relationship, the hop's direction allows crossing it from cur (outgoing:
// cur is its start; incoming: its end; undirected: either, a self-loop once), it
// is still stored in the view, and its type and properties satisfy the hop. Any
// other value — NULL, or a value that is not a relationship — fills no hop.
//
// The relationship is located by its identity — the stable handle and the stored
// endpoints the value carries, the triple [patternEvaluator.walkBoundRelList]
// matches on — in its start node's adjacency entry, so the slot, and with it the
// [relInstance] relationship isomorphism tracks, is the one every other hop of
// the pattern sees. The lookup reads one entry, where the unbound incoming hop
// scans the graph.
func (pe *patternEvaluator) boundRelHop(cur graph.NodeID, s step, row expr.RowContext) (c candidateHop, bound, found bool) {
	// A synthetic name was minted for an anonymous relationship, which binds
	// nothing; skipping it also spares the row lookup on every anonymous hop.
	if s.rel == nil || !ir.UserNamed(s.rel.Variable) {
		return c, false, false
	}
	v, present := row[*s.rel.Variable]
	if !present {
		return c, false, false
	}
	id, start, end, isRel := relIdentity(v)
	if !isRel {
		return c, true, false
	}
	dir := hopDirection(s.rel)
	switch {
	case start == cur && dir != ast.RelDirectionIncoming:
		c.forward = true
	case end == cur && dir != ast.RelDirectionOutgoing:
		c.forward = false
	default:
		return c, true, false
	}
	mapper := pe.g.AdjList().Mapper()
	srcKey, srcOK := mapper.Resolve(start)
	dstKey, dstOK := mapper.Resolve(end)
	if !srcOK || !dstOK {
		return c, true, false
	}
	view := pe.g.EntryView(start)
	for i, nb := range view.Neighbours {
		handle := handleAt(view.Handles, i)
		if nb != end || handle != id {
			continue
		}
		if !pe.edgeMatchesRel(srcKey, dstKey, s.rel) || !pe.slotQualifies(start, end, srcKey, dstKey, handle, s) {
			return c, true, false
		}
		c.srcID, c.dstID, c.srcKey, c.dstKey, c.handle, c.slot = start, end, srcKey, dstKey, handle, i
		return c, true, true
	}
	return c, true, false
}

// relIdentity extracts the identity of a relationship value — its stable handle
// and its stored start and end nodes — from either representation a row may
// carry. ok is false for every other value, NULL included.
func relIdentity(v expr.Value) (id uint64, start, end graph.NodeID, ok bool) {
	switch r := v.(type) {
	case expr.RelationshipValue:
		return r.ID, graph.NodeID(r.StartID), graph.NodeID(r.EndID), true
	case *expr.LazyRelationshipValue:
		return r.ID(), graph.NodeID(r.StartID()), graph.NodeID(r.EndID()), true
	}
	return 0, 0, 0, false
}

// checkStartNode validates that the start node (at srcID) satisfies the
// optional labels/properties in np and is consistent with any bound variable.
//
// props holds np's property-map values as [patternEvaluator.resolveNodeProps]
// evaluated them.
func (pe *patternEvaluator) checkStartNode(np *ast.NodePattern, props *nodePropValues, srcID graph.NodeID, row expr.RowContext) bool {
	if np == nil {
		return true
	}
	// If variable is bound, it must equal srcID.
	if np.Variable != nil {
		varName := *np.Variable
		if v, ok := row[varName]; ok {
			mapper := pe.g.AdjList().Mapper()
			boundID, resolved := nodeIDFromValue(v, mapper)
			if !resolved || boundID != srcID {
				return false
			}
		}
	}
	return pe.checkNodePattern(np, props, srcID)
}

// checkEndNode validates that the candidate destination node satisfies the
// optional labels/properties of the hop's node pattern and any bound variable
// constraint. s must have passed through [patternEvaluator.resolveStep] when it
// declares a property map.
func (pe *patternEvaluator) checkEndNode(s step, dstID graph.NodeID, row expr.RowContext) bool {
	np := s.node
	if np == nil {
		return true
	}
	if np.Variable != nil {
		varName := *np.Variable
		if v, ok := row[varName]; ok {
			mapper := pe.g.AdjList().Mapper()
			boundID, resolved := nodeIDFromValue(v, mapper)
			if !resolved || boundID != dstID {
				return false
			}
		}
	}
	return pe.checkNodePattern(np, s.nodeProps, dstID)
}

// resolveNodeProps evaluates the property map of np against row, the row the
// node is matched under, with the query's parameters, function registry and
// subquery evaluator — as MATCH's planned Filter does. ok is false when the map
// can match no node: a value that raises no error but compares as NULL is kept,
// because it equals nothing and [patternEvaluator.checkNodePattern] then rejects
// every node, which is what MATCH answers for `(:N {k: null})`.
//
// It used to be evaluated inside checkNodePattern against an EMPTY row and no
// registry, so a value that read an outer variable — `WHERE (a)-->(:N {k:
// x.k})` — was NULL and matched nothing, and a function call failed and was
// swallowed as a non-match (rmp #2913).
//
// A map that is not a literal (a parameter) constrains nothing, as before, and
// an empty literal constrains nothing either; both yield nil. The values are
// returned in the order of the map literal's keys.
func (pe *patternEvaluator) resolveNodeProps(ctx context.Context, np *ast.NodePattern, row expr.RowContext) (*nodePropValues, bool, error) {
	if np == nil || np.Properties == nil {
		return nil, true, nil
	}
	ml, isLit := np.Properties.(*ast.MapLiteral)
	if !isLit || len(ml.Keys) == 0 {
		return nil, true, nil
	}
	constant := constantPropMap(ml)
	if constant {
		if props, ok := pe.constNodeProps[np]; ok {
			return props, true, nil
		}
	}
	props := &nodePropValues{keys: ml.Keys, vals: make([]expr.Value, len(ml.Values))}
	for i, e := range ml.Values {
		// pe.params (rmp #2507): StripLiterals hoists a string literal inside a
		// WHERE onto an auto-parameter, and a pattern predicate is a WHERE.
		v, err := expr.EvalWith(ctx, e, row, pe.params, pe.reg, pe.subEval, pe)
		if err != nil {
			return nil, false, err
		}
		props.vals[i] = v
	}
	if constant {
		if pe.constNodeProps == nil {
			pe.constNodeProps = make(map[*ast.NodePattern]*nodePropValues, 1)
		}
		pe.constNodeProps[np] = props
	}
	return props, true, nil
}

// nodePropValues holds the values of a node pattern's property map, aligned with
// the keys of its map literal. It is held by pointer so that [step], which is
// copied at every hop of the recursive match, stays four words.
type nodePropValues struct {
	keys []string // the map literal's keys; shared, never written
	vals []expr.Value
}

// constantPropMap reports whether every value of ml is a literal scalar or a
// parameter, so the map evaluates to the same value on every row of a query run.
func constantPropMap(ml *ast.MapLiteral) bool {
	for _, v := range ml.Values {
		switch v.(type) {
		case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral, *ast.BoolLiteral,
			*ast.NullLiteral, *ast.Parameter:
		default:
			return false
		}
	}
	return true
}

// sameHopRefs records which property maps of one pattern read a variable that
// the same element or hop binds (rmp #2916).
//
// openCypher evaluates an element's property map as the equivalent predicate on
// the element (clauses/match/Match2.feature [5], Match1.feature [4]), and MATCH —
// the parity oracle, the TCK being silent on references inside one pattern —
// evaluates it once the element and its hop are bound: a relationship map and
// its end node's map both see the hop's relationship, its end node, and every
// element before them, and a start node's map sees the start node. A variable
// bound only by a LATER hop reads as NULL there. The enumerating evaluator
// resolved every map before the hop's candidate was chosen, so a map reading the
// hop's own relationship or end node read NULL and matched nothing.
//
// start marks the start node's map reading the start node; hops[i] marks the
// i-th hop (in [collectSteps] order) whose relationship or end-node map reads
// the hop's relationship variable or end-node variable. References to earlier
// elements need no mark: [patternEvaluator.enumerateSteps] resolves a hop
// against the row that already binds them. Only the enumerating route consults
// it: a bare pattern predicate may not introduce a variable
// (expressions/pattern/Pattern1.feature [10]), so every variable the maps of
// [patternEvaluator.matchPattern] read is already on the outer row.
type sameHopRefs struct {
	start bool
	hops  []bool
}

// sameHopRefsOf returns the [sameHopRefs] of pp, or nil when no map of pp reads a
// variable its own element or hop binds, computing it once per query run.
func (pe *patternEvaluator) sameHopRefsOf(pp *ast.PathPattern) *sameHopRefs {
	if refs, ok := pe.sameHopRefs[pp]; ok {
		return refs
	}
	refs := computeSameHopRefs(pp)
	if pe.sameHopRefs == nil {
		pe.sameHopRefs = make(map[*ast.PathPattern]*sameHopRefs, 1)
	}
	pe.sameHopRefs[pp] = refs
	return refs
}

// computeSameHopRefs is [patternEvaluator.sameHopRefsOf] without the cache.
func computeSameHopRefs(pp *ast.PathPattern) *sameHopRefs {
	var refs sameHopRefs
	found := false
	if np := pp.Head.Node; np != nil && np.Properties != nil && np.Variable != nil {
		refs.start = exprMayRead(np.Properties, *np.Variable, "")
		found = refs.start
	}
	for el := pp.Head.Next; el != nil; el = el.Next {
		rel := el.Relationship
		if rel == nil {
			continue
		}
		var relVar, nodeVar string
		if rel.Variable != nil {
			relVar = *rel.Variable
		}
		np := el.Node
		if np != nil && np.Variable != nil {
			nodeVar = *np.Variable
		}
		late := (rel.Properties != nil && exprMayRead(rel.Properties, relVar, nodeVar)) ||
			(np != nil && np.Properties != nil && exprMayRead(np.Properties, relVar, nodeVar))
		refs.hops = append(refs.hops, late)
		found = found || late
	}
	if !found {
		return nil
	}
	return &refs
}

// exprMayRead reports whether e may read the variable a or b; an empty name
// matches nothing. It fails closed: an expression kind it does not walk — a
// nested pattern, a subquery, or a kind added later — counts as a read, which
// costs a per-candidate evaluation but never a wrong answer. A name shadowed by
// a comprehension or reduce variable also counts, for the same reason.
func exprMayRead(e ast.Expression, a, b string) bool {
	switch n := e.(type) {
	case nil:
		return false
	case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral, *ast.BoolLiteral,
		*ast.OverflowIntLit, *ast.NullLiteral, *ast.StarLiteral, *ast.Parameter:
		return false
	case *ast.Variable:
		return n != nil && n.Name != "" && (n.Name == a || n.Name == b)
	case *ast.Property:
		return exprMayRead(n.Receiver, a, b)
	case *ast.LabelPredicate:
		return exprMayRead(n.Receiver, a, b)
	case *ast.UnaryOp:
		return exprMayRead(n.Operand, a, b)
	case *ast.BinaryOp:
		return exprMayRead(n.Left, a, b) || exprMayRead(n.Right, a, b)
	case *ast.FunctionInvocation:
		return anyMayRead(n.Args, a, b)
	case *ast.ListLiteral:
		return anyMayRead(n.Elements, a, b)
	case *ast.MapLiteral:
		return anyMayRead(n.Values, a, b)
	case *ast.SubscriptExpr:
		return exprMayRead(n.Expr, a, b) || exprMayRead(n.Index, a, b)
	case *ast.SliceExpr:
		return exprMayRead(n.Expr, a, b) || exprMayRead(n.From, a, b) || exprMayRead(n.To, a, b)
	case *ast.CaseExpression:
		if exprMayRead(n.Subject, a, b) || exprMayRead(n.ElseExpr, a, b) {
			return true
		}
		for _, alt := range n.Alternatives {
			if exprMayRead(alt.Condition, a, b) || exprMayRead(alt.Consequent, a, b) {
				return true
			}
		}
		return false
	case *ast.ListComprehension:
		return exprMayRead(n.Source, a, b) || exprMayRead(n.Predicate, a, b) || exprMayRead(n.Projection, a, b)
	case *ast.ReduceExpr:
		return exprMayRead(n.Init, a, b) || exprMayRead(n.Source, a, b) || exprMayRead(n.Projection, a, b)
	case *ast.MapProjection:
		if exprMayRead(n.Subject, a, b) {
			return true
		}
		for _, it := range n.Items {
			if exprMayRead(it.Value, a, b) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// anyMayRead is [exprMayRead] over a list of expressions.
func anyMayRead(es []ast.Expression, a, b string) bool {
	for _, e := range es {
		if exprMayRead(e, a, b) {
			return true
		}
	}
	return false
}

// checkNodePattern validates that nodeID satisfies the label constraints
// declared in np and its property map, whose values props holds as
// [patternEvaluator.resolveNodeProps] evaluated them.
func (pe *patternEvaluator) checkNodePattern(np *ast.NodePattern, props *nodePropValues, nodeID graph.NodeID) bool {
	if len(np.Labels) == 0 && props == nil {
		return true
	}
	mapper := pe.g.AdjList().Mapper()
	key, resolved := mapper.Resolve(nodeID)
	if !resolved {
		return false
	}
	// Label check: every declared label must be present.
	if len(np.Labels) > 0 {
		nodeLabels := pe.g.NodeLabels(key)
		labelSet := make(map[string]struct{}, len(nodeLabels))
		for _, l := range nodeLabels {
			labelSet[l] = struct{}{}
		}
		for _, required := range np.Labels {
			if _, ok := labelSet[required]; !ok {
				return false
			}
		}
	}
	// Property check: every declared property must match.
	if props != nil {
		rawProps := pe.g.NodeProperties(key)
		for i, want := range props.vals {
			have, ok := rawProps[props.keys[i]]
			if !ok {
				return false
			}
			if !expr.IsTruthy(lpgPropToExpr(have).Equal(want)) {
				return false
			}
		}
	}
	return true
}

// nodePatternFilter returns false when np has labels/properties that the
// given row does not satisfy. Used for single-node (no-hop) patterns.
func (pe *patternEvaluator) nodePatternFilter(_ *ast.NodePattern, _ expr.RowContext) bool {
	return true // single-node patterns with no hops are always considered matched if the node exists
}

// nodeIDFromValue extracts a graph.NodeID from an expr.Value (NodeValue or
// IntegerValue). Returns (0, false) when v does not represent a graph node.
func nodeIDFromValue(v expr.Value, mapper *graph.Mapper[string]) (graph.NodeID, bool) {
	switch t := v.(type) {
	case expr.NodeValue:
		return graph.NodeID(t.ID), true
	case expr.IntegerValue:
		id := graph.NodeID(t)
		_, ok := mapper.Resolve(id)
		return id, ok
	}
	return 0, false
}

// allNodeIDs returns all currently interned NodeIDs from the mapper.
func allNodeIDs(mapper *graph.Mapper[string]) []graph.NodeID {
	maxID := mapper.MaxNodeID()
	ids := make([]graph.NodeID, 0, int(maxID))
	mapper.Walk(func(id graph.NodeID, _ string) bool {
		ids = append(ids, id)
		return true
	})
	return ids
}
