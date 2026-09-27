package cypher

// pattern_eval_varlen.go — variable-length hops in the pattern evaluator
// (rmp #2898).
//
// # Semantics
//
// A variable-length hop -[:T*min..max]-> matches every PATH of min to max
// relationships, and each relationship it crosses fills a relationship slot of
// the pattern exactly as a fixed hop's does: no relationship may appear twice in
// one match, whether both occurrences are inside the variable-length hop or one
// is on a fixed hop before or after it (openCypher relationship isomorphism; the
// TCK pins it in clauses/match/Match4.feature [7] and relies on it inside a
// pattern predicate in expressions/pattern/Pattern1.feature [10] and [18]). Nodes
// may repeat. *0 binds the end node to the start node (Match5.feature [8]), and an
// omitted upper bound is unbounded (Match4.feature [4] walks 21 hops).
//
// A node-visited breadth-first search, which is what this evaluator used to run,
// answers a different question — which nodes are reachable, at their shortest
// distance — and is wrong in both directions: it misses a longer path to a node
// already reached (a → b → c for *2 when a → c exists), every path back to the
// start node, and every path beyond its old 15-hop cap; and because it tracked no
// relationships it let a fixed hop re-use one the variable-length hop crossed.
//
// # Algorithm
//
// The exact search is a depth-first enumeration of relationship-isomorphic paths
// ([patternEvaluator.varLenExists], [patternEvaluator.varLenEnum]). Each crossed
// relationship is pushed onto the pattern's [relPath], which the fixed hops of
// the same pattern already consult, so uniqueness holds across the whole pattern
// in both directions. An existential evaluation stops at its first witness.
//
// Its worst case is the number of relationship-isomorphic paths of length at most
// max, O(b^max) for branching factor b; deciding whether a path of an EXACT length
// exists is NP-hard in general, so no exact polynomial algorithm is available.
// Two things keep it bounded:
//
//   - The traversal budget. Every relationship slot the search reads is charged
//     against the per-row and per-query limits [exec.VarLengthExpand] applies
//     ([exec.DefaultVarLenMaxEdgesTraversed],
//     [exec.DefaultVarLenMaxTotalEdgesTraversed]), and exceeding either returns
//     [exec.ErrVarLenCapExceeded], the error MATCH returns for the same work.
//   - The reachability fast path ([patternEvaluator.varLenReach]). When the
//     variable-length hop is the whole pattern and min ≤ 1, a node-visited
//     breadth-first search is EXACT for every end node other than the start: the
//     shortest walk to a node is a simple path, a simple path repeats no
//     relationship, and its length is ≥ 1 ≥ min and ≤ max. It costs O(V + E).
//     Only the start node itself — reachable solely through a cycle — is left to
//     the exact search, and only when the end-node pattern could accept it. With
//     min ≥ 2 the argument fails (a → c is the shortest walk while *2 needs
//     a → b → c), so those ranges always take the exact search.
//
// # What is and is not memoised
//
// A search state is (node, depth, relationships used). Memoising on (node,
// depth) alone is UNSOUND: over the single relationship a — b,
// (a)-[:R]-(b)-[:R*1..1]-(a) fails from (b, 0) with that relationship used and
// would succeed from the same (node, depth) with it free. The full state almost
// never repeats, so it is not memoised either. What is memoised is a function of
// the snapshot and one node alone: the relationship slots INTO a node, which this
// evaluator can only find by scanning every node ([patternEvaluator.scanIncoming]),
// are cached for the duration of one top-level evaluation
// ([patternEvaluator.incomingSlotsOf]).
//
// # Bound end node
//
// When the hop is the whole pattern, both of its ends are bound and it is
// traversed INCOMING, the search runs from the bound end outwards instead: an
// outgoing expansion reads one adjacency entry, an incoming one scans the graph.

