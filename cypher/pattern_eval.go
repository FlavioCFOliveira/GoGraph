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
//     incoming, or undirected) and filters by relationship type (if given).
//  4. For variable-length hops it performs a BFS bounded by the declared
//     min/max depth.
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
	return &patternEvaluator{g: g, maxCollectItems: resolvePatternCompBudget(maxCollectItems)}
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
// and returns the collected list value. Currently handles single-hop
// patterns of the form `(anchor)-[:T]->(other)` and undirected /
// incoming variants, which covers Pattern2 [7] (`size([(x)-->(:Y) |
// 1])`). Multi-hop and variable-length comprehensions fall back to an
// empty list — these are not yet observed in the openCypher TCK.
func (pe *patternEvaluator) EvalPatternComp(ctx context.Context, pc *ast.PatternComprehension, row expr.RowContext, params map[string]expr.Value, reg expr.FunctionRegistry) (expr.Value, error) {
	if pe.g == nil || pc == nil || pc.Pattern == nil || pc.Pattern.Head == nil {
		return expr.ListValue{}, nil
	}
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
// in pp (path / node / relationship variables) to its matched value.
// Restricted to single-hop, fixed-length patterns — sufficient for
// Pattern2 [7]. Multi-hop is handled by recursing through each
// successive step; variable-length is not yet supported and is silently
// treated as zero matches.
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

	steps := collectSteps(pp.Head)
	for _, sid := range startIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !pe.checkStartNode(startNode, sid, row) {
			continue
		}
		base := cloneRow(row)
		if startNode != nil && startNode.Variable != nil {
			base[*startNode.Variable] = nodeValueForID(pe.g, sid)
		}
		if err := pe.enumerateSteps(ctx, sid, steps, base, cb); err != nil {
			return err
		}
	}
	return nil
}

