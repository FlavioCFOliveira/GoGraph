package ir_test

// subquery_return_test.go — rmp #2675: [ir.TranslateSubquery] must translate the
// body's TRAILING projection, not only its reading clauses.
//
// # Why a PLAN-SHAPE suite exists alongside the answer suite
//
// cypher/count_subquery_body_projection_test.go proves the ANSWER, which is what
// the defect was about. It cannot cover two of the six projection surfaces the
// task enumerates, and the reason is not a gap in that suite — it is the
// specified semantics:
//
//   - a plain `RETURN <expr>` cannot change how many rows the body produces, so
//     COUNT and EXISTS both answer the same with it translated or dropped;
//   - a bare `ORDER BY` cannot either, and the reference implementation says so
//     explicitly: for a COUNT body whose horizon is a plain projection with no
//     pagination and no selections, Neo4j OVERRIDES that horizon with
//     `AggregatingQueryProjection(count(*))` and then removes the interesting
//     order outright — "And also remove any ORDER BY since that won't have any
//     impact in a COUNT subquery anyway"
//     (github.com/neo4j/neo4j, release tag 2026.07.1, commit
//     f213380f812b820a1b312e2ea52cb3d8f1931ccc,
//     community/cypher/cypher-planner/src/main/scala/org/neo4j/cypher/internal/compiler/ast/convert/plannerQuery/CreateIrExpressions.scala,
//     `case countExpression @ CountExpression(q)`). Its test
//     "Rewrites CountExpression with ORDER BY" in the sibling
//     CreateIrExpressionsTest.scala pins exactly that: the resulting planner
//     query carries no ORDER BY and no tail.
//
// So for those two an answer-level differential is IMPOSSIBLE, not merely
// awkward, and a case whose right answer equals the wrong one is not evidence.
// The observable that does discriminate for them is the plan: before the fix the
// body's projection produced no operator at all. Every case below therefore
// asserts on the operator the clause must produce, and every one of them fails
// with the `q.Return` translation removed.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
)

// subqueryBody parses `MATCH (a) RETURN COUNT { <body> }` and returns the
// parsed body as the *ast.SingleQuery that the subquery evaluator hands to
// [ir.TranslateSubquery].
//
// It goes through the real parser rather than a hand-built AST because the
// defect was precisely that one parser-populated field — SingleQuery.Return —
// went unread. A hand-built body would let the test set the field the code
// under test is supposed to notice, which is the wrong way round.
func subqueryBody(t *testing.T, body string) *ast.SingleQuery {
	t.Helper()
	src := "MATCH (a) RETURN COUNT { " + body + " }"
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		t.Fatalf("Parse(%q) returned %T, want *ast.SingleQuery", src, q)
	}
	if sq.Return == nil || sq.Return.Projection == nil || len(sq.Return.Projection.Items) == 0 {
		t.Fatalf("Parse(%q) produced no RETURN item to read the subquery from", src)
	}
	cs, ok := sq.Return.Projection.Items[0].Expr.(*ast.CountSubquery)
	if !ok {
		t.Fatalf("Parse(%q) put %T in the RETURN item, want *ast.CountSubquery",
			src, sq.Return.Projection.Items[0].Expr)
	}
	if cs.Query == nil {
		t.Fatalf("Parse(%q) produced the PATTERN form, so there is no body RETURN to translate", src)
	}
	if cs.Query.Return == nil {
		t.Fatalf("Parse(%q) left SingleQuery.Return nil, so this case cannot prove the field is read", src)
	}
	return cs.Query
}

