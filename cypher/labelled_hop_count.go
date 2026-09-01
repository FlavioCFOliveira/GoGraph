package cypher

// labelled_hop_count.go — counting a single labelled hop from the adjacency
// (rmp #2235).
//
// # Why this is not the degree rewrite
//
// It would be natural to read this as "#2232 but with labels", and building it
// that way would be a mistake. #2232 implements Neo4j's degree rewrite, whose
// recogniser (QuerySolvableByGetDegree) requires Selections.empty — and a label
// on a pattern node IS a Selection (HasLabels) in Neo4j's query graph. A labelled
// far node is therefore ineligible for a degree rewrite in Neo4j too, and for a
// structural reason rather than an oversight: a degree counts every out-edge and
// has no way to ask anything about where an edge lands.
//
// So this is a DIFFERENT optimisation living in a separate file with a separate
// recogniser and a separate counter. #2232's recogniser is left exactly as narrow
// as Neo4j's. That separation is deliberate — the round-3 lesson was that
// widening a recogniser steals shapes from other, cardinality-reducing rewrites,
// and the way to avoid paying it again is not to widen anything.
//
// # What it does instead
//
// One walk of the anchor's adjacency, counting the edges whose relationship type
// matches and whose far endpoint carries the required labels. That is Theta(d)
// where a degree is Theta(1), but it replaces a full inner-plan drive per outer
// row — no plan Init, no Argument seeding, no row materialisation, no neighbour
// resolution — and the measured constant is what dominates at property-graph
// degrees.
//
// # Where the label answer comes from
//
// Membership is tested against [label.Index] through the graph's own node index,
// which is the same index the label scan reads (cypher/exec.ScanLabel resolves
// through the same registry and bitmap). The count and a plan that scanned the
// label therefore cannot disagree about which nodes carry it.
//
// Liveness is NOT tested here. It belongs to
// [lpg.Graph.OutDegreeMatchingBoundedByID], which applies the same tombstone gate
// as every other degree walker, so this path cannot drift from them.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// labelledHopRewriteCount counts how many times a labelled single-hop count was
// answered from the adjacency instead of by driving an inner plan. Tests assert
// on it so an eligibility change cannot silently stop firing — the runtime
// counter, never another rendering.
//
//nolint:gochecknoglobals // process-wide diagnostic counter, matching degreeRewriteCount
var labelledHopRewriteCount atomic.Uint64

// labelledHopShape describes a recognised `(anchor)-[:T]->(:L…)` pattern: a
// single outgoing hop to a far node qualified only by labels.
//
// Names are held rather than resolved ids for the same reason
// [degreeShape] holds them: a label or type may not be interned when the pattern
// is first recognised and may be by the time a later row evaluates it.
// [lpg.LabelRegistry] is append-only and never reassigns an id, so a resolution
// is cached on first success and retried until then.
type labelledHopShape struct {
	anchorVar string

	typeName string
	typed    bool
	relType  lpg.LabelID
	haveRel  bool

	// farLabels are the labels the far node must carry, ALL of them — a
	// multi-label pattern node is a conjunction.
	farLabels []string
	// farIDs is farLabels resolved to interned ids, populated only once every
	// one of them resolves. A label that has never been interned is carried by
	// no node, which makes the count zero rather than unresolvable.
	farIDs []uint32
}

