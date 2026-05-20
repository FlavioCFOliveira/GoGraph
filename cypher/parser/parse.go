package parser

import (
	"github.com/antlr4-go/antlr/v4"

	"gograph/cypher/ast"
	"gograph/cypher/parser/gen"
)

// errorListener collects ANTLR syntax errors and converts them to
// [ParseError] values.
type errorListener struct {
	*antlr.DefaultErrorListener
	errs []*ParseError
}

func (e *errorListener) SyntaxError(
	_ antlr.Recognizer,
	_ interface{},
	line, column int,
	msg string,
	_ antlr.RecognitionException,
) {
	e.errs = append(e.errs, &ParseError{Line: line, Column: column, Message: msg})
}

// Parse lexes and parses a Cypher query string and converts the resulting
// parse tree into a typed AST node.
//
// Errors:
//   - [*ParseError] — syntax error from the ANTLR lexer/parser.
//   - [*SemaError]  — unsupported grammar rule encountered during tree walking.
func Parse(query string) (ast.Query, error) {
	// Lex.
	lexErrListener := &errorListener{}
	input := antlr.NewInputStream(query)
	lexer := gen.NewCypherLexer(input)
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrListener)

	// Parse.
	parseErrListener := &errorListener{}
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := gen.NewCypherParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(parseErrListener)
	p.BuildParseTrees = true

	tree := p.Script()

	// Report lex errors first.
	if len(lexErrListener.errs) > 0 {
		return nil, lexErrListener.errs[0]
	}
	if len(parseErrListener.errs) > 0 {
		return nil, parseErrListener.errs[0]
	}

	// Walk the parse tree.
	v := newVisitor()
	result := v.visit(tree)

	if se, ok := result.(*SemaError); ok {
		return nil, se
	}

	if q, ok := result.(ast.Query); ok {
		return q, nil
	}

	// Script might return a *SingleQuery which satisfies ast.Query.
	if sq, ok := result.(*ast.SingleQuery); ok {
		return sq, nil
	}

	return nil, &ParseError{Message: "visitor produced no AST node"}
}