// planKinds returns the concrete type names of every operator in plan, root
// first, so a case can assert on the presence of one without depending on the
// exact tree shape around it.
func planKinds(plan ir.LogicalPlan) []string {
	var out []string
	var walk func(ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil {
			return
		}
		out = append(out, fmt.Sprintf("%T", p))
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return out
}

func hasKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// TestTranslateSubquery_TranslatesTheBodysTrailingProjection is the plan-level
// half of rmp #2675.
//
// Before the fix [ir.TranslateSubquery] looped over q.ReadingClauses and
// returned, so EVERY case below produced a plan with no Projection in it at all
// — the body's DISTINCT, ORDER BY, SKIP, LIMIT and aggregation were dropped on
// the floor. Each case names the operator its clause must produce; the
// Projection assertion is common to all of them because openCypher's projection
// is what every one of those clauses hangs off.
func TestTranslateSubquery_TranslatesTheBodysTrailingProjection(t *testing.T) {
	const argTag uint32 = 7

	cases := []struct {
		name     string
		body     string
		wantKind string
		why      string
	}{
		{
			name:     "RETURN of a plain item",
			body:     "MATCH (a)-[:K]->(x) RETURN x",
			wantKind: "*ir.Projection",
			why: "the horizon itself. It cannot change the row count — which is why the answer " +
				"suite cannot discriminate on it — but a body whose projection produces no " +
				"operator has not been translated at all",
		},
		{
			name:     "RETURN of a computed expression",
			body:     "MATCH (a)-[:K]->(x) RETURN x.id + 1",
			wantKind: "*ir.Projection",
			why:      "the expression must be evaluated by an operator, not discarded with the clause",
		},
		{
			name:     "RETURN DISTINCT",
			body:     "MATCH (a)-[:K]->(x) RETURN DISTINCT a",
			wantKind: "*ir.Distinct",
			why:      "DISTINCT dedupes, so it changes the counted quantity",
		},
		{
			name:     "ORDER BY",
			body:     "MATCH (a)-[:K]->(x) RETURN x ORDER BY x.id",
			wantKind: "*ir.Sort",
			why: "ORDER BY alone cannot change a COUNT or an EXISTS answer (Neo4j deletes it " +
				"outright for COUNT), so the plan is the only place its loss is visible",
		},
		{
			name:     "SKIP",
			body:     "MATCH (a)-[:K]->(x) RETURN x SKIP 1",
			wantKind: "*ir.Skip",
			why:      "SKIP truncates the front of the stream, so it changes the counted quantity",
		},
		{
			name:     "LIMIT",
			body:     "MATCH (a)-[:K]->(x) RETURN x LIMIT 1",
			wantKind: "*ir.Limit",
			why:      "LIMIT truncates the tail of the stream, so it changes the counted quantity",
		},
		{
			name:     "ORDER BY with LIMIT fuses into Top",
			body:     "MATCH (a)-[:K]->(x) RETURN x ORDER BY x.id LIMIT 1",
			wantKind: "*ir.Top",
			why: "applyProjectionTail fuses Sort+Limit into Top; the fused form must be reached " +
				"from a subquery body too, not only from a top-level RETURN",
		},
		{
			name:     "RETURN that aggregates",
			body:     "MATCH (a)-[:K]->(x) RETURN count(*)",
			wantKind: "*ir.EagerAggregation",
			why: "an aggregating RETURN with no grouping key collapses the whole body to ONE row, " +
				"which is the headline wrong answer of rmp #2675",
		},
		{
			name:     "RETURN that aggregates with a grouping key",
			body:     "MATCH (a)-[:K]->(x) RETURN x, count(*)",
			wantKind: "*ir.EagerAggregation",
			why:      "the grouping-key form takes the same operator and must not be missed either",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := subqueryBody(t, tc.body)
			plan, err := ir.TranslateSubquery(body, []string{"a"}, argTag)
			if err != nil {
				t.Fatalf("TranslateSubquery(%q): %v", tc.body, err)
			}
			kinds := planKinds(plan)

			if !hasKind(kinds, "*ir.Projection") {
				t.Errorf("the body's RETURN produced no *ir.Projection, so its trailing projection "+
					"was dropped (rmp #2675).\n  body: %s\n  plan: %v", tc.body, kinds)
			}
			if !hasKind(kinds, tc.wantKind) {
				t.Errorf("the body's RETURN produced no %s.\n  body: %s\n  why it must: %s\n  plan: %v",
					tc.wantKind, tc.body, tc.why, kinds)
			}

			// A ProduceResults inside a subquery pipeline is not a cosmetic
			// difference: cypher/api.go handles that operator only at the plan
			// ROOT, so one left in the middle would fail the physical build. The
			// answer suite would catch it as an error rather than a wrong number,
			// but the message would name the builder and not this function.
			if hasKind(kinds, "*ir.ProduceResults") {
				t.Errorf("the inner plan carries a *ir.ProduceResults; the physical builder handles "+
					"that operator only at the plan root and will fail on it here.\n  body: %s\n  plan: %v",
					tc.body, kinds)
			}

			// The leading Argument is what carries the outer row into the inner
			// pipeline. A projection layered on top must not displace it.
			if !hasKind(kinds, "*ir.Argument") {
				t.Errorf("the correlation Argument leaf is gone from the inner plan, so the body is "+
					"no longer correlated with the outer row.\n  body: %s\n  plan: %v", tc.body, kinds)
			}
		})
	}
}

// TestTranslateSubquery_ReturnlessBodyIsUnchanged is the control.
//
// The overwhelming majority of subquery bodies in the wild — and every one of
// the openCypher TCK's thirteen brace-subquery occurrences — carry no trailing
// RETURN. Those must translate exactly as they did before, with no projection
// bolted on. Without this control the suite above would be satisfied by a change
// that projected unconditionally.
func TestTranslateSubquery_ReturnlessBodyIsUnchanged(t *testing.T) {
	const argTag uint32 = 11

	for _, body := range []string{
		"MATCH (a)-[:K]->(x)",
		"MATCH (a)-[:K]->(x) WHERE x.id > 4",
		"UNWIND [1, 2] AS u MATCH (a)-[:K]->(x)",
	} {
		t.Run(body, func(t *testing.T) {
			src := "MATCH (a) RETURN COUNT { " + body + " }"
			q, err := parser.Parse(src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", src, err)
			}
			cs := q.(*ast.SingleQuery).Return.Projection.Items[0].Expr.(*ast.CountSubquery)
			if cs.Query == nil {
				t.Fatalf("Parse(%q) produced the pattern form; this control needs the block form", src)
			}
			if cs.Query.Return != nil {
				t.Fatalf("Parse(%q) set SingleQuery.Return on a RETURN-less body, so this control "+
					"is not testing what it claims", src)
			}

			plan, err := ir.TranslateSubquery(cs.Query, []string{"a"}, argTag)
			if err != nil {
				t.Fatalf("TranslateSubquery(%q): %v", body, err)
			}
			kinds := planKinds(plan)

			// None of these bodies carries a horizon of any kind, so none may gain
			// a Projection. (A body whose last clause is a WITH does carry one, and
			// it reaches the plan through ReadingClauses — which is exactly why WITH
			// was never affected by the defect. Such a body is not spelled here
			// because a WITH must be followed by another clause to be well formed.)
			if hasKind(kinds, "*ir.Projection") {
				t.Errorf("a horizon-free body gained a *ir.Projection it did not ask for.\n"+
					"  body: %s\n  plan: %v", body, kinds)
			}
			if hasKind(kinds, "*ir.ProduceResults") {
				t.Errorf("a RETURN-less body produced a *ir.ProduceResults.\n  body: %s\n  plan: %v", body, kinds)
			}
		})
	}
}
