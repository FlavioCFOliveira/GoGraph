package cypher_test

// rel_uniqueness_clause_scope_test.go — regression gate for rmp #2252: the
// relationship-uniqueness scope is the CLAUSE, not one path pattern.
//
// The analyser's seen-set used to live inside a single *ast.PathPattern, so a
// relationship variable reused in a SIBLING comma pattern of the same MATCH
// escaped the check and the query was silently accepted — returning one row with
// count 0 instead of a diagnostic. The runtime honoured the clause scope all
// along (cypher/ir/match.go resets SiblingRelVars per clause and seeds it from
// the preceding comma patterns), so the analyser was the half that disagreed.
//
// The three shapes below are the whole contract, and the third is why the fix
// cannot simply widen the scope: uniqueness deliberately does NOT cross a clause
// boundary, which TCK Match3 scenario [24] pins.

import (
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// requireRelUniquenessError asserts the query is refused with a
// KindRelationshipUniqueness scope error at the expected 1-based column.
func requireRelUniquenessError(t *testing.T, query string, wantCol uint32) {
	t.Helper()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
	err := runQuery(t, eng, query)
	if err == nil {
		t.Fatalf("query was ACCEPTED; want a relationship-uniqueness error:\n  %s", query)
	}
	var se *sema.ScopeError
	if !errors.As(err, &se) {
		t.Fatalf("error is not a *sema.ScopeError (%T): %v", err, err)
	}
	if se.Kind != sema.KindRelationshipUniqueness {
		t.Errorf("kind = %v, want KindRelationshipUniqueness: %v", se.Kind, err)
	}
	if se.Pos.Column != wantCol {
		t.Errorf("error column = %d, want %d (the position convention must match the single-pattern form): %v",
			se.Pos.Column, wantCol, err)
	}
	// The message must name the real scope. It said "the same path pattern"
	// until #2252, which was a false description of the sibling case.
	if got := err.Error(); !strings.Contains(got, "in the same clause") {
		t.Errorf("message does not name the CLAUSE scope: %v", got)
	}
}

// TestRelUniqueness_SamePathPattern is the shape that always worked. It is here
// so a fix to the sibling case cannot regress it.
func TestRelUniqueness_SamePathPattern(t *testing.T) {
	t.Parallel()
	requireRelUniquenessError(t, "MATCH (a)-[r]->()-[r]->(a) RETURN count(*) AS n", 17)
}

// TestRelUniqueness_SiblingCommaPattern is the defect: the second occurrence is
// in a sibling comma pattern of the SAME clause and must be refused.
func TestRelUniqueness_SiblingCommaPattern(t *testing.T) {
	t.Parallel()
	requireRelUniquenessError(t, "MATCH (a)-[r]->(b), (b)-[r]->(a) RETURN count(*) AS n", 23)
}

// TestRelUniqueness_SiblingCommaPatternInOtherClauses covers every other
// construct that carries comma-separated patterns, because the seen-set is now
// owned by the shared pattern walker and all of them inherit the clause scope.
func TestRelUniqueness_SiblingCommaPatternInOtherClauses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
		col   uint32
	}{
		{"optional_match", "OPTIONAL MATCH (a)-[r]->(b), (b)-[r]->(a) RETURN count(*) AS n", 32},
		{"create", "CREATE (a)-[r:T]->(b), (b)-[r:T]->(a)", 26},
		{"exists_subquery", "RETURN EXISTS { (a)-[r]->(b), (b)-[r]->(a) } AS e", 33},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			requireRelUniquenessError(t, tc.query, tc.col)
		})
	}
}

// TestRelUniqueness_AcceptedShapes is the boundary, and it is the half that
// keeps the fix from over-rejecting. Each of these must still be ACCEPTED.
func TestRelUniqueness_AcceptedShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
	}{
		// TCK Match3 [24]: reuse across a clause boundary is legal and must
		// still return a row. This is the case a wider scope would break.
		{"across_clause_boundary_with_WITH", "MATCH (a1)-[r:T]->() WITH r, a1 MATCH (a1)-[r:T]->(b2) RETURN a1, r, b2"},
		// Two MATCH clauses with no WITH between them: still two clauses.
		{"across_two_match_clauses", "MATCH (a)-[r]->(b) MATCH (b)-[r]->(a) RETURN count(*) AS n"},
		// DISTINCT variables in sibling patterns: nothing is reused.
		{"distinct_vars_in_siblings", "MATCH (a)-[r]->(b), (c)-[s]->(d) RETURN count(*) AS n"},
		// An anonymous relationship in each sibling: no variable to reuse.
		{"anonymous_rels_in_siblings", "MATCH (a)-->(b), (b)-->(a) RETURN count(*) AS n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
			if err := runQuery(t, eng, tc.query); err != nil {
				t.Fatalf("query was REFUSED; the clause-scoped check over-rejects:\n  %s\n  %v", tc.query, err)
			}
		})
	}
}
