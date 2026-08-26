package cypher_test

// subquery_union_refused_test.go — a UNION inside an EXISTS or COUNT subquery
// body must be REFUSED, never answered from one branch (rmp #2615).
//
// # The defect this replaces
//
// The grammar admits `regularQuery` in both positions so a UNION parses, but
// ast.ExistsSubquery.Query and ast.CountSubquery.Query are typed
// *ast.SingleQuery and cannot hold one. The visitor kept the first branch and
// discarded the rest — "multi-union inside EXISTS is unusual" — with no error
// and no notification. MEASURED on a node with a :W edge and no :Z edge:
//
//	EXISTS { MATCH (x)-[:Z]->() RETURN 1 UNION MATCH (x)-[:W]->() RETURN 1 }
//	  returned false, where the second branch matches
//
// # Why the COUNT example in the report cannot show it
//
// `COUNT { MATCH (x)-[:K]->() RETURN 1 UNION MATCH (x)-[:W]->() RETURN 1 }` is
// NOT discriminating: UNION de-duplicates, both branches yield the row `1`, so
// the correct answer is 1 and the branch-dropping answer is also 1. The cases
// below use branches that return DIFFERENT values, so a dropped branch changes
// the answer.
//
// # Refused, not supported, and the divergence is stated rather than hidden
//
// Both reference engines ANSWER this query: Neo4j's existsExpression and
// countExpression admit regularQuery, which carries UNION (Cypher5Parser.g4),
// and Memgraph's existsSubquery and countSubquery admit cypherQuery, which
// carries cypherUnion (Cypher.g4). Refusing stops the silent wrong answer today;
// matching the references is a feature and is filed separately. The openCypher 9
// TCK does not cover subquery expressions at all, so neither choice moves the
// conformance count.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// unionSubqueryFixture seeds a node 'a' with one :W edge and NO :Z edge, so a
// subquery whose FIRST branch looks for :Z and whose second looks for :W is
// answered differently depending on whether the second branch survives.
func unionSubqueryFixture(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for _, q := range []string{
		"CREATE (:N {id:'a'})",
		"CREATE (:N {id:'b'})",
		"MATCH (a:N {id:'a'}),(b:N {id:'b'}) CREATE (a)-[:W]->(b)",
	} {
		if _, err := eng.RunInTx(ctx, q, nil); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	return eng
}

func TestSubqueryUnionIsRefused(t *testing.T) {
	eng := unionSubqueryFixture(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		query string
		// wrongAnswer is what the silent-drop behaviour produced, quoted so a
		// future reader can see what refusing prevents.
		wrongAnswer string
	}{
		{
			name: "EXISTS",
			query: "MATCH (x:N {id:'a'}) RETURN EXISTS { MATCH (x)-[:Z]->() RETURN 1 " +
				"UNION MATCH (x)-[:W]->() RETURN 1 } AS c",
			wrongAnswer: "false, although the :W branch matches",
		},
		{
			name: "COUNT",
			query: "MATCH (x:N {id:'a'}) RETURN COUNT { MATCH (x)-[:Z]->() RETURN 1 " +
				"UNION MATCH (x)-[:W]->() RETURN 2 } AS c",
			wrongAnswer: "0, although the :W branch contributes a row",
		},
		{
			name: "UNION ALL is refused too",
			query: "MATCH (x:N {id:'a'}) RETURN COUNT { MATCH (x)-[:Z]->() RETURN 1 " +
				"UNION ALL MATCH (x)-[:W]->() RETURN 2 } AS c",
			wrongAnswer: "0, although the :W branch contributes a row",
		},
		{
			name: "three branches",
			query: "MATCH (x:N {id:'a'}) RETURN COUNT { MATCH (x)-[:Z]->() RETURN 1 " +
				"UNION MATCH (x)-[:Y]->() RETURN 2 UNION MATCH (x)-[:W]->() RETURN 3 } AS c",
			wrongAnswer: "0, although the third branch contributes a row",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := eng.Run(ctx, tc.query, nil)
			if err == nil {
				var got string
				if res.Next() {
					got = strings.TrimSpace(strings.Trim(res.ValueAt(0).String(), "\n"))
				}
				_ = res.Close()
				t.Fatalf("the query was ANSWERED (%s) instead of refused. A UNION body cannot be "+
					"held by the subquery AST, so answering it means answering from ONE BRANCH: "+
					"the silent wrong answer was %s (#2615)", got, tc.wrongAnswer)
			}
			if !strings.Contains(err.Error(), "UNION") {
				t.Errorf("the refusal does not name UNION, so a caller cannot tell what to "+
					"rewrite: %v", err)
			}
		})
	}
}

// TestSubqueryWithoutUnionStillWorks is the control. Without it, every
// assertion above is satisfied by a build that refuses EVERY subquery.
func TestSubqueryWithoutUnionStillWorks(t *testing.T) {
	eng := unionSubqueryFixture(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"EXISTS matching", "MATCH (x:N {id:'a'}) RETURN EXISTS { MATCH (x)-[:W]->() RETURN 1 } AS c", "true"},
		{"EXISTS not matching", "MATCH (x:N {id:'a'}) RETURN EXISTS { MATCH (x)-[:Z]->() RETURN 1 } AS c", "false"},
		{"COUNT", "MATCH (x:N {id:'a'}) RETURN COUNT { MATCH (x)-[:W]->() RETURN 1 } AS c", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := eng.Run(ctx, tc.query, nil)
			if err != nil {
				t.Fatalf("a subquery WITHOUT a UNION was refused: %v. The refusal must reach the "+
					"UNION body and nothing else", err)
			}
			var got string
			if res.Next() {
				got = res.ValueAt(0).String()
			}
			_ = res.Close()
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