import (
	"context"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// varLenCtxStride is how many charged slots pass between two cancellation
// checks in the exact search.
const varLenCtxStride = 1024

// varLenSearch is the invariant part of one variable-length hop's search.
type varLenSearch struct {
	s         step
	remaining []step
	row       expr.RowContext
	minDepth  int64
	maxDepth  int64
	dir       ast.RelDirection

	// target is the only node the hop may end on when targetBound is set: the
	// bound end-node variable, or — after the search was reversed — the original
	// start node, whose pattern the caller has already checked (swapped).
	target      graph.NodeID
	targetBound bool
	swapped     bool
	// onlyStart restricts acceptance to the start node: the fast path has already
	// ruled out every other end node.
	onlyStart bool
	start     graph.NodeID
}

// accepts reports whether the hop may end on id. It is a function of the node
// alone; the depth window is the caller's.
func (vs *varLenSearch) accepts(pe *patternEvaluator, id graph.NodeID) bool {
	if vs.onlyStart && id != vs.start {
		return false
	}
	if vs.swapped {
		return id == vs.target
	}
	if vs.targetBound {
		return id == vs.target && pe.endNodePatternOK(vs.s.node, id)
	}
	return pe.checkEndNode(vs.s.node, id, vs.row)
}

// varLenBounds extracts min/max depth from a relationship pattern's range
// quantifier, applying the openCypher defaults: *1.. when unspecified, and no
// upper bound when the maximum is omitted (math.MaxInt64). A path cannot repeat a
// relationship, so an unbounded search still ends after at most |E| hops.
func varLenBounds(rel *ast.RelationshipPattern) (minDepth, maxDepth int64) {
	minDepth = 1
	maxDepth = math.MaxInt64
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

// isVarLen reports whether s is a variable-length hop.
func (s step) isVarLen() bool { return s.rel != nil && s.rel.Range != nil }

// hopDirection returns the traversal direction of rel, outgoing by default.
func hopDirection(rel *ast.RelationshipPattern) ast.RelDirection {
	if rel == nil {
		return ast.RelDirectionOutgoing
	}
	return rel.Direction
}

// beginEval opens a top-level evaluation: the per-row traversal budget and the
// incoming-slot cache belong to one evaluation of one row. A nested evaluation —
// a pattern inside a comprehension's WHERE or projection — shares its caller's.
func (pe *patternEvaluator) beginEval() {
	if pe.evalNesting == 0 {
		pe.varLenRowTraversals = 0
		if pe.incomingCache != nil {
			clear(pe.incomingCache)
		}
	}
	pe.evalNesting++
}

// endEval closes the evaluation beginEval opened.
func (pe *patternEvaluator) endEval() { pe.evalNesting-- }

// chargeVarLen records n relationship slots read by the exact search against the
// per-row and per-query budgets, and reports cancellation at a fixed stride.
func (pe *patternEvaluator) chargeVarLen(ctx context.Context, n int) error {
	before := pe.varLenTotalTraversals
	pe.varLenRowTraversals += n
	pe.varLenTotalTraversals += n
	if pe.varLenRowTraversals > exec.DefaultVarLenMaxEdgesTraversed ||
		pe.varLenTotalTraversals > exec.DefaultVarLenMaxTotalEdgesTraversed {
		return exec.ErrVarLenCapExceeded
	}
	if before/varLenCtxStride != pe.varLenTotalTraversals/varLenCtxStride {
		return ctx.Err()
	}
	return nil
}

// frameBuffer returns the reusable candidate buffer of the current search frame.
// Frames nest strictly — a frame's buffer is iterated only while no shallower
// frame runs — so indexing by the live frame count, not by depth, keeps two
// variable-length hops of one pattern from sharing a buffer.
func (pe *patternEvaluator) frameBuffer() []candidateHop {
	for len(pe.varLenBufs) <= pe.varLenFrame {
		pe.varLenBufs = append(pe.varLenBufs, nil)
	}
	return pe.varLenBufs[pe.varLenFrame][:0]
}

// incomingSlotsOf returns every adjacency slot pointing at id, self-loops
// included, memoised for the current top-level evaluation. The slots into a node
// are a function of the snapshot and the node alone, so the cache is exact; it is
// what keeps an incoming search from rescanning the graph at every visit.
func (pe *patternEvaluator) incomingSlotsOf(ctx context.Context, id graph.NodeID) ([]incomingSlot, error) {
	if hits, ok := pe.incomingCache[id]; ok {
		return hits, nil
	}
	hits, err := pe.scanIncoming(ctx, id, true, true, nil)
	if err != nil {
		return nil, err
	}
	if pe.incomingCache == nil {
		pe.incomingCache = make(map[graph.NodeID][]incomingSlot)
	}
	pe.incomingCache[id] = hits
	return hits, nil
}

// varLenCandidates appends to buf every relationship slot the hop may cross from
// cur in direction dir: of an accepted type, per slot as well as per pair, and not
// already on used. An undirected hop reads a self-loop once, from the outgoing
// leg, as [patternEvaluator.enumerateSteps] does. Every slot read is charged.
func (pe *patternEvaluator) varLenCandidates(ctx context.Context, cur graph.NodeID, rel *ast.RelationshipPattern, dir ast.RelDirection, used relPath, buf []candidateHop) ([]candidateHop, error) {
	mapper := pe.g.AdjList().Mapper()
	curKey, ok := mapper.Resolve(cur)
	if !ok {
		return buf, nil
	}
	if dir != ast.RelDirectionIncoming {
		view := pe.g.EntryView(cur)
		if err := pe.chargeVarLen(ctx, len(view.Neighbours)); err != nil {
			return buf, err
		}
		for i, dstID := range view.Neighbours {
			inst := relInstance{src: cur, slot: i}
			if relUsed(used, inst) {
				continue
			}
			dstKey, ok := mapper.Resolve(dstID)
			if !ok {
				continue
			}
			handle := handleAt(view.Handles, i)
			if !pe.edgeMatchesRel(curKey, dstKey, rel) || !pe.slotMatchesRelType(cur, dstID, handle, rel) {
				continue
			}
			buf = append(buf, candidateHop{srcID: cur, dstID: dstID, srcKey: curKey, dstKey: dstKey, handle: handle, slot: i, forward: true})
		}
	}
	if dir != ast.RelDirectionOutgoing {
		hits, err := pe.incomingSlotsOf(ctx, cur)
		if err != nil {
			return buf, err
		}
		if err := pe.chargeVarLen(ctx, len(hits)); err != nil {
			return buf, err
		}
		for _, h := range hits {
			if h.id == cur && dir != ast.RelDirectionIncoming {
				continue // an undirected hop has read this self-loop outgoing
			}
			inst := relInstance{src: h.id, slot: h.slot}
			if relUsed(used, inst) {
				continue
			}
			if !pe.edgeMatchesRel(h.key, curKey, rel) || !pe.slotMatchesRelType(h.id, cur, h.handle, rel) {
				continue
			}
			buf = append(buf, candidateHop{srcID: h.id, dstID: cur, srcKey: h.key, dstKey: curKey, handle: h.handle, slot: h.slot, forward: false})
		}
	}
	return buf, nil
}

// matchVarLen evaluates the variable-length hop s from srcID and, on each path
// that satisfies it, the remaining steps; it reports whether any complete match
// exists. used carries the relationships of the hops before it and receives the
// ones it crosses (see the file comment).
func (pe *patternEvaluator) matchVarLen(ctx context.Context, srcID graph.NodeID, s step, remaining []step, row expr.RowContext, used relPath) (bool, error) {
	minDepth, maxDepth := varLenBounds(s.rel)
	if list, bound := boundRelList(s.rel, row); bound {
		end, next, ok, err := pe.walkBoundRelList(ctx, srcID, s, row, used, list, minDepth, maxDepth)
		if err != nil || !ok {
			return false, err
		}
		return pe.matchSteps(ctx, end, remaining, row, next)
	}
	vs := varLenSearch{s: s, remaining: remaining, row: row, minDepth: minDepth, maxDepth: maxDepth, dir: hopDirection(s.rel), start: srcID}
	target, bound, ok := pe.boundEndID(s.node, row)
	if bound && !ok {
		return false, nil
	}
	vs.target, vs.targetBound = target, bound
	from := srcID
	if len(remaining) == 0 && len(used) == 0 {
		if vs.targetBound && vs.dir == ast.RelDirectionIncoming {
			if !pe.endNodePatternOK(s.node, vs.target) {
				return false, nil
			}
			from, vs.target, vs.swapped, vs.dir = vs.target, srcID, true, ast.RelDirectionOutgoing
			vs.start = from
		}
		if minDepth <= 1 {
			// from itself is reachable only through a cycle. A directed shortest
			// cycle is a simple cycle, so the reachability search decides it too;
			// an undirected one is not (a — b — a crosses one relationship twice).
			cycle := minDepth == 1 && vs.accepts(pe, from)
			directed := vs.dir == ast.RelDirectionOutgoing || vs.dir == ast.RelDirectionIncoming
			found, err := pe.varLenReach(ctx, &vs, from, cycle && directed)
			if err != nil || found || !cycle || directed {
				return found, err
			}
			vs.onlyStart = true
		}
	}
	return pe.varLenExists(ctx, &vs, from, 0, ensureRelPath(used))
}

// varLenExists is the exact existential search from cur at depth hops.
func (pe *patternEvaluator) varLenExists(ctx context.Context, vs *varLenSearch, cur graph.NodeID, depth int64, used relPath) (bool, error) {
	if depth >= vs.minDepth && vs.accepts(pe, cur) {
		if ok, err := pe.matchSteps(ctx, cur, vs.remaining, vs.row, used); err != nil || ok {
			return ok, err
		}
	}
	if depth >= vs.maxDepth {
		return false, nil
	}
	cands, err := pe.varLenCandidates(ctx, cur, vs.s.rel, vs.dir, used, pe.frameBuffer())
	pe.varLenBufs[pe.varLenFrame] = cands
	if err != nil {
		return false, err
	}
	pe.varLenFrame++
	defer func() { pe.varLenFrame-- }()
	for _, c := range cands {
		found, err := pe.varLenExists(ctx, vs, c.traversalDst(), depth+1, pushRel(used, c.instance()))
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

// varLenReach is the reachability fast path (see the file comment): a
// node-visited breadth-first search from `from`, exact for every end node other
// than from when the hop is the whole pattern and min ≤ 1. from itself is
// accepted at depth 0, that is when min is 0, and — when cycle is set, which the
// caller does only for a DIRECTED hop — at depth d + 1 when a node dequeued at
// depth d < max carries a qualifying relationship back to from: the shortest path
// from → … → u followed by u → from is a simple cycle, which repeats no
// relationship, and its length d + 1 ≥ 1 ≥ min.
func (pe *patternEvaluator) varLenReach(ctx context.Context, vs *varLenSearch, from graph.NodeID, cycle bool) (bool, error) {
	frontier := []patBFSNode{{id: from, depth: 0}}
	visited := map[graph.NodeID]struct{}{from: {}}
	mapper := pe.g.AdjList().Mapper()
	fromKey, fromOK := mapper.Resolve(from)
	for len(frontier) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		cur := frontier[0]
		frontier = frontier[1:]
		if cur.depth >= vs.minDepth && vs.accepts(pe, cur.id) {
			return true, nil
		}
		if cur.depth >= vs.maxDepth {
			continue
		}
		curKey, resolved := mapper.Resolve(cur.id)
		if !resolved {
			continue
		}
		if cycle && fromOK && pe.closesCycle(curKey, fromKey, vs) {
			return true, nil
		}
		if err := pe.bfsExpandStep(ctx, mapper, cur.id, curKey, vs.s.rel, vs.dir, visited, &frontier, cur.depth); err != nil {
			return false, err
		}
	}
	return false, nil
}

// closesCycle reports whether the node keyed curKey carries a relationship of the
// hop's type back to the start node keyed fromKey, in the hop's direction.
func (pe *patternEvaluator) closesCycle(curKey, fromKey string, vs *varLenSearch) bool {
	if vs.dir == ast.RelDirectionIncoming {
		return pe.edgeMatchesRel(fromKey, curKey, vs.s.rel)
	}
	return pe.edgeMatchesRel(curKey, fromKey, vs.s.rel)
}

// enumerateVarLen is [patternEvaluator.enumerateSteps] for a variable-length hop:
// it calls cb once per complete match, with the end node and the hop's
// relationship variable — a list of the relationships crossed, in traversal
// order — bound on the row.
func (pe *patternEvaluator) enumerateVarLen(ctx context.Context, srcID graph.NodeID, s step, remaining []step, row expr.RowContext, used relPath, cb func(expr.RowContext) error) error {
	minDepth, maxDepth := varLenBounds(s.rel)
	if list, bound := boundRelList(s.rel, row); bound {
		end, next, ok, err := pe.walkBoundRelList(ctx, srcID, s, row, used, list, minDepth, maxDepth)
		if err != nil || !ok {
			return err
		}
		out := cloneRow(row)
		if s.node != nil && s.node.Variable != nil {
			out[*s.node.Variable] = nodeValueForID(pe.g, end)
		}
		return pe.enumerateSteps(ctx, end, remaining, out, next, cb)
	}
	vs := varLenSearch{s: s, remaining: remaining, row: row, minDepth: minDepth, maxDepth: maxDepth, dir: hopDirection(s.rel), start: srcID}
	return pe.varLenEnum(ctx, &vs, srcID, 0, ensureRelPath(used), nil, cb)
}

// varLenEnum is the exact enumerating search from cur at depth hops; hops is the
// path so far. Sibling extensions share hops' backing array, which is sound
// because each is consumed — materialised at acceptance — before the next sibling
// overwrites it.
func (pe *patternEvaluator) varLenEnum(ctx context.Context, vs *varLenSearch, cur graph.NodeID, depth int64, used relPath, hops []candidateHop, cb func(expr.RowContext) error) error {
	if depth >= vs.minDepth && vs.accepts(pe, cur) {
		next := cloneRow(vs.row)
		if n := vs.s.node; n != nil && n.Variable != nil {
			next[*n.Variable] = nodeValueForID(pe.g, cur)
		}
		if r := vs.s.rel; r.Variable != nil {
			rels := make(expr.ListValue, len(hops))
			for i, h := range hops {
				rels[i] = relValueFromHop(pe.g, h, r)
			}
			next[*r.Variable] = rels
		}
		if err := pe.enumerateSteps(ctx, cur, vs.remaining, next, used, cb); err != nil {
			return err
		}
	}
	if depth >= vs.maxDepth {
		return nil
	}
	cands, err := pe.varLenCandidates(ctx, cur, vs.s.rel, vs.dir, used, pe.frameBuffer())
	pe.varLenBufs[pe.varLenFrame] = cands
	if err != nil {
		return err
	}
	pe.varLenFrame++
	defer func() { pe.varLenFrame-- }()
	for _, c := range cands {
		if err := pe.varLenEnum(ctx, vs, c.traversalDst(), depth+1, pushRel(used, c.instance()), append(hops, c), cb); err != nil {
			return err
		}
	}
	return nil
}

// boundRelList reports whether the variable-length hop's relationship variable is
// already bound on the row, and to what. openCypher lets a bound list of
// relationships stand for the path it spells (clauses/match/Match4.feature [8],
// Match9.feature [6] and [7]).
func boundRelList(rel *ast.RelationshipPattern, row expr.RowContext) (expr.Value, bool) {
	if rel == nil || rel.Variable == nil {
		return nil, false
	}
	v, ok := row[*rel.Variable]
	return v, ok
}

// walkBoundRelList matches the variable-length hop s against the bound value v:
// v must be a list of relationships that, in order, form a path from srcID in the
// hop's direction, each of an accepted type and none already on used, of a length
// within [minDepth, maxDepth], ending on a node the end-node pattern accepts. It
// returns that end node and used extended by the list. Any other value — NULL, a
// single relationship, a list holding a non-relationship — matches nothing.
func (pe *patternEvaluator) walkBoundRelList(ctx context.Context, srcID graph.NodeID, s step, row expr.RowContext, used relPath, v expr.Value, minDepth, maxDepth int64) (graph.NodeID, relPath, bool, error) {
	list, ok := v.(expr.ListValue)
	if !ok || int64(len(list)) < minDepth || int64(len(list)) > maxDepth {
		return 0, used, false, nil
	}
	dir := hopDirection(s.rel)
	cur := srcID
	used = ensureRelPath(used)
	for _, item := range list {
		rv, ok := item.(expr.RelationshipValue)
		if !ok {
			return 0, used, false, nil
		}
		cands, err := pe.varLenCandidates(ctx, cur, s.rel, dir, used, pe.frameBuffer())
		pe.varLenBufs[pe.varLenFrame] = cands
		if err != nil {
			return 0, used, false, err
		}
		matched := false
		for _, c := range cands {
			if c.handle == rv.ID && uint64(c.srcID) == rv.StartID && uint64(c.dstID) == rv.EndID {
				used = pushRel(used, c.instance())
				cur = c.traversalDst()
				matched = true
				break
			}
		}
		if !matched {
			return 0, used, false, nil
		}
	}
	if !pe.checkEndNode(s.node, cur, row) {
		return 0, used, false, nil
	}
	return cur, used, true, nil
}

// bfsExpandStep appends unvisited neighbours reachable in direction dir from
// (curID, curKey) to frontier, respecting the edge-type filter in rel.
func (pe *patternEvaluator) bfsExpandStep(ctx context.Context, mapper *graph.Mapper[string], curID graph.NodeID, curKey string, rel *ast.RelationshipPattern, dir ast.RelDirection, visited map[graph.NodeID]struct{}, frontier *[]patBFSNode, depth int64) error {
	switch dir {
	case ast.RelDirectionOutgoing:
		pe.bfsExpandOutgoing(mapper, curID, curKey, rel, visited, frontier, depth)
		return nil
	case ast.RelDirectionIncoming:
		return pe.bfsExpandIncoming(ctx, curID, curKey, rel, visited, frontier, depth)
	default: // undirected
		pe.bfsExpandOutgoing(mapper, curID, curKey, rel, visited, frontier, depth)
		return pe.bfsExpandIncoming(ctx, curID, curKey, rel, visited, frontier, depth)
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

// patBFSNode is a frontier element for the variable-length pattern BFS.
type patBFSNode struct {
	id    graph.NodeID
	depth int64
}

// bfsExpandIncoming appends reverse-direction neighbours to frontier for BFS.
// The sources are found by [patternEvaluator.scanIncoming], which skips visited
// nodes, and their edge type is tested only after its walk has returned
// (rmp #2896).
func (pe *patternEvaluator) bfsExpandIncoming(ctx context.Context, dstID graph.NodeID, dstKey string, rel *ast.RelationshipPattern, visited map[graph.NodeID]struct{}, frontier *[]patBFSNode, depth int64) error {
	hits, err := pe.scanIncoming(ctx, dstID, true, false, visited)
	if err != nil {
		return err
	}
	for _, hit := range hits {
		if !pe.edgeMatchesRel(hit.key, dstKey, rel) {
			continue
		}
		visited[hit.id] = struct{}{}
		*frontier = append(*frontier, patBFSNode{id: hit.id, depth: depth + 1})
	}
	return nil
}
