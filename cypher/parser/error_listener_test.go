package parser

import (
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// errorCase30 — 30+ invalid Cypher queries with diagnostic assertions.
//
// Each entry specifies:
//   - name:         test name
//   - query:        the invalid input
//   - wantLine:     expected error line (1-based)
//   - wantCol:      expected error column (0-based)
//   - wantToken:    non-empty substring that must appear in the offending token
//                   (leave blank if the error is a lex error with no token)
//   - wantInMsg:    substring that must appear in Error() output
// ---------------------------------------------------------------------------

type errorCase30 struct {
	name      string
	query     string
	wantLine  int
	wantCol   int
	wantToken string // substring match against OffendingToken
	wantInMsg string // substring match against Error() output
}

func invalidQueryCases() []errorCase30 {
	return []errorCase30{
		// ---- Missing RETURN clause -------------------------------------------
		{
			name:      "bare_match_no_return",
			query:     "MATCH (n)",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "return_keyword_only",
			query:     "RETURN",
			wantLine:  1,
			wantCol:   6,
			wantInMsg: "parse error",
		},
		{
			name:      "return_distinct_only",
			query:     "RETURN DISTINCT",
			wantLine:  1,
			wantInMsg: "parse error",
		},

		// ---- Unclosed parentheses -------------------------------------------
		{
			name:      "unclosed_node_paren",
			query:     "MATCH (n RETURN n",
			wantLine:  1,
			wantToken: "RETURN",
			wantInMsg: "unexpected",
		},
		{
			name:      "unclosed_expr_paren",
			query:     "RETURN (1 + 2",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "unclosed_list_literal",
			query:     "RETURN [1, 2",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "unclosed_map_literal",
			query:     "RETURN {a: 1",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "unclosed_call_args",
			query:     "CALL db.labels( RETURN n",
			wantLine:  1,
			wantToken: "RETURN",
			wantInMsg: "unexpected",
		},

		// ---- Invalid operator syntax ----------------------------------------
		{
			name:      "dangling_plus",
			query:     "RETURN 1 +",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "dangling_multiply",
			query:     "RETURN 1 *",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "dangling_comparison",
			query:     "MATCH (n) WHERE n.x = RETURN n",
			wantLine:  1,
			wantToken: "RETURN",
			wantInMsg: "unexpected",
		},
		{
			name:      "double_comma_return",
			query:     "RETURN 1,,2",
			wantLine:  1,
			wantToken: ",",
			wantInMsg: "unexpected",
		},
		{
			name:      "double_comma_set",
			query:     "MATCH (n) SET n.x = 1,, n.y = 2",
			wantLine:  1,
			wantToken: ",",
			wantInMsg: "unexpected",
		},
		{
			name:      "trailing_comma_map",
			query:     "RETURN {a: 1,}",
			wantLine:  1,
			wantToken: "}",
			wantInMsg: "unexpected",
		},

		// ---- Missing relationship direction ----------------------------------
		{
			// ANTLR error-recovery replaces the missing '(' and reports the error
			// at RETURN, which is the next token it cannot match.
			name:      "missing_node_parens_in_match",
			query:     "MATCH n RETURN n",
			wantLine:  1,
			wantToken: "RETURN",
			wantInMsg: "unexpected",
		},
		{
			// ANTLR inserts a virtual '(' and the error has no offending symbol.
			name:      "create_no_parens",
			query:     "CREATE n",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			// Same recovery behaviour as create_no_parens.
			name:      "merge_no_parens",
			query:     "MERGE n",
			wantLine:  1,
			wantInMsg: "parse error",
		},

		// ---- Invalid property syntax ----------------------------------------
		{
			name:      "property_access_no_key",
			query:     "RETURN n.",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "empty_label_in_match",
			query:     "MATCH (n:) RETURN n",
			wantLine:  1,
			wantToken: ")",
			wantInMsg: "unexpected",
		},
		{
			name:      "empty_label_in_create",
			query:     "CREATE (n:)",
			wantLine:  1,
			wantToken: ")",
			wantInMsg: "unexpected",
		},
		{
			name:      "set_no_value",
			query:     "MATCH (n) SET n.x =",
			wantLine:  1,
			wantInMsg: "parse error",
		},

		// ---- Reserved keyword misuse / wrong position -----------------------
		{
			name:      "where_after_return",
			query:     "MATCH (n) RETURN n WHERE n.x = 1",
			wantLine:  1,
			wantToken: "WHERE",
			wantInMsg: "unexpected",
		},
		{
			name:      "order_without_by",
			query:     "MATCH (n) RETURN n ORDER",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "return_as_no_alias",
			query:     "RETURN 1 AS",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "on_without_create_match",
			query:     "MERGE (n:Person) ON",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "on_create_without_set",
			query:     "MERGE (n:Person) ON CREATE",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "unwind_no_as",
			query:     "UNWIND [1,2,3] RETURN x",
			wantLine:  1,
			wantToken: "RETURN",
			wantInMsg: "unexpected",
		},
		{
			name:      "unwind_no_expr",
			query:     "UNWIND AS x RETURN x",
			wantLine:  1,
			wantToken: "AS",
			wantInMsg: "unexpected",
		},

		// ---- WHERE clause errors --------------------------------------------
		{
			name:      "where_no_predicate",
			query:     "MATCH (n) WHERE RETURN n",
			wantLine:  1,
			wantToken: "RETURN",
			wantInMsg: "unexpected",
		},
		{
			name:      "and_no_left_side",
			query:     "MATCH (n) WHERE AND n.x = 1 RETURN n",
			wantLine:  1,
			wantToken: "AND",
			wantInMsg: "unexpected",
		},
		{
			name:      "and_no_right_side",
			query:     "MATCH (n) WHERE n.x = 1 AND RETURN n",
			wantLine:  1,
			wantToken: "RETURN",
			wantInMsg: "unexpected",
		},
		{
			name:      "not_no_operand",
			query:     "MATCH (n) WHERE NOT RETURN n",
			wantLine:  1,
			wantToken: "RETURN",
			wantInMsg: "unexpected",
		},

		// ---- DELETE / REMOVE / SET errors -----------------------------------
		{
			name:      "delete_no_expr",
			query:     "MATCH (n) DELETE",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "remove_no_item",
			query:     "MATCH (n) REMOVE",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "set_no_item",
			query:     "MATCH (n) SET",
			wantLine:  1,
			wantInMsg: "parse error",
		},

		// ---- UNION errors ---------------------------------------------------
		{
			name:      "union_no_second_query",
			query:     "RETURN 1 UNION",
			wantLine:  1,
			wantInMsg: "parse error",
		},
		{
			name:      "union_all_no_second_query",
			query:     "RETURN 1 UNION ALL",
			wantLine:  1,
			wantInMsg: "parse error",
		},

		// ---- CALL errors ----------------------------------------------------
		{
			name:      "bare_call",
			query:     "CALL",
			wantLine:  1,
			wantInMsg: "parse error",
		},

		// ---- Completely invalid input ---------------------------------------
		{
			// "THIS" is a valid ID token so the offending token is populated,
			// producing the "unexpected" format rather than the "parse error" format.
			name:      "completely_invalid",
			query:     "THIS IS NOT CYPHER",
			wantLine:  1,
			wantToken: "THIS",
			wantInMsg: "unexpected",
		},
		{
			name:      "two_consecutive_exprs",
			query:     "RETURN 1 2",
			wantLine:  1,
			wantToken: "2",
			wantInMsg: "unexpected",
		},
		{
			name:      "double_union",
			query:     "MATCH (n) RETURN n UNION UNION RETURN n",
			wantLine:  1,
			wantToken: "UNION",
			wantInMsg: "unexpected",
		},

		// ---- CASE errors ----------------------------------------------------
		{
			name:      "when_no_condition",
			query:     "RETURN CASE WHEN THEN 1 END",
			wantLine:  1,
			wantToken: "THEN",
			wantInMsg: "unexpected",
		},
		{
			name:      "when_no_then",
			query:     "RETURN CASE WHEN 1 = 1 END",
			wantLine:  1,
			wantToken: "END",
			wantInMsg: "unexpected",
		},

		// ---- Multi-line: error on line 2 ------------------------------------
		{
			name:      "multiline_error_line2",
			query:     "MATCH (n)\nRETURN",
			wantLine:  2,
			wantInMsg: "parse error",
		},
		{
			name:      "multiline_unclosed_paren_line2",
			query:     "MATCH\n(n RETURN n",
			wantLine:  2,
			wantToken: "RETURN",
			wantInMsg: "unexpected",
		},
	}
}

// TestInvalidQueryErrorListener verifies that the enhanced errorListener
// produces:
//  1. A [*ParseError] (not nil) for each invalid query.
//  2. The error is on the expected line.
//  3. OffendingToken contains the expected substring (when non-empty in the case).
//  4. Error() output contains the expected substring.
func TestInvalidQueryErrorListener(t *testing.T) {
	cases := invalidQueryCases()
	if len(cases) < 30 {
		t.Fatalf("need at least 30 invalid-query cases, have %d", len(cases))
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tc.query)
			if err == nil {
				t.Fatalf("Parse(%q) expected an error, got nil", tc.query)
			}

			var pe *ParseError
			if !errors.As(err, &pe) {
				// SemaErrors are acceptable only when no ParseError is expected.
				// All cases in this table should produce ParseError.
				t.Fatalf("expected *ParseError, got %T: %v", err, err)
			}

			// Line check.
			if pe.Line != tc.wantLine {
				t.Errorf("line: want %d, got %d (err=%v)", tc.wantLine, pe.Line, pe)
			}

			// Column check (only when non-zero in the case).
			if tc.wantCol != 0 && pe.Column != tc.wantCol {
				t.Errorf("column: want %d, got %d (err=%v)", tc.wantCol, pe.Column, pe)
			}

			// OffendingToken substring check.
			if tc.wantToken != "" && !strings.Contains(pe.OffendingToken, tc.wantToken) {
				t.Errorf("offending token: want substring %q, got %q", tc.wantToken, pe.OffendingToken)
			}

			// Error() output substring check.
			if tc.wantInMsg != "" && !strings.Contains(pe.Error(), tc.wantInMsg) {
				t.Errorf("Error() message: want substring %q, got:\n  %s", tc.wantInMsg, pe.Error())
			}
		})
	}
}

