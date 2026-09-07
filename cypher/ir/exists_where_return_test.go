package ir_test

// exists_where_return_test.go — rmp #2779: [ir.FromAST]'s WHERE-position
// EXISTS lowering must translate the body's TRAILING projection, exactly as
// [ir.TranslateSubquery] does since rmp #2675.
//
// # What was wrong
//
// A WHERE predicate that is a top-level `EXISTS { … }` is not evaluated as an
// expression: the translator intercepts it and emits a [ir.SemiApply] (or an
// [ir.AntiSemiApply] for the NOT form), whose Inner is the body's plan. That
// lowering — existsSubPlan in cypher/ir/exists.go — looped the body's reading
// clauses and returned, never reading SingleQuery.Return. Every clause the
// body's RETURN carries was therefore dropped on the floor, and since EXISTS is
// "the body produced at least one row", dropping a LIMIT, a SKIP or an
// aggregation is a wrong ANSWER and not a lost optimisation.
//
// The same predicate with `AND true` appended is no longer a top-level EXISTS,
// so it goes down the expression-evaluated path through
// [ir.TranslateSubquery] — which #2675 fixed — and answered CORRECTLY. Two
// spellings of one predicate disagreeing is what made the defect observable;
// the answer-level differential is in cypher/exists_where_body_projection_test.go.
//
// # Why this plan-shape suite exists alongside that answer suite
//
// Same reason as cypher/ir/subquery_return_test.go, which is this file's sibling
// for the expression path: a plain `RETURN <expr>` and a bare `ORDER BY` are
// cardinality-preserving BY SPECIFICATION, so no existence test can discriminate
// on them at answer level. The observable that does is the plan — before the fix
// the body's projection produced no operator at all.
//
// # Where the required behaviour comes from
//
// github.com/neo4j/neo4j, release tag 2026.07.1 (commit
// f213380f812b820a1b312e2ea52cb3d8f1931ccc),
// community/cypher/cypher-planner/src/main/scala/org/neo4j/cypher/internal/compiler/ast/convert/plannerQuery/CreateIrExpressions.scala,
// `case existsExpression @ ExistsExpression(q)`: the body query is converted
// WHOLE, with no horizon override of any kind. EXISTS is therefore evaluated
// over the body's own final projection. The full citation chain, including why an
// aggregating body over an empty input is TRUE, is in the header of
// cypher/count_subquery_body_projection_test.go.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
)

// whereExistsInner plans `MATCH (a:Anchor) WHERE <pred> RETURN a` and returns
// the Inner subplan of the single SemiApply / AntiSemiApply the predicate must
// lower to, together with that operator's own type name.
//
// It goes through the real parser and the real [ir.FromAST] rather than calling
// the unexported lowering directly, because the defect was precisely that one
// parser-populated field — SingleQuery.Return on the subquery body — went
// unread on this path while being read on the other. Reaching the code the way
// a query does is what makes the case evidence.
func whereExistsInner(t *testing.T, pred string) (ir.LogicalPlan, string) {
	t.Helper()
	src := "MATCH (a:Anchor) WHERE " + pred + " RETURN a"
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	plan, err := ir.FromAST(q)
	if err != nil {
		t.Fatalf("FromAST(%q): %v", src, err)
	}

	var inner ir.LogicalPlan
	var kind string
	var found int
	var walk func(ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil {
			return
		}
		switch op := p.(type) {
		case *ir.SemiApply:
			found++
			inner, kind = op.Inner, "*ir.SemiApply"
		case *ir.AntiSemiApply:
			found++
			inner, kind = op.Inner, "*ir.AntiSemiApply"
		}
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)

	// A predicate that did NOT lower to a SemiApply would make every assertion
	// below vacuous — it would be testing the expression path instead, which is
	// a different function and already covered by subquery_return_test.go. Fail
	// loudly rather than silently pass.
	if found != 1 {
		t.Fatalf("FromAST(%q) produced %d SemiApply/AntiSemiApply operators, want exactly 1: "+
			"the predicate did not take the WHERE-position lowering this file tests.\n  plan: %v",
			src, found, planKinds(plan))
	}
	return inner, kind
}

