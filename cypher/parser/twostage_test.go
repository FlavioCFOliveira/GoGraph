package parser

// twostage_test.go — the equivalence gate for two-stage parsing (rmp #2850,
// sprint 362 parsing campaign).
//
// [ParseStatement] now parses under [antlr.BailErrorStrategy] first and
// re-parses under the DefaultErrorStrategy only when the first stage does not
// accept. Two properties have to hold, and neither is provable by inspection:
//
//	ACCEPTANCE   the bailing stage accepts exactly the statements the full
//	             strategy accepts, and builds the same tree for them.
//	             TestBailStageAgreesWithFullPass proves it over the corpus, over
//	             a table of invalid statements, and over every one-token prefix
//	             of every corpus statement — 2 000-odd inputs whose acceptance
//	             is not known in advance, which is what makes the sweep a test
//	             rather than a restatement.
//
//	DIAGNOSTICS  every statement that failed before fails with a BYTE-IDENTICAL
//	             *ParseError now. TestParseErrorsUnchanged proves it against
//	             values captured from the pre-change code and written in below
//	             as literals, field by field.
//
// The literals were emitted by running the pre-change ParseStatement over the
// table at commit 49cc8bb5c6c281796a8dab2649ee9aca575203f0. They are not
// derived from the current code, so a regression cannot update them silently.

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser/gen"
)

// ---------------------------------------------------------------------------
// Diagnostics: field-for-field equality with the pre-change values
// ---------------------------------------------------------------------------

// parseErrorGolden is one invalid statement and the *ParseError the pre-change
// ParseStatement returned for it. want is compared field by field, and text is
// the Error() rendering, so a change to either the fields or the formatting is
// caught.
type parseErrorGolden struct {
	name  string
	query string
	want  ParseError
	text  string
}

