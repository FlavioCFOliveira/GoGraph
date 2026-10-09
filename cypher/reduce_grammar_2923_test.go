package cypher_test

// reduce_grammar_2923_test.go — rmp #2923: reduce() inside a subscript, a slice,
// a map value or a function argument failed to parse.
//
// reduce(acc = init, x IN list | expr) used to be a hand-written parser
// function spliced into the generated parser after generation, invisible to the
// ATN. Adaptive prediction for any construct whose alternatives are decided by
// looking PAST the reduce() call — the subscript-versus-slice decision of a
// postfix list operator is one — simulated the ATN, found the `|` illegal in an
// expression and rejected the whole input. reduce() is now a rule of the
// grammar, so prediction sees it.
//
// The identifier cases pin the other side of the change: reduce() is recognised
// through a REDUCE keyword token, and the word must stay usable wherever an
// identifier is legal, as it was when it lexed as a plain identifier.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestReduceGrammar2923_Positions(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	for _, tc := range []struct {
		name, query string
		want        string
	}{
		{"subscript", `WITH [10, 20, 30] AS x, [1, 1] AS l RETURN x[reduce(a = 0, y IN l | a + y)] AS v`, "30"},
		{"subscript-negative", `WITH [10, 20, 30] AS x RETURN x[reduce(a = 0, y IN [1] | a - y)] AS v`, "30"},
		{"slice-from", `WITH [10, 20, 30] AS x RETURN x[reduce(a = 0, y IN [1] | a + y)..] AS v`, "[20, 30]"},
		{"slice-to", `WITH [10, 20, 30] AS x RETURN x[..reduce(a = 0, y IN [1, 1] | a + y)] AS v`, "[10, 20]"},
		{"slice-both", `WITH [10, 20, 30] AS x RETURN x[reduce(a = 0, y IN [1] | a + y)..reduce(a = 0, y IN [1, 2] | a + y)] AS v`, "[20, 30]"},
		{"subscript-then-property", `WITH [{w: 7}] AS q RETURN q[reduce(a = 0, y IN [] | a + y)].w AS v`, "7"},
		{"map-value", `RETURN {k: reduce(a = 0, y IN [1, 2, 3] | a + y)}.k AS v`, "6"},
		{"map-value-second-key", `RETURN {j: 1, k: reduce(a = 0, y IN [1, 2] | a + y)}.k AS v`, "3"},
		{"function-argument", `RETURN abs(reduce(a = 0, y IN [1, 2] | a - y)) AS v`, "3"},
		{"function-argument-second", `RETURN coalesce(null, reduce(a = 0, y IN [4] | a + y)) AS v`, "4"},
		{"function-argument-size", `RETURN size(reduce(a = [], y IN [1, 2] | a + y)) AS v`, "2"},
		{"list-comprehension", `RETURN [z IN [1, 2] | reduce(a = z, y IN [10] | a + y)] AS v`, "[11, 12]"},
		{"nested", `RETURN reduce(a = 0, y IN [reduce(b = 0, w IN [1, 2] | b + w)] | a + y) AS v`, "3"},
		{"upper-case", `RETURN REDUCE(a = 0, y IN [1, 2] | a + y) AS v`, "3"},
		{"in-where", `UNWIND [1, 2, 3] AS n WITH n WHERE n = reduce(a = 0, y IN [1, 1] | a + y) RETURN n AS v`, "2"},
		{"in-case", `RETURN CASE WHEN reduce(a = 0, y IN [1] | a + y) = 1 THEN 'one' ELSE 'other' END AS v`, "one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := collectRowStrings(t, eng, tc.query, "v")
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("%q = %v, want [%s]", tc.query, got, tc.want)
			}
		})
	}
}

// TestReduceGrammar2923_IdentifierSurvives asserts that the word reduce is still
// accepted wherever an identifier is: as a variable, a property key, a label, a
// relationship type, a map key and a map-projection subject.
func TestReduceGrammar2923_IdentifierSurvives(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	res, err := eng.RunAny(ctx, `CREATE (:reduce {reduce: 5})-[:reduce]->(:Other)`, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("seed drain: %v", err)
	}
	res.Close()
	for _, tc := range []struct {
		name, query, want string
	}{
		{"variable", `WITH 3 AS reduce RETURN reduce + 1 AS v`, "4"},
		{"node-variable", `MATCH (reduce:reduce) RETURN reduce.reduce AS v`, "5"},
		{"relationship-type", `MATCH ()-[reduce:reduce]->() RETURN type(reduce) AS v`, "reduce"},
		{"map-key", `RETURN {reduce: 2}.reduce AS v`, "2"},
		{"map-projection", `MATCH (reduce:reduce) RETURN reduce{.reduce}.reduce AS v`, "5"},
		{"variable-in-reduce", `WITH [1, 2] AS reduce RETURN reduce(reduce = 0, y IN reduce | reduce + y) AS v`, "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := collectRowStrings(t, eng, tc.query, "v")
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("%q = %v, want [%s]", tc.query, got, tc.want)
			}
		})
	}
}