// TestWhereExists_TranslatesTheBodysTrailingProjection is the plan-level half of
// rmp #2779.
//
// Before the fix EVERY case below produced an Inner plan with no Projection in
// it at all. Each case names the operator its clause must produce; the
// Projection assertion is common to all of them because openCypher's projection
// is what every one of those clauses hangs off.
func TestWhereExists_TranslatesTheBodysTrailingProjection(t *testing.T) {
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
				"operator has not been translated at all. This is also the shape the openCypher " +
				"TCK's own RETURN-bearing EXISTS bodies have (ExistentialSubquery2 [1])",
		},
		{
			name:     "RETURN of a literal",
			body:     "MATCH (a)-[:K]->(x) RETURN true",
			wantKind: "*ir.Projection",
			why:      "the exact body of ExistentialSubquery2 [1] and ExistentialSubquery3 [1] [2] [3]",
		},
		{
			name:     "RETURN DISTINCT",
			body:     "MATCH (a)-[:K]->(x) RETURN DISTINCT x.v",
			wantKind: "*ir.Distinct",
			why:      "DISTINCT dedupes, so it can empty a body that matched",
		},
		{
			name:     "ORDER BY",
			body:     "MATCH (a)-[:K]->(x) RETURN x ORDER BY x.v",
			wantKind: "*ir.Sort",
			why: "ORDER BY alone cannot change an EXISTS answer, so the plan is the only place " +
				"its loss is visible",
		},
		{
			name:     "SKIP",
			body:     "MATCH (a)-[:K]->(x) RETURN x SKIP 1",
			wantKind: "*ir.Skip",
			why:      "SKIP truncates the front of the stream, so a SKIP past the end empties the body",
		},
		{
			name:     "LIMIT",
			body:     "MATCH (a)-[:K]->(x) RETURN x LIMIT 0",
			wantKind: "*ir.Limit",
			why:      "LIMIT 0 empties the body outright, and an existence test is where that is otherwise invisible",
		},
		{
			name:     "ORDER BY with LIMIT fuses into Top",
			body:     "MATCH (a)-[:K]->(x) RETURN x ORDER BY x.v LIMIT 1",
			wantKind: "*ir.Top",
			why:      "applyProjectionTail fuses Sort+Limit into Top; the fused form must be reached from here too",
		},
		{
			name:     "RETURN that aggregates",
			body:     "MATCH (a)-[:K]->(x) RETURN count(*)",
			wantKind: "*ir.EagerAggregation",
			why: "an aggregation with no grouping key emits ONE row even over an empty input " +
				"(TCK Return4.feature [6]), so it makes EXISTS true where the pattern matched nothing",
		},
		{
			name:     "RETURN that aggregates with a grouping key",
			body:     "MATCH (a)-[:K]->(x) RETURN x.v, count(*)",
			wantKind: "*ir.EagerAggregation",
			why:      "the grouping-key form takes the same operator and must not be missed either",
		},
		{
			name:     "WITH before the RETURN",
			body:     "MATCH (a)-[:K]->(x) WITH x ORDER BY x.v RETURN x LIMIT 0",
			wantKind: "*ir.Limit",
			why: "a WITH reaches the plan through ReadingClauses and was never affected; this case " +
				"pins that the TRAILING RETURN above it is translated as well",
		},
	}

	for _, tc := range cases {
		for _, form := range []struct{ pred, wantOp string }{
			{"EXISTS { " + tc.body + " }", "*ir.SemiApply"},
			{"NOT EXISTS { " + tc.body + " }", "*ir.AntiSemiApply"},
		} {
			t.Run(tc.name+" / "+form.wantOp, func(t *testing.T) {
				inner, gotOp := whereExistsInner(t, form.pred)
				if gotOp != form.wantOp {
					t.Fatalf("predicate %q lowered to %s, want %s", form.pred, gotOp, form.wantOp)
				}
				kinds := planKinds(inner)

				if !hasKind(kinds, "*ir.Projection") {
					t.Errorf("the body's RETURN produced no *ir.Projection, so its trailing "+
						"projection was dropped (rmp #2779).\n  predicate: %s\n  inner plan: %v",
						form.pred, kinds)
				}
				if !hasKind(kinds, tc.wantKind) {
					t.Errorf("the body's RETURN produced no %s.\n  predicate: %s\n  why it must: %s\n"+
						"  inner plan: %v", tc.wantKind, form.pred, tc.why, kinds)
				}

				// A ProduceResults inside a subquery pipeline is not cosmetic:
				// cypher/api.go handles that operator only at the plan ROOT, so one
				// left here would fail the physical build.
				if hasKind(kinds, "*ir.ProduceResults") {
					t.Errorf("the inner plan carries a *ir.ProduceResults; the physical builder "+
						"handles that operator only at the plan root and will fail on it here.\n"+
						"  predicate: %s\n  inner plan: %v", form.pred, kinds)
				}

				// The leading Argument is what carries the outer row into the inner
				// pipeline. A projection layered on top must not displace it.
				if !hasKind(kinds, "*ir.Argument") {
					t.Errorf("the correlation Argument leaf is gone from the inner plan, so the "+
						"body is no longer correlated with the outer row.\n  predicate: %s\n"+
						"  inner plan: %v", form.pred, kinds)
				}
			})
		}
	}
}

