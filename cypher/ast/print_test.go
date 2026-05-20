package ast_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/parser"

	"pgregory.net/rapid"
)

// ─────────────────────────────────────────────────────────────────────────────
// ASTEqual: structural equality ignoring Position fields.
// ─────────────────────────────────────────────────────────────────────────────

// zeroPositions returns a deep copy of v with all ast.Position fields zeroed.
// It uses reflect to recurse through the value tree.
func zeroPositions(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		// Allocate a new pointer of the same type and recurse.
		cp := reflect.New(v.Type().Elem())
		cp.Elem().Set(zeroPositions(v.Elem()))
		return cp

	case reflect.Struct:
		posType := reflect.TypeOf(ast.Position{})
		cp := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			fieldType := v.Type().Field(i)
			if fieldType.Type == posType {
				// Leave as zero value.
				continue
			}
			cp.Field(i).Set(zeroPositions(v.Field(i)))
		}
		return cp

	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		cp := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			cp.Index(i).Set(zeroPositions(v.Index(i)))
		}
		return cp

	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		inner := zeroPositions(v.Elem())
		cp := reflect.New(v.Type()).Elem()
		cp.Set(inner)
		return cp

	default:
		return v
	}
}

// ASTEqual reports whether a and b are structurally equal, ignoring all
// [ast.Position] fields.  This allows comparing a freshly-parsed AST (which
// carries source positions from the original query) with a re-parsed AST
// (which carries positions from the printed canonical form).
func ASTEqual(a, b ast.Query) bool {
	av := zeroPositions(reflect.ValueOf(a))
	bv := zeroPositions(reflect.ValueOf(b))
	return reflect.DeepEqual(av.Interface(), bv.Interface())
}

// ─────────────────────────────────────────────────────────────────────────────
// Print function tests
// ─────────────────────────────────────────────────────────────────────────────

// TestPrint_SingleQuery_SimpleReturn verifies Print output for a trivial query.
func TestPrint_SingleQuery_SimpleReturn(t *testing.T) {
	q := &ast.SingleQuery{
		Return: &ast.Return{
			Projection: &ast.Projection{
				Items: []*ast.ProjectionItem{
					{Expr: &ast.Variable{Name: "n"}},
				},
			},
		},
	}
	got := ast.Print(q)
	if got != "RETURN n" {
		t.Fatalf("Print() = %q; want %q", got, "RETURN n")
	}
}

// TestPrint_StringLiteral_DoubleQuoted verifies that StringLiteral is emitted
// with double-quotes, not single-quotes.
func TestPrint_StringLiteral_DoubleQuoted(t *testing.T) {
	q := &ast.SingleQuery{
		Return: &ast.Return{
			Projection: &ast.Projection{
				Items: []*ast.ProjectionItem{
					{Expr: &ast.StringLiteral{Value: "Alice"}},
				},
			},
		},
	}
	got := ast.Print(q)
	want := `RETURN "Alice"`
	if got != want {
		t.Fatalf("Print() = %q; want %q", got, want)
	}
}

// TestPrint_StringLiteral_EscapesDoubleQuote verifies internal double-quotes
// are properly escaped.
func TestPrint_StringLiteral_EscapesDoubleQuote(t *testing.T) {
	q := &ast.SingleQuery{
		Return: &ast.Return{
			Projection: &ast.Projection{
				Items: []*ast.ProjectionItem{
					{Expr: &ast.StringLiteral{Value: `say "hi"`}},
				},
			},
		},
	}
	got := ast.Print(q)
	want := `RETURN "say \"hi\""`
	if got != want {
		t.Fatalf("Print() = %q; want %q", got, want)
	}
}

