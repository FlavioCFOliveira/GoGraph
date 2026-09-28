package parser

// subscript_property_2918_test.go — rmp #2918.
//
// openCypher applies list subscripts, slices and property lookups as postfix
// operators of one operand, in any order: openCypher 9's
// oC_NonArithmeticOperatorExpression is `oC_Atom ( oC_ListOperatorExpression |
// oC_PropertyLookup )* oC_NodeLabels?`, and the current openCypher.bnf
// (opencypher/openCypher 677cbafa, lines 741-759) defines <postfix expression>
// as a primary followed by any sequence of <static property reference>,
// <dynamic element reference> and <slicing>. The vendored grammar placed the
// subscript at the atomicExpression level, after the property chain, so
// nothing could follow it: `q[0].w` failed with `unexpected "."` in every
// clause, and `q[0]:Label` with `unexpected ":"`. Only `(q[0]).w`, the form
// the TCK uses, parsed.
//
// The same misplacement made a subscript on the right operand of STARTS WITH /
// ENDS WITH / CONTAINS apply to the predicate's result, and admitted a
// subscript after IS NULL and after a label test, neither of which openCypher
// derives.
//
// Layer: short.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// shape2918 renders the operator structure of e compactly, so a test states
// which operator applies to which operand rather than only that parsing
// succeeded.
func shape2918(e ast.Expression) string {
	switch x := e.(type) {
	case *ast.Variable:
		return x.Name
	case *ast.IntLiteral:
		return fmt.Sprint(x.Value)
	case *ast.FloatLiteral:
		return fmt.Sprint(x.Value)
	case *ast.StringLiteral:
		return "'" + x.Value + "'"
	case *ast.ListLiteral:
		parts := make([]string, len(x.Elements))
		for i, el := range x.Elements {
			parts[i] = shape2918(el)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case *ast.MapLiteral:
		parts := make([]string, len(x.Keys))
		for i, k := range x.Keys {
			parts[i] = k + ":" + shape2918(x.Values[i])
		}
		return "{" + strings.Join(parts, ",") + "}"
	case *ast.Property:
		return "prop(" + shape2918(x.Receiver) + "," + x.Key + ")"
	case *ast.SubscriptExpr:
		return "sub(" + shape2918(x.Expr) + "," + shape2918(x.Index) + ")"
	case *ast.SliceExpr:
		from, to := "_", "_"
		if x.From != nil {
			from = shape2918(x.From)
		}
		if x.To != nil {
			to = shape2918(x.To)
		}
		return "slice(" + shape2918(x.Expr) + "," + from + "," + to + ")"
	case *ast.LabelPredicate:
		return "label(" + shape2918(x.Receiver) + "," + strings.Join(x.Labels, ":") + ")"
	case *ast.BinaryOp:
		return "op(" + x.Operator + "," + shape2918(x.Left) + "," + shape2918(x.Right) + ")"
	case *ast.UnaryOp:
		return "op(" + x.Operator + "," + shape2918(x.Operand) + ")"
	case *ast.FunctionInvocation:
		parts := make([]string, len(x.Args))
		for i, a := range x.Args {
			parts[i] = shape2918(a)
		}
		return x.Name + "(" + strings.Join(parts, ",") + ")"
	default:
		return fmt.Sprintf("%T", e)
	}
}

// TestSubscriptPostfixShape2918 pins the operator structure of each postfix
// chain: every subscript, slice, property lookup and label test takes the
// expression to its left as its operand.
func TestSubscriptPostfixShape2918(t *testing.T) {
	t.Parallel()
	cases := []struct{ query, want string }{
		{"RETURN [{w:1}][0].w", "prop(sub([{w:1}],0),w)"},
		{"RETURN q[0].w", "prop(sub(q,0),w)"},
		{"RETURN q[0][1].w", "prop(sub(sub(q,0),1),w)"},
		{"RETURN q[0].a.b", "prop(prop(sub(q,0),a),b)"},
		{"RETURN q[1..2][0].w", "prop(sub(slice(q,1,2),0),w)"},
		{"RETURN q[..2].w", "prop(slice(q,_,2),w)"},
		{"RETURN q[1..].w", "prop(slice(q,1,_),w)"},
		{"RETURN (q[0]).w", "prop(sub(q,0),w)"},
		{"RETURN f(x)[0].w", "prop(sub(f(x),0),w)"},
		{"RETURN n.list[0].w", "prop(sub(prop(n,list),0),w)"},
		{"RETURN n.a.b[0].c[1].d", "prop(sub(prop(sub(prop(prop(n,a),b),0),c),1),d)"},
		{"RETURN q[0].w[0]", "sub(prop(sub(q,0),w),0)"},
		{"RETURN q[0]['w']", "sub(sub(q,0),'w')"},
		{"RETURN q[0]:Label", "label(sub(q,0),Label)"},
		{"RETURN q[0].w:A:B", "label(prop(sub(q,0),w),A:B)"},
		{"RETURN x[q[0].w]", "sub(x,prop(sub(q,0),w))"},
		{"RETURN x[q[0].w..2]", "slice(x,prop(sub(q,0),w),2)"},
		{"RETURN x[..q[0].w]", "slice(x,_,prop(sub(q,0),w))"},
		{"RETURN -q[0].w", "op(-,prop(sub(q,0),w))"},
		{"RETURN q[0].w IS NULL", "op(IS NULL,prop(sub(q,0),w))"},
		{"RETURN 3 IN q[0].w", "op(IN,3,prop(sub(q,0),w))"},
		{"RETURN 3 IN list[0]", "op(IN,3,sub(list,0))"},
		{"RETURN 'ab' STARTS WITH ['a'][0]", "op(STARTS WITH,'ab',sub(['a'],0))"},
		{"RETURN 'ab' ENDS WITH q[0].w", "op(ENDS WITH,'ab',prop(sub(q,0),w))"},
		{"RETURN 'ab' CONTAINS q[1..2][0]", "op(CONTAINS,'ab',sub(slice(q,1,2),0))"},
		{"RETURN {k: q[0].w}", "{k:prop(sub(q,0),w)}"},
		{"RETURN [q[0].w, 1]", "[prop(sub(q,0),w),1]"},
		{"RETURN abs(q[0].w)", "abs(prop(sub(q,0),w))"},
		// The float-literal reconstruction (T937) still sees `1.5` as one
		// literal when a subscript follows it.
		{"RETURN 1.5[0]", "sub(1.5,0)"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			q, err := Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			got := shape2918(q.(*ast.SingleQuery).Return.Projection.Items[0].Expr)
			if got != tc.want {
				t.Errorf("Parse(%q) shape = %s, want %s", tc.query, got, tc.want)
			}
		})
	}
}

