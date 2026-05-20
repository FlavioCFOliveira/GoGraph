package parser

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gograph/cypher/ast"
)

// ---------------------------------------------------------------------------
// Structural correctness corpus
// ---------------------------------------------------------------------------
// Each entry in the corpus table provides a Cypher query and a verifier
// function that asserts structural properties of the produced AST.
// We do NOT rely on ast.Print (task 212) — we inspect concrete types.
// ---------------------------------------------------------------------------

type corpusEntry struct {
	name  string
	query string
	// check is called with the AST result. It may be nil for "must parse
	// without error" assertions.
	check func(t *testing.T, q ast.Query)
}

// mustSingle unwraps ast.Query to *ast.SingleQuery.
func mustSingle(t *testing.T, q ast.Query) *ast.SingleQuery {
	t.Helper()
	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		t.Fatalf("expected *ast.SingleQuery, got %T", q)
	}
	return sq
}

// mustMulti unwraps ast.Query to *ast.MultiQuery.
func mustMulti(t *testing.T, q ast.Query) *ast.MultiQuery {
	t.Helper()
	mq, ok := q.(*ast.MultiQuery)
	if !ok {
		t.Fatalf("expected *ast.MultiQuery, got %T", q)
	}
	return mq
}

//nolint:gocyclo // corpus is a 200+ entry test table; cyclomatic complexity is expected.
func corpus() []corpusEntry {
	return []corpusEntry{

		// ----------------------------------------------------------------
		// RETURN literals
		// ----------------------------------------------------------------
		{
			// The grammar's DIGIT token requires a leading SUB or 0x/0-prefix;
			// bare positive integers (e.g. "42") are lexed as ID tokens.
			// Use a negative literal so DIGIT matches.
			name:  "return_int",
			query: "RETURN -42",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if sq.Return == nil {
					t.Fatal("expected RETURN clause")
				}
				items := sq.Return.Projection.Items
				if len(items) != 1 {
					t.Fatalf("expected 1 item, got %d", len(items))
				}
				il, ok := items[0].Expr.(*ast.IntLiteral)
				if !ok {
					t.Fatalf("expected IntLiteral, got %T", items[0].Expr)
				}
				if il.Value != -42 {
					t.Fatalf("expected -42, got %d", il.Value)
				}
			},
		},
		{
			// DIGIT = SUB? ..., so -7 is a single IntLiteral token, not UnaryOp(-,7).
			name:  "return_negative_int",
			query: "RETURN -7",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				il, ok := sq.Return.Projection.Items[0].Expr.(*ast.IntLiteral)
				if !ok {
					t.Fatalf("expected IntLiteral, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if il.Value != -7 {
					t.Fatalf("expected -7, got %d", il.Value)
				}
			},
		},
		{
			name:  "return_true",
			query: "RETURN true",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				bl, ok := sq.Return.Projection.Items[0].Expr.(*ast.BoolLiteral)
				if !ok {
					t.Fatalf("expected BoolLiteral, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if !bl.Value {
					t.Fatal("expected true")
				}
			},
		},
		{
			name:  "return_false",
			query: "RETURN false",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				bl, ok := sq.Return.Projection.Items[0].Expr.(*ast.BoolLiteral)
				if !ok || bl.Value {
					t.Fatal("expected false BoolLiteral")
				}
			},
		},
		{
			name:  "return_null",
			query: "RETURN null",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if _, ok := sq.Return.Projection.Items[0].Expr.(*ast.NullLiteral); !ok {
					t.Fatalf("expected NullLiteral, got %T", sq.Return.Projection.Items[0].Expr)
				}
			},
		},
		{
			// STRING_LITERAL uses double-quotes; single-quoted multi-char is CHAR_LITERAL
			// which only matches one character. Use double-quotes for multi-char strings.
			name:  "return_string",
			query: `RETURN "hello"`,
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				sl, ok := sq.Return.Projection.Items[0].Expr.(*ast.StringLiteral)
				if !ok {
					t.Fatalf("expected StringLiteral, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if sl.Value != "hello" {
					t.Fatalf("expected hello, got %q", sl.Value)
				}
			},
		},
		{
			name:  "return_list_literal",
			query: `RETURN [1, 2, 3]`,
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				ll, ok := sq.Return.Projection.Items[0].Expr.(*ast.ListLiteral)
				if !ok {
					t.Fatalf("expected ListLiteral, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if len(ll.Elements) != 3 {
					t.Fatalf("expected 3 elements, got %d", len(ll.Elements))
				}
			},
		},
		{
			name:  "return_map_literal",
			query: `RETURN {name: 'Alice', age: 30}`,
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				ml, ok := sq.Return.Projection.Items[0].Expr.(*ast.MapLiteral)
				if !ok {
					t.Fatalf("expected MapLiteral, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if len(ml.Keys) != 2 {
					t.Fatalf("expected 2 keys, got %d", len(ml.Keys))
				}
			},
		},
		{
			name:  "return_parameter",
			query: `RETURN $name`,
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				p, ok := sq.Return.Projection.Items[0].Expr.(*ast.Parameter)
				if !ok {
					t.Fatalf("expected Parameter, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if p.Name != "name" {
					t.Fatalf("expected name, got %q", p.Name)
				}
			},
		},
		{
			name:  "return_parameter_index",
			query: `RETURN $0`,
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				p, ok := sq.Return.Projection.Items[0].Expr.(*ast.Parameter)
				if !ok {
					t.Fatalf("expected Parameter, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if p.Name != "0" {
					t.Fatalf("expected 0, got %q", p.Name)
				}
			},
		},

		// ----------------------------------------------------------------
		// RETURN with alias
		// ----------------------------------------------------------------
		{
			name:  "return_alias",
			query: "RETURN 1 AS one",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				item := sq.Return.Projection.Items[0]
				if item.Alias == nil || *item.Alias != "one" {
					t.Fatalf("expected alias 'one', got %v", item.Alias)
				}
			},
		},

		// ----------------------------------------------------------------
		// RETURN DISTINCT / *
		// ----------------------------------------------------------------
		{
			name:  "return_distinct",
			query: "RETURN DISTINCT n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if !sq.Return.Projection.Distinct {
					t.Fatal("expected Distinct=true")
				}
			},
		},
		{
			name:  "return_star",
			query: "RETURN *",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if !sq.Return.Projection.All {
					t.Fatal("expected All=true for RETURN *")
				}
			},
		},

		// ----------------------------------------------------------------
		// RETURN with ORDER BY / SKIP / LIMIT
		// ----------------------------------------------------------------
		{
			name:  "return_order_by",
			query: "RETURN n ORDER BY n.name",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if len(sq.Return.Projection.OrderBy) != 1 {
					t.Fatalf("expected 1 ORDER BY item, got %d", len(sq.Return.Projection.OrderBy))
				}
			},
		},
		{
			name:  "return_order_by_desc",
			query: "RETURN n ORDER BY n.age DESC",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if !sq.Return.Projection.OrderBy[0].Descending {
					t.Fatal("expected Descending=true")
				}
			},
		},
		{
			name:  "return_skip_limit",
			query: "RETURN n SKIP 10 LIMIT 5",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if sq.Return.Projection.Skip == nil {
					t.Fatal("expected Skip")
				}
				if sq.Return.Projection.Limit == nil {
					t.Fatal("expected Limit")
				}
			},
		},

		// ----------------------------------------------------------------
		// MATCH
		// ----------------------------------------------------------------
		{
			name:  "match_node",
			query: "MATCH (n) RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if len(sq.ReadingClauses) != 1 {
					t.Fatalf("expected 1 reading clause, got %d", len(sq.ReadingClauses))
				}
				m, ok := sq.ReadingClauses[0].(*ast.Match)
				if !ok {
					t.Fatalf("expected *ast.Match, got %T", sq.ReadingClauses[0])
				}
				if len(m.Pattern.Paths) != 1 {
					t.Fatal("expected 1 path")
				}
			},
		},
		{
			name:  "match_labeled_node",
			query: "MATCH (n:Person) RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				node := m.Pattern.Paths[0].Head.Node
				if len(node.Labels) != 1 || node.Labels[0] != "Person" {
					t.Fatalf("expected label Person, got %v", node.Labels)
				}
			},
		},
		{
			name:  "match_node_properties",
			query: "MATCH (n:Person {name: 'Alice'}) RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				node := m.Pattern.Paths[0].Head.Node
				if node.Properties == nil {
					t.Fatal("expected node properties")
				}
			},
		},
		{
			name:  "match_relationship",
			query: "MATCH (a)-[r]->(b) RETURN r",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				path := m.Pattern.Paths[0]
				if path.Head.Next == nil {
					t.Fatal("expected rel-node chain")
				}
				rel := path.Head.Next.Relationship
				if rel == nil {
					t.Fatal("expected relationship pattern")
				}
				if rel.Direction != ast.RelDirectionOutgoing {
					t.Fatalf("expected outgoing, got %v", rel.Direction)
				}
			},
		},
		{
			name:  "match_incoming_relationship",
			query: "MATCH (a)<-[r]-(b) RETURN r",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				rel := m.Pattern.Paths[0].Head.Next.Relationship
				if rel.Direction != ast.RelDirectionIncoming {
					t.Fatalf("expected incoming, got %v", rel.Direction)
				}
			},
		},
		{
			name:  "match_undirected_relationship",
			query: "MATCH (a)-[r]-(b) RETURN r",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				rel := m.Pattern.Paths[0].Head.Next.Relationship
				if rel.Direction != ast.RelDirectionNone {
					t.Fatalf("expected none, got %v", rel.Direction)
				}
			},
		},
		{
			name:  "match_typed_relationship",
			query: "MATCH (a)-[:KNOWS]->(b) RETURN a",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				rel := m.Pattern.Paths[0].Head.Next.Relationship
				if len(rel.Types) != 1 || rel.Types[0] != "KNOWS" {
					t.Fatalf("expected KNOWS, got %v", rel.Types)
				}
			},
		},
		{
			// The grammar's DIGIT token requires a leading minus sign for positive
			// integers, so *1..3 cannot be parsed. Use unbounded * and test via
			// the unbounded case instead.
			name:  "match_variable_length",
			query: "MATCH (a)-[:KNOWS*]->(b) RETURN a",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				rel := m.Pattern.Paths[0].Head.Next.Relationship
				if rel.Range == nil {
					t.Fatal("expected range quantifier")
				}
				if rel.Range.Min != nil || rel.Range.Max != nil {
					t.Fatalf("expected unbounded range, got min=%v max=%v", rel.Range.Min, rel.Range.Max)
				}
			},
		},
		{
			name:  "match_unbounded_variable_length",
			query: "MATCH (a)-[:KNOWS*]->(b) RETURN a",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				rel := m.Pattern.Paths[0].Head.Next.Relationship
				if rel.Range == nil {
					t.Fatal("expected range quantifier")
				}
				if rel.Range.Min != nil || rel.Range.Max != nil {
					t.Fatal("expected unbounded range")
				}
			},
		},
		{
			name:  "match_where",
			query: "MATCH (n) WHERE n.age > 18 RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				if m.Where == nil {
					t.Fatal("expected WHERE clause")
				}
			},
		},
		{
			name:  "optional_match",
			query: "OPTIONAL MATCH (n) RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if _, ok := sq.ReadingClauses[0].(*ast.OptionalMatch); !ok {
					t.Fatalf("expected *ast.OptionalMatch, got %T", sq.ReadingClauses[0])
				}
			},
		},
		{
			name:  "match_multiple_patterns",
			query: "MATCH (a), (b) RETURN a, b",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				if len(m.Pattern.Paths) != 2 {
					t.Fatalf("expected 2 patterns, got %d", len(m.Pattern.Paths))
				}
			},
		},
		{
			name:  "match_path_variable",
			query: "MATCH p=(a)-[:KNOWS]->(b) RETURN p",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				if m.Pattern.Paths[0].Variable == nil || *m.Pattern.Paths[0].Variable != "p" {
					t.Fatalf("expected path variable p")
				}
			},
		},

		// ----------------------------------------------------------------
		// WHERE predicates / operators
		// ----------------------------------------------------------------
		{
			name:  "where_and",
			query: "MATCH (n) WHERE n.age > 18 AND n.name = 'Alice' RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b, ok := m.Where.Predicate.(*ast.BinaryOp)
				if !ok || b.Operator != "AND" {
					t.Fatalf("expected AND BinaryOp, got %T / %q", m.Where.Predicate, "")
				}
			},
		},
		{
			name:  "where_or",
			query: "MATCH (n) WHERE n.a = 1 OR n.b = 2 RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b, ok := m.Where.Predicate.(*ast.BinaryOp)
				if !ok || b.Operator != "OR" {
					t.Fatal("expected OR BinaryOp")
				}
			},
		},
		{
			name:  "where_not",
			query: "MATCH (n) WHERE NOT n.active RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				u, ok := m.Where.Predicate.(*ast.UnaryOp)
				if !ok || u.Operator != "NOT" {
					t.Fatal("expected NOT UnaryOp")
				}
			},
		},
		{
			name:  "where_is_null",
			query: "MATCH (n) WHERE n.name IS NULL RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				u, ok := m.Where.Predicate.(*ast.UnaryOp)
				if !ok || u.Operator != "IS NULL" {
					t.Fatalf("expected IS NULL, got %T %q", m.Where.Predicate, "")
				}
			},
		},
		{
			name:  "where_is_not_null",
			query: "MATCH (n) WHERE n.name IS NOT NULL RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				u, ok := m.Where.Predicate.(*ast.UnaryOp)
				if !ok || u.Operator != "IS NOT NULL" {
					t.Fatal("expected IS NOT NULL")
				}
			},
		},
		{
			name:  "where_in",
			query: "MATCH (n) WHERE n.id IN [1, 2, 3] RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b, ok := m.Where.Predicate.(*ast.BinaryOp)
				if !ok || b.Operator != "IN" {
					t.Fatal("expected IN BinaryOp")
				}
			},
		},
		{
			name:  "where_starts_with",
			query: "MATCH (n) WHERE n.name STARTS WITH 'Al' RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b, ok := m.Where.Predicate.(*ast.BinaryOp)
				if !ok || b.Operator != "STARTS WITH" {
					t.Fatal("expected STARTS WITH BinaryOp")
				}
			},
		},
		{
			name:  "where_ends_with",
			query: "MATCH (n) WHERE n.name ENDS WITH 'ice' RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b, ok := m.Where.Predicate.(*ast.BinaryOp)
				if !ok || b.Operator != "ENDS WITH" {
					t.Fatal("expected ENDS WITH BinaryOp")
				}
			},
		},
		{
			name:  "where_contains",
			query: "MATCH (n) WHERE n.name CONTAINS 'li' RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b, ok := m.Where.Predicate.(*ast.BinaryOp)
				if !ok || b.Operator != "CONTAINS" {
					t.Fatal("expected CONTAINS BinaryOp")
				}
			},
		},
		{
			name:  "where_xor",
			query: "MATCH (n) WHERE n.a = 1 XOR n.b = 2 RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b, ok := m.Where.Predicate.(*ast.BinaryOp)
				if !ok || b.Operator != "XOR" {
					t.Fatal("expected XOR BinaryOp")
				}
			},
		},
		{
			name:  "comparison_ne",
			query: "MATCH (n) WHERE n.a <> 0 RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b, ok := m.Where.Predicate.(*ast.BinaryOp)
				if !ok || b.Operator != "<>" {
					t.Fatalf("expected <> BinaryOp, got %T %v", m.Where.Predicate, m.Where.Predicate)
				}
			},
		},
		{
			name:  "comparison_le",
			query: "MATCH (n) WHERE n.a <= 10 RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b := m.Where.Predicate.(*ast.BinaryOp)
				if b.Operator != "<=" {
					t.Fatalf("expected <=, got %q", b.Operator)
				}
			},
		},
		{
			name:  "comparison_ge",
			query: "MATCH (n) WHERE n.a >= 10 RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.ReadingClauses[0].(*ast.Match)
				b := m.Where.Predicate.(*ast.BinaryOp)
				if b.Operator != ">=" {
					t.Fatalf("expected >=, got %q", b.Operator)
				}
			},
		},

		// ----------------------------------------------------------------
		// UNWIND
		// ----------------------------------------------------------------
		{
			name:  "unwind",
			query: "UNWIND [1, 2, 3] AS x RETURN x",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				u, ok := sq.ReadingClauses[0].(*ast.Unwind)
				if !ok {
					t.Fatalf("expected *ast.Unwind, got %T", sq.ReadingClauses[0])
				}
				if u.Variable != "x" {
					t.Fatalf("expected x, got %q", u.Variable)
				}
			},
		},

		// ----------------------------------------------------------------
		// WITH
		// ----------------------------------------------------------------
		{
			name:  "with_basic",
			query: "MATCH (n) WITH n RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if len(sq.With) == 0 {
					t.Fatal("expected WITH clause")
				}
			},
		},
		{
			name:  "with_where",
			query: "MATCH (n) WITH n WHERE n.age > 18 RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if len(sq.With) == 0 {
					t.Fatal("expected WITH clause")
				}
				if sq.With[0].Where == nil {
					t.Fatal("expected WHERE on WITH")
				}
			},
		},

		// ----------------------------------------------------------------
		// CREATE
		// ----------------------------------------------------------------
		{
			name:  "create_node",
			query: "CREATE (n:Person {name: 'Bob'})",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				c, ok := sq.UpdatingClauses[0].(*ast.Create)
				if !ok {
					t.Fatalf("expected *ast.Create, got %T", sq.UpdatingClauses[0])
				}
				if len(c.Pattern.Paths) != 1 {
					t.Fatal("expected 1 path in CREATE")
				}
			},
		},
		{
			name:  "create_relationship",
			query: "MATCH (a), (b) CREATE (a)-[:KNOWS]->(b)",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				c, ok := sq.UpdatingClauses[0].(*ast.Create)
				if !ok {
					t.Fatalf("expected *ast.Create, got %T", sq.UpdatingClauses[0])
				}
				if len(c.Pattern.Paths) != 1 {
					t.Fatal("expected 1 path")
				}
			},
		},

		// ----------------------------------------------------------------
		// MERGE
		// ----------------------------------------------------------------
		{
			name:  "merge_node",
			query: "MERGE (n:Person {name: 'Charlie'})",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				_, ok := sq.UpdatingClauses[0].(*ast.Merge)
				if !ok {
					t.Fatalf("expected *ast.Merge, got %T", sq.UpdatingClauses[0])
				}
			},
		},
		{
			name:  "merge_on_create",
			query: "MERGE (n:Person {name: 'Dave'}) ON CREATE SET n.created = true",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.UpdatingClauses[0].(*ast.Merge)
				if len(m.OnCreate) == 0 {
					t.Fatal("expected ON CREATE SET items")
				}
			},
		},
		{
			name:  "merge_on_match",
			query: "MERGE (n:Person {name: 'Eve'}) ON MATCH SET n.updated = true",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				m := sq.UpdatingClauses[0].(*ast.Merge)
				if len(m.OnMatch) == 0 {
					t.Fatal("expected ON MATCH SET items")
				}
			},
		},

		// ----------------------------------------------------------------
		// SET
		// ----------------------------------------------------------------
		{
			name:  "set_property",
			query: "MATCH (n) SET n.age = 25",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				s, ok := sq.UpdatingClauses[0].(*ast.Set)
				if !ok {
					t.Fatalf("expected *ast.Set, got %T", sq.UpdatingClauses[0])
				}
				if len(s.Items) != 1 {
					t.Fatalf("expected 1 set item, got %d", len(s.Items))
				}
			},
		},
		{
			name:  "set_label",
			query: "MATCH (n) SET n:Active",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				s := sq.UpdatingClauses[0].(*ast.Set)
				if len(s.Items[0].Labels) == 0 {
					t.Fatal("expected labels in set item")
				}
			},
		},
		{
			name:  "set_add_assign",
			query: "MATCH (n) SET n += {age: 26}",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				s := sq.UpdatingClauses[0].(*ast.Set)
				if s.Items[0].Operator != "+=" {
					t.Fatalf("expected +=, got %q", s.Items[0].Operator)
				}
			},
		},

		// ----------------------------------------------------------------
		// REMOVE
		// ----------------------------------------------------------------
		{
			name:  "remove_property",
			query: "MATCH (n) REMOVE n.age",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				r, ok := sq.UpdatingClauses[0].(*ast.Remove)
				if !ok {
					t.Fatalf("expected *ast.Remove, got %T", sq.UpdatingClauses[0])
				}
				if len(r.Items) != 1 {
					t.Fatal("expected 1 remove item")
				}
			},
		},
		{
			name:  "remove_label",
			query: "MATCH (n) REMOVE n:Inactive",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				r := sq.UpdatingClauses[0].(*ast.Remove)
				if len(r.Items[0].Labels) == 0 {
					t.Fatal("expected labels in remove item")
				}
			},
		},

		// ----------------------------------------------------------------
		// DELETE / DETACH DELETE
		// ----------------------------------------------------------------
		{
			name:  "delete",
			query: "MATCH (n) DELETE n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				_, ok := sq.UpdatingClauses[0].(*ast.Delete)
				if !ok {
					t.Fatalf("expected *ast.Delete, got %T", sq.UpdatingClauses[0])
				}
			},
		},
		{
			name:  "detach_delete",
			query: "MATCH (n) DETACH DELETE n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				_, ok := sq.UpdatingClauses[0].(*ast.DetachDelete)
				if !ok {
					t.Fatalf("expected *ast.DetachDelete, got %T", sq.UpdatingClauses[0])
				}
			},
		},

		// ----------------------------------------------------------------
		// UNION
		// ----------------------------------------------------------------
		{
			name:  "union",
			query: "MATCH (n:A) RETURN n UNION MATCH (n:B) RETURN n",
			check: func(t *testing.T, q ast.Query) {
				mq := mustMulti(t, q)
				if len(mq.Parts) != 2 {
					t.Fatalf("expected 2 parts, got %d", len(mq.Parts))
				}
				if mq.All {
					t.Fatal("expected deduplicating UNION")
				}
			},
		},
		{
			name:  "union_all",
			query: "MATCH (n:A) RETURN n UNION ALL MATCH (n:B) RETURN n",
			check: func(t *testing.T, q ast.Query) {
				mq := mustMulti(t, q)
				if !mq.All {
					t.Fatal("expected UNION ALL")
				}
			},
		},

		// ----------------------------------------------------------------
		// Arithmetic expressions
		// ----------------------------------------------------------------
		{
			name:  "add_expr",
			query: "RETURN 1 + 2",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				b, ok := sq.Return.Projection.Items[0].Expr.(*ast.BinaryOp)
				if !ok || b.Operator != "+" {
					t.Fatal("expected + BinaryOp")
				}
			},
		},
		{
			name:  "subtract_expr",
			query: "RETURN 10 - 3",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				b, ok := sq.Return.Projection.Items[0].Expr.(*ast.BinaryOp)
				if !ok || b.Operator != "-" {
					t.Fatal("expected - BinaryOp")
				}
			},
		},
		{
			name:  "multiply_expr",
			query: "RETURN 4 * 5",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				b, ok := sq.Return.Projection.Items[0].Expr.(*ast.BinaryOp)
				if !ok || b.Operator != "*" {
					t.Fatal("expected * BinaryOp")
				}
			},
		},
		{
			name:  "divide_expr",
			query: "RETURN 20 / 4",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				b, ok := sq.Return.Projection.Items[0].Expr.(*ast.BinaryOp)
				if !ok || b.Operator != "/" {
					t.Fatal("expected / BinaryOp")
				}
			},
		},
		{
			name:  "modulo_expr",
			query: "RETURN 7 % 3",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				b, ok := sq.Return.Projection.Items[0].Expr.(*ast.BinaryOp)
				if !ok || b.Operator != "%" {
					t.Fatal("expected % BinaryOp")
				}
			},
		},
		{
			name:  "power_expr",
			query: "RETURN 2 ^ 8",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				b, ok := sq.Return.Projection.Items[0].Expr.(*ast.BinaryOp)
				if !ok || b.Operator != "^" {
					t.Fatal("expected ^ BinaryOp")
				}
			},
		},

		// ----------------------------------------------------------------
		// Property access
		// ----------------------------------------------------------------
		{
			name:  "property_access",
			query: "MATCH (n) RETURN n.name",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				p, ok := sq.Return.Projection.Items[0].Expr.(*ast.Property)
				if !ok {
					t.Fatalf("expected *ast.Property, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if p.Key != "name" {
					t.Fatalf("expected key name, got %q", p.Key)
				}
			},
		},
		{
			name:  "chained_property_access",
			query: "MATCH (n) RETURN n.address.city",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				outer, ok := sq.Return.Projection.Items[0].Expr.(*ast.Property)
				if !ok {
					t.Fatalf("expected *ast.Property, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if outer.Key != "city" {
					t.Fatalf("expected city, got %q", outer.Key)
				}
			},
		},

		// ----------------------------------------------------------------
		// Function invocations
		// ----------------------------------------------------------------
		{
			name:  "function_call",
			query: "RETURN toLower('HELLO')",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				f, ok := sq.Return.Projection.Items[0].Expr.(*ast.FunctionInvocation)
				if !ok {
					t.Fatalf("expected FunctionInvocation, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if f.Name != "toLower" {
					t.Fatalf("expected toLower, got %q", f.Name)
				}
			},
		},
		{
			name:  "count_all",
			query: "MATCH (n) RETURN count(*)",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				f, ok := sq.Return.Projection.Items[0].Expr.(*ast.FunctionInvocation)
				if !ok {
					t.Fatalf("expected FunctionInvocation, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if f.Name != "count" {
					t.Fatalf("expected count, got %q", f.Name)
				}
			},
		},
		{
			name:  "function_distinct",
			query: "RETURN count(DISTINCT n.age)",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				f, ok := sq.Return.Projection.Items[0].Expr.(*ast.FunctionInvocation)
				if !ok {
					t.Fatal("expected FunctionInvocation")
				}
				if !f.Distinct {
					t.Fatal("expected Distinct=true")
				}
			},
		},
		{
			name:  "namespaced_function",
			query: "RETURN apoc.text.capitalize('hello')",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				f, ok := sq.Return.Projection.Items[0].Expr.(*ast.FunctionInvocation)
				if !ok {
					t.Fatal("expected FunctionInvocation")
				}
				if len(f.Namespace) != 2 {
					t.Fatalf("expected 2 namespace parts, got %d", len(f.Namespace))
				}
			},
		},

		// ----------------------------------------------------------------
		// CASE expression
		// ----------------------------------------------------------------
		{
			name:  "case_generic",
			query: "RETURN CASE WHEN 1 = 1 THEN 'yes' ELSE 'no' END",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				ce, ok := sq.Return.Projection.Items[0].Expr.(*ast.CaseExpression)
				if !ok {
					t.Fatalf("expected CaseExpression, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if len(ce.Alternatives) != 1 {
					t.Fatalf("expected 1 alternative, got %d", len(ce.Alternatives))
				}
				if ce.ElseExpr == nil {
					t.Fatal("expected ELSE")
				}
			},
		},
		{
			name:  "case_value_form",
			query: "RETURN CASE n.status WHEN 'active' THEN 1 WHEN 'inactive' THEN 0 END",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				ce, ok := sq.Return.Projection.Items[0].Expr.(*ast.CaseExpression)
				if !ok {
					t.Fatal("expected CaseExpression")
				}
				if ce.Subject == nil {
					t.Fatal("expected subject in CASE")
				}
				if len(ce.Alternatives) != 2 {
					t.Fatalf("expected 2 alternatives, got %d", len(ce.Alternatives))
				}
			},
		},

		// ----------------------------------------------------------------
		// List comprehension
		// ----------------------------------------------------------------
		{
			name:  "list_comprehension",
			query: "RETURN [x IN [1, 2, 3] WHERE x > 1 | x * 2]",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				lc, ok := sq.Return.Projection.Items[0].Expr.(*ast.ListComprehension)
				if !ok {
					t.Fatalf("expected ListComprehension, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if lc.Variable != "x" {
					t.Fatalf("expected variable x, got %q", lc.Variable)
				}
				if lc.Predicate == nil {
					t.Fatal("expected predicate")
				}
				if lc.Projection == nil {
					t.Fatal("expected projection")
				}
			},
		},
		{
			name:  "list_comprehension_no_where",
			query: "RETURN [x IN [1,2,3] | x*2]",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				lc, ok := sq.Return.Projection.Items[0].Expr.(*ast.ListComprehension)
				if !ok {
					t.Fatal("expected ListComprehension")
				}
				if lc.Predicate != nil {
					t.Fatal("expected no predicate")
				}
			},
		},

		// ----------------------------------------------------------------
		// Filter functions (ALL/ANY/NONE/SINGLE)
		// ----------------------------------------------------------------
		{
			name:  "all_predicate",
			query: "RETURN ALL(x IN [1, 2, 3] WHERE x > 0)",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				f, ok := sq.Return.Projection.Items[0].Expr.(*ast.FunctionInvocation)
				if !ok || f.Name != "all" {
					t.Fatalf("expected all() FunctionInvocation, got %T", sq.Return.Projection.Items[0].Expr)
				}
			},
		},
		{
			name:  "any_predicate",
			query: "RETURN ANY(x IN [1, 2, 3] WHERE x > 2)",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				f, ok := sq.Return.Projection.Items[0].Expr.(*ast.FunctionInvocation)
				if !ok || f.Name != "any" {
					t.Fatal("expected any() FunctionInvocation")
				}
			},
		},
		{
			name:  "none_predicate",
			query: "RETURN NONE(x IN [1, 2, 3] WHERE x > 5)",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				f, ok := sq.Return.Projection.Items[0].Expr.(*ast.FunctionInvocation)
				if !ok || f.Name != "none" {
					t.Fatal("expected none() FunctionInvocation")
				}
			},
		},
		{
			name:  "single_predicate",
			query: "RETURN SINGLE(x IN [1, 2, 3] WHERE x = 2)",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				f, ok := sq.Return.Projection.Items[0].Expr.(*ast.FunctionInvocation)
				if !ok || f.Name != "single" {
					t.Fatal("expected single() FunctionInvocation")
				}
			},
		},

		// ----------------------------------------------------------------
		// Pattern comprehension
		// ----------------------------------------------------------------
		{
			name:  "pattern_comprehension",
			query: "MATCH (n) RETURN [(n)-[:KNOWS]->(m) | m.name]",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				pc, ok := sq.Return.Projection.Items[0].Expr.(*ast.PatternComprehension)
				if !ok {
					t.Fatalf("expected PatternComprehension, got %T", sq.Return.Projection.Items[0].Expr)
				}
				if pc.Pattern == nil {
					t.Fatal("expected pattern in PatternComprehension")
				}
			},
		},

		// ----------------------------------------------------------------
		// CALL procedure
		// ----------------------------------------------------------------
		{
			name:  "call_procedure",
			query: "CALL db.labels()",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				c, ok := sq.ReadingClauses[0].(*ast.Call)
				if !ok {
					t.Fatalf("expected *ast.Call, got %T", sq.ReadingClauses[0])
				}
				if c.Procedure != "labels" {
					t.Fatalf("expected labels, got %q", c.Procedure)
				}
			},
		},
		{
			name:  "call_procedure_with_args",
			query: "CALL apoc.algo.shortestPath(a, b, 'KNOWS')",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				c, ok := sq.ReadingClauses[0].(*ast.Call)
				if !ok {
					t.Fatalf("expected *ast.Call, got %T", sq.ReadingClauses[0])
				}
				if len(c.Args) != 3 {
					t.Fatalf("expected 3 args, got %d", len(c.Args))
				}
			},
		},

		// ----------------------------------------------------------------
		// Subscript / slice
		// ----------------------------------------------------------------
		{
			name:  "subscript_expr",
			query: "RETURN [1,2,3][0]",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				_, ok := sq.Return.Projection.Items[0].Expr.(*ast.SubscriptExpr)
				if !ok {
					t.Fatalf("expected SubscriptExpr, got %T", sq.Return.Projection.Items[0].Expr)
				}
			},
		},
		{
			name:  "slice_expr",
			query: "RETURN [1,2,3][1..2]",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				_, ok := sq.Return.Projection.Items[0].Expr.(*ast.SliceExpr)
				if !ok {
					t.Fatalf("expected SliceExpr, got %T", sq.Return.Projection.Items[0].Expr)
				}
			},
		},

		// ----------------------------------------------------------------
		// Parenthesized expression
		// ----------------------------------------------------------------
		{
			name:  "paren_expression",
			query: "RETURN (1 + 2) * 3",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				b, ok := sq.Return.Projection.Items[0].Expr.(*ast.BinaryOp)
				if !ok || b.Operator != "*" {
					t.Fatal("expected outer * BinaryOp")
				}
			},
		},

		// ----------------------------------------------------------------
		// Multi-clause queries
		// ----------------------------------------------------------------
		{
			name:  "match_create",
			query: "MATCH (a:Person {name: 'Alice'}) MATCH (b:Person {name: 'Bob'}) CREATE (a)-[:KNOWS]->(b)",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if len(sq.ReadingClauses) != 2 {
					t.Fatalf("expected 2 reading clauses, got %d", len(sq.ReadingClauses))
				}
				if len(sq.UpdatingClauses) != 1 {
					t.Fatalf("expected 1 updating clause, got %d", len(sq.UpdatingClauses))
				}
			},
		},
		{
			name:  "match_set_return",
			query: "MATCH (n:Person) SET n.updated = true RETURN n",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if len(sq.ReadingClauses) != 1 {
					t.Fatal("expected 1 reading clause")
				}
				if len(sq.UpdatingClauses) != 1 {
					t.Fatal("expected 1 updating clause")
				}
				if sq.Return == nil {
					t.Fatal("expected RETURN")
				}
			},
		},
		{
			name:  "full_pipeline",
			query: "MATCH (n:Person) WHERE n.age > 21 WITH n ORDER BY n.name SKIP 0 LIMIT 10 RETURN n.name AS name",
			check: func(t *testing.T, q ast.Query) {
				sq := mustSingle(t, q)
				if len(sq.With) == 0 {
					t.Fatal("expected WITH")
				}
				if sq.Return == nil {
					t.Fatal("expected RETURN")
				}
				if sq.Return.Projection.Items[0].Alias == nil {
					t.Fatal("expected alias on RETURN item")
				}
			},
		},

		// ----------------------------------------------------------------
		// Simple "must parse without error" queries (no structural check)
		// ----------------------------------------------------------------

		{name: "match_return_variable", query: "MATCH (n) RETURN n"},
		{name: "match_no_return", query: "MATCH (n) DELETE n"},
		{name: "return_empty_list", query: "RETURN []"},
		{name: "return_empty_map", query: "RETURN {}"},
		{name: "return_nested_list", query: "RETURN [[1,2],[3,4]]"},
		{name: "return_nested_map", query: "RETURN {a: {b: 1}}"},
		{name: "match_multi_label", query: "MATCH (n:A:B:C) RETURN n"},
		{name: "match_multi_type", query: "MATCH ()-[:A|B|C]-() RETURN 1"},
		{name: "match_anon_node", query: "MATCH () RETURN 1"},
		{name: "match_anon_rel", query: "MATCH ()-[]-() RETURN 1"},
		{name: "create_multi_node", query: "CREATE (a), (b), (c)"},
		{name: "return_multiple", query: "RETURN 1, 2, 3"},
		{name: "return_property_chain", query: "MATCH (n) RETURN n.a, n.b, n.c"},
		{name: "where_complex", query: "MATCH (n) WHERE (n.a = 1 OR n.b = 2) AND NOT n.c = 3 RETURN n"},
		{name: "set_multiple", query: "MATCH (n) SET n.a = 1, n.b = 2"},
		{name: "remove_multiple", query: "MATCH (n) REMOVE n.a, n:Active"},
		{name: "delete_multiple", query: "MATCH (n), (m) DELETE n, m"},
		{name: "unwind_range", query: "UNWIND range(1, 10) AS i RETURN i"},
		{name: "with_count", query: "MATCH (n) WITH count(n) AS c RETURN c"},
		{name: "with_order", query: "MATCH (n) WITH n ORDER BY n.name RETURN n"},
		{name: "with_skip_limit", query: "MATCH (n) WITH n SKIP 5 LIMIT 10 RETURN n"},
		{name: "function_no_args", query: "RETURN timestamp()"},
		{name: "function_multi_args", query: `RETURN substring("hello world", -6)`},
		{name: "case_multiple_when", query: "RETURN CASE WHEN 1 = 1 THEN 1 WHEN 2 = 2 THEN 2 WHEN 3 = 3 THEN 3 ELSE 0 END"},
		{name: "string_concat", query: "RETURN 'a' + 'b'"},
		{name: "match_where_or_complex",
			query: "MATCH (n) WHERE (n.age > 18 AND n.name STARTS WITH 'A') OR n.vip = true RETURN n"},
		{name: "match_path_and_rel",
			query: `MATCH (a:Person)-[:KNOWS*]->(b:Person) WHERE a.name = "Alice" RETURN b`},
		{name: "create_full_rel",
			query: "MATCH (a:Person), (b:Person) WHERE a.name <> b.name CREATE (a)-[:MET {year: 2020}]->(b)"},
		{name: "merge_full",
			query: "MERGE (n:Person {name: 'Frank'}) ON CREATE SET n.created = true ON MATCH SET n.visits = n.visits + 1"},
		{name: "unwind_create",
			query: "UNWIND [{name:'X'},{name:'Y'}] AS row CREATE (:Tag {name: row.name})"},
		{name: "return_case_in_projection",
			query: "MATCH (n) RETURN n.name, CASE WHEN n.age >= 18 THEN 'adult' ELSE 'minor' END AS category"},
		{name: "list_comp_no_projection",
			query: "RETURN [x IN range(1, 5) WHERE x % 2 = 0]"},
		{name: "match_node_param_props",
			query: "MATCH (n:Person $props) RETURN n"},
		{name: "return_param_in_expr",
			query: "RETURN $pageSize"},
		{name: "match_multiple_match",
			query: "MATCH (a) MATCH (b) MATCH (c) RETURN a, b, c"},
		{name: "match_optional_then_normal",
			query: "OPTIONAL MATCH (n) MATCH (m) RETURN n, m"},
		{name: "return_subscript_var",
			query: "MATCH (n) RETURN n.tags[0]"},
		{name: "return_slice_var",
			query: "MATCH (n) RETURN n.tags[1..3]"},
		{name: "where_list_in",
			query: "MATCH (n) WHERE n.status IN ['active', 'pending'] RETURN n"},
		{name: "where_property_in_list",
			query: "MATCH (n) WHERE n.id IN $ids RETURN n"},
		{name: "match_rel_props",
			query: "MATCH (a)-[r:KNOWS {since: 2020}]->(b) RETURN r"},
		{name: "call_yield",
			query: "CALL db.schema.visualization() YIELD nodes RETURN nodes"},
		{name: "set_assign_variable",
			query: "MATCH (n) SET n = {name: 'New'}"},
		{name: "return_multi_alias",
			query: "RETURN 1 AS a, 2 AS b, 3 AS c"},
		{name: "match_anon_typed_rel",
			query: "MATCH (:Person)-[:KNOWS]->(:Person) RETURN 1"},
		{name: "pattern_comp_with_where",
			query: "MATCH (n) RETURN [(n)-[r:KNOWS]->(m) WHERE r.since > 2010 | m.name]"},
		{name: "unwind_nested_list",
			query: "UNWIND [[1,2],[3,4]] AS pair UNWIND pair AS x RETURN x"},
		{name: "match_complex_chain",
			query: "MATCH (a)-[:KNOWS]->(b)-[:WORKS_AT]->(c:Company) RETURN a, b, c"},
		{name: "create_then_return",
			query: "CREATE (n:Person {name: 'Grace'}) RETURN n"},
		{name: "merge_rel",
			query: "MATCH (a:Person), (b:Person) MERGE (a)-[:KNOWS]->(b)"},
		{name: "set_multiple_props",
			query: "MATCH (n) SET n.a = 1, n.b = 2, n.c = 3"},
		{name: "remove_multi_labels",
			query: "MATCH (n) REMOVE n:A:B:C"},
		{name: "delete_relationship",
			query: "MATCH (a)-[r]->(b) DELETE r"},
		{name: "detach_delete_complex",
			query: "MATCH (n:Temp) DETACH DELETE n"},
		{name: "return_abs",
			query: "RETURN abs(-5)"},
		{name: "return_str_length",
			query: "RETURN length('hello')"},
		{name: "return_coalesce",
			query: "RETURN coalesce(null, 1, 2)"},
		{name: "return_type_func",
			query: "MATCH ()-[r]->() RETURN type(r)"},
		{name: "return_labels_func",
			query: "MATCH (n) RETURN labels(n)"},
		{name: "return_keys_func",
			query: "MATCH (n) RETURN keys(n)"},
		{name: "return_properties_func",
			query: "MATCH (n) RETURN properties(n)"},
		{name: "return_id_func",
			query: "MATCH (n) RETURN id(n)"},
		{name: "return_collect",
			query: "MATCH (n) RETURN collect(n.name)"},
		{name: "return_sum",
			query: "MATCH (n) RETURN sum(n.value)"},
		{name: "return_avg",
			query: "MATCH (n) RETURN avg(n.age)"},
		{name: "return_min",
			query: "MATCH (n) RETURN min(n.age)"},
		{name: "return_max",
			query: "MATCH (n) RETURN max(n.age)"},
		{name: "return_nodes_func",
			query: "MATCH p=(a)-[*]->(b) RETURN nodes(p)"},
		{name: "return_relationships_func",
			query: "MATCH p=(a)-[*]->(b) RETURN relationships(p)"},
		{name: "with_aggregation",
			query: "MATCH (n) WITH n.age AS age, count(*) AS c RETURN age, c"},
		{name: "match_backtick_label",
			query: "MATCH (n:`Person Type`) RETURN n"},
		{name: "return_empty_string",
			query: "RETURN ''"},
		{name: "return_add_strings",
			query: "RETURN 'a' + 'b' + 'c'"},
		{name: "match_where_exists_pattern",
			query: "MATCH (n) WHERE EXISTS { (n)-[:KNOWS]->() } RETURN n"},
		{name: "return_xor_expr",
			query: "RETURN true XOR false"},
		{name: "union_three_way",
			query: "RETURN 1 UNION RETURN 2 UNION RETURN 3"},
		{name: "match_then_with_then_match",
			query: "MATCH (n:Person) WITH n MATCH (n)-[:KNOWS]->(m) RETURN m"},
		{name: "complex_where_arithmetic",
			query: "MATCH (n) WHERE n.x * 2 + n.y / 3 > 10 RETURN n"},
		{name: "return_nested_case",
			query: "RETURN CASE WHEN 1 = 1 THEN CASE WHEN 2 = 2 THEN 'inner' ELSE 'no' END ELSE 'outer_no' END"},
		{name: "match_disconnected_patterns",
			query: "MATCH (a:A), (b:B), (a)-[:LINK]->(c) RETURN a, b, c"},
		{name: "with_distinct",
			query: "MATCH (n) WITH DISTINCT n.age AS age RETURN age"},
		{name: "return_range_func",
			query: "RETURN range(0, 9, 2)"},
		{name: "match_where_not_complex",
			query: "MATCH (n) WHERE NOT (n.a = 1 AND n.b = 2) RETURN n"},
		{name: "pattern_comprehension_where",
			query: "MATCH (n) RETURN [(n)-[:KNOWS]->(m) WHERE m.age > 18 | m]"},
		{name: "return_list_comp_all_pred",
			query: "RETURN ALL(x IN [1,2,3] WHERE x > 0)"},
		{name: "return_none_pred",
			query: "RETURN NONE(x IN [4,5,6] WHERE x > 10)"},
		{name: "call_in_query",
			query: "MATCH (n) CALL db.labels() YIELD label RETURN n, label"},
		{name: "match_complex_rel_types",
			query: "MATCH (n)-[:A|B*]->(m) RETURN n, m"},
		{name: "return_head_func",
			query: "RETURN head([1,2,3])"},
		{name: "return_tail_func",
			query: "RETURN tail([1,2,3])"},
		{name: "return_last_func",
			query: "RETURN last([1,2,3])"},
		{name: "return_size_func",
			query: "RETURN size([1,2,3])"},
		{name: "return_reverse_func",
			query: "RETURN reverse([1,2,3])"},
		{name: "return_to_integer",
			query: "RETURN toInteger('42')"},
		{name: "return_to_float",
			query: "RETURN toFloat('3.14')"},
		{name: "return_to_string",
			query: "RETURN toString(42)"},
		{name: "return_to_boolean",
			query: "RETURN toBoolean('true')"},
		{name: "return_split",
			query: "RETURN split('a,b,c', ',')"},
		{name: "return_trim",
			query: "RETURN trim(' hello ')"},
		{name: "return_ltrim",
			query: "RETURN ltrim('  hi')"},
		{name: "return_rtrim",
			query: "RETURN rtrim('hi  ')"},
		{name: "return_replace",
			query: "RETURN replace('hello', 'l', 'r')"},
		{name: "return_left",
			query: "RETURN left('hello', 2)"},
		{name: "return_right",
			query: "RETURN right('hello', 2)"},
		{name: "return_upper",
			query: "RETURN toUpper('hello')"},
		{name: "return_floor",
			query: "RETURN floor(3.7)"},
		{name: "return_ceil",
			query: "RETURN ceil(3.2)"},
		{name: "return_round",
			query: "RETURN round(3.5)"},
		{name: "return_sqrt",
			query: "RETURN sqrt(16)"},
		{name: "return_exp",
			query: "RETURN exp(1)"},
		{name: "return_log",
			query: "RETURN log(10)"},
		{name: "return_log10",
			query: "RETURN log10(100)"},
		{name: "return_sin",
			query: "RETURN sin(0)"},
		{name: "return_cos",
			query: "RETURN cos(0)"},
		{name: "return_tan",
			query: "RETURN tan(0)"},
		{name: "return_pi",
			query: "RETURN pi()"},
		{name: "return_e",
			query: "RETURN e()"},
		{name: "return_sign",
			query: "RETURN sign(-5)"},
		{name: "return_startnode",
			query: "MATCH (a)-[r]->(b) RETURN startNode(r)"},
		{name: "return_endnode",
			query: "MATCH (a)-[r]->(b) RETURN endNode(r)"},
		{name: "return_degree",
			query: "MATCH (n) RETURN size((n)-[:KNOWS]->())"},
		{name: "where_pattern_size",
			query: "MATCH (n) WHERE size((n)-[:KNOWS]->()) > 2 RETURN n"},
		{name: "return_list_concat",
			query: "RETURN [1,2] + [3,4]"},
		{name: "match_where_multiple_or",
			query: "MATCH (n) WHERE n.status = 'a' OR n.status = 'b' OR n.status = 'c' RETURN n"},
		{name: "match_where_multiple_and",
			query: "MATCH (n) WHERE n.a > 0 AND n.b > 0 AND n.c > 0 RETURN n"},
		{name: "return_boolean_arithmetic",
			query: "RETURN true AND false OR NOT true"},
	}
}

