package cypher_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newConcurrentSubqueryEngine builds a small fixed graph: A KNOWS B, A KNOWS C,
// B KNOWS D, and A WORKS_AT ACME. Every engine is fresh, so its plan cache is
// EMPTY — which is the whole point, see the file-level note on priming below.
func newConcurrentSubqueryEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:Person {name:'A', age:30})-[:KNOWS {since:2020}]->(:Person {name:'B', age:40})`)
	runSetup(t, eng, `MATCH (a:Person {name:'A'}) CREATE (a)-[:KNOWS {since:1999}]->(:Person {name:'C', age:50})`)
	runSetup(t, eng, `MATCH (b:Person {name:'B'}) CREATE (b)-[:KNOWS]->(:Person {name:'D', age:60})`)
	runSetup(t, eng, `MATCH (a:Person {name:'A'}) CREATE (a)-[:WORKS_AT]->(:Company {name:'ACME'})`)
	return eng
}

// runAllRows renders every row of q as a stable string, so two runs can be
// compared as whole result sets rather than one column at a time.
func runAllRows(ctx context.Context, eng *cypher.Engine, q string) ([]string, error) {
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	var out []string
	for res.Next() {
		rec := res.Record()
		keys := make([]string, 0, len(rec))
		for k := range rec {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		row := ""
		for _, k := range keys {
			row += fmt.Sprintf("%s=%T(%v);", k, rec[k], rec[k])
		}
		out = append(out, row)
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// TestSubqueryConcurrentFirstExecution_2508 is the regression gate for rmp #2508:
// a data race on the SHARED cached AST when concurrent FIRST executions translate
// a subquery-bearing query.
//
// Only the OUTER plan is cached. A subquery's inner plan was re-translated on
// every execution, over the shared cached AST, and the translation walk named
// anonymous entities by WRITING into it (the translator.freshAnonVar sites in
// ir/match.go). Concurrent first executions therefore wrote and read the same
// NodePattern.Variable / RelationshipPattern.Variable fields. It was never
// benign: each translator mints its own names, so one plan could reference a
// variable the other translator never bound — a wrong ANSWER, not a duplicated
// identical write.
//
// PRIMING IS THE LOAD-BEARING DETAIL. The offending write is nil-guarded, so it
// happens only on the first translation. A primed plan cache makes this
// UNREPRODUCIBLE — a primed attempt reported no race at all. Every shape below
// therefore gets its OWN fresh engine and is executed concurrently as its FIRST
// execution. That is also why the pre-existing subquery batteries were blind to
// the whole class: nothing drove concurrent first-execution of a subquery.
//
// WHAT ACTUALLY DETECTS #2508 HERE IS -race, AND THAT WAS MEASURED. With the fix
// reverted, this test fails under `-race` on every run, but passes 3 of 3 runs
// WITHOUT it: the racing writes store equal-length synthetic names, so the wrong
// answer they can produce did not materialise on this fixture. Do not read the
// result comparison below as an independent detector of this defect — it is not
// one, and `make ci` running `go test -race ./...` is what makes this gate bite.
//
// The result comparison is still worth its cost, for a different failure: a
// regression that mints a COLLIDING name (the rmp #2507 class, where the inner
// scope claims a name the seed row already carries) is a silent wrong answer with
// no race at all. Each shape is therefore answered SERIALLY on a separate fresh
// engine, and all 16 concurrent answers must equal that baseline.
//
// THIS TEST IS ALSO THE DRIFT DETECTOR for ir.NameSubqueryAnonymousEntities. That
// pass states the naming rules a second time, mirroring ir/match.go. If the two
// ever drift, the entity the pass stops covering is minted per execution again
// and reappears here.
func TestSubqueryConcurrentFirstExecution_2508(t *testing.T) {
	t.Parallel()

	// Each case names the translation path it drives, because the point of the
	// battery is coverage of the paths that mutate the AST, not of query syntax.
	cases := []struct {
		name  string
		path  string
		query string
	}{
		{
			name:  "count_correlated_projection",
			path:  "ir.matchPathPatternWithArg — the shape that was reported racing",
			query: `MATCH (n:Person) RETURN COUNT { MATCH (n)-[:KNOWS]->(m:Person) WHERE m.age >= 0 } AS c ORDER BY c`,
		},
		{
			name:  "exists_correlated_projection",
			path:  "ir.matchPathPatternWithArg via the EXISTS spelling",
			query: `MATCH (n:Person) RETURN EXISTS { MATCH (n)-[:KNOWS]->(m:Person) } AS c ORDER BY c`,
		},
		{
			name:  "count_uncorrelated_pattern",
			path:  "ir.matchPathPattern — no outer variable, so the non-Arg path",
			query: `MATCH (n:Person {name:'A'}) RETURN COUNT { MATCH (:Person)-[:KNOWS]->(:Person) } AS c`,
		},
		{
			name:  "count_pattern_form_anonymous_both_ends",
			path:  "ir.matchPathPatternWithArg, pattern form (no inner MATCH keyword)",
			query: `MATCH (n:Person) RETURN COUNT { (n)-[:KNOWS]->(:Person) } AS c ORDER BY c`,
		},
		{
			name:  "exists_multi_hop_anonymous_middle",
			path:  "ir.matchPathPatternWithArg with a chained hop, so a synthetic name is READ back",
			query: `MATCH (n:Person) RETURN EXISTS { MATCH (n)-[:KNOWS]->()-[:KNOWS]->(z:Person) } AS c ORDER BY c`,
		},
		{
			name:  "count_with_orderby_in_body",
			path:  "ir.rewriteOrderByForAggregation — rewrites *ast.SortItem.Expr in place",
			query: `MATCH (n:Person) RETURN COUNT { MATCH (n)-[:KNOWS]->(m:Person) WITH m.age AS a ORDER BY m.age RETURN a } AS c ORDER BY c`,
		},
		{
			name:  "nested_subquery",
			path:  "recursive descent — a subquery inside a subquery body",
			query: `MATCH (n:Person) RETURN COUNT { MATCH (n)-[:KNOWS]->(m:Person) WHERE EXISTS { MATCH (m)-[:KNOWS]->(:Person) } } AS c ORDER BY c`,
		},
		{
			name:  "pattern_predicate_inside_body",
			path:  "a bare *ast.PathPattern used as an expression inside a subquery body",
			query: `MATCH (n:Person) RETURN COUNT { MATCH (n)-[:KNOWS]->(m:Person) WHERE (m)-[:KNOWS]->() } AS c ORDER BY c`,
		},
		{
			name: "nested_subquery_in_inline_property_map",
			path: "ir.anonSubqueryNamer.namePathPattern — a subquery hidden in a pattern's inline property map",
			// The inner COUNT { } lives inside the inline property MapLiteral of a
			// node pattern that is itself inside a subquery body. The pass reached
			// the outer body's own entities but not the property expressions
			// hanging off its patterns, so the inner body stayed anonymous and was
			// minted per execution again. This shape reproduced #2508's race under
			// -race WITH the first version of the fix in place; it is the reason
			// the namer's pattern descent now walks NodePattern.Properties and
			// RelationshipPattern.Properties, exactly as topLevelExprWalker does.
			query: `MATCH (n:Person) RETURN COUNT { MATCH (m:Person {age: COUNT { (n)-[:KNOWS]->() }}) RETURN m } AS c ORDER BY c`,
		},
		{
			name: "nested_subquery_in_pattern_comprehension_property",
			path: "ir.anonSubqueryNamer.namePathPattern via *ast.PatternComprehension",
			// Same gap through the other route into a PathPattern from an
			// expression: the comprehension's pattern carries the property map
			// that hides the nested subquery.
			query: `MATCH (n:Person) RETURN COUNT { MATCH (m:Person) WHERE size([(m)-[:KNOWS]->(z {name: COUNT { (n)-[:KNOWS]->() }}) | z]) >= 0 RETURN m } AS c ORDER BY c`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			// Serial baseline on its own fresh engine. This is the oracle; if the
			// query itself is unsupported the test says so here rather than
			// silently passing on an error both arms happen to share.
			want, err := runAllRows(ctx, newConcurrentSubqueryEngine(t), tc.query)
			if err != nil {
				t.Fatalf("serial baseline failed (path: %s): %v", tc.path, err)
			}
			if len(want) == 0 {
				t.Fatalf("serial baseline returned NO rows, so the concurrent arm would assert nothing (path: %s)", tc.path)
			}

			// Concurrent first execution on a SEPARATE fresh engine: 16 goroutines
			// released from one barrier, none of them having primed the cache.
			eng := newConcurrentSubqueryEngine(t)
			const goroutines = 16
			var release sync.WaitGroup
			release.Add(1)
			var done sync.WaitGroup
			got := make([][]string, goroutines)
			errs := make([]error, goroutines)
			for i := range goroutines {
				done.Add(1)
				go func(i int) {
					defer done.Done()
					release.Wait()
					got[i], errs[i] = runAllRows(ctx, eng, tc.query)
				}(i)
			}
			release.Done()
			done.Wait()

			for i := range goroutines {
				if errs[i] != nil {
					t.Errorf("goroutine %d failed: %v", i, errs[i])
					continue
				}
				if len(got[i]) != len(want) {
					t.Errorf("goroutine %d returned %d rows, serial baseline returned %d\n  got  %v\n  want %v",
						i, len(got[i]), len(want), got[i], want)
					continue
				}
				for r := range want {
					if got[i][r] != want[r] {
						t.Errorf("goroutine %d row %d = %q, serial baseline = %q", i, r, got[i][r], want[r])
					}
				}
			}
		})
	}
}