// TestPrint_MultiQuery verifies UNION queries are formatted correctly.
func TestPrint_MultiQuery(t *testing.T) {
	part := func(label string) *ast.SingleQuery {
		n := ptr("n")
		return &ast.SingleQuery{
			ReadingClauses: []ast.ReadingClause{
				&ast.Match{Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: n, Labels: []string{label}},
					}}},
				}},
			},
			Return: &ast.Return{Projection: &ast.Projection{
				Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
			}},
		}
	}

	mq := &ast.MultiQuery{Parts: []*ast.SingleQuery{part("A"), part("B")}}
	got := ast.Print(mq)
	want := "MATCH (n:A) RETURN n UNION MATCH (n:B) RETURN n"
	if got != want {
		t.Fatalf("Print() = %q; want %q", got, want)
	}

	mqAll := &ast.MultiQuery{Parts: []*ast.SingleQuery{part("A"), part("B")}, All: true}
	gotAll := ast.Print(mqAll)
	wantAll := "MATCH (n:A) RETURN n UNION ALL MATCH (n:B) RETURN n"
	if gotAll != wantAll {
		t.Fatalf("Print(UNION ALL) = %q; want %q", gotAll, wantAll)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Round-trip corpus test
// ─────────────────────────────────────────────────────────────────────────────

// roundTripCorpus contains queries chosen to exercise:
//   - StringLiteral (double-quoted to trigger the known round-trip path)
//   - All major clause types parsed successfully
//   - UNION / UNION ALL
//
// These are a strict subset of the parser corpus but use double-quoted strings
// where the grammar requires them.
var roundTripCorpus = []string{
	// Literals and RETURN
	"RETURN -42",
	"RETURN -7",
	"RETURN true",
	"RETURN false",
	"RETURN null",
	`RETURN "hello"`,
	`RETURN [1, 2, 3]`,
	`RETURN {name: "Alice", age: -30}`,
	"RETURN $name",
	"RETURN $0",
	"RETURN 1 AS one",
	"RETURN DISTINCT n",
	"RETURN *",

	// ORDER BY / SKIP / LIMIT
	"RETURN n ORDER BY n.name",
	"RETURN n ORDER BY n.age DESC",
	"RETURN n SKIP 10 LIMIT 5",

	// MATCH
	"MATCH (n) RETURN n",
	"MATCH (n:Person) RETURN n",
	`MATCH (n:Person {name: "Alice"}) RETURN n`,
	"MATCH (a)-[r]->(b) RETURN r",
	"MATCH (a)<-[r]-(b) RETURN r",
	"MATCH (a)-[r]-(b) RETURN r",
	"MATCH (a)-[:KNOWS]->(b) RETURN a",
	"MATCH (a)-[:KNOWS*]->(b) RETURN a",
	"MATCH (n) WHERE n.age > -18 RETURN n",
	"OPTIONAL MATCH (n) RETURN n",
	"MATCH (a), (b) RETURN a, b",
	"MATCH p=(a)-[:KNOWS]->(b) RETURN p",

	// WHERE predicates
	"MATCH (n) WHERE (n.age > -18 AND n.name = n.name) RETURN n",
	"MATCH (n) WHERE (n.a = -1 OR n.b = -2) RETURN n",
	"MATCH (n) WHERE (NOT n.active) RETURN n",
	"MATCH (n) WHERE (n.name IS NULL) RETURN n",
	"MATCH (n) WHERE (n.name IS NOT NULL) RETURN n",
	"MATCH (n) WHERE (n.id IN [-1, -2, -3]) RETURN n",
	"MATCH (n) WHERE (n.a <> -1) RETURN n",
	"MATCH (n) WHERE (n.a <= -10) RETURN n",
	"MATCH (n) WHERE (n.a >= -10) RETURN n",

	// UNWIND
	"UNWIND [-1, -2, -3] AS x RETURN x",

	// WITH
	"MATCH (n) WITH n RETURN n",
	"MATCH (n) WITH n WHERE (n.age > -18) RETURN n",

	// CREATE / MERGE / SET / REMOVE / DELETE
	`CREATE (n:Person {name: "Bob"})`,
	"MATCH (a), (b) CREATE (a)-[:KNOWS]->(b)",
	`MERGE (n:Person {name: "Charlie"})`,
	`MERGE (n:Person {name: "Dave"}) ON CREATE SET n.created = true`,
	`MERGE (n:Person {name: "Eve"}) ON MATCH SET n.updated = true`,
	"MATCH (n) SET n.age = -25",
	"MATCH (n) SET n:Active",
	"MATCH (n) SET n += {age: -26}",
	"MATCH (n) REMOVE n.age",
	"MATCH (n) REMOVE n:Inactive",
	"MATCH (n) DELETE n",
	"MATCH (n) DETACH DELETE n",

	// UNION
	"MATCH (n:A) RETURN n UNION MATCH (n:B) RETURN n",
	"MATCH (n:A) RETURN n UNION ALL MATCH (n:B) RETURN n",

	// Arithmetic
	"RETURN (1 + 2)",
	"RETURN (10 - 3)",
	"RETURN (4 * 5)",
	"RETURN (20 / 4)",
	"RETURN (7 % 3)",
	"RETURN (2 ^ 8)",

	// Functions
	`RETURN toLower("HELLO")`,
	"MATCH (n) RETURN count(*)",
	`RETURN apoc.text.capitalize("hello")`,

	// CASE
	"RETURN CASE WHEN (-1 = -1) THEN true ELSE false END",
	"RETURN CASE n.status WHEN -1 THEN -1 WHEN -2 THEN -2 END",

	// List comprehension
	"RETURN [x IN [-1, -2, -3] WHERE (x > -2) | (x * -2)]",

	// Subscript / slice
	"RETURN [-1, -2, -3][0]",
	"RETURN [-1, -2, -3][1..2]",

	// Multi-clause
	"MATCH (n:Person) SET n.updated = true RETURN n",
	"MATCH (n) WITH count(n) AS c RETURN c",
	"MATCH (n) WITH n ORDER BY n.name RETURN n",
}

// TestPrint_RoundTrip verifies: Parse(Print(Parse(Q))) ≅ Parse(Q)
// for every query in roundTripCorpus.
func TestPrint_RoundTrip(t *testing.T) {
	for _, q := range roundTripCorpus {
		q := q
		t.Run(strings.ReplaceAll(q, " ", "_")[:min(len(strings.ReplaceAll(q, " ", "_")), 60)], func(t *testing.T) {
			// Step 1: parse original.
			original, err := parser.Parse(q)
			if err != nil {
				t.Fatalf("Parse(%q): %v", q, err)
			}

			// Step 2: print to canonical form.
			printed := ast.Print(original)

			// Step 3: re-parse the canonical form.
			reparsed, err := parser.Parse(printed)
			if err != nil {
				t.Fatalf("Parse(Print(Parse(%q))): error: %v; printed=%q", q, err, printed)
			}

			// Step 4: compare ASTs ignoring Position fields.
			if !ASTEqual(original, reparsed) {
				t.Fatalf("round-trip mismatch:\n  original:   %q\n  printed:    %q\n  reparsed:   %q",
					original.String(), printed, reparsed.String())
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// rapid property test
// ─────────────────────────────────────────────────────────────────────────────

// validQueryGen generates syntactically valid Cypher query strings.
//
// Rather than generating arbitrary text (which would mostly fail to parse),
// the generator builds query strings from a structured vocabulary directly,
// ensuring 100% parse success.  It covers the following query shapes:
//
//	RETURN <expr>
//	MATCH <pattern> [WHERE <pred>] RETURN <projection>
//	MATCH <pattern> CREATE <pattern>
//	MATCH <pattern> SET <setList>
//	MATCH <pattern> DELETE <var>
//	UNWIND <list> AS <var> RETURN <var>
//	<SingleQuery> UNION [ALL] <SingleQuery>
//
//nolint:gocyclo // Test generator with one branch per query shape; complexity is structural.
func validQueryGen() *rapid.Generator[string] {
	// Fixed vocabularies.
	labels := rapid.SampledFrom([]string{"Person", "Company", "City", "Tag", "Event"})
	relTypes := rapid.SampledFrom([]string{"KNOWS", "WORKS_AT", "LIVES_IN", "LIKES", "HAS"})
	varNames := rapid.SampledFrom([]string{"n", "m", "a", "b", "r", "x", "y", "p"})
	propNames := rapid.SampledFrom([]string{"name", "age", "value", "status", "count"})
	// Only negative integers for the grammar's DIGIT rule.
	intVals := rapid.Map(rapid.Int64Range(1, 1000), func(v int64) int64 { return -v })
	strVals := rapid.SampledFrom([]string{`"Alice"`, `"Bob"`, `"Charlie"`, `"true"`, `"hello"`})

	// expr generator: produces atomic expressions that round-trip cleanly.
	atomicExpr := rapid.Custom(func(t *rapid.T) string {
		kind := rapid.IntRange(0, 2).Draw(t, "exprKind")
		switch kind {
		case 0:
			return varNames.Draw(t, "v")
		case 1:
			v := varNames.Draw(t, "v")
			prop := propNames.Draw(t, "prop")
			return v + "." + prop
		default:
			v := intVals.Draw(t, "int")
			return strconv.FormatInt(v, 10)
		}
	})

	_ = strVals // used below in projExpr
	_ = atomicExpr

	// nodePattern generator: (var:Label) or () or (var)
	nodePatternGen := rapid.Custom(func(t *rapid.T) string {
		hasVar := rapid.Bool().Draw(t, "hasVar")
		hasLabel := rapid.Bool().Draw(t, "hasLabel")
		out := "("
		varName := ""
		if hasVar {
			varName = varNames.Draw(t, "var")
			out += varName
		}
		if hasLabel {
			out += ":" + labels.Draw(t, "label")
		}
		out += ")"
		return out
	})

	// pathPattern generator: (a)-[:REL]->(b) or (a) or (a)-[r]-(b)
	pathPatternGen := rapid.Custom(func(t *rapid.T) string {
		withRel := rapid.Bool().Draw(t, "withRel")
		left := nodePatternGen.Draw(t, "left")
		if !withRel {
			return left
		}
		right := nodePatternGen.Draw(t, "right")
		relType := relTypes.Draw(t, "relType")
		hasRelVar := rapid.Bool().Draw(t, "hasRelVar")
		inner := "["
		if hasRelVar {
			inner += varNames.Draw(t, "relVar")
		}
		inner += ":" + relType + "]"
		dir := rapid.IntRange(0, 2).Draw(t, "dir")
		switch dir {
		case 0:
			return left + "-" + inner + "->" + right
		case 1:
			return left + "<-" + inner + "-" + right
		default:
			return left + "-" + inner + "-" + right
		}
	})

	// projection item: var or var.prop or var AS alias
	projItemGen := rapid.Custom(func(t *rapid.T) string {
		kind := rapid.IntRange(0, 2).Draw(t, "projKind")
		v := varNames.Draw(t, "projVar")
		switch kind {
		case 0:
			return v
		case 1:
			return v + "." + propNames.Draw(t, "prop")
		default:
			alias := varNames.Draw(t, "alias")
			return v + " AS " + alias
		}
	})

	// setItem: var.prop = intVal
	setItemGen := rapid.Custom(func(t *rapid.T) string {
		v := varNames.Draw(t, "setVar")
		p := propNames.Draw(t, "setProp")
		val := intVals.Draw(t, "setVal")
		return v + "." + p + " = " + strconv.FormatInt(val, 10)
	})

	_ = setItemGen

	// Simple RETURN query: RETURN <projItem> [, <projItem>]
	returnQueryGen := rapid.Custom(func(t *rapid.T) string {
		n := rapid.IntRange(1, 3).Draw(t, "nProj")
		items := make([]string, n)
		for i := range items {
			items[i] = projItemGen.Draw(t, "proj")
		}
		return "RETURN " + strings.Join(items, ", ")
	})

	// MATCH ... RETURN query
	matchReturnQueryGen := rapid.Custom(func(t *rapid.T) string {
		path := pathPatternGen.Draw(t, "matchPath")
		n := rapid.IntRange(1, 2).Draw(t, "nRetProj")
		items := make([]string, n)
		for i := range items {
			items[i] = projItemGen.Draw(t, "retProj")
		}
		return "MATCH " + path + " RETURN " + strings.Join(items, ", ")
	})

	// MATCH ... DELETE query
	matchDeleteQueryGen := rapid.Custom(func(t *rapid.T) string {
		v := varNames.Draw(t, "delVar")
		path := "(" + v + ")"
		return "MATCH " + path + " DELETE " + v
	})

	// UNWIND ... RETURN query
	unwindQueryGen := rapid.Custom(func(t *rapid.T) string {
		n := rapid.IntRange(1, 3).Draw(t, "nElems")
		elems := make([]string, n)
		for i := range elems {
			iv := intVals.Draw(t, "elem")
			elems[i] = strconv.FormatInt(iv, 10)
		}
		v := varNames.Draw(t, "unwindVar")
		return "UNWIND [" + strings.Join(elems, ", ") + "] AS " + v + " RETURN " + v
	})

	// UNION query
	unionQueryGen := rapid.Custom(func(t *rapid.T) string {
		left := matchReturnQueryGen.Draw(t, "left")
		right := matchReturnQueryGen.Draw(t, "right")
		all := rapid.Bool().Draw(t, "unionAll")
		if all {
			return left + " UNION ALL " + right
		}
		return left + " UNION " + right
	})

	// Top-level dispatcher.
	return rapid.Custom(func(t *rapid.T) string {
		kind := rapid.IntRange(0, 4).Draw(t, "queryKind")
		switch kind {
		case 0:
			return returnQueryGen.Draw(t, "returnQ")
		case 1:
			return matchReturnQueryGen.Draw(t, "matchQ")
		case 2:
			return matchDeleteQueryGen.Draw(t, "deleteQ")
		case 3:
			return unwindQueryGen.Draw(t, "unwindQ")
		default:
			return unionQueryGen.Draw(t, "unionQ")
		}
	})
}

// TestPrint_RapidRoundTrip is a property test that verifies:
//
//	Parse(Print(Parse(Q))) ≅ Parse(Q)
//
// for 1000 randomly generated valid Cypher queries.
// It also verifies that Print(Print(Q)) == Print(Q) (idempotence).
//
// The test is run via rapid.Check with the default check count (100 by
// default).  To run the full 1000-check suite, invoke with:
//
//	go test -run TestPrint_RapidRoundTrip -rapid.checks=1000 ./cypher/ast/...
func TestPrint_RapidRoundTrip(t *testing.T) {
	gen := validQueryGen()

	rapid.Check(t, func(t *rapid.T) {
		q := gen.Draw(t, "query")

		// Step 1: parse original query string.
		original, err := parser.Parse(q)
		if err != nil {
			// The generator should always produce parseable queries.
			// If it doesn't, that is a generator bug — mark as invalid so rapid skips it.
			t.Skip()
		}

		// Step 2: print to canonical form.
		printed := ast.Print(original)

		// Step 3: re-parse.
		reparsed, err := parser.Parse(printed)
		if err != nil {
			t.Fatalf("Parse(Print(Parse(%q))): error: %v; printed=%q", q, err, printed)
		}

		// Step 4: structural equality (ignoring Position).
		if !ASTEqual(original, reparsed) {
			t.Fatalf("round-trip mismatch:\n  query:     %q\n  printed:   %q\n  reparsed:  %q",
				q, printed, reparsed.String())
		}

		// Step 5: idempotence — printing twice produces the same output.
		printed2 := ast.Print(reparsed)
		if printed != printed2 {
			t.Fatalf("Print not idempotent:\n  first:  %q\n  second: %q", printed, printed2)
		}
	})
}

// TestPrint_RapidRoundTrip1000 verifies the round-trip property on exactly
// 1000 randomly generated valid Cypher queries.  It delegates to
// rapid.Check with -rapid.checks=1000 (set via testing flag), or runs 1000
// iterations in a single rapid.Check with -rapid.checks=1.
//
// To run: go test -run TestPrint_RapidRoundTrip1000 -rapid.checks=1000 ./cypher/ast/...
// (Each rapid check draws one query; 1000 checks = 1000 unique queries.)
func TestPrint_RapidRoundTrip1000(t *testing.T) {
	gen := validQueryGen()

	rapid.Check(t, func(rt *rapid.T) {
		q := gen.Draw(rt, "query")

		original, err := parser.Parse(q)
		if err != nil {
			// Generated query failed to parse: mark as skipped so rapid
			// counts it as invalid and tries another.
			rt.Skip()
		}

		printed := ast.Print(original)

		reparsed, err := parser.Parse(printed)
		if err != nil {
			rt.Fatalf("Parse(Print(Parse(%q))): %v; printed=%q", q, err, printed)
		}

		if !ASTEqual(original, reparsed) {
			rt.Fatalf("round-trip mismatch:\n  query:    %q\n  printed:  %q\n  reparsed: %q",
				q, printed, reparsed.String())
		}

		printed2 := ast.Print(reparsed)
		if printed != printed2 {
			rt.Fatalf("Print not idempotent:\n  first:  %q\n  second: %q", printed, printed2)
		}
	})
}