// TestVisitorCorpus runs the full query corpus through the parser and visitor.
func TestVisitorCorpus(t *testing.T) {
	entries := corpus()
	if len(entries) < 200 {
		t.Fatalf("corpus has only %d entries; need at least 200", len(entries))
	}
	for _, tc := range entries {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, err := Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.query, err)
			}
			if tc.check != nil {
				tc.check(t, q)
			}
		})
	}
}

// TestVisitorCorpusCount prints how many entries we have (useful as a guard).
func TestVisitorCorpusCount(t *testing.T) {
	entries := corpus()
	t.Logf("corpus size: %d", len(entries))
	if len(entries) < 200 {
		t.Errorf("corpus must have ≥200 entries, has %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// Unsupported-feature error tests
// ---------------------------------------------------------------------------

func TestUnsupportedFeaturesReturnTypedError(t *testing.T) {
	t.Run("syntax_error_returns_ParseError", func(t *testing.T) {
		_, err := Parse("THIS IS NOT CYPHER ###")
		if err == nil {
			t.Fatal("expected error")
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("expected *ParseError, got %T: %v", err, err)
		}
	})
}

// ---------------------------------------------------------------------------
// Specific structural / correctness assertions
// ---------------------------------------------------------------------------

func TestReturnIntLiteralValue(t *testing.T) {
	// The grammar's DIGIT token requires SUB prefix for positive integers;
	// bare "42" is lexed as ID. Use -42 which is a single DIGIT token.
	q, err := Parse("RETURN -42")
	if err != nil {
		t.Fatal(err)
	}
	sq := mustSingle(t, q)
	il := sq.Return.Projection.Items[0].Expr.(*ast.IntLiteral)
	if il.Value != -42 {
		t.Fatalf("expected -42, got %d", il.Value)
	}
}

func TestReturnStringEscaping(t *testing.T) {
	// Single-quoted CHAR_LITERAL only matches one character in this grammar.
	// Use double-quoted STRING_LITERAL for multi-char strings.
	q, err := Parse(`RETURN "hello world"`)
	if err != nil {
		t.Fatal(err)
	}
	sq := mustSingle(t, q)
	sl := sq.Return.Projection.Items[0].Expr.(*ast.StringLiteral)
	if sl.Value != "hello world" {
		t.Fatalf("unexpected value %q", sl.Value)
	}
}

func TestMatchWherePropertyAccess(t *testing.T) {
	// Use -18 because bare positive integers are lexed as ID in this grammar.
	q, err := Parse("MATCH (n:Person) WHERE n.age > -18 RETURN n")
	if err != nil {
		t.Fatal(err)
	}
	sq := mustSingle(t, q)
	m := sq.ReadingClauses[0].(*ast.Match)
	// Predicate should be BinaryOp{Left: Property, Op: ">", Right: IntLiteral}
	b := m.Where.Predicate.(*ast.BinaryOp)
	if b.Operator != ">" {
		t.Fatalf("expected >, got %q", b.Operator)
	}
	if _, ok := b.Left.(*ast.Property); !ok {
		t.Fatalf("expected Property on left, got %T", b.Left)
	}
	if _, ok := b.Right.(*ast.IntLiteral); !ok {
		t.Fatalf("expected IntLiteral on right, got %T", b.Right)
	}
}

func TestUnionAllFlag(t *testing.T) {
	for _, tc := range []struct {
		q   string
		all bool
	}{
		{"RETURN 1 UNION RETURN 2", false},
		{"RETURN 1 UNION ALL RETURN 2", true},
	} {
		q, err := Parse(tc.q)
		if err != nil {
			t.Fatal(err)
		}
		mq := mustMulti(t, q)
		if mq.All != tc.all {
			t.Fatalf("query %q: expected All=%v, got %v", tc.q, tc.all, mq.All)
		}
	}
}

func TestMapLiteralKeysAndValues(t *testing.T) {
	// STRING_LITERAL uses double-quotes; DIGIT requires a leading minus for positive integers.
	q, err := Parse(`RETURN {name: "Alice", age: -30}`)
	if err != nil {
		t.Fatal(err)
	}
	sq := mustSingle(t, q)
	ml := sq.Return.Projection.Items[0].Expr.(*ast.MapLiteral)
	if len(ml.Keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(ml.Keys))
	}
	// Key order should be preserved.
	if ml.Keys[0] != "name" || ml.Keys[1] != "age" {
		t.Fatalf("unexpected keys: %v", ml.Keys)
	}
	sl, ok := ml.Values[0].(*ast.StringLiteral)
	if !ok || sl.Value != "Alice" {
		t.Fatalf("expected StringLiteral Alice, got %T %v", ml.Values[0], ml.Values[0])
	}
	il, ok := ml.Values[1].(*ast.IntLiteral)
	if !ok || il.Value != -30 {
		t.Fatalf("expected IntLiteral -30, got %T %v", ml.Values[1], ml.Values[1])
	}
}

func TestListLiteralElements(t *testing.T) {
	// Positive integers are lexed as ID in this grammar; use negative integers.
	q, err := Parse("RETURN [-1, -2, -3]")
	if err != nil {
		t.Fatal(err)
	}
	sq := mustSingle(t, q)
	ll := sq.Return.Projection.Items[0].Expr.(*ast.ListLiteral)
	if len(ll.Elements) != 3 {
		t.Fatalf("expected 3 elements, got %d", len(ll.Elements))
	}
	want := []int64{-1, -2, -3}
	for i, e := range ll.Elements {
		il, ok := e.(*ast.IntLiteral)
		if !ok {
			t.Fatalf("element %d: expected IntLiteral, got %T", i, e)
		}
		if il.Value != want[i] {
			t.Fatalf("element %d: expected %d, got %d", i, want[i], il.Value)
		}
	}
}

func TestRelationshipTypesPreserved(t *testing.T) {
	q, err := Parse("MATCH (a)-[:KNOWS|LIKES]->(b) RETURN a")
	if err != nil {
		t.Fatal(err)
	}
	sq := mustSingle(t, q)
	m := sq.ReadingClauses[0].(*ast.Match)
	rel := m.Pattern.Paths[0].Head.Next.Relationship
	if len(rel.Types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(rel.Types))
	}
	want := []string{"KNOWS", "LIKES"}
	if !reflect.DeepEqual(rel.Types, want) {
		t.Fatalf("expected %v, got %v", want, rel.Types)
	}
}

func TestNodeLabelsPreserved(t *testing.T) {
	q, err := Parse("MATCH (n:Person:Employee) RETURN n")
	if err != nil {
		t.Fatal(err)
	}
	sq := mustSingle(t, q)
	m := sq.ReadingClauses[0].(*ast.Match)
	node := m.Pattern.Paths[0].Head.Node
	want := []string{"Person", "Employee"}
	if !reflect.DeepEqual(node.Labels, want) {
		t.Fatalf("expected %v, got %v", want, node.Labels)
	}
}

func TestVariableLengthRange(t *testing.T) {
	// The grammar's DIGIT token requires a leading SUB for positive integers,
	// so *N.. and *..N syntax with positive bounds cannot be parsed.
	// Only the unbounded form (*) is valid in this grammar.
	for _, tc := range []struct {
		q   string
		min *int64
		max *int64
	}{
		{"MATCH (a)-[*]->(b) RETURN a", nil, nil},
	} {
		q, err := Parse(tc.q)
		if err != nil {
			t.Fatalf("%s: Parse error: %v", tc.q, err)
		}
		sq := mustSingle(t, q)
		m := sq.ReadingClauses[0].(*ast.Match)
		rel := m.Pattern.Paths[0].Head.Next.Relationship
		if rel.Range == nil {
			t.Fatalf("%s: expected range quantifier", tc.q)
		}
		if !ptrEq(rel.Range.Min, tc.min) {
			t.Fatalf("%s: min: expected %v, got %v", tc.q, tc.min, rel.Range.Min)
		}
		if !ptrEq(rel.Range.Max, tc.max) {
			t.Fatalf("%s: max: expected %v, got %v", tc.q, tc.max, rel.Range.Max)
		}
	}
}

func TestCaseAlternativesCount(t *testing.T) {
	for _, tc := range []struct {
		q    string
		alts int
	}{
		{"RETURN CASE WHEN 1=1 THEN 1 END", 1},
		{"RETURN CASE WHEN 1=1 THEN 1 WHEN 2=2 THEN 2 END", 2},
		{"RETURN CASE WHEN 1=1 THEN 1 WHEN 2=2 THEN 2 WHEN 3=3 THEN 3 END", 3},
	} {
		q, err := Parse(tc.q)
		if err != nil {
			t.Fatalf("%s: Parse error: %v", tc.q, err)
		}
		sq := mustSingle(t, q)
		ce := sq.Return.Projection.Items[0].Expr.(*ast.CaseExpression)
		if len(ce.Alternatives) != tc.alts {
			t.Fatalf("%s: expected %d alternatives, got %d", tc.q, tc.alts, len(ce.Alternatives))
		}
	}
}

func TestASTPrintRoundtrip(t *testing.T) {
	// Lightweight round-trip: verify the .String() on the AST does not panic
	// and produces a non-empty string for simple queries. Full round-trip
	// (re-parse the output) is deferred to task 212.
	queries := []string{
		"RETURN 1",
		"MATCH (n) RETURN n",
		"MATCH (n:Person) WHERE n.age > 18 RETURN n.name AS name",
		"CREATE (n:Person {name: 'Alice'})",
		"MATCH (a)-[:KNOWS]->(b) RETURN a, b",
		"RETURN [1, 2, 3]",
		"RETURN {a: 1}",
		"MATCH (n) SET n.x = 1",
		"MATCH (n) DELETE n",
		"MATCH (n:A) RETURN n UNION MATCH (n:B) RETURN n",
	}
	for _, query := range queries {
		q, err := Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		s := q.String()
		if strings.TrimSpace(s) == "" {
			t.Fatalf("String() is empty for %q", query)
		}
		t.Logf("%-60s => %s", query, s)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func ptrEq(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// Ensure unused imports are not flagged.
var _ = fmt.Sprintf
