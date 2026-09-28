package cypher_test

// subscript_property_2918_test.go — rmp #2918, evaluation side.
//
// A list subscript or slice followed by a property lookup or a label test —
// `q[0].w`, `q[1..3][1].w`, `q[0]:Person` — failed to parse in every clause
// because the vendored grammar placed subscripts after the property chain
// (see cypher/parser/subscript_property_2918_test.go for the grammar
// citation). This test pins the VALUES those expressions evaluate to, so a
// fix that parses them into the wrong operator structure fails here too.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestSubscriptPropertyEvaluation2918 evaluates each postfix chain on
// newExistsGraph (4 :Person nodes; alice KNOWS bob (20) and LIKES dave (35)).
func TestSubscriptPropertyEvaluation2918(t *testing.T) {
	t.Parallel()
	eng := newExistsGraph(t)

	tests := []struct {
		name  string
		query string
		want  expr.Value
	}{
		{"literal list", `RETURN [{w: 1}][0].w AS v`, expr.IntegerValue(1)},
		{"variable", `WITH [{w: 1}, {w: 2}] AS q RETURN q[1].w AS v`, expr.IntegerValue(2)},
		{"double subscript", `WITH [[{w: 1}], [{w: 7}]] AS q RETURN q[1][0].w AS v`, expr.IntegerValue(7)},
		{"property chain", `WITH [{a: {b: 5}}] AS q RETURN q[0].a.b AS v`, expr.IntegerValue(5)},
		{"slice then subscript", `WITH [{w: 1}, {w: 2}, {w: 3}] AS q RETURN q[1..3][1].w AS v`, expr.IntegerValue(3)},
		{"parenthesised control", `WITH [{w: 1}] AS q RETURN (q[0]).w AS v`, expr.IntegerValue(1)},
		{"function result", `WITH [[{w: 4}]] AS x RETURN head(x)[0].w AS v`, expr.IntegerValue(4)},
		{"property then subscript then property", `WITH {list: [{w: 6}]} AS n RETURN n.list[0].w AS v`, expr.IntegerValue(6)},
		{"subscript after property after subscript", `WITH [{w: [9, 8]}] AS q RETURN q[0].w[1] AS v`, expr.IntegerValue(8)},
		{"missing key", `WITH [{w: 1}] AS q RETURN q[0].x AS v`, expr.Null},
		{"IS NULL", `WITH [{w: 1}] AS q RETURN q[0].x IS NULL AS v`, expr.BoolValue(true)},
		{"unary minus", `WITH [{w: 1}] AS q RETURN -q[0].w AS v`, expr.IntegerValue(-1)},
		{"arithmetic", `WITH [{w: 1}] AS q RETURN q[0].w + 1 AS v`, expr.IntegerValue(2)},
		// WHERE sits on its own WITH: a WHERE that subscripts a list projected
		// by the SAME WITH drops the row (`WITH [1] AS q WHERE q[0] = 1`
		// counts 0), on the base revision as much as after #2918; that defect
		// is separate from this parse fix.
		{"WHERE", `WITH [{w: 1}] AS q WITH q WHERE q[0].w = 1 RETURN count(*) AS v`, expr.IntegerValue(1)},
		{"ORDER BY", `UNWIND [[{w: 2}], [{w: 1}]] AS q WITH q ORDER BY q[0].w LIMIT 1 RETURN q[0].w AS v`, expr.IntegerValue(1)},
		{"UNWIND", `WITH [{w: [1, 2]}] AS q UNWIND q[0].w AS z RETURN sum(z) AS v`, expr.IntegerValue(3)},
		{"list comprehension", `WITH [[{w: 1}], [{w: 2}]] AS qs RETURN [rs IN qs WHERE rs[0].w > 1 | rs[0].w][0] AS v`, expr.IntegerValue(2)},
		{"generic CASE", `WITH [{w: 1}] AS q RETURN CASE WHEN q[0].w = 1 THEN q[0].w + 10 ELSE 0 END AS v`, expr.IntegerValue(11)},
		{"simple CASE", `WITH [{w: 1}] AS q RETURN CASE q[0].w WHEN 1 THEN 'one' END AS v`, expr.StringValue("one")},
		{"inside a subscript", `WITH [{w: 1}] AS q, [10, 20] AS x RETURN x[q[0].w] AS v`, expr.IntegerValue(20)},
		{"inside a slice", `WITH [{w: 1}] AS q, [10, 20, 30] AS x RETURN size(x[q[0].w..3]) AS v`, expr.IntegerValue(2)},
		{"slice upper bound", `WITH [{w: 1}] AS q, [10, 20, 30] AS x RETURN x[..q[0].w][0] AS v`, expr.IntegerValue(10)},
		{"map value", `WITH [{w: 1}] AS q RETURN {k: q[0].w}.k AS v`, expr.IntegerValue(1)},
		{"function argument", `WITH [{w: -3}] AS q RETURN abs(q[0].w) AS v`, expr.IntegerValue(3)},
		{"any()", `WITH [{w: 1}, {w: 2}] AS q RETURN any(r IN q WHERE r.w = q[1].w) AS v`, expr.BoolValue(true)},
		{"reduce() body", `WITH [{w: 2}] AS q RETURN reduce(s = 0, r IN [1, 2, 3] | s + q[0].w) AS v`, expr.IntegerValue(6)},
		{"pattern comprehension", `MATCH (a:Person {name: 'alice'}) RETURN size([(a)-->(b) WHERE [b][0].age > 25 | [b][0].name]) AS v`, expr.IntegerValue(1)},
		{"label test", `MATCH (n:Person {name: 'alice'}) WITH [n] AS q RETURN q[0]:Person AS v`, expr.BoolValue(true)},
		{"label test false", `MATCH (n:Person {name: 'alice'}) WITH [n] AS q RETURN q[0]:Robot AS v`, expr.BoolValue(false)},
		{"label test in WHERE", `MATCH (n:Person) WITH [n] AS q WITH q WHERE q[0]:Person RETURN count(*) AS v`, expr.IntegerValue(4)},
		{"node property", `MATCH (n:Person {name: 'alice'}) WITH [n] AS q RETURN q[0].age AS v`, expr.IntegerValue(30)},
		// The subscript binds tighter than the string predicate (openCypher);
		// before #2918 this read as ('ab' STARTS WITH ['a'])[0].
		{"STARTS WITH operand", `RETURN 'ab' STARTS WITH ['a'][0] AS v`, expr.BoolValue(true)},
		{"ENDS WITH operand", `WITH [{w: 'b'}] AS q RETURN 'ab' ENDS WITH q[0].w AS v`, expr.BoolValue(true)},
		{"IN operand", `WITH [{w: [1, 2]}] AS q RETURN 2 IN q[0].w AS v`, expr.BoolValue(true)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scalarValue(t, eng, tc.query, "v")
			if got != tc.want {
				t.Errorf("%s: got %v (%T), want %v (%T)\n  query: %s", tc.name, got, got, tc.want, tc.want, tc.query)
			}
		})
	}
}
