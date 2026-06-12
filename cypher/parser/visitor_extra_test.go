package parser

import (
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// -----------------------------------------------------------------------------
// parseInt helper coverage
// -----------------------------------------------------------------------------

func TestParseIntDispatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "decimal positive", input: "42", want: 42},
		{name: "decimal negative", input: "-7", want: -7},
		{name: "hex lower", input: "0x1A", want: 26},
		{name: "hex upper", input: "0X1A", want: 26},
		{name: "octal lower", input: "0o17", want: 15},
		{name: "octal upper", input: "0O17", want: 15},
		{name: "trimmed whitespace", input: "  42  ", want: 42},
		{name: "decimal overflow returns error", input: "99999999999999999999", wantErr: true},
		{name: "hex overflow returns error", input: "0xFFFFFFFFFFFFFFFFFF", wantErr: true},
		{name: "octal overflow returns error", input: "0o7777777777777777777777", wantErr: true},
		{name: "malformed hex returns error", input: "0xZZ", wantErr: true},
		{name: "malformed octal returns error", input: "0o88", wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseInt(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseInt(%q): err=%v wantErr=%v", tc.input, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("parseInt(%q) = %d; want %d", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// unquoteString helper coverage
// -----------------------------------------------------------------------------

func TestUnquoteString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "too short returns raw", input: `"`, want: `"`},
		{name: "no surrounding quote returns raw", input: "abc", want: "abc"},
		{name: "double-quoted plain", input: `"hello"`, want: "hello"},
		{name: "single-quoted plain", input: `'x'`, want: "x"},
		{name: "double-quoted with escapes", input: `"a\nb"`, want: "a\nb"},
		{name: "double-quoted with tab", input: `"a\tb"`, want: "a\tb"},
		{name: "double-quoted with cr", input: `"a\rb"`, want: "a\rb"},
		{name: "double-quoted with escaped backslash", input: `"a\\b"`, want: `a\b`},
		{name: "double-quoted with escaped double quote", input: `"a\"b"`, want: `a"b`},
		{name: "double-quoted with escaped single quote", input: `"a\'b"`, want: "a'b"},
		{name: "empty quoted", input: `""`, want: ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := unquoteString(tc.input)
			if got != tc.want {
				t.Errorf("unquoteString(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// VisitNumLit overflow / float-fallback / hex-overflow integration tests
// -----------------------------------------------------------------------------

// TestVisitNumLitOverflowReportsSemaError covers the slow-path overflow branch
// in VisitNumLit where a too-long hex literal arrives via Atom (not via the
// integer-decimal path).
func TestVisitNumLitHexOverflowReportsSemaError(t *testing.T) {
	t.Parallel()

	// Use a hex literal that visibly overflows int64 (33 hex digits = ~132 bits).
	// The leading "0x" reaches VisitAtom as an identifier symbol; the atom
	// handler routes hex/octal-prefixed identifiers through parseInt and
	// returns a SemaError on overflow.
	q := "RETURN 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"
	_, err := Parse(q)
	if err == nil {
		t.Fatalf("Parse(%q): expected error, got none", q)
	}
	var se *SemaError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SemaError, got %T: %v", err, err)
	}
	if !strings.Contains(se.Message, "out of range") {
		t.Errorf("Error()=%q; want substring %q", se.Error(), "out of range")
	}
}

// TestVisitNumLitDecimalOverflow covers the decimal-overflow branch where the
// literal has at most 19 digits — the visitor surfaces a SemaError because
// the OverflowIntLit sentinel path only activates above 19 digits.
func TestVisitNumLitDecimalOverflow(t *testing.T) {
	t.Parallel()

	// 9223372036854775808 is INT64_MAX + 1 and has exactly 19 digits.
	// Prefix with '-' so DIGIT lexes it; the actual numeric value is exactly
	// INT64_MIN, which is representable, so it must parse cleanly.
	q := "RETURN -9223372036854775808"
	_, err := Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): unexpected error %v", q, err)
	}

	// A 20-digit pure integer literal (no fractional part) exceeds int64
	// range.  After the #1384 fix the visitor returns a SemaError with the
	// "integer literal out of range" message instead of silently rounding to
	// a float64.
	q2 := "RETURN -99999999999999999999"
	_, err = Parse(q2)
	if err == nil {
		t.Fatalf("Parse(%q): expected IntegerOverflow error, got nil", q2)
	}
	var se *SemaError
	if !errors.As(err, &se) {
		t.Fatalf("Parse(%q): expected *SemaError, got %T: %v", q2, err, err)
	}
	if !strings.Contains(se.Message, "integer literal out of range") {
		t.Fatalf("Parse(%q): expected 'integer literal out of range', got: %v", q2, se.Message)
	}
}

// TestVisitNumLitFloatLiteral verifies float-literal parsing (including
// scientific notation) goes through VisitNumLit's "." branch.
func TestVisitNumLitFloatLiteral(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		want  float64
	}{
		{name: "negative float", query: "RETURN -3.14", want: -3.14},
		{name: "negative scientific", query: "RETURN -1.5e-2", want: -1.5e-2},
		{name: "zero dot rewritten to .5", query: "RETURN 0.5", want: 0.5},
		{name: "leading dot float", query: "RETURN .5", want: 0.5},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, err := Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			sq := q.(*ast.SingleQuery)
			fl, ok := sq.Return.Projection.Items[0].Expr.(*ast.FloatLiteral)
			if !ok {
				t.Fatalf("expected FloatLiteral, got %T", sq.Return.Projection.Items[0].Expr)
			}
			if fl.Value != tc.want {
				t.Errorf("value: got %v want %v", fl.Value, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// VisitAtom hex/octal identifier overflow path
// -----------------------------------------------------------------------------

// TestAtomHexLiteralValid covers the happy-path where a hex literal reaches
// VisitAtom (as an ID-typed Symbol) and is converted to an IntLiteral.
func TestAtomHexLiteralValid(t *testing.T) {
	t.Parallel()

	q, err := Parse("RETURN 0x1A")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sq := q.(*ast.SingleQuery)
	il, ok := sq.Return.Projection.Items[0].Expr.(*ast.IntLiteral)
	if !ok {
		t.Fatalf("expected IntLiteral, got %T", sq.Return.Projection.Items[0].Expr)
	}
	if il.Value != 26 {
		t.Errorf("value: got %d want 26", il.Value)
	}
}

// TestAtomOctalLiteralValid covers the same path for octal.
func TestAtomOctalLiteralValid(t *testing.T) {
	t.Parallel()

	q, err := Parse("RETURN 0o17")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sq := q.(*ast.SingleQuery)
	il, ok := sq.Return.Projection.Items[0].Expr.(*ast.IntLiteral)
	if !ok {
		t.Fatalf("expected IntLiteral, got %T", sq.Return.Projection.Items[0].Expr)
	}
	if il.Value != 15 {
		t.Errorf("value: got %d want 15", il.Value)
	}
}

// -----------------------------------------------------------------------------
// visitRangeLit branch coverage
// -----------------------------------------------------------------------------

func TestVisitRangeLitBranches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		query   string
		wantMin *int64
		wantMax *int64
	}{
		{
			name:    "bare star",
			query:   "MATCH (a)-[*]->(b) RETURN a",
			wantMin: nil,
			wantMax: nil,
		},
		{
			name:    "fixed length 3",
			query:   "MATCH (a)-[*3]->(b) RETURN a",
			wantMin: int64Ptr(3),
			wantMax: int64Ptr(3),
		},
		{
			name:    "range 1..3",
			query:   "MATCH (a)-[*1..3]->(b) RETURN a",
			wantMin: int64Ptr(1),
			wantMax: int64Ptr(3),
		},
		{
			name:    "range 2..",
			query:   "MATCH (a)-[*2..]->(b) RETURN a",
			wantMin: int64Ptr(2),
			wantMax: nil,
		},
		{
			name:    "range ..5",
			query:   "MATCH (a)-[*..5]->(b) RETURN a",
			wantMin: nil,
			wantMax: int64Ptr(5),
		},
		{
			name:    "range ..",
			query:   "MATCH (a)-[*..]->(b) RETURN a",
			wantMin: nil,
			wantMax: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, err := Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			sq := q.(*ast.SingleQuery)
			m := sq.ReadingClauses[0].(*ast.Match)
			rel := m.Pattern.Paths[0].Head.Next.Relationship
			if rel.Range == nil {
				t.Fatalf("expected range quantifier")
			}
			if !int64PtrEq(rel.Range.Min, tc.wantMin) {
				t.Errorf("min: got %v want %v", rel.Range.Min, tc.wantMin)
			}
			if !int64PtrEq(rel.Range.Max, tc.wantMax) {
				t.Errorf("max: got %v want %v", rel.Range.Max, tc.wantMax)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// VisitSubqueryExist / VisitSubqueryCount branch coverage
// -----------------------------------------------------------------------------

func TestSubqueryExistsForms(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
	}{
		{name: "EXISTS regular query", query: "MATCH (n) WHERE EXISTS { MATCH (n)-[:R]->(m) RETURN m } RETURN n"},
		{name: "EXISTS pattern only", query: "MATCH (n) WHERE EXISTS { (n)-[:R]->(m) } RETURN n"},
		{name: "EXISTS pattern with where", query: "MATCH (n) WHERE EXISTS { (n)-[:R]->(m) WHERE m.x > -1 } RETURN n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, err := Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			sq := q.(*ast.SingleQuery)
			m := sq.ReadingClauses[0].(*ast.Match)
			if m.Where == nil {
				t.Fatal("expected WHERE clause")
			}
			if _, ok := m.Where.Predicate.(*ast.ExistsSubquery); !ok {
				t.Fatalf("expected ExistsSubquery predicate, got %T", m.Where.Predicate)
			}
		})
	}
}

func TestSubqueryCountForms(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
	}{
		{name: "COUNT regular query", query: "MATCH (n) WHERE COUNT { MATCH (n)-[:R]->(m) RETURN m } > -1 RETURN n"},
		{name: "COUNT pattern only", query: "MATCH (n) WHERE COUNT { (n)-[:R]->(m) } > -1 RETURN n"},
		{name: "COUNT pattern with where", query: "MATCH (n) WHERE COUNT { (n)-[:R]->(m) WHERE m.x > -1 } > -1 RETURN n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, err := Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			sq := q.(*ast.SingleQuery)
			m := sq.ReadingClauses[0].(*ast.Match)
			if m.Where == nil {
				t.Fatal("expected WHERE clause")
			}
			bo, ok := m.Where.Predicate.(*ast.BinaryOp)
			if !ok {
				t.Fatalf("expected BinaryOp predicate, got %T", m.Where.Predicate)
			}
			if _, ok := bo.Left.(*ast.CountSubquery); !ok {
				t.Fatalf("expected CountSubquery on Left, got %T", bo.Left)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// CALL no-paren normalisation end-to-end
// -----------------------------------------------------------------------------

func TestCallNoParenEndToEnd(t *testing.T) {
	t.Parallel()

	q, err := Parse("CALL db.labels YIELD label RETURN label")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sq := q.(*ast.SingleQuery)
	c, ok := sq.ReadingClauses[0].(*ast.Call)
	if !ok {
		t.Fatalf("expected Call, got %T", sq.ReadingClauses[0])
	}
	if c.Procedure != "labels" {
		t.Errorf("procedure: got %q want %q", c.Procedure, "labels")
	}
	if len(c.Yield) != 1 || c.Yield[0].Name != "label" {
		t.Errorf("yield items: got %+v", c.Yield)
	}
}

// -----------------------------------------------------------------------------
// Round-trip Parse → Print → Parse
// -----------------------------------------------------------------------------

// TestRoundTripParsePrintParse asserts that for a representative corpus of
// valid queries, the AST survives a Print + re-Parse cycle. Structural
// equivalence is asserted by re-parsing the printed form and verifying that
// it yields the same top-level type and clause counts as the original AST.
// We intentionally do NOT compare byte-for-byte; the printer normalises
// formatting (parens, quotes) by design (see ast.Print docs).
func TestRoundTripParsePrintParse(t *testing.T) {
	t.Parallel()

	queries := []string{
		"RETURN -1",
		"RETURN -1.5",
		`RETURN "hello"`,
		"RETURN true",
		"RETURN false",
		"RETURN null",
		"RETURN [-1, -2, -3]",
		`RETURN {a: -1, b: "x"}`,
		"MATCH (n) RETURN n",
		"MATCH (n:Person) RETURN n",
		"MATCH (n:Person:Employee) RETURN n.name",
		"MATCH (a)-[:KNOWS]->(b) RETURN a, b",
		"MATCH (a)-[*]->(b) RETURN a",
		"MATCH (n) WHERE n.age > -18 RETURN n",
		"MATCH (n) WHERE NOT n.x = -1 RETURN n",
		"MATCH (n) WHERE n.x > -1 AND n.y < -1 RETURN n",
		"MATCH (n) RETURN DISTINCT n",
		"MATCH (n) RETURN n ORDER BY n.name DESC",
		"MATCH (n) RETURN n SKIP -1 LIMIT -10",
		"CREATE (n:Person)",
		"MATCH (n) SET n.x = -1",
		"MATCH (n) REMOVE n.x",
		"MATCH (n) DELETE n",
		"MATCH (n) DETACH DELETE n",
		"MATCH (n:A) RETURN n UNION MATCH (n:B) RETURN n",
		"MATCH (n:A) RETURN n UNION ALL MATCH (n:B) RETURN n",
		"RETURN CASE WHEN -1 = -1 THEN -1 ELSE -2 END",
		"WITH -1 AS x RETURN x",
		`MATCH (n) WHERE n.name STARTS WITH "A" RETURN n`,
		`MATCH (n) WHERE n.name ENDS WITH "z" RETURN n`,
		`MATCH (n) WHERE n.name CONTAINS "foo" RETURN n`,
		"MATCH (n) WHERE n.x IS NULL RETURN n",
		"MATCH (n) WHERE n.x IS NOT NULL RETURN n",
		`MATCH (n) WHERE n.x IN [-1, -2, -3] RETURN n`,
		"UNWIND [-1, -2, -3] AS x RETURN x",
	}

	for _, src := range queries {
		src := src
		t.Run(src, func(t *testing.T) {
			t.Parallel()

			q1, err := Parse(src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", src, err)
			}

			printed := ast.Print(q1)
			if strings.TrimSpace(printed) == "" {
				t.Fatalf("Print produced empty string for %q", src)
			}

			q2, err := Parse(printed)
			if err != nil {
				t.Fatalf("Re-Parse failed for printed form\n  src     = %q\n  printed = %q\n  err     = %v",
					src, printed, err)
			}

			// Compare top-level types and clause counts.
			switch a := q1.(type) {
			case *ast.SingleQuery:
				b, ok := q2.(*ast.SingleQuery)
				if !ok {
					t.Fatalf("type mismatch after roundtrip: %T -> %T (printed=%q)", q1, q2, printed)
				}
				if len(a.ReadingClauses) != len(b.ReadingClauses) {
					t.Errorf("ReadingClauses count: got %d want %d", len(b.ReadingClauses), len(a.ReadingClauses))
				}
				if len(a.UpdatingClauses) != len(b.UpdatingClauses) {
					t.Errorf("UpdatingClauses count: got %d want %d", len(b.UpdatingClauses), len(a.UpdatingClauses))
				}
				if (a.Return == nil) != (b.Return == nil) {
					t.Errorf("Return presence: got %v want %v", b.Return != nil, a.Return != nil)
				}
				if len(a.With) != len(b.With) {
					t.Errorf("With count: got %d want %d", len(b.With), len(a.With))
				}
			case *ast.MultiQuery:
				b, ok := q2.(*ast.MultiQuery)
				if !ok {
					t.Fatalf("type mismatch after roundtrip: %T -> %T (printed=%q)", q1, q2, printed)
				}
				if len(a.Parts) != len(b.Parts) {
					t.Errorf("Parts count: got %d want %d", len(b.Parts), len(a.Parts))
				}
				if a.All != b.All {
					t.Errorf("All flag: got %v want %v", b.All, a.All)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Gate tests — #1384: pure integer literals >19 digits must raise IntegerOverflow
// -----------------------------------------------------------------------------

// TestVisitNumLit_LargeIntegerRaisesIntegerOverflow is the gate test for #1384.
// RETURN 10000000000000000000000 AS n must raise IntegerOverflow, not silently
// accept the integer as a rounded float64.
//
// Gate semantics:
//
//	Before fix: Parse succeeds → test fails (expected error, got nil)
//	After fix:  Parse returns *SemaError "integer literal out of range" → test passes
func TestVisitNumLit_LargeIntegerRaisesIntegerOverflow(t *testing.T) {
	t.Parallel()

	// 23 digits, no decimal point — overflows int64, must not be silently rounded.
	_, err := Parse("RETURN 10000000000000000000000 AS n")
	if err == nil {
		t.Fatal("expected IntegerOverflow error for 23-digit integer literal, got nil")
	}
	var se *SemaError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SemaError, got %T: %v", err, err)
	}
	if !strings.Contains(se.Message, "integer literal out of range") {
		t.Fatalf("expected 'integer literal out of range' in message, got: %v", se.Message)
	}
}

// TestVisitNumLit_LongFloatLiteralAccepted verifies that a float literal whose
// integer part exceeds 19 digits (ANTLR splits NNN and .frac) still succeeds.
// This is a non-regression test: it must pass both before and after the #1384 fix.
func TestVisitNumLit_LongFloatLiteralAccepted(t *testing.T) {
	t.Parallel()

	// 23-digit integer part + .0 — this is a valid float literal in openCypher.
	_, err := Parse("RETURN 10000000000000000000000.0 AS n")
	if err != nil {
		t.Fatalf("expected long float literal to parse successfully, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func int64Ptr(v int64) *int64 { return &v }

func int64PtrEq(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