// TestMultipleErrorsPerQuery verifies that ParseStrict collects more than one
// error when the input contains multiple independent syntax problems separated
// by a statement boundary (';'), which prevents ANTLR's single-shot recovery
// from swallowing the second error.
func TestMultipleErrorsPerQuery(t *testing.T) {
	// Two separate syntactic problems in one input: a bare RETURN before ';',
	// then another bare RETURN after ';'. Each is an independent error.
	query := "RETURN , ; RETURN ,"
	_, errs := ParseStrict(query)
	if len(errs) < 2 {
		t.Fatalf("expected at least 2 errors for %q, got %d: %v", query, len(errs), errs)
	}
	for i, e := range errs {
		var pe *ParseError
		if !errors.As(e, &pe) {
			t.Errorf("error[%d]: expected *ParseError, got %T: %v", i, e, e)
		}
	}
}

// TestParseStrictReturnsAllErrors verifies the contract that ParseStrict
// returns nil for a valid query.
func TestParseStrictReturnsAllErrors(t *testing.T) {
	q, errs := ParseStrict("MATCH (n) RETURN n")
	if errs != nil {
		t.Fatalf("expected no errors for valid query, got: %v", errs)
	}
	if q == nil {
		t.Fatal("expected non-nil AST for valid query")
	}
}

