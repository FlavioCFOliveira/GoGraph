package cypher_test

// string_operator_after_with_2981_test.go — regression gate for rmp #2981.
//
// A STARTS WITH or ENDS WITH predicate that followed a WITH clause failed to
// parse: `WITH 1 AS x MATCH (m:P) WHERE m.name STARTS WITH 'a' RETURN m.name`
// was rejected with `unexpected "RETURN" ..., expected one of {WITH, UNION}`.
// The hand-written MultiPartQ loop in the generated parser decides whether
// another WITH clause follows by scanning the token stream for a top-level
// WITH token, and it counted the second word of the string operator as one.
//
// openCypher 9 defines the operator as `STARTS WITH` / `ENDS WITH` inside
// oC_StringPredicateExpression and allows it in any WHERE, so every query below
// must parse and return the rows the predicate selects. The fixture's names are
// chosen so that STARTS WITH 'a' and ENDS WITH 'a' select different, non-empty
// row sets.

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestStringOperatorAfterWith_2981(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e := cypher.NewEngine(g)
	if _, errText := wpsDrain(t, e,
		"CREATE (:P {name: 'alpha'}), (:P {name: 'abc'}), (:P {name: 'beta'}), "+
			"(:P {name: 'delta'}), (:P:STARTS {name: 'zeta', starts: 'zz'})"); errText != "" {
		t.Fatalf("fixture: %s", errText)
	}

	startsA := []string{`n="abc"`, `n="alpha"`}
	endsA := []string{`n="alpha"`, `n="beta"`, `n="delta"`, `n="zeta"`}

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		// The reported reproduction and its ENDS WITH variant.
		{"after WITH/STARTS", "WITH 1 AS x MATCH (m:P) WHERE m.name STARTS WITH 'a' RETURN m.name AS n", startsA},
		{"after WITH/ENDS", "WITH 1 AS x MATCH (m:P) WHERE m.name ENDS WITH 'a' RETURN m.name AS n", endsA},
		// The operand is a parameter-free variable rather than a literal.
		{"after WITH/STARTS variable operand", "WITH 'a' AS p MATCH (m:P) WHERE m.name STARTS WITH p RETURN m.name AS n", startsA},
		// Letter case, a comment and a line break between the two words.
		{"after WITH/lower case", "with 1 as x match (m:P) where m.name starts with 'a' return m.name as n", startsA},
		{"after WITH/comment and newline", "WITH 1 AS x MATCH (m:P) WHERE m.name ENDS /* op */\n\t WITH 'a' RETURN m.name AS n", endsA},
		// A later WITH clause after the operator must still be found.
		{"after WITH/followed by WITH", "WITH 1 AS x MATCH (m:P) WHERE m.name STARTS WITH 'a' WITH m RETURN m.name AS n", startsA},
		{"after WITH/in WITH WHERE", "MATCH (m:P) WITH m WHERE m.name ENDS WITH 'a' WITH m RETURN m.name AS n", endsA},
		// After UNWIND, inside a multi-part query.
		{"after UNWIND/STARTS", "UNWIND ['a'] AS p WITH p MATCH (m:P) WHERE m.name STARTS WITH p RETURN m.name AS n", startsA},
		{"after UNWIND/ENDS", "WITH 1 AS x UNWIND ['a'] AS p MATCH (m:P) WHERE m.name ENDS WITH p RETURN m.name AS n", endsA},
		// Inside subqueries whose body is a multi-part query. The CALL { }
		// subquery clause is not supported (docs/cypher.md), so EXISTS { } and
		// COUNT { } carry the subquery cases.
		{"COUNT subquery/STARTS", "MATCH (m:P) WHERE COUNT { WITH m AS q MATCH (q) WHERE q.name STARTS WITH 'a' RETURN q } = 1 RETURN m.name AS n", startsA},
		{"COUNT subquery/ENDS", "MATCH (m:P) WHERE COUNT { WITH m AS q MATCH (q) WHERE q.name ENDS WITH 'a' RETURN q } = 1 RETURN m.name AS n", endsA},
		{"EXISTS subquery/STARTS", "MATCH (m:P) WHERE EXISTS { WITH m AS q MATCH (q) WHERE q.name STARTS WITH 'a' RETURN q } RETURN m.name AS n", startsA},
		{"EXISTS subquery/ENDS", "MATCH (m:P) WHERE EXISTS { WITH m AS q MATCH (q) WHERE q.name ENDS WITH 'a' RETURN q } RETURN m.name AS n", endsA},
		// STARTS and ENDS used as names, directly followed by a WITH clause:
		// that WITH is a clause and must still be recognised.
		{"label STARTS then WITH clause", "WITH 1 AS x MATCH (m:P) WHERE m:STARTS WITH m RETURN m.name AS n", []string{`n="zeta"`}},
		{"property ends then WITH clause", "WITH 1 AS x MATCH (m:P) WITH m ORDER BY m.ends WITH m WHERE m.name STARTS WITH 'a' RETURN m.name AS n", startsA},
		// A property named starts as the operand of the operator.
		{"property starts as operand", "WITH 1 AS x MATCH (m:P) WHERE m.starts STARTS WITH 'z' RETURN m.name AS n", []string{`n="zeta"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, errText := wpsDrain(t, e, c.query)
			if errText != "" {
				t.Fatalf("%s\nerror: %s", c.query, errText)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("%s\ngot  %v\nwant %v", c.query, got, c.want)
			}
		})
	}
}
