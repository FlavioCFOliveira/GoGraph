// Package parser translates the antlr4-generated parse tree into the typed AST
// defined in gograph/cypher/ast.
package parser

import (
	"fmt"
	"strings"

	"gograph/cypher/ast"
)

// SemaError is returned by the visitor when a parse-tree node corresponds to a
// grammar rule that is not supported in the read+write+DDL+procedure scope
// (FOREACH, CALL{}, multi-graph) or when a structural semantic constraint is
// violated.
type SemaError struct {
	// Rule is the grammar rule name that triggered the error (e.g. "foreach").
	Rule string
	// Pos is the source position of the offending node.
	Pos ast.Position
	// Message is a human-readable description of the problem.
	Message string
}

// Error implements the error interface.
func (e *SemaError) Error() string {
	return fmt.Sprintf("sema error at %s in rule %q: %s", e.Pos, e.Rule, e.Message)
}

// ParseError wraps ANTLR syntax errors reported during lexing or parsing.
type ParseError struct {
	// Line and Column of the first problematic token (1-based line, 0-based column).
	Line   int
	Column int
	// OffendingToken is the text of the token that triggered the error.
	// It is empty for lexer errors where no token was formed.
	OffendingToken string
	// Expected is the human-readable list of token names that were valid at
	// the error position. It is nil when the expected set cannot be determined
	// (e.g. lexer errors).
	Expected []string
	// Message is the raw ANTLR error message, included as a fallback.
	Message string
}

// Error returns a human-readable description of the syntax error.
//
// Format: "unexpected '<token>' at <line>:<col>, expected one of {A, B, C}"
// When OffendingToken is empty the "unexpected" clause is omitted.
// When Expected is empty the "expected" clause is omitted.
func (e *ParseError) Error() string {
	var b strings.Builder
	if e.OffendingToken != "" {
		fmt.Fprintf(&b, "unexpected %q at %d:%d", e.OffendingToken, e.Line, e.Column)
	} else {
		fmt.Fprintf(&b, "parse error at %d:%d", e.Line, e.Column)
	}
	switch len(e.Expected) {
	case 0:
		// no expected set; append the raw ANTLR message as context.
		if e.Message != "" {
			b.WriteString(": ")
			b.WriteString(e.Message)
		}
	case 1:
		fmt.Fprintf(&b, ", expected %s", e.Expected[0])
	default:
		fmt.Fprintf(&b, ", expected one of {%s}", strings.Join(e.Expected, ", "))
	}
	return b.String()
}
