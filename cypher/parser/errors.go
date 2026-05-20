// Package parser translates the antlr4-generated parse tree into the typed AST
// defined in gograph/cypher/ast.
package parser

import (
	"fmt"

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

func (e *SemaError) Error() string {
	return fmt.Sprintf("sema error at %s in rule %q: %s", e.Pos, e.Rule, e.Message)
}

// ParseError wraps ANTLR syntax errors reported during lexing or parsing.
type ParseError struct {
	// Line and Column of the first problematic token.
	Line   int
	Column int
	// Message is the ANTLR error message.
	Message string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse error at %d:%d: %s", e.Line, e.Column, e.Message)
}
