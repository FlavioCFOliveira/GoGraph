package ir

import (
	"fmt"

	"gograph/cypher/ast"
)

// TranslateError is returned by [FromAST] when it encounters an AST construct
// that the translator does not support. Callers can use [errors.As] to inspect
// the unsupported clause name and its source position.
type TranslateError struct {
	// UnsupportedClause is the name of the clause or construct that triggered
	// the error (e.g. "FOREACH", "multi-graph UNION").
	UnsupportedClause string
	// Pos is the source position of the unsupported clause.
	Pos ast.Position
}

// Error implements the error interface.
func (e *TranslateError) Error() string {
	return fmt.Sprintf("translate: unsupported clause %q at %s", e.UnsupportedClause, e.Pos.String())
}