// parseErrorGoldens spans every route into the error path: the ERRCHAR
// promotion (five characters, including the stray `~` the `=~` exemption must
// NOT cover), unterminated literals of all three quoting styles, plain parser
// errors of every reported shape (mismatched input, no viable alternative,
// extraneous input, missing token), the empty input, and the incomplete-WITH
// input that drives antlr4-go v4.13.1 into the unchecked type assertion
// [recoverParseScript] guards.
var parseErrorGoldens = []parseErrorGolden{
	{
		name:  "errchar_bang_neq",
		query: "MATCH (n) WHERE n.v != 2 RETURN n",
		want: ParseError{
			OffendingToken: "!",
			Message:        "unrecognised character",
			Expected:       nil,
			Line:           1,
			Column:         20,
		},
		text: "unexpected \"!\" at 1:20: unrecognised character",
	},
	{
		name:  "errchar_label_negation",
		query: "MATCH (n:!A) RETURN n",
		want: ParseError{
			OffendingToken: "!",
			Message:        "unrecognised character",
			Expected:       nil,
			Line:           1,
			Column:         9,
		},
		text: "unexpected \"!\" at 1:9: unrecognised character",
	},
	{
		name:  "errchar_hash",
		query: "MATCH (n) RETURN n # trailing",
		want: ParseError{
			OffendingToken: "#",
			Message:        "unrecognised character",
			Expected:       nil,
			Line:           1,
			Column:         19,
		},
		text: "unexpected \"#\" at 1:19: unrecognised character",
	},
	{
		name:  "errchar_ampersand",
		query: "MATCH (n:A&B) RETURN n",
		want: ParseError{
			OffendingToken: "&",
			Message:        "unrecognised character",
			Expected:       nil,
			Line:           1,
			Column:         10,
		},
		text: "unexpected \"&\" at 1:10: unrecognised character",
	},
	{
		name:  "errchar_stray_tilde",
		query: "MATCH (n) WHERE n.a ~ 1 RETURN n",
		want: ParseError{
			OffendingToken: "~",
			Message:        "unrecognised character",
			Expected:       nil,
			Line:           1,
			Column:         20,
		},
		text: "unexpected \"~\" at 1:20: unrecognised character",
	},
	{
		name:  "unterminated_double",
		query: `MATCH (n) WHERE n.name = "abc RETURN n`,
		want: ParseError{
			OffendingToken: "\"",
			Message:        "unterminated string literal",
			Expected:       nil,
			Line:           1,
			Column:         25,
		},
		text: "unexpected \"\\\"\" at 1:25: unterminated string literal",
	},
	{
		name:  "unterminated_single",
		query: `MATCH (n) WHERE n.name = 'abc RETURN n`,
		want: ParseError{
			OffendingToken: "'",
			Message:        "unterminated string literal",
			Expected:       nil,
			Line:           1,
			Column:         25,
		},
		text: "unexpected \"'\" at 1:25: unterminated string literal",
	},
	{
		name:  "unterminated_backtick",
		query: "MATCH (n:`Lab RETURN n",
		want: ParseError{
			OffendingToken: "`",
			Message:        "no viable alternative at input 'MATCH (n:`'",
			Expected: []string{"'CALL'", "'YIELD'", "'CREATE'", "'DELETE'", "'DESC'", "'DETACH'",
				"'EXISTS'", "'MATCH'", "'MERGE'", "'ON'", "'OPTIONAL'", "'ORDER'", "'REMOVE'",
				"'RETURN'", "'SET'", "'SKIP'", "'WITH'", "'UNION'", "'UNWIND'", "'AND'",
				"'FOREACH'", "'EXPLAIN'"},
			Line:   1,
			Column: 9,
		},
		text: "unexpected \"`\" at 1:9, expected one of {'CALL', 'YIELD', 'CREATE', 'DELETE', 'DESC', 'DETACH', 'EXISTS', 'MATCH', 'MERGE', 'ON', 'OPTIONAL', 'ORDER', 'REMOVE', 'RETURN', 'SET', 'SKIP', 'WITH', 'UNION', 'UNWIND', 'AND', 'FOREACH', 'EXPLAIN'}",
	},
	{
		name:  "parse_missing_projection",
		query: "MATCH (n) RETURN",
		want: ParseError{
			OffendingToken: "",
			Message:        "mismatched input '<EOF>' expecting {'(', '{', '[', '-', '+', '*', '$', 'FILTER', 'EXTRACT', 'COUNT', 'ANY', 'NONE', 'SINGLE', 'ALL', 'EXISTS', 'DISTINCT', 'NOT', 'FALSE', 'TRUE', 'NULL', 'CASE', 'EXPLAIN', 'PROFILE', ID, ESC_LITERAL, CHAR_LITERAL, STRING_LITERAL, DIGIT}",
			Expected: []string{"'('", "')'", "'{'", "'}'", "'['", "']'", "'-'", "'+'", "'/'", "'*'",
				"'`'", "'$'", "'CALL'", "'FILTER'", "'EXTRACT'", "'COUNT'", "'ANY'", "'NONE'",
				"'SINGLE'", "'ALL'", "'ASC'", "'EXISTS'", "'LIMIT'", "'DISTINCT'", "'ENDS'",
				"'NOT'", "'OR'", "'FALSE'", "'TRUE'", "'NULL'", "'CONSTRAINT'", "'CASE'",
				"'WHEN'", "'EXPLAIN'", "'PROFILE'", "ID", "ESC_LITERAL", "CHAR_LITERAL",
				"STRING_LITERAL", "DIGIT", "FLOAT"},
			Line:   1,
			Column: 16,
		},
		text: "parse error at 1:16, expected one of {'(', ')', '{', '}', '[', ']', '-', '+', '/', '*', '`', '$', 'CALL', 'FILTER', 'EXTRACT', 'COUNT', 'ANY', 'NONE', 'SINGLE', 'ALL', 'ASC', 'EXISTS', 'LIMIT', 'DISTINCT', 'ENDS', 'NOT', 'OR', 'FALSE', 'TRUE', 'NULL', 'CONSTRAINT', 'CASE', 'WHEN', 'EXPLAIN', 'PROFILE', ID, ESC_LITERAL, CHAR_LITERAL, STRING_LITERAL, DIGIT, FLOAT}",
	},
	{
		name:  "parse_unclosed_paren",
		query: "MATCH (n RETURN n",
		want: ParseError{
			OffendingToken: "RETURN",
			Message:        "no viable alternative at input 'MATCH (n RETURN'",
			Expected: []string{"'CALL'", "'YIELD'", "'CREATE'", "'DELETE'", "'DESC'", "'DETACH'",
				"'EXISTS'", "'MATCH'", "'MERGE'", "'ON'", "'OPTIONAL'", "'ORDER'", "'REMOVE'",
				"'RETURN'", "'SET'", "'SKIP'", "'WITH'", "'UNION'", "'UNWIND'", "'AND'",
				"'FOREACH'", "'EXPLAIN'"},
			Line:   1,
			Column: 9,
		},
		text: "unexpected \"RETURN\" at 1:9, expected one of {'CALL', 'YIELD', 'CREATE', 'DELETE', 'DESC', 'DETACH', 'EXISTS', 'MATCH', 'MERGE', 'ON', 'OPTIONAL', 'ORDER', 'REMOVE', 'RETURN', 'SET', 'SKIP', 'WITH', 'UNION', 'UNWIND', 'AND', 'FOREACH', 'EXPLAIN'}",
	},
	{
		name:  "parse_dangling_plus",
		query: "RETURN 1 +",
		want: ParseError{
			OffendingToken: "",
			Message:        "mismatched input '<EOF>' expecting {'(', '{', '[', '-', '+', '$', 'FILTER', 'EXTRACT', 'COUNT', 'ANY', 'NONE', 'SINGLE', 'ALL', 'EXISTS', 'FALSE', 'TRUE', 'NULL', 'CASE', 'EXPLAIN', 'PROFILE', ID, ESC_LITERAL, CHAR_LITERAL, STRING_LITERAL, DIGIT}",
			Expected: []string{"'('", "')'", "'{'", "'}'", "'['", "']'", "'-'", "'+'", "'/'", "'$'",
				"'CALL'", "'FILTER'", "'EXTRACT'", "'COUNT'", "'ANY'", "'NONE'", "'SINGLE'",
				"'ALL'", "'ASC'", "'EXISTS'", "'LIMIT'", "'FALSE'", "'TRUE'", "'NULL'",
				"'CONSTRAINT'", "'CASE'", "'WHEN'", "'EXPLAIN'", "'PROFILE'", "ID",
				"ESC_LITERAL", "CHAR_LITERAL", "STRING_LITERAL", "DIGIT", "FLOAT"},
			Line:   1,
			Column: 10,
		},
		text: "parse error at 1:10, expected one of {'(', ')', '{', '}', '[', ']', '-', '+', '/', '$', 'CALL', 'FILTER', 'EXTRACT', 'COUNT', 'ANY', 'NONE', 'SINGLE', 'ALL', 'ASC', 'EXISTS', 'LIMIT', 'FALSE', 'TRUE', 'NULL', 'CONSTRAINT', 'CASE', 'WHEN', 'EXPLAIN', 'PROFILE', ID, ESC_LITERAL, CHAR_LITERAL, STRING_LITERAL, DIGIT, FLOAT}",
	},
	{
		name:  "parse_set_no_target",
		query: "CREATE (n) SET",
		want: ParseError{
			OffendingToken: "",
			Message:        "no viable alternative at input 'CREATE (n) SET'",
			Expected: []string{"'CALL'", "'YIELD'", "'CREATE'", "'DELETE'", "'DESC'", "'DETACH'",
				"'EXISTS'", "'MATCH'", "'MERGE'", "'ON'", "'OPTIONAL'", "'ORDER'", "'REMOVE'",
				"'RETURN'", "'SET'", "'SKIP'", "'WITH'", "'UNION'", "'UNWIND'", "'AND'",
				"'FOREACH'", "'EXPLAIN'"},
			Line:   1,
			Column: 14,
		},
		text: "parse error at 1:14, expected one of {'CALL', 'YIELD', 'CREATE', 'DELETE', 'DESC', 'DETACH', 'EXISTS', 'MATCH', 'MERGE', 'ON', 'OPTIONAL', 'ORDER', 'REMOVE', 'RETURN', 'SET', 'SKIP', 'WITH', 'UNION', 'UNWIND', 'AND', 'FOREACH', 'EXPLAIN'}",
	},
	{
		name:  "parse_not_cypher",
		query: "FOO BAR BAZ",
		want: ParseError{
			OffendingToken: "FOO",
			Message:        "mismatched input 'FOO' expecting {'CALL', 'CREATE', 'DELETE', 'DETACH', 'MATCH', 'MERGE', 'OPTIONAL', 'REMOVE', 'RETURN', 'SET', 'WITH', 'UNWIND', 'FOREACH', 'EXPLAIN', 'PROFILE'}",
			Expected: []string{"'CALL'", "'YIELD'", "'CREATE'", "'DELETE'", "'DESC'", "'DETACH'",
				"'EXISTS'", "'MATCH'", "'MERGE'", "'ON'", "'OPTIONAL'", "'ORDER'", "'REMOVE'",
				"'RETURN'", "'SET'", "'SKIP'", "'WITH'", "'UNION'", "'UNWIND'", "'AND'",
				"'FOREACH'", "'EXPLAIN'", "'PROFILE'", "ID"},
			Line:   1,
			Column: 0,
		},
		text: "unexpected \"FOO\" at 1:0, expected one of {'CALL', 'YIELD', 'CREATE', 'DELETE', 'DESC', 'DETACH', 'EXISTS', 'MATCH', 'MERGE', 'ON', 'OPTIONAL', 'ORDER', 'REMOVE', 'RETURN', 'SET', 'SKIP', 'WITH', 'UNION', 'UNWIND', 'AND', 'FOREACH', 'EXPLAIN', 'PROFILE', ID}",
	},
	{
		name:  "parse_extra_paren",
		query: "MATCH (n)) RETURN n",
		want: ParseError{
			OffendingToken: ")",
			Message:        "no viable alternative at input 'MATCH (n))'",
			Expected: []string{"'CALL'", "'YIELD'", "'CREATE'", "'DELETE'", "'DESC'", "'DETACH'",
				"'EXISTS'", "'MATCH'", "'MERGE'", "'ON'", "'OPTIONAL'", "'ORDER'", "'REMOVE'",
				"'RETURN'", "'SET'", "'SKIP'", "'WITH'", "'UNION'", "'UNWIND'", "'AND'",
				"'FOREACH'", "'EXPLAIN'"},
			Line:   1,
			Column: 9,
		},
		text: "unexpected \")\" at 1:9, expected one of {'CALL', 'YIELD', 'CREATE', 'DELETE', 'DESC', 'DETACH', 'EXISTS', 'MATCH', 'MERGE', 'ON', 'OPTIONAL', 'ORDER', 'REMOVE', 'RETURN', 'SET', 'SKIP', 'WITH', 'UNION', 'UNWIND', 'AND', 'FOREACH', 'EXPLAIN'}",
	},
	{
		name:  "parse_order_without_by",
		query: "MATCH (n) RETURN n ORDER",
		want: ParseError{
			OffendingToken: "",
			Message:        "mismatched input '<EOF>' expecting 'BY'",
			Expected:       []string{"'BY'", "'CREATE'"},
			Line:           1,
			Column:         24,
		},
		text: "parse error at 1:24, expected one of {'BY', 'CREATE'}",
	},
	{
		name:  "parse_empty_projection",
		query: "RETURN ,",
		want: ParseError{
			OffendingToken: ",",
			Message:        "mismatched input ',' expecting {'(', '{', '[', '-', '+', '*', '$', 'FILTER', 'EXTRACT', 'COUNT', 'ANY', 'NONE', 'SINGLE', 'ALL', 'EXISTS', 'DISTINCT', 'NOT', 'FALSE', 'TRUE', 'NULL', 'CASE', 'EXPLAIN', 'PROFILE', ID, ESC_LITERAL, CHAR_LITERAL, STRING_LITERAL, DIGIT}",
			Expected: []string{"'('", "')'", "'{'", "'}'", "'['", "']'", "'-'", "'+'", "'/'", "'*'",
				"'`'", "'$'", "'CALL'", "'FILTER'", "'EXTRACT'", "'COUNT'", "'ANY'", "'NONE'",
				"'SINGLE'", "'ALL'", "'ASC'", "'EXISTS'", "'LIMIT'", "'DISTINCT'", "'ENDS'",
				"'NOT'", "'OR'", "'FALSE'", "'TRUE'", "'NULL'", "'CONSTRAINT'", "'CASE'",
				"'WHEN'", "'EXPLAIN'", "'PROFILE'", "ID", "ESC_LITERAL", "CHAR_LITERAL",
				"STRING_LITERAL", "DIGIT", "FLOAT"},
			Line:   1,
			Column: 7,
		},
		text: "unexpected \",\" at 1:7, expected one of {'(', ')', '{', '}', '[', ']', '-', '+', '/', '*', '`', '$', 'CALL', 'FILTER', 'EXTRACT', 'COUNT', 'ANY', 'NONE', 'SINGLE', 'ALL', 'ASC', 'EXISTS', 'LIMIT', 'DISTINCT', 'ENDS', 'NOT', 'OR', 'FALSE', 'TRUE', 'NULL', 'CONSTRAINT', 'CASE', 'WHEN', 'EXPLAIN', 'PROFILE', ID, ESC_LITERAL, CHAR_LITERAL, STRING_LITERAL, DIGIT, FLOAT}",
	},
	{
		// The input that drives antlr4-go v4.13.1's DefaultErrorStrategy into
		// an unchecked type assertion. The first stage does NOT panic on it,
		// the second stage does, and [recoverParseScript] converts it — so this
		// case also proves the retry reproduces a GENUINE runtime panic rather
		// than swallowing it.
		name:  "parse_panic_with_return",
		query: "MATCH (n) WITH RETURN n",
		want: ParseError{
			OffendingToken: "",
			Message:        "parser panic: interface conversion: antlr.Transition is *antlr.EpsilonTransition, not *antlr.RuleTransition",
			Expected:       nil,
			Line:           0,
			Column:         0,
		},
		text: "parse error at 0:0: parser panic: interface conversion: antlr.Transition is *antlr.EpsilonTransition, not *antlr.RuleTransition",
	},
	{
		name:  "empty_input",
		query: "",
		want: ParseError{
			OffendingToken: "",
			Message:        "mismatched input '<EOF>' expecting {'CALL', 'CREATE', 'DELETE', 'DETACH', 'MATCH', 'MERGE', 'OPTIONAL', 'REMOVE', 'RETURN', 'SET', 'WITH', 'UNWIND', 'FOREACH', 'EXPLAIN', 'PROFILE'}",
			Expected: []string{"'CALL'", "'YIELD'", "'CREATE'", "'DELETE'", "'DESC'", "'DETACH'",
				"'EXISTS'", "'MATCH'", "'MERGE'", "'ON'", "'OPTIONAL'", "'ORDER'", "'REMOVE'",
				"'RETURN'", "'SET'", "'SKIP'", "'WITH'", "'UNION'", "'UNWIND'", "'AND'",
				"'FOREACH'", "'EXPLAIN'", "'PROFILE'", "ID"},
			Line:   1,
			Column: 0,
		},
		text: "parse error at 1:0, expected one of {'CALL', 'YIELD', 'CREATE', 'DELETE', 'DESC', 'DETACH', 'EXISTS', 'MATCH', 'MERGE', 'ON', 'OPTIONAL', 'ORDER', 'REMOVE', 'RETURN', 'SET', 'SKIP', 'WITH', 'UNION', 'UNWIND', 'AND', 'FOREACH', 'EXPLAIN', 'PROFILE', ID}",
	},
	{
		name:  "parse_unclosed_list",
		query: "RETURN [1, 2",
		want: ParseError{
			OffendingToken: "",
			Message:        "missing ']' at '<EOF>'",
			Expected:       []string{"']'", "'-'"},
			Line:           1,
			Column:         12,
		},
		text: "parse error at 1:12, expected one of {']', '-'}",
	},
	{
		name:  "parse_case_no_cond",
		query: "RETURN CASE WHEN THEN 1 END",
		want: ParseError{
			OffendingToken: "THEN",
			Message:        "extraneous input 'THEN' expecting {'(', '{', '[', '-', '+', '$', 'FILTER', 'EXTRACT', 'COUNT', 'ANY', 'NONE', 'SINGLE', 'ALL', 'EXISTS', 'NOT', 'FALSE', 'TRUE', 'NULL', 'CASE', 'EXPLAIN', 'PROFILE', ID, ESC_LITERAL, CHAR_LITERAL, STRING_LITERAL, DIGIT}",
			Expected: []string{"'('", "')'", "'{'", "'}'", "'['", "']'", "'-'", "'+'", "'/'", "'$'",
				"'CALL'", "'FILTER'", "'EXTRACT'", "'COUNT'", "'ANY'", "'NONE'", "'SINGLE'",
				"'ALL'", "'ASC'", "'EXISTS'", "'LIMIT'", "'NOT'", "'OR'", "'FALSE'", "'TRUE'",
				"'NULL'", "'CONSTRAINT'", "'CASE'", "'WHEN'", "'EXPLAIN'", "'PROFILE'", "ID",
				"ESC_LITERAL", "CHAR_LITERAL", "STRING_LITERAL", "DIGIT", "FLOAT"},
			Line:   1,
			Column: 17,
		},
		text: "unexpected \"THEN\" at 1:17, expected one of {'(', ')', '{', '}', '[', ']', '-', '+', '/', '$', 'CALL', 'FILTER', 'EXTRACT', 'COUNT', 'ANY', 'NONE', 'SINGLE', 'ALL', 'ASC', 'EXISTS', 'LIMIT', 'NOT', 'OR', 'FALSE', 'TRUE', 'NULL', 'CONSTRAINT', 'CASE', 'WHEN', 'EXPLAIN', 'PROFILE', ID, ESC_LITERAL, CHAR_LITERAL, STRING_LITERAL, DIGIT, FLOAT}",
	},
	{
		name:  "parse_nul_bytes",
		query: "\x00\x01\x02\x03",
		want: ParseError{
			OffendingToken: "\x00",
			Message:        "unrecognised character",
			Expected:       nil,
			Line:           1,
			Column:         0,
		},
		text: "unexpected \"\\x00\" at 1:0: unrecognised character",
	},
}

