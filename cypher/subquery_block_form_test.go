package cypher_test

// subquery_block_form_test.go — the RETURN-less statement-block body of
// EXISTS { … } and COUNT { … } (task #2216).
//
// Before this was fixed, `EXISTS { MATCH (a)-[:KNOWS]->(b) }` was a parse
// error ("unexpected right brace"), because the grammar admitted only a
// `regularQuery` (which requires a RETURN or an updating clause) or a bare
// pattern. A body made of reading clauses alone was unreachable.
//
// AUTHORITY. The openCypher 2024.3 BNF (github.com/opencypher/openCypher,
// grammar/openCypher.bnf) derives this form:
//
//	exists expression            ::= EXISTS { subquery expression argument }   (824-825)
//	subquery expression argument ::= procedure specification | graph pattern   (827-829)
//	procedure specification      ::= statement block                          (8-10)
//	linear statement             ::= primitive statement... [ primitive result statement ]  (28-29)
//
// The trailing result statement is OPTIONAL, so a lone MATCH is a well-formed
// statement block. The openCypher TCK does not exercise the EXISTS-brace
// syntax at all (grep for it across cypher/tck/features returns nothing),
// which is why no existing gate caught the defect — hence this file.
//
// CROSS-VALIDATION. Every expected value below was verified against Neo4j
// 5.26.28 Community (Docker neo4j:5.26-community, cypher-shell) on an
// identically-built graph, rather than asserted from memory. GoGraph agreed
// with Neo4j on all nine forms, including the rejection of an updating clause
// inside EXISTS.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestSubqueryBlockForm_Exists covers the EXISTS bodies that consist only of
// reading clauses. The graph is newExistsGraph:
//
//	alice(30) ──KNOWS──► bob(20)
//	alice(30) ──LIKES──► dave(35)
//	charlie(40)  (isolated)
func TestSubqueryBlockForm_Exists(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	tests := []struct {
		name  string
		query string
		want  []string
		why   string
	}{
		{
			name:  "match-only",
			query: `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) } RETURN a.name AS name`,
			want:  []string{"alice"},
			why:   "only alice has an outgoing :KNOWS edge",
		},
		{
			name:  "match-with-inner-where-true",
			query: `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[]->(b) WHERE b.age > 30 } RETURN a.name AS name`,
			want:  []string{"alice"},
			why:   "alice LIKES dave (age 35 > 30)",
		},
		{
			name:  "match-with-inner-where-false",
			query: `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[]->(b) WHERE b.age > 40 } RETURN a.name AS name`,
			want:  nil,
			why:   "no neighbour is older than 40 — proves the inner WHERE is really applied, not dropped",
		},
		{
			name:  "two-reading-clauses-no-match",
			query: `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) MATCH (b)-[]->(c) } RETURN a.name AS name`,
			want:  nil,
			why:   "bob has no outgoing edge, so the second MATCH eliminates alice",
		},
		{
			name:  "two-reading-clauses-match",
			query: `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) MATCH (b) } RETURN a.name AS name`,
			want:  []string{"alice"},
			why:   "the second MATCH re-binds the already-bound b, so alice survives",
		},
		{
			name:  "unwind-then-match",
			query: `MATCH (a:Person) WHERE EXISTS { UNWIND [1, 2] AS x MATCH (a)-[:KNOWS]->(b) } RETURN a.name AS name`,
			want:  []string{"alice"},
			why:   "UNWIND is a reading clause, so it is legal in a block body",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := collectColumn(t, eng, tc.query, "name")
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s: got %v, want %v (%s)", tc.name, got, tc.want, tc.why)
			}
		})
	}
}

// TestSubqueryBlockForm_Count covers COUNT { … } bodies made only of reading
// clauses. COUNT returns the number of rows the body produces, so it verifies
// more than existence: a wrong result would show up as a wrong degree.
func TestSubqueryBlockForm_Count(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	tests := []struct {
		name  string
		query string
		want  map[string]int64
	}{
		{
			name:  "out-degree",
			query: `MATCH (a:Person) RETURN a.name AS name, COUNT { MATCH (a)-[]->(b) } AS deg`,
			want:  map[string]int64{"alice": 2, "bob": 0, "charlie": 0, "dave": 0},
		},
		{
			name:  "out-degree-with-inner-where",
			query: `MATCH (a:Person) RETURN a.name AS name, COUNT { MATCH (a)-[]->(b) WHERE b.age > 30 } AS deg`,
			want:  map[string]int64{"alice": 1, "bob": 0, "charlie": 0, "dave": 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := eng.Run(context.Background(), tc.query, nil)
			if err != nil {
				t.Fatalf("Run %q: %v", tc.query, err)
			}
			got := make(map[string]int64, len(tc.want))
			for _, row := range collectRecords(t, res) {
				name, ok := row["name"].(expr.StringValue)
				if !ok {
					t.Fatalf("column name: expected StringValue, got %T", row["name"])
				}
				deg, ok := row["deg"].(expr.IntegerValue)
				if !ok {
					t.Fatalf("column deg: expected IntegerValue, got %T (%v)", row["deg"], row["deg"])
				}
				got[string(name)] = int64(deg)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%s: got %d rows, want %d (%v)", tc.name, len(got), len(tc.want), got)
			}
			for k, wantDeg := range tc.want {
				if got[k] != wantDeg {
					t.Errorf("%s: %s degree = %d, want %d", tc.name, k, got[k], wantDeg)
				}
			}
		})
	}
}

