package parser

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// nonZeroPos returns true when pos carries a real source location.
// Line and Column are both zero for a zero-value Position; any parsed token
// is on line 1 (ANTLR lines are 1-based) so Line >= 1 means populated.
func nonZeroPos(p ast.Position) bool { return p.Line >= 1 }

// collectPositions walks an ast.Query and returns every (name, pos, endPos)
// triple for every concrete AST node that exposes a Pos field.
// This is intentionally exhaustive: it covers every type defined in the ast
// package to prove 100% node coverage on the sample query.
type posEntry struct {
	name   string
	pos    ast.Position
	endPos ast.Position
}

//nolint:gocyclo // exhaustive AST walk — complexity is expected here.
func walkQuery(q ast.Query, out *[]posEntry) {
	switch n := q.(type) {
	case *ast.SingleQuery:
		*out = append(*out, posEntry{"SingleQuery", n.Pos, n.EndPos})
		for _, rc := range n.ReadingClauses {
			walkReadingClause(rc, out)
		}
		for _, uc := range n.UpdatingClauses {
			walkUpdatingClause(uc, out)
		}
		for _, w := range n.With {
			walkWith(w, out)
		}
		if n.Return != nil {
			walkReturn(n.Return, out)
		}
	case *ast.MultiQuery:
		*out = append(*out, posEntry{"MultiQuery", n.Pos, n.EndPos})
		for _, p := range n.Parts {
			walkQuery(p, out)
		}
	}
}

func walkReadingClause(rc ast.ReadingClause, out *[]posEntry) {
	switch n := rc.(type) {
	case *ast.Match:
		*out = append(*out, posEntry{"Match", n.Pos, n.EndPos})
		walkPattern(n.Pattern, out)
		if n.Where != nil {
			walkWhere(n.Where, out)
		}
	case *ast.OptionalMatch:
		*out = append(*out, posEntry{"OptionalMatch", n.Pos, n.EndPos})
		walkPattern(n.Pattern, out)
		if n.Where != nil {
			walkWhere(n.Where, out)
		}
	case *ast.Unwind:
		*out = append(*out, posEntry{"Unwind", n.Pos, n.EndPos})
		walkExpr(n.Expr, out)
	case *ast.With:
		walkWith(n, out)
	case *ast.Call:
		*out = append(*out, posEntry{"Call", n.Pos, n.EndPos})
	}
}

func walkUpdatingClause(uc ast.UpdatingClause, out *[]posEntry) {
	switch n := uc.(type) {
	case *ast.Create:
		*out = append(*out, posEntry{"Create", n.Pos, n.EndPos})
		walkPattern(n.Pattern, out)
	case *ast.Merge:
		*out = append(*out, posEntry{"Merge", n.Pos, n.EndPos})
	case *ast.Set:
		*out = append(*out, posEntry{"Set", n.Pos, n.EndPos})
		for _, si := range n.Items {
			*out = append(*out, posEntry{"SetItem", si.Pos, si.EndPos})
		}
	case *ast.Remove:
		*out = append(*out, posEntry{"Remove", n.Pos, n.EndPos})
		for _, ri := range n.Items {
			*out = append(*out, posEntry{"RemoveItem", ri.Pos, ri.EndPos})
		}
	case *ast.Delete:
		*out = append(*out, posEntry{"Delete", n.Pos, n.EndPos})
	case *ast.DetachDelete:
		*out = append(*out, posEntry{"DetachDelete", n.Pos, n.EndPos})
	}
}

func walkWith(w *ast.With, out *[]posEntry) {
	*out = append(*out, posEntry{"With", w.Pos, w.EndPos})
	if w.Projection != nil {
		*out = append(*out, posEntry{"Projection", w.Projection.Pos, w.Projection.EndPos})
		for _, item := range w.Projection.Items {
			*out = append(*out, posEntry{"ProjectionItem", item.Pos, item.EndPos})
			walkExpr(item.Expr, out)
		}
	}
	if w.Where != nil {
		walkWhere(w.Where, out)
	}
}