// TestErrorMessageFormat verifies that error messages for queries with a known
// offending token match the expected human-readable format.
func TestErrorMessageFormat(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantFmt string // a substring of Error() that demonstrates the format
	}{
		{
			name:    "unexpected_token_format",
			query:   "MATCH (n RETURN n",
			wantFmt: "unexpected",
		},
		{
			name:    "error_includes_line",
			query:   "MATCH (n RETURN n",
			wantFmt: "1:",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.query)
			if err == nil {
				t.Fatalf("Parse(%q): expected error, got nil", tc.query)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantFmt) {
				t.Errorf("Error() = %q; want substring %q", msg, tc.wantFmt)
			}
		})
	}
}

// TestOffendingTokenNonEmpty verifies that for parser errors (not lex errors)
// involving a concrete mismatched keyword, OffendingToken is populated.
func TestOffendingTokenNonEmpty(t *testing.T) {
	// RETURN is mismatched where the grammar expects something else.
	queries := []struct {
		query string
		token string
	}{
		{"MATCH (n RETURN n", "RETURN"},
		{"RETURN 1,,2", ","},
		{"MATCH (n) RETURN n WHERE n.x = 1", "WHERE"},
	}
	for _, tc := range queries {
		_, err := Parse(tc.query)
		if err == nil {
			t.Fatalf("Parse(%q): expected error, got nil", tc.query)
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("expected *ParseError, got %T", err)
		}
		if !strings.Contains(pe.OffendingToken, tc.token) {
			t.Errorf("query %q: OffendingToken=%q, want substring %q", tc.query, pe.OffendingToken, tc.token)
		}
	}
}

// TestAsParseErrors verifies the convenience splitter.
func TestAsParseErrors(t *testing.T) {
	_, errs := ParseStrict("RETURN , ,")
	if len(errs) == 0 {
		t.Fatal("expected errors")
	}
	pes, other := AsParseErrors(errs)
	if len(pes) == 0 {
		t.Error("expected at least one *ParseError")
	}
	_ = other // may or may not be empty
}