// TestSubqueryBlockForm_EquivalentToReturnForm pins the invariant that adding
// an explicit RETURN to the body cannot change an EXISTS/COUNT result. The
// block form is exactly the RETURN-less spelling of the same subquery.
func TestSubqueryBlockForm_EquivalentToReturnForm(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	pairs := []struct{ block, withReturn string }{
		{
			block:      `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) } RETURN a.name AS name`,
			withReturn: `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) RETURN b } RETURN a.name AS name`,
		},
		{
			block:      `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[]->(b) WHERE b.age > 30 } RETURN a.name AS name`,
			withReturn: `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[]->(b) WHERE b.age > 30 RETURN b } RETURN a.name AS name`,
		},
	}

	for _, p := range pairs {
		blockGot := collectColumn(t, eng, p.block, "name")
		returnGot := collectColumn(t, eng, p.withReturn, "name")
		if !slices.Equal(blockGot, returnGot) {
			t.Errorf("block form %v != RETURN form %v\n  block:  %s\n  return: %s",
				blockGot, returnGot, p.block, p.withReturn)
		}
	}
}

// TestSubqueryBlockForm_RejectsUpdateClause pins the retained guard: EXISTS is
// a read-only existence check, so an updating clause in the body is a compile
// error. Neo4j 5.26.28 rejects the same query with "An Exists Expression
// cannot contain any updates".
//
// Note the block body itself cannot express an update — `readingStatement` is
// only MATCH / UNWIND / CALL — so this exercises the regularQuery path, which
// is where the guard has always lived.
func TestSubqueryBlockForm_RejectsUpdateClause(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	const q = `MATCH (a:Person) WHERE EXISTS { MATCH (a) SET a.x = 1 RETURN a } RETURN a.name AS name`
	_, err := eng.Run(context.Background(), q, nil)
	if err == nil {
		t.Fatalf("EXISTS with an updating clause was accepted; want rejection")
	}
	if !strings.Contains(err.Error(), "InvalidClauseComposition") {
		t.Errorf("unexpected error for update-in-EXISTS: %v", err)
	}
}

