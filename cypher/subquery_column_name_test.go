package cypher_test

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// TestSubqueryColumnNames_2508 pins the RESULT COLUMN NAME of an un-aliased
// projection whose expression is an EXISTS { } / COUNT { } subquery.
//
// WHY THIS TEST EXISTS AND WHY THE TCK CANNOT REPLACE IT. openCypher names an
// un-aliased projection column after the expression as written, and GoGraph
// derives that name by re-rendering the AST (ir.exprToColumnName, falling through
// to ast.Expression.String()). rmp #2508's fix names a subquery's anonymous
// pattern entities BEFORE the plan cache, so those synthetic names became part of
// the rendering and leaked into the column name:
//
//	RETURN COUNT { (n)-[:K]->(:P) }
//	  was: COUNT { (n)-[:K]->(:P) }
//	  became: COUNT { (n)-[__anon_sq_1:K]->(__anon_sq_0:P) }
//
// That is an API-visible change, because Record() is keyed by this string. A sweep
// of cypher/tck/features/**/*.feature finds ZERO un-aliased RETURN EXISTS{}/COUNT{}
// scenarios, so a green 3897/3897 TCK run is STRUCTURALLY BLIND to it and cannot
// be cited as covering this. Hence this test.
//
// The assertion is deliberately on the WHOLE column name rather than on the
// absence of the prefix: "no __anon_ substring" would also pass if the column
// were named something else entirely.
func TestSubqueryColumnNames_2508(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "count_pattern_form_anonymous_far_node",
			query: `MATCH (n:Person) RETURN COUNT { (n)-[:KNOWS]->(:Person) }`,
			want:  `COUNT { (n)-[:KNOWS]->(:Person) }`,
		},
		{
			name:  "exists_pattern_form_fully_anonymous_tail",
			query: `MATCH (n:Person) RETURN EXISTS { (n)-[:KNOWS]->() }`,
			want:  `EXISTS { (n)-[:KNOWS]->() }`,
		},
		{
			name:  "exists_full_query_form_multi_hop",
			query: `MATCH (n:Person) RETURN EXISTS { MATCH (n)-[:KNOWS]->()-[:KNOWS]->() }`,
			want:  `EXISTS { MATCH (n)-[:KNOWS]->()-[:KNOWS]->() }`,
		},
		{
			name:  "count_with_named_inner_entities_is_unaffected",
			query: `MATCH (n:Person) RETURN COUNT { (n)-[r:KNOWS]->(m:Person) }`,
			want:  `COUNT { (n)-[r:KNOWS]->(m:Person) }`,
		},
		{
			name:  "an_alias_always_wins",
			query: `MATCH (n:Person) RETURN COUNT { (n)-[:KNOWS]->(:Person) } AS friends`,
			want:  `friends`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := newConcurrentSubqueryEngine(t)
			res, err := eng.Run(ctx, tc.query, nil)
			if err != nil {
				t.Fatalf("Run(%s): %v", tc.query, err)
			}
			defer func() { _ = res.Close() }()

			if !res.Next() {
				t.Fatalf("query returned no rows, so no column name was observed: %v", res.Err())
			}
			var got []string
			for k := range res.Record() {
				got = append(got, k)
			}
			sort.Strings(got)
			if err := res.Err(); err != nil {
				t.Fatalf("drain: %v", err)
			}

			if len(got) != 1 {
				t.Fatalf("expected exactly 1 column, got %d: %q", len(got), got)
			}
			if got[0] != tc.want {
				t.Errorf("column name mismatch\n  query %s\n  got   %q\n  want  %q", tc.query, got[0], tc.want)
				if strings.Contains(got[0], "__anon_") {
					t.Errorf("  -> an engine-internal synthetic name leaked into the public column name")
				}
			}
		})
	}
}