// TestParseErrorsUnchanged asserts every field of the *ParseError
// [ParseStatement] returns is what the pre-change code returned.
func TestParseErrorsUnchanged(t *testing.T) {
	if len(parseErrorGoldens) < 10 {
		t.Fatalf("the table carries %d statements; the gate requires at least 10",
			len(parseErrorGoldens))
	}
	for _, g := range parseErrorGoldens {
		t.Run(g.name, func(t *testing.T) {
			_, mode, err := ParseStatement(g.query)
			if err == nil {
				t.Fatalf("%q parses cleanly; it used to fail with %q", g.query, g.text)
			}
			if mode != PlanModeNone {
				t.Errorf("PlanMode = %v, want PlanModeNone on the error path", mode)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("error is %T (%v), want *ParseError", err, err)
			}
			// errors.As also matches a WRAPPED *ParseError, which would change
			// what a caller sees from Error(). Pin the returned error itself.
			if got := err.Error(); got != g.text {
				t.Errorf("returned error renders as\n  %q\nwant\n  %q", got, g.text)
			}
			if pe.OffendingToken != g.want.OffendingToken {
				t.Errorf("OffendingToken = %q, want %q", pe.OffendingToken, g.want.OffendingToken)
			}
			if pe.Message != g.want.Message {
				t.Errorf("Message = %q,\n  want %q", pe.Message, g.want.Message)
			}
			if pe.Line != g.want.Line {
				t.Errorf("Line = %d, want %d", pe.Line, g.want.Line)
			}
			if pe.Column != g.want.Column {
				t.Errorf("Column = %d, want %d", pe.Column, g.want.Column)
			}
			if !reflect.DeepEqual(pe.Expected, g.want.Expected) {
				t.Errorf("Expected =\n  %q\nwant\n  %q", pe.Expected, g.want.Expected)
			}
			if got := pe.Error(); got != g.text {
				t.Errorf("Error() =\n  %q\nwant\n  %q", got, g.text)
			}
			// Parse must report the same value: it delegates to ParseStatement.
			if _, err2 := Parse(g.query); !reflect.DeepEqual(err2, err) {
				t.Errorf("Parse returned %v, ParseStatement returned %v", err2, err)
			}
		})
	}
}

