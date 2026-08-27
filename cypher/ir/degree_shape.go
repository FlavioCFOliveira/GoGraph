package ir

// degree_shape.go — the ROW-INDEPENDENT half of the degree-count eligibility
// test (rmp #2264).
//
// # Why this lives in ir rather than beside the rewrite that uses it
//
// The runtime degree rewrite (cypher.recogniseDegreePattern) decides whether an
// EXISTS/COUNT subquery, or a `size([ … ])` pattern comprehension, can be
// answered from the anchor's adjacency degree instead of by enumerating its
// neighbours. That decision has two parts: a STRUCTURAL test on the pattern,
// which needs nothing but the AST, and a BINDING test, which needs the outer
// row.
//
// The IR translator has to make the structural half of the same decision, one
// phase earlier and with no row in hand: projectionsWithComprehensions hoists
// every pattern comprehension in a RETURN or WITH item into a RollUpApply and
// substitutes a variable, so by evaluation time there is no comprehension left
// in a projection for the rewrite to recognise. That hoist is what made the two
// spellings of one question diverge — `RETURN COUNT { (a)-[:K]->() }` answered
// from the degree in 2.159 ms while `RETURN size([ (a)-[:K]->(x) | 1 ])` built
// a 100 000-element list in 52.737 ms, a 24.4× gap on the same graph, same
// question, same answer.
//
// Sharing the structural test is what keeps the fix safe. Two copies of these
// clauses would drift, and a translator that skipped the hoist for a shape the
// runtime then refused would leave the comprehension to the general evaluator —
// correct, but silently back to the slow path with no signal. One definition,
// used by both, cannot drift.
//
// # Eligibility is Neo4j's QuerySolvableByGetDegree
//
// Every clause below mirrors one in cypher/degree_rewrite.go, which documents
// the correspondence to Neo4j 5.26's getDegreeRewriter.scala at pinned SHA
// eccd584a64d468af3daeab421478fe78567c518f. The governing rule is
// `Selections.empty`: any predicate at all — a label, a property map, an inline
// WHERE — disqualifies the pattern, because a degree counts every out-edge and
// cannot filter.

import (
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// DegreeShape carries the structural facts a degree count needs, extracted once
// from the AST. It says nothing about whether the pattern is answerable for a
// particular row; that is the caller's remaining obligation.
type DegreeShape struct {
	// AnchorVar is the variable naming the node whose degree is wanted. The
	// caller must confirm it is bound in the row before using the shape.
	AnchorVar string
	// FarVar is the far endpoint's variable, or "" when the pattern does not
	// name one. When it is non-empty the caller must confirm it is NOT bound in
	// the row: a bound far endpoint makes the pattern an expand-into between two
	// fixed nodes, not a degree.
	FarVar string
	// RelType is the single relationship type, or "" when the pattern is
	// untyped. Cypher has no empty relationship type, so "" is unambiguous.
	RelType string
}

// DegreeCountableShape reports whether pat is structurally degree-answerable,
// and returns the extracted shape when it is.
//
// where is the pattern's inline WHERE; a non-nil WHERE is a Selection and
// disqualifies the pattern outright.
//
// This is the row-INDEPENDENT half of the test. A true result means the pattern
// has the right shape, not that it can be answered for any given row: the
// caller must still check that Shape.AnchorVar is bound and that Shape.FarVar,
// when non-empty, is not.
func DegreeCountableShape(pat *ast.Pattern, where *ast.Where) (DegreeShape, bool) {
	var zero DegreeShape
	if pat == nil || where != nil {
		return zero, false
	}
	// Exactly one path. Two comma-separated paths are a join, not a degree.
	if len(pat.Paths) != 1 {
		return zero, false
	}
	path := pat.Paths[0]
	// A path variable makes the matched path itself observable; shortestPath is
	// a different operator entirely.
	if path == nil || path.Variable != nil || path.Shortest != ast.ShortestNone {
		return zero, false
	}
	// Exactly node — relationship — node, and nothing after it.
	head := path.Head
	if head == nil || head.Node == nil || head.Relationship != nil {
		return zero, false
	}
	second := head.Next
	if second == nil || second.Relationship == nil || second.Node == nil || second.Next != nil {
		return zero, false
	}

	// ANCHOR: a variable carrying no predicate of its own. A label or property
	// here is a Selection the degree would silently ignore, which would be a
	// wrong answer rather than a slow one.
	anchor := head.Node
	if anchor.Variable == nil || len(anchor.Labels) != 0 || anchor.Properties != nil {
		return zero, false
	}

	// RELATIONSHIP: single hop, not named BY THE USER, untyped or exactly one
	// type, no properties, outgoing.
	//
	// The name test is [UserNamed], not `!= nil`: a user-bound relationship makes
	// the edge observable, which a bare degree cannot supply, but a SYNTHETIC name
	// observes nothing. Spelled `!= nil` this guard silently stopped firing once
	// NameSubqueryAnonymousEntities began naming subquery relationships up front,
	// and the shapes fell back to driving an inner plan per outer row (rmp #2508).
	rel := second.Relationship
	if rel.Range != nil || UserNamed(rel.Variable) || rel.Properties != nil {
		return zero, false
	}
	if rel.Direction != ast.RelDirectionOutgoing {
		return zero, false
	}
	if len(rel.Types) > 1 {
		return zero, false
	}

	// FAR NODE: no label, no property.
	far := second.Node
	if len(far.Labels) != 0 || far.Properties != nil {
		return zero, false
	}

	sh := DegreeShape{AnchorVar: *anchor.Variable}
	// Only a USER name goes into FarVar, so the field keeps the meaning its
	// godoc states — "" when the pattern does not name the far endpoint. A
	// synthetic name is never bound in the outer row, so recording it would
	// change no decision, but it would make the documented invariant false.
	if UserNamed(far.Variable) {
		sh.FarVar = *far.Variable
	}
	if len(rel.Types) == 1 {
		sh.RelType = rel.Types[0]
	}
	return sh, true
}

// DegreeCountableComprehension reports whether pc is a pattern comprehension
// whose length can be answered from a degree — that is, whether
// `size(pc)` is a candidate for the runtime degree rewrite.
//
// A projection is ignored by a degree count, and that is sound rather than a
// shortcut: it is applied once per match and cannot change how MANY matches
// there are. An inner WHERE is a different matter — it filters matches — so a
// comprehension carrying one is refused, as is one that binds a path variable.
func DegreeCountableComprehension(pc *ast.PatternComprehension) bool {
	if pc == nil || pc.Predicate != nil || pc.Variable != nil || pc.Pattern == nil {
		return false
	}
	_, ok := DegreeCountableShape(&ast.Pattern{Paths: []*ast.PathPattern{pc.Pattern}}, nil)
	return ok
}

// isDegreeCountableSizeCall reports whether n is exactly `size(pc)` for a
// degree-answerable pattern comprehension pc — the one shape the translator
// leaves unhoisted so the runtime degree rewrite can serve it.
//
// The function name is matched case-insensitively and only in the unnamespaced
// form, matching how cypher/expr resolves it: a namespaced `foo.size(...)` is a
// different function and must keep the ordinary hoist.
func isDegreeCountableSizeCall(n *ast.FunctionInvocation) bool {
	if n == nil || len(n.Namespace) != 0 || len(n.Args) != 1 {
		return false
	}
	if !strings.EqualFold(n.Name, "size") {
		return false
	}
	pc, ok := n.Args[0].(*ast.PatternComprehension)
	return ok && DegreeCountableComprehension(pc)
}
