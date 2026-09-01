package ir

// subquery_body.go — the pattern-form view of a BLOCK-form EXISTS { } / COUNT { }
// subquery body (rmp #2648).
//
// # The problem
//
// GoGraph's AST carries an EXISTS/COUNT subquery in one of TWO shapes
// ([ast.CountSubquery], [ast.ExistsSubquery]):
//
//	COUNT { (a)-[:K]->(:Q) }        →  Pattern + Where set, Query nil
//	COUNT { MATCH (a)-[:K]->(:Q) }  →  Query set, Pattern nil
//
// Both adjacency-answered fast paths — [DegreeCountableShape] and the labelled
// single-hop recogniser in cypher/labelled_hop_count.go — take an
// [ast.Pattern]. Both nil-guard on it at their first line, so for the SECOND
// spelling they were structurally unreachable: the same question, asked the
// other way round, drove a compiled inner plan once per outer row.
//
// # Reference design: Neo4j has ONE representation, not two
//
// Read at github.com/neo4j/neo4j, release tag 2026.07.1 (commit
// f213380f812b820a1b312e2ea52cb3d8f1931ccc):
//
//   - community/cypher/front-end/ast/src/main/scala/org/neo4j/cypher/internal/ast/CountExpression.scala
//     declares `case class CountExpression(query: Query)`. There is no pattern
//     field. The AST cannot express the pattern form at all.
//   - community/cypher/front-end/parser/v25/ast-factory/src/main/scala/org/neo4j/cypher/internal/parser/v25/ast/factory/ExpressionBuilder.scala,
//     private method `subqueryBuilder`, is where that happens: given a
//     `patternList` instead of a `queryWithLocalDefinitions`, it BUILDS
//     `SingleQuery(Match(optional = false, matchMode, Pattern.ForMatch(parts),
//     hints = Seq.empty, where, None))` and returns it as the one `Query`.
//     `exitCountExpression` and `exitExistsExpression` both go through it.
//   - community/cypher/cypher-planner/src/main/scala/org/neo4j/cypher/internal/compiler/ast/convert/plannerQuery/CreateIrExpressions.scala
//     therefore never sees a "pattern form": `case countExpression @
//     CountExpression(q)` converts the single `Query` to a planner query IR.
//   - community/cypher/cypher-planner/src/main/scala/org/neo4j/cypher/internal/compiler/planner/logical/steps/getDegreeRewriter.scala
//     then decides eligibility by pattern-matching the resulting QueryGraph
//     (`QuerySolvableByGetDegree`, `ExistsQuerySolvableByGetDegree`), not the
//     syntax. Spelling-independence is a property of the representation, not of
//     the recogniser.
//
// So Neo4j's normalisation runs pattern → block, in the PARSER, and its degree
// recogniser is spelling-agnostic by construction rather than by a second rule.
//
// # What Neo4j's guards actually say, and why GoGraph cannot copy them
//
// `QuerySolvableByGetDegree.unapply` requires, as written: exactly one
// `PatternRelationship` of `SimplePatternLength`; empty quantified path
// patterns; exactly one `argumentId`, which must be one of the two pattern
// nodes; `patternNodes == Set(firstNode, secondNode)`; `Selections.empty` —
// no predicate ANYWHERE, a label included; empty optional matches, hints and
// shortest-relationship patterns; `InterestingOrder.empty`; a horizon that is
// either a `RegularQueryProjection` with `QueryPagination.empty` and
// `Selections.empty` or any `AggregatingQueryProjection`; and `tail = None`.
// `getDegreeRewriter.isEligible` then requires the IR expression's
// dependencies to contain the anchor and neither the relationship nor the far
// node.
//
// Two of those clauses are the load-bearing ones for this file, and they are
// where Neo4j's premises stop holding in GoGraph:
//
//   - `tail = None` is what excludes a WITH, a UNION, a DISTINCT projection and
//     a SKIP/LIMIT — because `CreateIrExpressions` reacts to each of those by
//     appending a tail carrying the `count(*)` aggregation instead of
//     overriding the final horizon.
//   - the horizon clause ADMITS an arbitrary RETURN item list, because for a
//     COUNT `CreateIrExpressions` REPLACES the projection with
//     `AggregatingQueryProjection(count(*))` whenever the horizon is a
//     `RegularQueryProjection` with no pagination and no selections.
//
// GoGraph has no query-graph IR at the point where its recognisers run: they
// run at RUNTIME, per subquery occurrence, on the AST, and they are syntactic.
// Reproducing Neo4j's admitted set would mean re-deriving its horizon rules as
// a SECOND syntactic policy here — no DISTINCT, no SKIP/LIMIT, no ORDER BY, no
// aggregating return item — and adopting, with it, a behaviour Neo4j accepts as
// a side effect of owning the whole planner: the projection is never evaluated,
// so any error it would raise (`RETURN 1/0`) and any aggregation it performs
// disappear. That is a wider claim than this task measured, and it buys nothing
// for the shapes that motivated it.
//
// # The boundary GoGraph takes, and why
//
// Neo4j's STRATEGY is adopted: make one canonical form reach one recogniser, so
// the two spellings cannot diverge. Neo4j's DIRECTION is inverted, because
// GoGraph's recognisers consume an [ast.Pattern] and its parser has already
// produced two shapes — so GoGraph normalises block → pattern.
//
// The boundary is the EXACT INVERSE of GoGraph's own desugaring: the body must
// be precisely what cypher/subquery_eval.go's countToSingleQuery /
// existsToSingleQuery would have BUILT from a pattern form — one non-optional
// MATCH, its optional WHERE, and nothing else. At that point the two spellings
// are provably the same query, so the normalisation preserves semantics by
// construction rather than by argument. TestPatternFormOf_IsInverseOfDesugaring
// asserts the round trip.
//
// The WHERE is RETURNED, not refused and not dropped. Both recognisers already
// refuse a non-nil Where (it is a Selection they cannot evaluate), so the block
// form is then refused for the IDENTICAL reason as the pattern form — which is
// the whole point — and a future recogniser taught to evaluate a simple
// predicate inherits the block form for free. Dropping it here is the rmp #2242
// defect, one layer up.
//
// Deliberately left out, and recorded rather than silently absent: the
// RETURN-bearing body that Neo4j admits (`COUNT { MATCH (a)-->(b) RETURN b }`),
// every tail-producing body (WITH, DISTINCT, SKIP/LIMIT, ORDER BY, UNION), and
// OPTIONAL MATCH. The last one is not conservatism but correctness:
// `COUNT { OPTIONAL MATCH (a)-[:K]->(:Q) }` yields ONE row of nulls when
// nothing matches, so its count is 1 where a degree says 0 — Neo4j refuses it
// too, through `optionalMatches` having to be empty.

