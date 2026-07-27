package cypher_test

// genpatch_behaviour_test.go — regression guard for the hand-written parser
// patches in cypher/parser/grammar/gen-patches.patch.
//
// Those patches carry four behaviours that cannot live in the ANTLR grammar:
// the numeric-ID workarounds (integer literals and variable-length range
// bounds tokenise as ID rather than DIGIT because of lexer-rule ordering),
// chained WITH, optional CALL parentheses, and reduce().
//
// The patch is re-applied by `make generate-cypher-parser` on top of freshly
// generated code, and several of its hunks pin ABSOLUTE ATN state numbers. Any
// grammar change shifts those numbers, so a regeneration can leave the patch
// applying cleanly by line context while silently pointing at the wrong
// states. Task #2216 hit exactly that: adding one alternative to
// subqueryExist/subqueryCount shifted every state after them by +10 and four
// hunks had to be reconciled by hand.
//
// A build that merely compiles is therefore NOT evidence that the patch still
// works. This test is that evidence, and it must be run after every
// regeneration.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestGenPatchBehaviours pins each behaviour carried by gen-patches.patch to a
// concrete result, so a mis-reconciled patch fails here rather than surfacing
// as a mysterious parse error later.
//
// The graph is newExistsGraph: 4 :Person nodes, alice with two outgoing edges
// (KNOWS→bob, LIKES→dave), charlie isolated.
func TestGenPatchBehaviours(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	tests := []struct {
		patch string // which hand-patched behaviour this covers
		name  string
		query string
		col   string
		want  interface{}
	}{
		// literalNumericIDFix / numLitNumericIDFix — a purely-numeric token is
		// lexed as ID, not DIGIT, and must still parse as an integer literal.
		{"numeric-ID literal", "in-list", `RETURN 3 IN [1, 2, 3] AS v`, "v", expr.BoolValue(true)},
		{"numeric-ID literal", "bare", `RETURN 42 AS v`, "v", expr.IntegerValue(42)},
		{"numeric-ID literal", "arithmetic", `RETURN 1 + 2 AS v`, "v", expr.IntegerValue(3)},

		// rangeLitNumericIDFix — the bounds of a variable-length pattern.
		{"numeric-ID range", "bounded", `MATCH (a:Person {name: 'alice'})-[*1..2]->(b) RETURN count(b) AS v`, "v", expr.IntegerValue(2)},
		{"numeric-ID range", "lower-only", `MATCH (a:Person {name: 'alice'})-[*1..]->(b) RETURN count(b) AS v`, "v", expr.IntegerValue(2)},
		{"numeric-ID range", "unbounded", `MATCH (a:Person {name: 'alice'})-[*]->(b) RETURN count(b) AS v`, "v", expr.IntegerValue(2)},

		// chained WITH — WITH directly followed by another WITH.
		{"chained WITH", "two", `MATCH (n:Person) WITH n WITH n RETURN count(n) AS v`, "v", expr.IntegerValue(4)},
		{"chained WITH", "three", `MATCH (n:Person) WITH n WITH n WITH n RETURN count(n) AS v`, "v", expr.IntegerValue(4)},

		// optional CALL parentheses — both spellings must work.
		{"optional CALL parens", "with-parens", `CALL db.labels() YIELD label RETURN count(label) AS v`, "v", expr.IntegerValue(1)},
		{"optional CALL parens", "without-parens", `CALL db.labels YIELD label RETURN count(label) AS v`, "v", expr.IntegerValue(1)},

		// reduce() — a hand-written parser rule, not generated from the grammar.
		{"reduce()", "sum", `RETURN reduce(acc = 0, x IN [1, 2, 3] | acc + x) AS v`, "v", expr.IntegerValue(6)},
		{"reduce()", "concat", `RETURN reduce(s = '', x IN ['a', 'b'] | s + x) AS v`, "v", expr.StringValue("ab")},
	}

	for _, tc := range tests {
		t.Run(tc.patch+"/"+tc.name, func(t *testing.T) {
			t.Parallel()
			got := scalarValue(t, eng, tc.query, tc.col)
			if got != tc.want {
				t.Errorf("%s (%s): got %v (%T), want %v (%T)\n  query: %s",
					tc.patch, tc.name, got, got, tc.want, tc.want, tc.query)
			}
		})
	}
}

// TestSubqueryBlockForm_CallReadingClause pins that CALL — the third
// readingStatement alternative alongside MATCH and UNWIND — is usable in a
// RETURN-less subquery body, as docs/cypher.md states.
func TestSubqueryBlockForm_CallReadingClause(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	const q = `MATCH (a:Person) WHERE EXISTS { CALL db.labels() YIELD label } RETURN count(a) AS v`
	got := scalarValue(t, eng, q, "v")
	if want := interface{}(expr.IntegerValue(4)); got != want {
		t.Errorf("CALL in a block body: got %v, want %v\n  query: %s", got, want, q)
	}
}
