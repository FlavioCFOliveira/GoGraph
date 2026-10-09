package cypher_test

// pattern_comprehension_unbound_2900_test.go — rmp #2900.
//
// A pattern comprehension may introduce variables of its own: they are local
// to the comprehension (openCypher TCK expressions/pattern/Pattern2.feature [4]
// "Introduce a new node variable in pattern comprehension" and [5] "… a new
// relationship variable …"). One whose variables are ALL new is therefore
// matched against the whole graph, once per outer row. The restriction on
// introducing variables applies to a pattern used as a PREDICATE
// (Pattern1.feature [10] "Fail on introducing unbounded variables in
// pattern"), not to a comprehension.
//
// In the leading clause of a query the comprehension has no driving plan, and
// the translator built its RollUpApply with a nil Outer; RollUpApply.Vars
// dereferenced it, so `RETURN [(a)-[]-(b)-[]-(a) | 1]` failed with an internal
// panic instead of returning its list. Each route below is asserted against
// the MATCH count of the same pattern, on every fixture, so the routes cannot
// agree on a wrong answer.
//
// Layer: short. goleak-clean (engine and graph are local).

import (
	"context"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

func TestPatternComprehension_LeadingClauseUnboundVariables(t *testing.T) {
	t.Parallel()
	patterns := []string{
		`(a)-[]-(b)-[]-(a)`,
		`(a)-[]-(b)`,
		`(a)-->()`,
		`(a)-[r:R]->(b)`,
		`(a)-[:R]-()-[:R]-(a)`,
	}
	for _, fx := range puFixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			eng := puEngine(t, fx)
			for _, p := range patterns {
				want := puRows(t, eng, `MATCH `+p+` RETURN count(*) AS n`, "n")
				for _, q := range []string{
					// The #2900 shapes: a leading RETURN, with the comprehension
					// nested in an expression and at the top of the item.
					`RETURN size([` + p + ` | 1]) AS n`,
					`RETURN [` + p + ` | 1] AS l`,
					// A leading WITH takes the WITH translator.
					`WITH [` + p + ` | 1] AS l RETURN size(l) AS n`,
					// Control: a driving row ahead of the comprehension.
					`WITH 1 AS x RETURN size([` + p + ` | 1]) AS n`,
				} {
					got := comprehensionSizes(t, eng, q)
					if len(got) != 1 || len(want) != 1 || got[0] != want[0] {
						t.Errorf("%q: got %v, want %v (MATCH count)", q, got, want)
					}
				}
			}
		})
	}
}

// comprehensionSizes runs query and returns, per row, its n column, or the
// length of its l column when the query returns the list itself, rendered as
// puRows renders an integer.
func comprehensionSizes(t *testing.T, eng *cypher.Engine, query string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Errorf("Close(%q): %v", query, cerr)
		}
	}()
	var out []string
	for res.Next() {
		rec := res.Record()
		if l, ok := rec["l"]; ok {
			list, isList := l.(expr.ListValue)
			if !isList {
				t.Fatalf("%q: column l is %T, want expr.ListValue", query, l)
			}
			out = append(out, strconv.Itoa(len(list)))
			continue
		}
		n, ok := rec["n"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("%q: column n is %T, want expr.IntegerValue", query, rec["n"])
		}
		out = append(out, n.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	return out
}