// enumerateSteps recursively walks the remaining hop list, extending
// the running RowContext with each hop's bindings. When the list is
// empty the callback is invoked with the accumulated row.
func (pe *patternEvaluator) enumerateSteps(ctx context.Context, srcID graph.NodeID, steps []step, row expr.RowContext, cb func(expr.RowContext) error) error {
	if len(steps) == 0 {
		return cb(row)
	}
	s := steps[0]
	remaining := steps[1:]
	if s.rel != nil && s.rel.Range != nil {
		// Variable-length: not handled by the comprehension evaluator yet.
		return nil
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

	candidates := func() []candidateHop {
		switch dir {
		case ast.RelDirectionOutgoing:
			return pe.collectOutgoingCandidates(srcID, srcKey, s)
		case ast.RelDirectionIncoming:
			// A self-loop IS an incoming edge and must be enumerated here.
			return pe.collectIncomingCandidates(srcID, srcKey, s, true)
		default:
			// Undirected: the outgoing collector has already emitted every
			// self-loop, and openCypher matches each relationship of an
			// undirected pattern exactly ONCE, so the incoming leg must not
			// emit it a second time.
			out := pe.collectOutgoingCandidates(srcID, srcKey, s)
			return append(out, pe.collectIncomingCandidates(srcID, srcKey, s, false)...)
		}
	}()
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Advance to the TRAVERSAL destination, not to the hop's stored dstID.
		// The two coincide on a forward leg but are opposite on the reverse leg
		// of an incoming / undirected hop, where the anchor is the stored dstID
		// and the neighbour we are walking to is the stored srcID (rmp #2505).
		dstID := c.traversalDst()
		if !pe.checkEndNode(s.node, dstID, row) {
			continue
		}
		next := cloneRow(row)
		if s.node != nil && s.node.Variable != nil {
			next[*s.node.Variable] = nodeValueForID(pe.g, dstID)
		}
		if s.rel != nil && s.rel.Variable != nil {
			next[*s.rel.Variable] = relValueFromHop(pe.g, c, s.rel)
		}
		if err := pe.enumerateSteps(ctx, dstID, remaining, next, cb); err != nil {
			return err
		}
	}
	return nil
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
type candidateHop struct {
	srcKey, dstKey string
	srcID, dstID   graph.NodeID
	handle         uint64
	forward        bool
}

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
		if !pe.slotMatchesRelType(srcID, dstID, handle, s.rel) {
			continue
		}
		out = append(out, candidateHop{srcID: srcID, dstID: dstID, srcKey: srcKey, dstKey: dstKey, handle: handle, forward: true})
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
func (pe *patternEvaluator) collectIncomingCandidates(dstID graph.NodeID, dstKey string, s step, includeSelfLoop bool) []candidateHop {
	mapper := pe.g.AdjList().Mapper()
	var out []candidateHop
	mapper.Walk(func(candidateID graph.NodeID, candidateKey string) bool {
		if candidateID == dstID && !includeSelfLoop {
			return true
		}
		// Emit one candidate PER parallel slot pointing at dstID (not just the
		// first), each carrying its own handle, so an untyped `[r]` incoming hop
		// over a multi-type parallel pair enumerates every instance — mirroring
		// the outgoing path and the primary Expand path (rmp #2017).
		candView := pe.g.EntryView(candidateID)
		nbs, handles := candView.Neighbours, candView.Handles
		for i, nb := range nbs {
			if nb != dstID {
				continue
			}
			if !pe.edgeMatchesRel(candidateKey, dstKey, s.rel) {
				continue
			}
			var handle uint64
			if i < len(handles) {
				handle = handles[i]
			}
			// The slot is stored candidateID → dstID, so the per-instance type
			// lookup uses that orientation even though the traversal crosses it
			// backwards (see [candidateHop]'s orientation contract).
			if !pe.slotMatchesRelType(candidateID, dstID, handle, s.rel) {
				continue
			}
			out = append(out, candidateHop{srcID: candidateID, dstID: dstID, srcKey: candidateKey, dstKey: dstKey, handle: handle, forward: false})
		}
		return true
	})
	return out
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

	for _, sid := range startIDs {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !pe.checkStartNode(startNode, sid, row) {
			continue
		}
		ok, err := pe.matchSteps(ctx, sid, steps, row)
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
type step struct {
	rel  *ast.RelationshipPattern
	node *ast.NodePattern
}

// collectSteps builds the ordered slice of (rel, node) steps from the
// PathElement linked list, starting at el.Next (skipping the head node which
// is handled separately).
func collectSteps(head *ast.PathElement) []step {
	var steps []step
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
func (pe *patternEvaluator) matchSteps(ctx context.Context, srcID graph.NodeID, steps []step, row expr.RowContext) (bool, error) {
	if len(steps) == 0 {
		return true, nil
	}
	s := steps[0]
	remaining := steps[1:]

	if s.rel != nil && s.rel.Range != nil {
		return pe.matchVarLen(ctx, srcID, s, remaining, row)
	}
	return pe.matchSingleHop(ctx, srcID, s, remaining, row)
}

// matchSingleHop follows a single fixed-length hop and recurses.
//
//nolint:gocyclo // direction × filter × recursion branches; extracted helpers bring each below 15
func (pe *patternEvaluator) matchSingleHop(ctx context.Context, srcID graph.NodeID, s step, remaining []step, row expr.RowContext) (bool, error) {
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
		return pe.matchOutgoing(ctx, srcID, srcKey, s, remaining, row)
	case ast.RelDirectionIncoming:
		return pe.matchIncoming(ctx, srcID, srcKey, s, remaining, row)
	default: // undirected: check both out and in
		if found, err := pe.matchOutgoing(ctx, srcID, srcKey, s, remaining, row); err != nil || found {
			return found, err
		}
		return pe.matchIncoming(ctx, srcID, srcKey, s, remaining, row)
	}
}

// matchOutgoing iterates the outgoing neighbours of srcID and recurses for
// each neighbour that passes the edge-type and end-node filters.
func (pe *patternEvaluator) matchOutgoing(ctx context.Context, srcID graph.NodeID, srcKey string, s step, remaining []step, row expr.RowContext) (bool, error) {
	mapper := pe.g.AdjList().Mapper()
	neighbours := pe.g.EntryView(srcID).Neighbours
	for _, dstID := range neighbours {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		dstKey, dstOK := mapper.Resolve(dstID)
		if !dstOK {
			continue
		}
		if !pe.edgeMatchesRel(srcKey, dstKey, s.rel) {
			continue
		}
		if !pe.checkEndNode(s.node, dstID, row) {
			continue
		}
		if ok, err := pe.matchSteps(ctx, dstID, remaining, row); err != nil || ok {
			return ok, err
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
func (pe *patternEvaluator) matchIncoming(ctx context.Context, dstID graph.NodeID, dstKey string, s step, remaining []step, row expr.RowContext) (bool, error) {
	mapper := pe.g.AdjList().Mapper()
	found := false
	var walkErr error
	mapper.Walk(func(candidateID graph.NodeID, candidateKey string) bool {
		if err := ctx.Err(); err != nil {
			walkErr = err
			return false
		}
		nbs := pe.g.EntryView(candidateID).Neighbours
		for _, nb := range nbs {
			if nb != dstID {
				continue
			}
			if !pe.edgeMatchesRel(candidateKey, dstKey, s.rel) {
				continue
			}
			if !pe.checkEndNode(s.node, candidateID, row) {
				continue
			}
			ok, err := pe.matchSteps(ctx, candidateID, remaining, row)
			if err != nil {
				walkErr = err
				return false
			}
			if ok {
				found = true
				return false // early stop
			}
		}
		return true
	})
	return found, walkErr
}

// matchVarLen evaluates a variable-length hop using BFS bounded by the
// declared min/max depth from s.rel.Range.
func (pe *patternEvaluator) matchVarLen(ctx context.Context, srcID graph.NodeID, s step, remaining []step, row expr.RowContext) (bool, error) {
	minDepth, maxDepth := varLenBounds(s.rel)

	// BFS: each frontier element is (nodeID, depth). We track visited nodes
	// to avoid cycles.
	frontier := []patBFSNode{{id: srcID, depth: 0}}
	visited := make(map[graph.NodeID]struct{})
	visited[srcID] = struct{}{}

	mapper := pe.g.AdjList().Mapper()
	dir := ast.RelDirectionOutgoing
	if s.rel != nil {
		dir = s.rel.Direction
	}

	for len(frontier) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		cur := frontier[0]
		frontier = frontier[1:]

		if cur.depth >= minDepth && cur.depth <= maxDepth {
			if ok, err := pe.bfsCheckNode(ctx, cur.id, s.node, remaining, row); err != nil || ok {
				return ok, err
			}
		}

		if cur.depth >= maxDepth {
			continue
		}
		curKey, resolved := mapper.Resolve(cur.id)
		if !resolved {
			continue
		}
		pe.bfsExpandStep(mapper, cur.id, curKey, s.rel, dir, visited, &frontier, cur.depth)
	}
	return false, nil
}

// varLenBounds extracts min/max depth from a relationship pattern's range
// quantifier, applying the openCypher defaults: *1.. when unspecified.
func varLenBounds(rel *ast.RelationshipPattern) (minDepth, maxDepth int64) {
	minDepth = 1
	maxDepth = patternVarLenMaxDefault
	if rel == nil || rel.Range == nil {
		return
	}
	if rel.Range.Min != nil {
		minDepth = *rel.Range.Min
	}
	if rel.Range.Max != nil {
		maxDepth = *rel.Range.Max
	}
	if minDepth < 0 {
		minDepth = 0
	}
	return
}

// bfsCheckNode tests whether nodeID satisfies the end-node pattern and, if so,
// recurses into the remaining steps. Returns (true, nil) on first full match.
func (pe *patternEvaluator) bfsCheckNode(ctx context.Context, nodeID graph.NodeID, np *ast.NodePattern, remaining []step, row expr.RowContext) (bool, error) {
	if !pe.checkEndNode(np, nodeID, row) {
		return false, nil
	}
	return pe.matchSteps(ctx, nodeID, remaining, row)
}

// bfsExpandStep appends unvisited neighbours reachable in direction dir from
// (curID, curKey) to frontier, respecting the edge-type filter in rel.
func (pe *patternEvaluator) bfsExpandStep(mapper *graph.Mapper[string], curID graph.NodeID, curKey string, rel *ast.RelationshipPattern, dir ast.RelDirection, visited map[graph.NodeID]struct{}, frontier *[]patBFSNode, depth int64) {
	switch dir {
	case ast.RelDirectionOutgoing:
		pe.bfsExpandOutgoing(mapper, curID, curKey, rel, visited, frontier, depth)
	case ast.RelDirectionIncoming:
		pe.bfsExpandIncoming(mapper, curID, curKey, rel, visited, frontier, depth)
	default: // undirected
		pe.bfsExpandOutgoing(mapper, curID, curKey, rel, visited, frontier, depth)
		pe.bfsExpandIncoming(mapper, curID, curKey, rel, visited, frontier, depth)
	}
}

// bfsExpandOutgoing appends unvisited forward neighbours of curID to frontier.
func (pe *patternEvaluator) bfsExpandOutgoing(mapper *graph.Mapper[string], curID graph.NodeID, curKey string, rel *ast.RelationshipPattern, visited map[graph.NodeID]struct{}, frontier *[]patBFSNode, depth int64) {
	nbs := pe.g.EntryView(curID).Neighbours
	for _, nbID := range nbs {
		if _, seen := visited[nbID]; seen {
			continue
		}
		nbKey, nbOK := mapper.Resolve(nbID)
		if !nbOK {
			continue
		}
		if !pe.edgeMatchesRel(curKey, nbKey, rel) {
			continue
		}
		visited[nbID] = struct{}{}
		*frontier = append(*frontier, patBFSNode{id: nbID, depth: depth + 1})
	}
}

// patternVarLenMaxDefault caps BFS depth for unbounded variable-length
// patterns (e.g. *). openCypher does not mandate a specific cap; we use 15
// as a practical limit that handles most real-world graph shapes without
// pathological runtime.
const patternVarLenMaxDefault = 15

// patBFSNode is a frontier element for the variable-length pattern BFS.
type patBFSNode struct {
	id    graph.NodeID
	depth int64
}

// bfsExpandIncoming appends reverse-direction neighbours to frontier for BFS.
func (pe *patternEvaluator) bfsExpandIncoming(mapper *graph.Mapper[string], dstID graph.NodeID, dstKey string, rel *ast.RelationshipPattern, visited map[graph.NodeID]struct{}, frontier *[]patBFSNode, depth int64) {
	mapper.Walk(func(candidateID graph.NodeID, candidateKey string) bool {
		if _, seen := visited[candidateID]; seen {
			return true
		}
		nbs := pe.g.EntryView(candidateID).Neighbours
		for _, nb := range nbs {
			if nb == dstID {
				if !pe.edgeMatchesRel(candidateKey, dstKey, rel) {
					continue
				}
				visited[candidateID] = struct{}{}
				*frontier = append(*frontier, patBFSNode{id: candidateID, depth: depth + 1})
				break
			}
		}
		return true
	})
}

// edgeMatchesRel reports whether the directed edge (srcKey → dstKey) satisfies
// the relationship pattern rel. When rel is nil or has no type constraints, all
// edges match.
func (pe *patternEvaluator) edgeMatchesRel(srcKey, dstKey string, rel *ast.RelationshipPattern) bool {
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

// checkStartNode validates that the start node (at srcID) satisfies the
// optional labels/properties in np and is consistent with any bound variable.
func (pe *patternEvaluator) checkStartNode(np *ast.NodePattern, srcID graph.NodeID, row expr.RowContext) bool {
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
	return pe.checkNodePattern(np, srcID)
}

// checkEndNode validates that the candidate destination node satisfies the
// optional labels/properties in np and any bound variable constraint.
func (pe *patternEvaluator) checkEndNode(np *ast.NodePattern, dstID graph.NodeID, row expr.RowContext) bool {
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
	return pe.checkNodePattern(np, dstID)
}

// checkNodePattern validates that nodeID satisfies the label and property
// constraints declared in np.
func (pe *patternEvaluator) checkNodePattern(np *ast.NodePattern, nodeID graph.NodeID) bool {
	if len(np.Labels) == 0 && np.Properties == nil {
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
	if np.Properties != nil {
		ml, ok := np.Properties.(*ast.MapLiteral)
		if !ok {
			return true // non-literal property filter — skip (conservative accept)
		}
		rawProps := pe.g.NodeProperties(key)
		for i, k := range ml.Keys {
			// pe.params, not nil (rmp #2507). The value is very often a PARAMETER
			// even when the query text spells a literal, because StripLiterals
			// hoists a string literal inside a WHERE — and a pattern predicate is a
			// WHERE. Passing nil made every such reference NULL and the whole
			// predicate reject its row.
			want, err := expr.Eval(ml.Values[i], expr.RowContext{}, pe.params, nil)
			if err != nil {
				return false
			}
			have, ok := rawProps[k]
			if !ok {
				return false
			}
			havePV := lpgPropToExpr(have)
			if !expr.IsTruthy(havePV.Equal(want)) {
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