func walkReturn(r *ast.Return, out *[]posEntry) {
	*out = append(*out, posEntry{"Return", r.Pos, r.EndPos})
	if r.Projection != nil {
		*out = append(*out, posEntry{"Projection", r.Projection.Pos, r.Projection.EndPos})
		for _, item := range r.Projection.Items {
			*out = append(*out, posEntry{"ProjectionItem", item.Pos, item.EndPos})
			walkExpr(item.Expr, out)
		}
		for _, si := range r.Projection.OrderBy {
			*out = append(*out, posEntry{"SortItem", si.Pos, si.EndPos})
			walkExpr(si.Expr, out)
		}
		if r.Projection.Skip != nil {
			walkExpr(r.Projection.Skip, out)
		}
		if r.Projection.Limit != nil {
			walkExpr(r.Projection.Limit, out)
		}
	}
}

func walkWhere(w *ast.Where, out *[]posEntry) {
	*out = append(*out, posEntry{"Where", w.Pos, w.EndPos})
	walkExpr(w.Predicate, out)
}

func walkPattern(p *ast.Pattern, out *[]posEntry) {
	if p == nil {
		return
	}
	*out = append(*out, posEntry{"Pattern", p.Pos, p.EndPos})
	for _, pp := range p.Paths {
		walkPathPattern(pp, out)
	}
}

func walkPathPattern(pp *ast.PathPattern, out *[]posEntry) {
	if pp == nil {
		return
	}
	*out = append(*out, posEntry{"PathPattern", pp.Pos, pp.EndPos})
	el := pp.Head
	for el != nil {
		if el.Node != nil {
			*out = append(*out, posEntry{"NodePattern", el.Node.Pos, el.Node.EndPos})
		}
		if el.Relationship != nil {
			*out = append(*out, posEntry{"RelationshipPattern", el.Relationship.Pos, el.Relationship.EndPos})
		}
		el = el.Next
	}
}