// twoStageValid are statements the two-stage path must keep ACCEPTING. The
// regex pair is the `=~` combiner exemption: `~` reaches ERRCHAR, and
// [isRegexCombiner] exempts exactly the one that completes the operator — the
// stray `~` in parseErrorGoldens proves the exemption is not blanket.
var twoStageValid = []struct{ name, query string }{
	{"regex_literal", "MATCH (n) WHERE n.a =~ '.*' RETURN n"},
	{"regex_param", "MATCH (n) WHERE n.a =~ $p RETURN n"},
	{"explain_prefix", "EXPLAIN MATCH (n) RETURN n"},
	{"profile_prefix", "PROFILE MATCH (n) RETURN n"},
	{"explain_identifier", "RETURN 1 AS explain"},
	{"shortest_path", "MATCH p = shortestPath((a)-[*..5]-(b)) RETURN p"},
	{"trailing_semicolon", "MATCH (n) RETURN n;"},
}

// TestTwoStageAcceptedStatementsUnchanged asserts the accepting path still
// accepts, and still reports the statement's PlanMode.
func TestTwoStageAcceptedStatementsUnchanged(t *testing.T) {
	for _, v := range twoStageValid {
		t.Run(v.name, func(t *testing.T) {
			q, mode, err := ParseStatement(v.query)
			if err != nil {
				t.Fatalf("%q: %v", v.query, err)
			}
			if q == nil {
				t.Fatalf("%q: nil AST with no error", v.query)
			}
			wantMode := PlanModeNone
			switch {
			case strings.HasPrefix(v.query, "EXPLAIN "):
				wantMode = PlanModeExplain
			case strings.HasPrefix(v.query, "PROFILE "):
				wantMode = PlanModeProfile
			}
			if mode != wantMode {
				t.Errorf("PlanMode = %v, want %v", mode, wantMode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Acceptance: the bailing stage accepts exactly what the full strategy accepts
// ---------------------------------------------------------------------------

// cypherRuleNames is the generated parser's rule vocabulary, read once. It is
// all [antlr.TreesStringTree] needs to name a rule node, so neither renderer
// below has to build a parser merely to reach it.
var cypherRuleNames = gen.NewCypherParser(
	antlr.NewCommonTokenStream(gen.NewCypherLexer(antlr.NewInputStream("")), antlr.TokenDefaultChannel),
).RuleNames

// fullStrategyTree parses normalized under the DefaultErrorStrategy and reports
// whether it was accepted with no diagnostic at all, together with the tree's
// string form. It is the reference [parseAccepting] is measured against.
func fullStrategyTree(normalized string) (tree string, accepted bool) {
	stream, lexErrListener := lexNormalized(normalized)
	if len(lexErrListener.errs) > 0 {
		return "", false
	}
	parseErrListener := &errorListener{}
	p := gen.NewCypherParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(parseErrListener)
	p.BuildParseTrees = true
	t, panicErr := recoverParseScript(p)
	if panicErr != nil || len(parseErrListener.errs) > 0 || t == nil {
		return "", false
	}
	return antlr.TreesStringTree(t, cypherRuleNames, nil), true
}

// bailStrategyTree is [parseAccepting] with the tree rendered the same way.
func bailStrategyTree(normalized string) (tree string, accepted bool) {
	t, err := parseAccepting(normalized)
	if err != nil || t == nil {
		return "", false
	}
	return antlr.TreesStringTree(t, cypherRuleNames, nil), true
}

// TestBailStageAgreesWithFullPass is the acceptance gate. For every input it
// asserts that the bailing stage and the full strategy agree on whether the
// statement is accepted, and — when both accept — that the concrete syntax
// trees are BYTE-IDENTICAL.
//
// The prefix sweep is the load-bearing part. Truncating each corpus statement
// at every byte produces thousands of inputs whose acceptance nobody chose in
// advance: some are still valid statements, most are not, and a disagreement
// anywhere in that set would mean the fast path accepts or rejects something
// the full pass does not.
func TestBailStageAgreesWithFullPass(t *testing.T) {
	normalize := func(q string) string {
		q, _ = rewriteShortestPath(q)
		return applyNormalizers(q)
	}
	// accepted counts the inputs on which the tree-identity half of the oracle
	// actually ran. Without it the sweep could stay green while accepting
	// nothing at all: a broken corpus loader or a broken normalizer would make
	// every comparison false == false, and the stronger assertion would go
	// silent rather than fail.
	accepted := 0
	compare := func(t *testing.T, label, raw string) {
		t.Helper()
		if err := guardInput(raw); err != nil {
			return
		}
		if err := validateUnicodeEscapes(raw); err != nil {
			return
		}
		norm := normalize(raw)
		wantTree, wantOK := fullStrategyTree(norm)
		gotTree, gotOK := bailStrategyTree(norm)
		if gotOK != wantOK {
			t.Fatalf("%s: bail accepted=%v, full accepted=%v for %q", label, gotOK, wantOK, raw)
		}
		if wantOK {
			accepted++
		}
		if wantOK && gotTree != wantTree {
			t.Fatalf("%s: trees differ for %q\nfull: %s\nbail: %s", label, raw, wantTree, gotTree)
		}
	}

	corpus := loadCorpus(t)
	t.Run("corpus", func(t *testing.T) {
		for _, s := range corpus {
			compare(t, s.ID, s.Text)
		}
	})
	t.Run("invalid_table", func(t *testing.T) {
		for _, g := range parseErrorGoldens {
			compare(t, g.name, g.query)
		}
	})
	t.Run("valid_table", func(t *testing.T) {
		for _, v := range twoStageValid {
			compare(t, v.name, v.query)
		}
	})
	t.Run("corpus_prefixes", func(t *testing.T) {
		checked := 0
		for _, s := range corpus {
			for cut := 1; cut < len(s.Text); cut++ {
				compare(t, fmt.Sprintf("%s[:%d]", s.ID, cut), s.Text[:cut])
				checked++
			}
		}
		if checked < 1000 {
			t.Fatalf("swept only %d prefixes; the sweep is no longer a sweep", checked)
		}
		t.Logf("swept %d corpus prefixes", checked)
	})
	t.Run("fuzz_seeds", func(t *testing.T) {
		for i, s := range []string{
			"MATCH (n) RETURN n", "RETURN 1", "", "@@@@@@@@@@@@@@@@@@@@@",
			"RETURN , ; RETURN ,", "MATCH (n RETURN n", "THIS IS NOT CYPHER",
			"MATCH (n) WHERE RETURN n UNION RETURN n UNION RETURN n",
			"RETURN [1, 2", "RETURN {a: 1,}",
			"@ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @",
			"\x00\x01\x02\x03", "MATCH\x00(n)\x00RETURN\x00n",
			"RETURN CASE WHEN THEN 1 END",
		} {
			compare(t, fmt.Sprintf("seed%02d", i), s)
		}
	})

	// The floor on ACCEPTED inputs: the corpus alone is 83 statements that must
	// all parse, and the prefix sweep adds more. Anything less means the
	// tree-identity comparison has stopped running.
	if accepted < len(corpus) {
		t.Fatalf("the tree-identity comparison ran on only %d inputs, fewer than the %d corpus "+
			"statements that must all parse: the oracle has gone silent", accepted, len(corpus))
	}
	t.Logf("tree-identity compared on %d accepted inputs", accepted)
}

// TestParseStrictKeepsDefaultStrategy fences the choice recorded in
// [ParseStrict]: it must not adopt the bail, because its contract is the FULL
// error set and a bailing stage stops at the first diagnostic.
func TestParseStrictKeepsDefaultStrategy(t *testing.T) {
	const query = "RETURN , ; RETURN ,"
	_, errs := ParseStrict(query)
	if len(errs) < 2 {
		t.Fatalf("ParseStrict(%q) returned %d errors, want ≥2: the full error set is its "+
			"contract, and a bailing first stage would have stopped at the first",
			query, len(errs))
	}
}

// ---------------------------------------------------------------------------
// The error path's cost
// ---------------------------------------------------------------------------

// benchInvalidStatements are the invalid statements the error-path benchmark
// measures: a lexical rejection (which never reaches the parser and therefore
// never pays the retry), a parser rejection, and the incomplete-WITH input
// whose retry panics inside antlr and is recovered.
var benchInvalidStatements = []struct{ name, query string }{
	{"lex_errchar", "MATCH (n) WHERE n.v != 2 RETURN n"},
	{"lex_unterminated", `MATCH (n) WHERE n.name = "abc RETURN n`},
	{"parse_no_projection", "MATCH (n) RETURN"},
	{"parse_unclosed_paren", "MATCH (n RETURN n"},
	{"parse_not_cypher", "FOO BAR BAZ"},
	{"parse_panic_retry", "MATCH (n) WITH RETURN n"},
}

// BenchmarkParseErrorPath records what an invalid statement costs now that it
// is parsed twice. It is evidence, not a gate: the accepting path is what the
// change optimises, and the error path is what it pays with.
func BenchmarkParseErrorPath(b *testing.B) {
	for _, s := range benchInvalidStatements {
		q := s.query
		// Warm the shared ATN/DFA so the reported number is steady state.
		for i := 0; i < 3; i++ {
			_, _, sinkErr = ParseStatement(q)
		}
		b.Run(s.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(q)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkAny, _, sinkErr = ParseStatement(q)
			}
		})
	}
}
