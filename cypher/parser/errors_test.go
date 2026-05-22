package parser

import (
	"strings"
	"testing"

	"gograph/cypher/ast"
)

// TestSemaErrorMessage covers SemaError.Error(), which is otherwise
// only exercised indirectly through Parse() calls that succeed in producing a
// valid AST.
func TestSemaErrorMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *SemaError
		want []string // substrings that must appear in Error()
	}{
		{
			name: "basic message",
			err: &SemaError{
				Rule:    "foreach",
				Pos:     ast.Position{Line: 3, Column: 5, Offset: 42},
				Message: "FOREACH is out of scope",
			},
			want: []string{"foreach", "FOREACH is out of scope", "3", "5"},
		},
		{
			name: "empty rule",
			err: &SemaError{
				Pos:     ast.Position{Line: 1, Column: 0, Offset: 0},
				Message: "unknown",
			},
			want: []string{"sema error", "unknown"},
		},
		{
			name: "zero position",
			err: &SemaError{
				Rule:    "atom",
				Message: "internal",
			},
			want: []string{"atom", "internal"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.err.Error()
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Errorf("Error()=%q; want substring %q", got, sub)
				}
			}
		})
	}
}

// TestParseErrorMessageVariants covers the three formatting branches in
// ParseError.Error():
//   - no Expected set, no offending token → "parse error at L:C: msg"
//   - one Expected token              → "...expected X"
//   - multiple Expected tokens        → "...expected one of {A, B, C}"
//
// These are not all hit by integration tests (which only see one canonical
// form per query), so we exercise them by constructing the error directly.
func TestParseErrorMessageVariants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *ParseError
		want []string
	}{
		{
			name: "empty offending token and empty expected",
			err: &ParseError{
				Line:    7,
				Column:  12,
				Message: "raw antlr",
			},
			want: []string{"parse error", "7:12", "raw antlr"},
		},
		{
			name: "empty offending token and one expected",
			err: &ParseError{
				Line:     1,
				Column:   0,
				Expected: []string{"'RETURN'"},
				Message:  "raw",
			},
			want: []string{"parse error", "1:0", "expected 'RETURN'"},
		},
		{
			name: "offending token alone",
			err: &ParseError{
				Line:           2,
				Column:         3,
				OffendingToken: "FOO",
				Message:        "raw",
			},
			want: []string{"unexpected", "FOO", "2:3"},
		},
		{
			name: "offending token plus multiple expected",
			err: &ParseError{
				Line:           4,
				Column:         5,
				OffendingToken: "BAR",
				Expected:       []string{"'A'", "'B'", "'C'"},
			},
			want: []string{"unexpected", "BAR", "expected one of", "'A'", "'B'", "'C'"},
		},
		{
			name: "offending token plus single expected",
			err: &ParseError{
				Line:           1,
				Column:         9,
				OffendingToken: "FOO",
				Expected:       []string{"'BAR'"},
			},
			want: []string{"unexpected", "FOO", "expected 'BAR'"},
		},
		{
			name: "offending token and no expected and no msg drops trailing colon",
			err: &ParseError{
				Line:           1,
				Column:         1,
				OffendingToken: "X",
			},
			want: []string{"unexpected", "X", "1:1"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.err.Error()
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Errorf("Error()=%q; want substring %q", got, sub)
				}
			}
		})
	}
}

// TestParseErrorIsError ensures *ParseError satisfies the error interface and
// the Error() string is never empty.
func TestParseErrorIsError(t *testing.T) {
	t.Parallel()
	var e error = &ParseError{Line: 1, Column: 0}
	if e.Error() == "" {
		t.Error("Error() returned empty string")
	}
}

// TestSemaErrorIsError ensures *SemaError satisfies the error interface and
// the Error() string is never empty.
func TestSemaErrorIsError(t *testing.T) {
	t.Parallel()
	var e error = &SemaError{Rule: "x"}
	if e.Error() == "" {
		t.Error("Error() returned empty string")
	}
}
