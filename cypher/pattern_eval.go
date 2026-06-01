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
	"github.com/FlavioCFOliveira/GoGraph/graph"
	lpg "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// patternEvaluator implements [expr.PatternEvaluator] using the live LPG
// graph. All edge traversal is performed via the adjacency-list API so no CSR
// snapshot is required.
type patternEvaluator struct {
	g *lpg.Graph[string, float64]
}

// newPatternEvaluator constructs the evaluator for one query run.
func newPatternEvaluator(g *lpg.Graph[string, float64]) *patternEvaluator {
	return &patternEvaluator{g: g}
}

// EvalPattern implements [expr.PatternEvaluator].
func (pe *patternEvaluator) EvalPattern(ctx context.Context, pp *ast.PathPattern, row expr.RowContext, _ map[string]expr.Value) (expr.Value, error) {
	if pe.g == nil || pp == nil || pp.Head == nil {
		return expr.BoolValue(false), nil
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
	err := pe.enumeratePatternMatches(ctx, pp, row, func(innerRow expr.RowContext) error {
		if pc.Predicate != nil {
			pv, perr := expr.EvalWith(ctx, pc.Predicate, innerRow, params, reg, nil, pe)
			if perr != nil {
				return perr
			}
			if !expr.IsTruthy(pv) {
				return nil
			}
		}
		var projVal = expr.Null
		if pc.Projection != nil {
			v, perr := expr.EvalWith(ctx, pc.Projection, innerRow, params, reg, nil, pe)
			if perr != nil {
				return perr
			}
			projVal = v
		}
		results = append(results, projVal)
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
			return pe.collectIncomingCandidates(srcID, srcKey, s)
		default:
			out := pe.collectOutgoingCandidates(srcID, srcKey, s)
			return append(out, pe.collectIncomingCandidates(srcID, srcKey, s)...)
		}
	}()
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !pe.checkEndNode(s.node, c.dstID, row) {
			continue
		}
		next := cloneRow(row)
		if s.node != nil && s.node.Variable != nil {
			next[*s.node.Variable] = nodeValueForID(pe.g, c.dstID)
		}
		if s.rel != nil && s.rel.Variable != nil {
			next[*s.rel.Variable] = relValueFromHop(pe.g, c, s.rel)
		}
		if err := pe.enumerateSteps(ctx, c.dstID, remaining, next, cb); err != nil {
			return err
		}
	}
	return nil
}

// candidateHop describes one (rel, dst) traversal candidate found by
// enumerateSteps.
type candidateHop struct {
	srcID, dstID   graph.NodeID
	srcKey, dstKey string
	forward        bool
}

func (pe *patternEvaluator) collectOutgoingCandidates(srcID graph.NodeID, srcKey string, s step) []candidateHop {
	mapper := pe.g.AdjList().Mapper()
	nbs, _ := pe.g.AdjList().LoadEntry(srcID)
	out := make([]candidateHop, 0, len(nbs))
	for _, dstID := range nbs {
		dstKey, ok := mapper.Resolve(dstID)
		if !ok {
			continue
		}
		if !pe.edgeMatchesRel(srcKey, dstKey, s.rel) {
			continue
		}
		out = append(out, candidateHop{srcID: srcID, dstID: dstID, srcKey: srcKey, dstKey: dstKey, forward: true})
	}
	return out
}

func (pe *patternEvaluator) collectIncomingCandidates(dstID graph.NodeID, dstKey string, s step) []candidateHop {
	mapper := pe.g.AdjList().Mapper()
	var out []candidateHop
	mapper.Walk(func(candidateID graph.NodeID, candidateKey string) bool {
		if candidateID == dstID {
			return true
		}
		nbs, _ := pe.g.AdjList().LoadEntry(candidateID)
		for _, nb := range nbs {
			if nb != dstID {
				continue
			}
			if !pe.edgeMatchesRel(candidateKey, dstKey, s.rel) {
				continue
			}
			out = append(out, candidateHop{srcID: candidateID, dstID: dstID, srcKey: candidateKey, dstKey: dstKey, forward: false})
			break
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
func nodeValueForID(g *lpg.Graph[string, float64], id graph.NodeID) expr.NodeValue {
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

// relValueFromHop materialises an expr.RelationshipValue for a single
// hop produced by enumerateSteps. The edge's storage direction is
// (forward ? srcKey→dstKey : dstKey→srcKey) — callers pass forward=true
// when the traversal followed the storage direction and false for the
// reverse leg of an undirected / incoming match.
func relValueFromHop(g *lpg.Graph[string, float64], hop candidateHop, _ *ast.RelationshipPattern) expr.RelationshipValue {
	srcKey, dstKey := hop.srcKey, hop.dstKey
	startID, endID := uint64(hop.srcID), uint64(hop.dstID)
	if !hop.forward {
		srcKey, dstKey = hop.dstKey, hop.srcKey
		startID, endID = endID, startID
	}
	var typeName string
	if labels := g.EdgeLabels(srcKey, dstKey); len(labels) > 0 {
		typeName = labels[0]
	}
	var props expr.MapValue
	if raw := g.EdgeProperties(srcKey, dstKey); len(raw) > 0 {
		props = make(expr.MapValue, len(raw))
		for k, pv := range raw {
			props[k] = lpgPropToExpr(pv)
		}
	}
	return expr.RelationshipValue{
		StartID:    startID,
		EndID:      endID,
		Type:       typeName,
		Properties: props,
	}
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
	neighbours, _ := pe.g.AdjList().LoadEntry(srcID)
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
func (pe *patternEvaluator) matchIncoming(ctx context.Context, dstID graph.NodeID, dstKey string, s step, remaining []step, row expr.RowContext) (bool, error) {
	mapper := pe.g.AdjList().Mapper()
	found := false
	var walkErr error
	mapper.Walk(func(candidateID graph.NodeID, candidateKey string) bool {
		if err := ctx.Err(); err != nil {
			walkErr = err
			return false
		}
		if candidateID == dstID {
			return true // skip self
		}
		nbs, _ := pe.g.AdjList().LoadEntry(candidateID)
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
	nbs, _ := pe.g.AdjList().LoadEntry(curID)
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
		nbs, _ := pe.g.AdjList().LoadEntry(candidateID)
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
		// No type constraint — any edge matches (but the edge must exist).
		return pe.g.AdjList().HasEdge(srcKey, dstKey)
	}
	labels := pe.g.EdgeLabels(srcKey, dstKey)
	if len(labels) == 0 {
		return false
	}
	// openCypher OR semantics: match if edge label equals any listed type.
	edgeLabel := labels[0]
	for _, t := range rel.Types {
		if edgeLabel == t {
			return true
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
			want, err := expr.Eval(ml.Values[i], expr.RowContext{}, nil, nil)
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