// TestWhereExists_ReturnlessBodyIsUnchanged is the control.
//
// Every one of the openCypher TCK's WHERE-position exists bodies that carries no
// RETURN must translate exactly as it did before, with no projection bolted on.
// Without this control the suite above would be satisfied by a change that
// projected unconditionally — which would put a Projection in front of bodies
// the TCK does cover, and the TCK is the gate that decides this task.
func TestWhereExists_ReturnlessBodyIsUnchanged(t *testing.T) {
	for _, body := range []string{
		"MATCH (a)-[:K]->(x)",
		"MATCH (a)-[:K]->(x) WHERE x.v > 4",
		"UNWIND [1, 2] AS u MATCH (a)-[:K]->(x)",
	} {
		for _, pred := range []string{"EXISTS { " + body + " }", "NOT EXISTS { " + body + " }"} {
			t.Run(pred, func(t *testing.T) {
				// Assert the premise the control rests on: the parser really did
				// leave SingleQuery.Return nil for this body. Otherwise the case
				// would be asserting nothing.
				src := "MATCH (a:Anchor) WHERE " + pred + " RETURN a"
				q, err := parser.Parse(src)
				if err != nil {
					t.Fatalf("Parse(%q): %v", src, err)
				}
				if es := firstExistsSubquery(q); es == nil {
					t.Fatalf("Parse(%q) produced no *ast.ExistsSubquery to inspect", src)
				} else if es.Query == nil {
					t.Fatalf("Parse(%q) produced the PATTERN form; this control needs the block form", src)
				} else if es.Query.Return != nil {
					t.Fatalf("Parse(%q) set SingleQuery.Return on a RETURN-less body, so this "+
						"control is not testing what it claims", src)
				}

				inner, _ := whereExistsInner(t, pred)
				kinds := planKinds(inner)
				if hasKind(kinds, "*ir.Projection") {
					t.Errorf("a horizon-free body gained a *ir.Projection it did not ask for.\n"+
						"  predicate: %s\n  inner plan: %v", pred, kinds)
				}
				if hasKind(kinds, "*ir.ProduceResults") {
					t.Errorf("a RETURN-less body produced a *ir.ProduceResults.\n"+
						"  predicate: %s\n  inner plan: %v", pred, kinds)
				}
			})
		}
	}
}

// firstExistsSubquery returns the first *ast.ExistsSubquery reachable from the
// WHERE predicate of q's first MATCH clause, or nil. Used only to validate the
// control's premise.
func firstExistsSubquery(q ast.Query) *ast.ExistsSubquery {
	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		return nil
	}
	for _, rc := range sq.ReadingClauses {
		m, ok := rc.(*ast.Match)
		if !ok || m.Where == nil {
			continue
		}
		switch p := m.Where.Predicate.(type) {
		case *ast.ExistsSubquery:
			return p
		case *ast.UnaryOp:
			if es, ok := p.Operand.(*ast.ExistsSubquery); ok {
				return es
			}
		}
	}
	return nil
}