import "github.com/FlavioCFOliveira/GoGraph/cypher/ast"

// PatternFormOf returns the (pattern, where) pair that a BLOCK-form
// EXISTS { … } / COUNT { … } body is exactly sugar for, and reports false for
// any other body.
//
// A true result means q is a body of the form `MATCH <pattern> [WHERE <pred>]`
// and nothing else, so an evaluator may treat it as the pattern-form spelling
// of the same subquery. The returned pointers are the AST's own — this function
// neither copies nor mutates anything, so the caller must not write through
// them.
//
// It says nothing about whether the pattern is answerable from the adjacency;
// that remains [DegreeCountableShape]'s and the labelled-hop recogniser's
// decision, applied to the pattern this returns exactly as it is applied to a
// pattern the user spelled directly.
//
// See the file comment for the Neo4j evidence behind the boundary, for what is
// deliberately excluded, and for why the WHERE is returned rather than refused.
func PatternFormOf(q *ast.SingleQuery) (*ast.Pattern, *ast.Where, bool) {
	if q == nil {
		return nil, nil, false
	}
	// A RETURN, a WITH, or an updating clause puts a HORIZON between the match
	// and the answer, and the horizon can change how many rows reach it —
	// DISTINCT, SKIP/LIMIT and an aggregating return item all do. This is the
	// clause that stands in for Neo4j's `tail = None` plus its horizon test, and
	// it is stricter: Neo4j admits a plain RETURN because its planner replaces
	// the projection with count(*), which GoGraph's runtime recogniser cannot do.
	if q.Return != nil || len(q.With) != 0 || len(q.UpdatingClauses) != 0 {
		return nil, nil, false
	}
	// Exactly one reading clause. Two of them are two joined patterns, not one
	// hop: `COUNT { MATCH (a)-[:K]->(b) MATCH (b)-[:K]->(c) }` counts paths, and
	// `COUNT { UNWIND [1,2] AS x MATCH (a)-[:K]->() }` counts each edge twice.
	// Neo4j refuses both by requiring a single PatternRelationship and
	// patternNodes == {firstNode, secondNode}.
	if len(q.ReadingClauses) != 1 {
		return nil, nil, false
	}
	// The type assertion is the guard, and it is total: [ast.OptionalMatch],
	// [ast.With], [ast.Return], [ast.Unwind], [ast.Call], [ast.Union] and
	// [ast.Where] all satisfy [ast.ReadingClause], so every one of them fails
	// here rather than needing a clause of its own. OPTIONAL MATCH failing is
	// load-bearing: it emits a row of nulls when nothing matches, so its count
	// is 1 where a degree says 0.
	//
	// Testing the type rather than enumerating the alternatives is also what
	// makes this safe against a new [ast.ReadingClause] implementation: an
	// unrecognised clause is refused by default.
	m, ok := q.ReadingClauses[0].(*ast.Match)
	if !ok || m.Pattern == nil {
		return nil, nil, false
	}
	return m.Pattern, m.Where, true
}