// TestSubqueryForms_AcceptanceMatrix is the regression guard for task #2216
// acceptance criterion 3: every subquery form that parsed before the fix must
// still parse afterwards. It is the permanent promotion of the round-4 audit
// probe (bench/r4audit/subq_test.go), which only printed results.
//
// The wantAccepted flags record GoGraph's behaviour as verified against Neo4j
// 5.26.28 where the two agree, and as a deliberate, documented gap where they
// do not — see the per-case comments. Turning a documented gap into an
// acceptance is a feature change and must flip the flag here, so the matrix
// cannot drift silently in either direction.
func TestSubqueryForms_AcceptanceMatrix(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	cases := []struct {
		name         string
		query        string
		wantAccepted bool
		note         string
	}{
		// ---- EXISTS ----
		{"exists-pattern-only", `MATCH (a:Person) WHERE EXISTS { (a)-[:KNOWS]->(b) } RETURN count(a) AS n`, true, ""},
		{"exists-match-nowhere", `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) } RETURN count(a) AS n`, true, "fixed by #2216"},
		{"exists-match-where", `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) WHERE b.age > 1 } RETURN count(a) AS n`, true, "fixed by #2216"},
		{"exists-match-return", `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) RETURN b } RETURN count(a) AS n`, true, ""},
		// REJECTED since rmp #2615, and the change of verdict is the point. It
		// was accepted and answered from its FIRST BRANCH ALONE, silently: the
		// subquery AST cannot hold a multi-branch query, so the visitor kept
		// Parts[0] and discarded the rest. Both reference engines ANSWER this
		// query — supporting it is #2627 — but answering one branch of it is a
		// wrong answer, and refusing is what stops that today.
		{"exists-union", `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) RETURN b UNION MATCH (a)<-[:KNOWS]-(b) RETURN b } RETURN count(a) AS n`, false, "UNION in a subquery body is refused by #2615; support is #2627"},
		{"exists-with", `MATCH (a:Person) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) WITH b WHERE b.age > 1 RETURN b } RETURN count(a) AS n`, true, ""},
		{"exists-unwind-match", `MATCH (a:Person) WHERE EXISTS { UNWIND [1] AS x MATCH (a)-[:KNOWS]->(b) } RETURN count(a) AS n`, true, "fixed by #2216"},

		// ---- COUNT ----
		{"count-pattern-only", `MATCH (a:Person) WHERE COUNT { (a)-[:KNOWS]->(b) } > 0 RETURN count(a) AS n`, true, ""},
		{"count-match-nowhere", `MATCH (a:Person) WHERE COUNT { MATCH (a)-[:KNOWS]->(b) } > 0 RETURN count(a) AS n`, true, "fixed by #2216"},
		{"count-match-return", `MATCH (a:Person) WHERE COUNT { MATCH (a)-[:KNOWS]->(b) RETURN b } > 0 RETURN count(a) AS n`, true, ""},
		{"count-in-return", `MATCH (a:Person) RETURN a.name AS name, COUNT { (a)-[:KNOWS]->() } AS deg`, true, ""},
		// The COUNT twin of exists-union. It had no matrix entry at all, which is
		// why the identical silent drop in VisitSubqueryCount went unrecorded
		// while the EXISTS one at least carried a comment (#2615).
		{"count-union", `MATCH (a:Person) WHERE COUNT { MATCH (a)-[:KNOWS]->(b) RETURN b UNION MATCH (a)<-[:KNOWS]-(b) RETURN b } > 0 RETURN count(a) AS n`, false, "UNION in a subquery body is refused by #2615; support is #2627"},

		// ---- pattern predicates (the pre-subquery spelling) ----
		// The target node must stay anonymous: a pattern predicate may not
		// introduce a new variable. GoGraph rejects that with
		// SyntaxError.UndefinedVariable and Neo4j 5.26.28 with
		// "PatternExpressions are not allowed to introduce new variables" —
		// the same ruling, so the anonymous spelling is the accepted one.
		{"pattern-pred-pos", `MATCH (a:Person) WHERE (a)-[:KNOWS]->(:Person) RETURN count(a) AS n`, true, ""},
		{"pattern-pred-neg", `MATCH (a:Person) WHERE NOT (a)-[:KNOWS]->(:Person) RETURN count(a) AS n`, true, ""},
		{"pattern-pred-new-var", `MATCH (a:Person) WHERE (a)-[:KNOWS]->(b) RETURN count(a) AS n`, false, "a pattern predicate may not introduce a new variable; Neo4j 5.26 agrees"},

		// ---- documented gaps: accepted by Neo4j 5.26, not by GoGraph. ----
		// Each is tracked separately and is deliberately out of #2216's scope,
		// which is the RETURN-less block body only.
		{"collect-subquery", `MATCH (a:Person) RETURN COLLECT { MATCH (a)-[:KNOWS]->(b) RETURN b.name } AS names`, false, "COLLECT { } subquery not implemented"},
		{"call-subquery-with", `MATCH (a:Person) CALL { WITH a MATCH (a)-[:KNOWS]->(b) RETURN b } RETURN count(b) AS n`, false, "CALL { } subquery not in the grammar"},
		{"call-subquery-import", `CALL { MATCH (n:Person) RETURN n LIMIT 1 } RETURN n`, false, "CALL { } subquery not in the grammar"},
		{"exists-legacy-func", `MATCH (a:Person) WHERE exists(a.name) RETURN count(a) AS n`, false, "legacy exists(prop) function not implemented; use IS NOT NULL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			accepted := parses(t, eng, tc.query)
			if accepted != tc.wantAccepted {
				verb := map[bool]string{true: "accepted", false: "rejected"}
				t.Errorf("%s: %s, want %s (%s)\n  query: %s",
					tc.name, verb[accepted], verb[tc.wantAccepted], tc.note, tc.query)
			}
		})
	}
}

// TestSubqueryBlockForm_DocumentedExampleRuns runs the EXACT query that
// docs/cypher.md publishes in the supported-predicates table for
// `EXISTS { MATCH … }`, so the documentation claim rests on the published query
// verbatim rather than on a paraphrase. This is the acceptance criterion that
// the documentation is true of the code.
//
// Task #2227 generalises this into a documentation gate that executes every
// Cypher example in docs/; until then this pins the one example whose falsity
// motivated task #2216.
func TestSubqueryBlockForm_DocumentedExampleRuns(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	// docs/cypher.md — | `EXISTS { MATCH … }` | `WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) }` |
	const documented = `MATCH (n) WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) } RETURN count(n) AS n`
	if !parses(t, eng, documented) {
		t.Errorf("the documented EXISTS example is rejected; docs/cypher.md is not true of the code\n  query: %s", documented)
	}
}

// parses reports whether the engine accepts and evaluates query without error.
func parses(t *testing.T, eng *cypher.Engine, query string) bool {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		return false
	}
	defer res.Close()
	for res.Next() {
	}
	return res.Err() == nil
}