// recogniseLabelledHopPattern reports whether pat is a single labelled hop that
// can be counted from the adjacency for an outer row shaped like row.
//
// pat is spelling-independent since rmp #2648: it is the pattern of a pattern-form
// subquery, or the pattern of the single MATCH of a block form that is exactly the
// same query ([ir.PatternFormOf]). The clauses below were not widened for that —
// the block form simply started arriving with a pattern instead of nil.
//
// The eligibility rules are [recogniseDegreePattern]'s, with exactly one
// difference: the far node MUST carry at least one label, where the degree
// recogniser requires it carry none. Everything a degree cannot express — an
// inner WHERE, a property, a named or ranged relationship, an incoming or
// undirected hop, a label on the anchor — is refused here too, because none of
// them is expressible as "walk the anchor's adjacency once".
//
// A label on the ANCHOR is refused specifically: the anchor is already bound, so
// a predicate on it belongs to the caller's row and is not this walk's business.
func recogniseLabelledHopPattern(pat *ast.Pattern, where *ast.Where, row expr.RowContext) (*labelledHopShape, bool) {
	if pat == nil || where != nil {
		return nil, false
	}
	if len(pat.Paths) != 1 {
		return nil, false
	}
	path := pat.Paths[0]
	if path == nil || path.Variable != nil || path.Shortest != ast.ShortestNone {
		return nil, false
	}

	// Exactly node — relationship — node, and nothing after it.
	head := path.Head
	if head == nil || head.Node == nil || head.Relationship != nil {
		return nil, false
	}
	second := head.Next
	if second == nil || second.Relationship == nil || second.Node == nil || second.Next != nil {
		return nil, false
	}

	// ANCHOR: a bound variable carrying no predicate of its own.
	anchor := head.Node
	if anchor.Variable == nil || len(anchor.Labels) != 0 || anchor.Properties != nil {
		return nil, false
	}
	if _, bound := row[*anchor.Variable]; !bound {
		return nil, false
	}

	// RELATIONSHIP: single hop, not named BY THE USER, untyped or exactly one
	// type, no properties, outgoing.
	//
	// [ir.UserNamed] rather than `!= nil`, for the reason given at the same guard
	// in ir/degree_shape.go: a synthetic name observes nothing, and spelling this
	// syntactically made the recogniser stop firing once subquery relationships
	// were named up front (rmp #2508).
	rel := second.Relationship
	if rel.Range != nil || ir.UserNamed(rel.Variable) || rel.Properties != nil {
		return nil, false
	}
	if rel.Direction != ast.RelDirectionOutgoing || len(rel.Types) > 1 {
		return nil, false
	}

	// FAR NODE: at least one label — that is what distinguishes this shape from
	// the degree one — and no properties. A property would be a Selection this
	// walk cannot evaluate. Its variable may be introduced here, but must not
	// name one the outer row already binds: two fixed endpoints make this an
	// expand-into, not a count of neighbours.
	far := second.Node
	if len(far.Labels) == 0 || far.Properties != nil {
		return nil, false
	}
	if far.Variable != nil {
		if _, bound := row[*far.Variable]; bound {
			return nil, false
		}
	}

	sh := &labelledHopShape{
		anchorVar: *anchor.Variable,
		farLabels: far.Labels,
	}
	if len(rel.Types) == 1 {
		sh.typed = true
		sh.typeName = rel.Types[0]
	}
	return sh, true
}

// count returns how many of the anchor's out-edges match the shape, capped at
// limit when limit >= 0.
//
// ok is false only when the anchor is not a resolvable node in this graph — a
// NULL binding, a deleted entity, or a value that is not a node — in which case
// the caller must fall back to the inner plan. An unresolvable LABEL is not such
// a case: a label the registry has never interned is carried by no node, so the
// answer is a real zero.
func (s *labelledHopShape) count(g *lpg.ReadView[string, float64], row expr.RowContext, limit int64) (int64, bool) {
	if g == nil {
		return 0, false
	}
	v, present := row[s.anchorVar]
	if !present {
		return 0, false
	}
	// Stay in id space: resolving the anchor to a node key and back would cost a
	// string hash per outer row, which is the cost this path exists to remove.
	id, resolved := nodeIDFromValue(v, g.AdjList().Mapper())
	if !resolved {
		return 0, false
	}

	if s.typed && !s.resolveRelType(g) {
		// The type is not interned, so no edge carries it.
		return 0, true
	}
	if !s.resolveFarLabels(g) {
		// A required label is not interned, so no node carries it.
		return 0, true
	}

	ceiling := maxDegreeLimit
	if limit >= 0 {
		if limit == 0 {
			return 0, true
		}
		ceiling = int(limit)
	}

	idx := g.NodeIndex()
	if idx == nil {
		return 0, false
	}
	n, found := g.OutDegreeMatchingBoundedByID(id, s.relType, s.typed, ceiling, func(dst graph.NodeID) bool {
		for _, lid := range s.farIDs {
			if !idx.Has(lid, dst) {
				return false
			}
		}
		return true
	})
	if !found {
		return 0, false
	}
	return int64(n), true
}

// resolveRelType caches the interned id of the relationship type, reporting
// false while the registry does not hold it.
func (s *labelledHopShape) resolveRelType(g *lpg.ReadView[string, float64]) bool {
	if s.haveRel {
		return true
	}
	id, ok := g.Registry().Lookup(s.typeName)
	if !ok {
		return false
	}
	s.relType, s.haveRel = id, true
	return true
}