// TestSubscriptPropertyEveryClause2918 parses a subscript followed by a
// property lookup in every position the report and its review named. Several
// sit where the parser must predict across the whole expression
// (AdaptivePredict): inside a subscript or slice, a list or pattern
// comprehension, a map value, a function argument, reduce() and CASE.
// reduce() INSIDE a subscript is absent on purpose: it fails for a reason
// #2918 does not touch (reduce() has no ATN production, so the subscript
// prediction rejects its `|`), on the base revision as much as after.
func TestSubscriptPropertyEveryClause2918(t *testing.T) {
	t.Parallel()
	queries := []string{
		"WITH [{w:1}] AS q RETURN q[0].w AS x",
		"WITH [{w:1}] AS q WHERE q[0].w = 1 RETURN 1",
		"WITH [{w:1}] AS q RETURN 1 ORDER BY q[0].w",
		"WITH [{w:1}] AS q RETURN q[0].w ORDER BY q[0].w DESC SKIP q[0].w LIMIT q[0].w",
		"WITH [{w:1}] AS q UNWIND q[0].w AS z RETURN z",
		"WITH [[{w:1}]] AS qs RETURN [rs IN qs | rs[0].w]",
		"WITH [[{w:1}]] AS qs RETURN [rs IN qs WHERE rs[0].w = 1 | rs[0].w]",
		"MATCH (a) RETURN [(a)-->(b) WHERE b.l[0].w = 1 | b.l[0].w]",
		"WITH [{w:1}] AS q RETURN CASE WHEN q[0].w = 1 THEN q[0].w ELSE q[0].a.b END",
		"WITH [{w:1}] AS q RETURN CASE q[0].w WHEN q[0].w THEN 2 END",
		"WITH [{w:1}] AS q, [1, 2] AS x RETURN x[q[0].w], x[q[0].w..2], x[..q[0].w]",
		"WITH [{w:1}] AS q RETURN {k: q[0].w, j: [q[0].w]}",
		"WITH [{w:1}] AS q RETURN count(q[0].w), abs(q[0].w), coalesce(q[0].w, q[1].w)",
		"WITH [{w:1}] AS q RETURN any(r IN q WHERE r.w = q[0].w)",
		"WITH [{w:1}] AS q RETURN reduce(s = 0, r IN q | s + q[0].w)",
		"WITH [{w:1}] AS q MATCH (n {k: q[0].w}) RETURN n",
		"WITH [{w:1}] AS q MATCH (n) WHERE n.k IN q[0].w RETURN n",
		"MATCH (n) WITH [n] AS q WHERE q[0]:Label RETURN q[0]:Label",
		"WITH [{w:1}] AS q CREATE (n {v: q[0].w}) SET n.u = q[0].w RETURN n",
		"WITH [{w:1}] AS q MERGE (n {v: q[0].w}) RETURN n",
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(q); err != nil {
				t.Errorf("Parse(%q): %v", q, err)
			}
		})
	}
}

// TestSubscriptPostfixRejections2918 pins what openCypher does NOT derive.
// A postfix operator applies only to a <postfix expression> (openCypher.bnf
// 677cbafa, lines 741-748); IS NULL (<null predicate part 2>, lines 703-704)
// and a label test (<is labeled predicate part 2>, lines 718-719) are
// comparison-predicate suffixes (lines 690-694), and nothing postfix can
// follow them. REMOVE and SET take a property chain on an atom, with no
// subscript (openCypher 9 oC_PropertyExpression: oC_Atom ( oC_PropertyLookup )+).
// The first two were accepted before #2918 — `x IS NULL[0]` as `x[0] IS NULL`.
func TestSubscriptPostfixRejections2918(t *testing.T) {
	t.Parallel()
	queries := []string{
		"RETURN x IS NULL[0]",
		"MATCH (n) RETURN n:L[0]",
		"WITH [{w:1}] AS q REMOVE q[0].w",
		"WITH [{w:1}] AS q SET q[0].w = 1",
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(q)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want a syntax error", q)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Errorf("Parse(%q) error = %T (%v), want *ParseError", q, err, err)
			}
		})
	}
}