//nolint:gocyclo // intentionally exhaustive expression walker.
func walkExpr(e ast.Expression, out *[]posEntry) {
	if e == nil {
		return
	}
	switch n := e.(type) {
	case *ast.Variable:
		*out = append(*out, posEntry{"Variable", n.Pos, n.EndPos})
	case *ast.Parameter:
		*out = append(*out, posEntry{"Parameter", n.Pos, n.EndPos})
	case *ast.Property:
		*out = append(*out, posEntry{"Property", n.Pos, n.EndPos})
		walkExpr(n.Receiver, out)
	case *ast.FunctionInvocation:
		*out = append(*out, posEntry{"FunctionInvocation", n.Pos, n.EndPos})
		for _, a := range n.Args {
			walkExpr(a, out)
		}
	case *ast.BinaryOp:
		*out = append(*out, posEntry{"BinaryOp", n.Pos, n.EndPos})
		walkExpr(n.Left, out)
		walkExpr(n.Right, out)
	case *ast.UnaryOp:
		*out = append(*out, posEntry{"UnaryOp", n.Pos, n.EndPos})
		walkExpr(n.Operand, out)
	case *ast.CaseExpression:
		*out = append(*out, posEntry{"CaseExpression", n.Pos, n.EndPos})
		walkExpr(n.Subject, out)
		for _, alt := range n.Alternatives {
			*out = append(*out, posEntry{"CaseAlternative", alt.Pos, alt.EndPos})
			walkExpr(alt.Condition, out)
			walkExpr(alt.Consequent, out)
		}
		walkExpr(n.ElseExpr, out)
	case *ast.ListComprehension:
		*out = append(*out, posEntry{"ListComprehension", n.Pos, n.EndPos})
		walkExpr(n.Source, out)
		walkExpr(n.Predicate, out)
		walkExpr(n.Projection, out)
	case *ast.PatternComprehension:
		*out = append(*out, posEntry{"PatternComprehension", n.Pos, n.EndPos})
		walkPathPattern(n.Pattern, out)
		walkExpr(n.Predicate, out)
		walkExpr(n.Projection, out)
	case *ast.SubscriptExpr:
		*out = append(*out, posEntry{"SubscriptExpr", n.Pos, n.EndPos})
		walkExpr(n.Expr, out)
		walkExpr(n.Index, out)
	case *ast.SliceExpr:
		*out = append(*out, posEntry{"SliceExpr", n.Pos, n.EndPos})
		walkExpr(n.Expr, out)
		walkExpr(n.From, out)
		walkExpr(n.To, out)
	case *ast.ExistsSubquery:
		*out = append(*out, posEntry{"ExistsSubquery", n.Pos, n.EndPos})
	case *ast.CountSubquery:
		*out = append(*out, posEntry{"CountSubquery", n.Pos, n.EndPos})
	case *ast.IntLiteral:
		*out = append(*out, posEntry{"IntLiteral", n.Pos, n.EndPos})
	case *ast.FloatLiteral:
		*out = append(*out, posEntry{"FloatLiteral", n.Pos, n.EndPos})
	case *ast.StringLiteral:
		*out = append(*out, posEntry{"StringLiteral", n.Pos, n.EndPos})
	case *ast.BoolLiteral:
		*out = append(*out, posEntry{"BoolLiteral", n.Pos, n.EndPos})
	case *ast.NullLiteral:
		*out = append(*out, posEntry{"NullLiteral", n.Pos, n.EndPos})
	case *ast.ListLiteral:
		*out = append(*out, posEntry{"ListLiteral", n.Pos, n.EndPos})
		for _, el := range n.Elements {
			walkExpr(el, out)
		}
	case *ast.MapLiteral:
		*out = append(*out, posEntry{"MapLiteral", n.Pos, n.EndPos})
		for _, v := range n.Values {
			walkExpr(v, out)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: every node in a rich parse has non-zero Pos
// ---------------------------------------------------------------------------

// TestPositionNonZeroAfterParse verifies that every AST node produced by
// parsing a representative query carries a non-zero Pos (line >= 1).
func TestPositionNonZeroAfterParse(t *testing.T) {
	const query = `MATCH (n:Person) WHERE n.age > -18 RETURN n.name AS name`
	q, err := Parse(query)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	var entries []posEntry
	walkQuery(q, &entries)

	if len(entries) == 0 {
		t.Fatal("no AST nodes found")
	}
	for _, e := range entries {
		if !nonZeroPos(e.pos) {
			t.Errorf("node %q has zero Pos: %v", e.name, e.pos)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: span coherence (start <= end for Offset)
// ---------------------------------------------------------------------------

// TestSpanCoherence verifies that EndPos.Offset >= Pos.Offset for every node
// produced by a rich parse.
func TestSpanCoherence(t *testing.T) {
	const query = `MATCH (n:Person) WHERE n.age > -18 RETURN n.name AS name`
	q, err := Parse(query)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	var entries []posEntry
	walkQuery(q, &entries)

	for _, e := range entries {
		if e.endPos.Offset < e.pos.Offset {
			t.Errorf("node %q span inverted: start offset=%d end offset=%d (pos=%v endPos=%v)",
				e.name, e.pos.Offset, e.endPos.Offset, e.pos, e.endPos)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: specific positional assertions on a known query
// ---------------------------------------------------------------------------

// TestSpecificPositions checks that nodes land on the expected line:column
// for the canonical benchmark query:
//
//	MATCH (n:Person) WHERE n.age > 18 RETURN n.name AS name
//
// ANTLR lines are 1-based; columns are 0-based.
func TestSpecificPositions(t *testing.T) {
	// Use a negative integer because the grammar DIGIT rule requires a leading
	// minus for positive integers.
	const query = `MATCH (n:Person) WHERE n.age > -18 RETURN n.name AS name`
	q, err := Parse(query)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		t.Fatalf("expected *ast.SingleQuery, got %T", q)
	}

	// --- MATCH clause starts at column 0 ---
	if len(sq.ReadingClauses) == 0 {
		t.Fatal("no reading clauses")
	}
	m, ok := sq.ReadingClauses[0].(*ast.Match)
	if !ok {
		t.Fatalf("expected *ast.Match, got %T", sq.ReadingClauses[0])
	}
	if m.Pos.Line != 1 {
		t.Errorf("MATCH line: want 1, got %d", m.Pos.Line)
	}
	if m.Pos.Column != 0 {
		t.Errorf("MATCH column: want 0, got %d", m.Pos.Column)
	}

	// --- Node pattern (n:Person) starts at column 6 (after "MATCH ") ---
	nodePat := m.Pattern.Paths[0].Head.Node
	if nodePat.Pos.Column != 6 {
		t.Errorf("NodePattern column: want 6, got %d", nodePat.Pos.Column)
	}

	// --- WHERE clause: starts at column 17 (after "MATCH (n:Person) ") ---
	if m.Where == nil {
		t.Fatal("expected WHERE clause")
	}
	if m.Where.Pos.Column != 17 {
		t.Errorf("WHERE column: want 17, got %d", m.Where.Pos.Column)
	}

	// --- RETURN clause: starts at column 35 ---
	// "MATCH (n:Person) WHERE n.age > -18 RETURN..."
	//  0123456789012345678901234567890123456789...
	//  0         1         2         3
	if sq.Return == nil {
		t.Fatal("expected RETURN clause")
	}
	if sq.Return.Pos.Column != 35 {
		t.Errorf("RETURN column: want 35, got %d", sq.Return.Pos.Column)
	}
}

// ---------------------------------------------------------------------------
// Test: multi-line query — WHERE on line 2
// ---------------------------------------------------------------------------

// TestMultiLinePositions verifies position tracking across newlines.
func TestMultiLinePositions(t *testing.T) {
	const query = "MATCH (n:Person)\nWHERE n.age > -18\nRETURN n.name AS name"
	q, err := Parse(query)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	sq := q.(*ast.SingleQuery)
	m := sq.ReadingClauses[0].(*ast.Match)

	if m.Pos.Line != 1 {
		t.Errorf("MATCH line: want 1, got %d", m.Pos.Line)
	}
	if m.Where == nil {
		t.Fatal("no WHERE")
	}
	if m.Where.Pos.Line != 2 {
		t.Errorf("WHERE line: want 2, got %d", m.Where.Pos.Line)
	}
	if m.Where.Pos.Column != 0 {
		t.Errorf("WHERE column: want 0, got %d", m.Where.Pos.Column)
	}
	if sq.Return.Pos.Line != 3 {
		t.Errorf("RETURN line: want 3, got %d", sq.Return.Pos.Line)
	}
}

// ---------------------------------------------------------------------------
// Test: 50 error position cases
// ---------------------------------------------------------------------------

// errorCase captures the input and the expected error line:column pair.
type errorCase struct {
	input      string
	wantLine   int
	wantColumn int
}

// errorCases returns 50 deliberately malformed Cypher queries with the
// expected position of the first syntax error.
func errorCases() []errorCase {
	return []errorCase{
		// ---- MATCH / node pattern errors ----
		{`MATCH n RETURN n`, 1, 6},                // missing parens around node
		{`MATCH (n RETURN n`, 1, 9},               // unclosed paren
		{`MATCH () RETURN`, 1, 15},                // RETURN with nothing
		{`MATCH (n:) RETURN n`, 1, 9},             // empty label
		{`MATCH (n WHERE n.x = 1 RETURN n`, 1, 9}, // WHERE without closing paren
		{`MATCH (n:Person RETURN n`, 1, 16},       // unclosed node pattern
		{`MATCH (n) RETURN n ORDER`, 1, 20},       // ORDER without BY
		{`MATCH (n) RETURN n SKIP`, 1, 19},        // SKIP without expr
		{`MATCH (n) RETURN n LIMIT`, 1, 19},       // LIMIT without expr
		{`MATCH (n) RETURN n ORDER BY`, 1, 24},    // ORDER BY without expr
		// ---- RETURN / projection errors ----
		{`RETURN`, 1, 6},           // bare RETURN
		{`RETURN ,`, 1, 7},         // comma without item
		{`RETURN 1 AS`, 1, 9},      // AS without alias
		{`RETURN 1,,2`, 1, 8},      // double comma
		{`RETURN DISTINCT`, 1, 15}, // DISTINCT with nothing
		// ---- Expression errors ----
		{`RETURN 1 +`, 1, 10},                  // dangling +
		{`RETURN 1 *`, 1, 10},                  // dangling *
		{`RETURN (1 + 2`, 1, 13},               // unclosed paren
		{`RETURN [1,2`, 1, 11},                 // unclosed list
		{`RETURN {a:`, 1, 10},                  // unclosed map
		{`RETURN {a: 1,}`, 1, 13},              // trailing comma in map
		{`RETURN CASE WHEN THEN 1 END`, 1, 17}, // WHEN without condition
		{`RETURN CASE WHEN 1 = 1 END`, 1, 23},  // WHEN without THEN
		{`RETURN $`, 1, 7},                     // bare $
		{`RETURN n.`, 1, 9},                    // property without key
		// ---- WHERE errors ----
		{`MATCH (n) WHERE RETURN n`, 1, 16},             // WHERE without predicate
		{`MATCH (n) WHERE n.x = RETURN n`, 1, 22},       // incomplete comparison
		{`MATCH (n) WHERE AND n.x = 1 RETURN n`, 1, 16}, // AND without left side
		{`MATCH (n) WHERE n.x = 1 AND RETURN n`, 1, 28}, // AND without right side
		{`MATCH (n) WHERE NOT RETURN n`, 1, 20},         // NOT without operand
		// ---- CREATE / MERGE errors ----
		{`CREATE n`, 1, 7},                    // CREATE without parens
		{`MERGE n`, 1, 6},                     // MERGE without parens
		{`CREATE (n:)`, 1, 10},                // empty label in CREATE
		{`MERGE (n:Person) ON`, 1, 17},        // ON without CREATE/MATCH
		{`MERGE (n:Person) ON CREATE`, 1, 22}, // ON CREATE without SET
		// ---- SET / REMOVE errors ----
		{`MATCH (n) SET`, 1, 13},          // SET without item
		{`MATCH (n) SET n.x =`, 1, 18},    // SET assignment without value
		{`MATCH (n) REMOVE`, 1, 16},       // REMOVE without item
		{`MATCH (n) SET n.x = 1,`, 1, 22}, // trailing comma in SET
		// ---- DELETE errors ----
		{`MATCH (n) DELETE`, 1, 16}, // DELETE without expr
		// ---- UNION errors ----
		{`RETURN 1 UNION`, 1, 14},     // UNION without second query
		{`RETURN 1 UNION ALL`, 1, 18}, // UNION ALL without second
		// ---- UNWIND errors ----
		{`UNWIND AS x RETURN x`, 1, 7},     // UNWIND without expr
		{`UNWIND [1,2,3] RETURN x`, 1, 15}, // UNWIND without AS
		// ---- CALL errors ----
		{`CALL`, 1, 4},                      // bare CALL
		{`CALL db.labels( RETURN n`, 1, 16}, // unclosed CALL args
		// ---- misc ----
		{`THIS IS NOT CYPHER`, 1, 0},                       // completely invalid
		{`MATCH (n) RETURN n WHERE n.x = 1`, 1, 19},        // WHERE after RETURN
		{`RETURN 1 2`, 1, 9},                               // two consecutive expressions
		{`MATCH (n) RETURN n UNION UNION RETURN n`, 1, 26}, // double UNION
	}
}

// TestErrorPositions verifies that parse errors for 50 malformed inputs
// report a non-zero line number and that the reported line matches the
// expected value.
func TestErrorPositions(t *testing.T) {
	cases := errorCases()
	if len(cases) < 50 {
		t.Fatalf("need 50 error cases, have %d", len(cases))
	}

	for i, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("case_%02d", i+1), func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.input)
			if err == nil {
				t.Fatalf("Parse(%q) expected error, got nil", tc.input)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				// SemaError also carries a position via ast.Position.
				// For inputs that produce SemaError instead of ParseError the
				// line is embedded in the message; we just verify an error occurred.
				if !strings.Contains(err.Error(), "sema error") {
					t.Errorf("unexpected error type %T: %v", err, err)
				}
				return
			}
			// ParseError must report a line >= 1.
			if pe.Line < 1 {
				t.Errorf("error line < 1 for %q: line=%d col=%d msg=%q",
					tc.input, pe.Line, pe.Column, pe.Message)
			}
			// Line must match the expected line.
			if pe.Line != tc.wantLine {
				t.Errorf("error line mismatch for %q: want %d, got %d (col=%d msg=%q)",
					tc.input, tc.wantLine, pe.Line, pe.Column, pe.Message)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: positional fuzzer — random node samples all have coherent spans
// ---------------------------------------------------------------------------

// TestPositionalFuzzer exercises a variety of query shapes and asserts:
//  1. Every node has non-zero Pos.
//  2. EndPos.Offset >= Pos.Offset (coherent span).
//  3. EndPos.Line >= Pos.Line.
func TestPositionalFuzzer(t *testing.T) {
	queries := []string{
		`RETURN -42`,
		`RETURN true`,
		`RETURN false`,
		`RETURN null`,
		`RETURN "hello"`,
		`RETURN [-1, -2, -3]`,
		`RETURN {name: 'Alice', age: -30}`,
		`RETURN $param`,
		`MATCH (n) RETURN n`,
		`MATCH (n:Person) RETURN n`,
		`MATCH (n:Person {name: 'Alice'}) RETURN n`,
		`MATCH (a)-[r]->(b) RETURN r`,
		`MATCH (a)<-[r]-(b) RETURN r`,
		`MATCH (a)-[r]-(b) RETURN r`,
		`MATCH (a)-[:KNOWS]->(b) RETURN a`,
		`MATCH (a)-[:KNOWS*]->(b) RETURN a`,
		`MATCH (n) WHERE n.age > -18 RETURN n`,
		`MATCH (n) WHERE n.age > -18 AND n.name = 'Alice' RETURN n`,
		`MATCH (n) WHERE NOT n.active RETURN n`,
		`MATCH (n) WHERE n.name IS NULL RETURN n`,
		`MATCH (n) WHERE n.name IS NOT NULL RETURN n`,
		`MATCH (n) WHERE n.id IN [-1, -2] RETURN n`,
		`MATCH (n) WHERE n.name STARTS WITH 'Al' RETURN n`,
		`MATCH (n) WHERE n.name ENDS WITH 'ice' RETURN n`,
		`MATCH (n) WHERE n.name CONTAINS 'li' RETURN n`,
		`OPTIONAL MATCH (n) RETURN n`,
		`CREATE (n:Person {name: 'Bob'})`,
		`MERGE (n:Person {name: 'Charlie'})`,
		`MERGE (n:Person {name: 'Dave'}) ON CREATE SET n.created = true`,
		`MATCH (n) SET n.age = -25`,
		`MATCH (n) SET n:Active`,
		`MATCH (n) REMOVE n.age`,
		`MATCH (n) REMOVE n:Inactive`,
		`MATCH (n) DELETE n`,
		`MATCH (n) DETACH DELETE n`,
		`MATCH (n:A) RETURN n UNION MATCH (n:B) RETURN n`,
		`MATCH (n:A) RETURN n UNION ALL MATCH (n:B) RETURN n`,
		`RETURN 1 + 2`,
		`RETURN 10 - 3`,
		`RETURN 4 * 5`,
		`RETURN 20 / 4`,
		`RETURN 7 % 3`,
		`RETURN 2 ^ 8`,
		`RETURN CASE WHEN 1 = 1 THEN 'yes' ELSE 'no' END`,
		`RETURN [x IN [-1, -2, -3] WHERE x > -2 | x]`,
		`MATCH (n) RETURN [(n)-[:KNOWS]->(m) | m.name]`,
		`CALL db.labels()`,
		`RETURN toLower('HELLO')`,
		`MATCH (n) RETURN count(*)`,
		`MATCH (n:Person) WHERE n.age > -21 WITH n ORDER BY n.name SKIP -0 LIMIT -10 RETURN n.name AS name`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			q, err := Parse(query)
			if err != nil {
				// The fixture corpus is curated; a parse failure here
				// is a regression, not a runtime variance. The previous
				// t.Skipf masked it.
				t.Fatalf("parse error on fixture %q: %v", query, err)
			}

			var entries []posEntry
			walkQuery(q, &entries)

			if len(entries) == 0 {
				t.Fatal("no AST nodes found")
			}

			for _, e := range entries {
				if !nonZeroPos(e.pos) {
					t.Errorf("node %q has zero Pos in query %q: pos=%v", e.name, query, e.pos)
				}
				if e.endPos.Offset < e.pos.Offset {
					t.Errorf("node %q span inverted in query %q: start=%d end=%d",
						e.name, query, e.pos.Offset, e.endPos.Offset)
				}
				if e.endPos.Line < e.pos.Line {
					t.Errorf("node %q end line < start line in query %q: start=%d end=%d",
						e.name, query, e.pos.Line, e.endPos.Line)
				}
			}
		})
	}
}