// resolveFarLabels caches the interned ids of every required far-node label,
// reporting false while any one of them is missing. It is all-or-nothing because
// a partially resolved conjunction would silently drop a predicate.
func (s *labelledHopShape) resolveFarLabels(g *lpg.ReadView[string, float64]) bool {
	if s.farIDs != nil {
		return true
	}
	ids := make([]uint32, 0, len(s.farLabels))
	reg := g.Registry()
	for _, name := range s.farLabels {
		id, ok := reg.Lookup(name)
		if !ok {
			return false
		}
		ids = append(ids, uint32(id))
	}
	s.farIDs = ids
	return true
}

// labelledHopShapeFor memoises the recogniser's verdict per subquery occurrence,
// so it runs once rather than once per outer row. A cached nil is a pattern
// already examined and rejected, and so is the nil
// [EngineOptions.DisableAdjacencyCountRewrites] returns — the caller's response
// to either is to drive the inner plan.
//
// sub is the subquery expression in EITHER spelling; the (pattern, where) pair is
// derived from it by [subqueryRecogniserBody] INSIDE the memo, so a block-form
// body costs one walk per occurrence rather than one per outer row (rmp #2648).
// The recogniser below is unchanged: it stopped being handed a nil pattern for
// the block form, it did not learn a second shape.
func (e *subqueryEvaluator) labelledHopShapeFor(sub ast.Expression, row expr.RowContext) *labelledHopShape {
	if e.adjacencyCountsDisabled {
		return nil
	}
	if sh, seen := e.labelledHop[sub]; seen {
		return sh
	}
	pat, where := subqueryRecogniserBody(sub)
	sh, ok := recogniseLabelledHopPattern(pat, where, row)
	if !ok {
		sh = nil
	}
	e.labelledHop[sub] = sh
	return sh
}

// countLabelledHop answers a COUNT/EXISTS over a labelled single hop from the
// adjacency, reporting ok=false when the shape does not apply and the caller must
// drive the inner plan.
func (e *subqueryEvaluator) countLabelledHop(sub ast.Expression, row expr.RowContext, limit int64) (int64, bool) {
	sh := e.labelledHopShapeFor(sub, row)
	if sh == nil {
		return 0, false
	}
	n, ok := sh.count(e.g, row, limit)
	if !ok {
		return 0, false
	}
	labelledHopRewriteCount.Add(1)
	return n, true
}

// matchLabelledHop answers a pattern PREDICATE — `WHERE (a)-[:K]->(:P)` — from
// the adjacency. It is existence only, so the walk is capped at one match and
// stops at the first neighbour that qualifies.
//
// ok is false when the shape does not apply and the caller must enumerate.
func (pe *patternEvaluator) matchLabelledHop(pp *ast.PathPattern, row expr.RowContext) (found, ok bool) {
	if pe.g == nil || pp == nil {
		return false, false
	}
	sh := pe.labelledHopShapeFor(pp, row)
	if sh == nil {
		return false, false
	}
	n, resolved := sh.count(pe.g, row, 1)
	if !resolved {
		return false, false
	}
	labelledHopRewriteCount.Add(1)
	return n > 0, true
}

// labelledHopShapeFor memoises the recogniser's verdict per pattern occurrence.
// The pattern evaluator sees the same *ast.PathPattern pointer on every outer
// row, so without this the recogniser — and the shape allocation — would repeat
// per row on the very path this optimisation exists to make cheap.
//
// It returns nil when [EngineOptions.DisableAdjacencyCountRewrites] is set, which
// sends [patternEvaluator.matchLabelledHop]'s caller to the general enumeration.
func (pe *patternEvaluator) labelledHopShapeFor(pp *ast.PathPattern, row expr.RowContext) *labelledHopShape {
	if pe.adjacencyCountsDisabled {
		return nil
	}
	if sh, seen := pe.labelledHop[pp]; seen {
		return sh
	}
	sh, ok := recogniseLabelledHopPattern(&ast.Pattern{Paths: []*ast.PathPattern{pp}}, nil, row)
	if !ok {
		sh = nil
	}
	if pe.labelledHop == nil {
		pe.labelledHop = make(map[*ast.PathPattern]*labelledHopShape)
	}
	pe.labelledHop[pp] = sh
	return sh
}
