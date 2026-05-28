package parser

import (
	"errors"

	"github.com/antlr4-go/antlr/v4"

	"gograph/cypher/ast"
	"gograph/cypher/parser/gen"
)

// maxParseErrors is the maximum number of syntax errors collected per parse.
// Once this cap is reached, additional errors are silently dropped. This
// prevents cascading error floods on pathological input while still surfacing
// the first meaningful errors to callers. See [errorListener.SyntaxError].
const maxParseErrors = 5

// errorListener collects ANTLR syntax errors and converts them to [ParseError]
// values with enriched diagnostics: offending token text and the set of tokens
// that were valid at the error position.
type errorListener struct {
	*antlr.DefaultErrorListener
	errs []*ParseError
}

// SyntaxError implements antlr.ErrorListener. It enriches the raw ANTLR message
// with the offending token text and the expected-token set whenever the
// recognizer is a parser (not a lexer).
func (l *errorListener) SyntaxError(
	recognizer antlr.Recognizer,
	offendingSymbol interface{},
	line, column int,
	msg string,
	e antlr.RecognitionException,
) {
	pe := &ParseError{
		Line:    line,
		Column:  column,
		Message: msg,
	}

	// Extract the offending token text. The offendingSymbol parameter carries
	// an antlr.Token when invoked from a parser rule; fall back to the
	// RecognitionException's token when available.
	if tok, ok := offendingSymbol.(antlr.Token); ok && tok != nil {
		text := tok.GetText()
		if text != "<EOF>" && text != "" {
			pe.OffendingToken = text
		}
	} else if e != nil {
		if tok := e.GetOffendingToken(); tok != nil {
			text := tok.GetText()
			if text != "<EOF>" && text != "" {
				pe.OffendingToken = text
			}
		}
	}

	// Extract the expected-token set. This is only meaningful for parser
	// errors; the Recognizer must implement antlr.Parser.
	if p, ok := recognizer.(antlr.Parser); ok {
		expected := p.GetExpectedTokens()
		if expected != nil {
			litNames := p.GetLiteralNames()
			symNames := p.GetSymbolicNames()
			pe.Expected = tokenSetNames(expected, litNames, symNames)
		}
	}

	// Drop errors beyond the cap to prevent cascading error floods on
	// pathological input. The cap is intentionally low: callers rarely
	// benefit from more than a handful of simultaneous syntax errors.
	if len(l.errs) >= maxParseErrors {
		return
	}
	l.errs = append(l.errs, pe)
}

// tokenSetNames converts an ANTLR IntervalSet of token types into a
// deduplicated slice of human-readable names. Literal names (e.g. "'RETURN'")
// are preferred; symbolic names (e.g. "RETURN") are used as fallback; token
// types with neither are omitted.
//
// The function is allocation-efficient: it uses the interval structure of the
// set to avoid unnecessary slice growth.
func tokenSetNames(set *antlr.IntervalSet, litNames, symNames []string) []string {
	intervals := set.GetIntervals()
	if len(intervals) == 0 {
		return nil
	}

	// Upper-bound capacity: sum of interval widths (may over-allocate for wide
	// intervals, but is exact for single-token intervals which are the common case).
	hint := 0
	for _, iv := range intervals {
		hint += iv.Stop - iv.Start + 1
	}
	names := make([]string, 0, hint)

	for _, iv := range intervals {
		for t := iv.Start; t <= iv.Stop; t++ {
			name := tokenName(t, litNames, symNames)
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// tokenName returns the display name for a token type integer.
// Literal names are quoted (e.g. "'RETURN'"); symbolic names are unquoted
// (e.g. "RETURN"); unknown tokens produce an empty string.
func tokenName(t int, litNames, symNames []string) string {
	if t > 0 && t < len(litNames) {
		if n := litNames[t]; n != "" && n != "<INVALID>" {
			return n
		}
	}
	if t > 0 && t < len(symNames) {
		if n := symNames[t]; n != "" && n != "<INVALID>" {
			return n
		}
	}
	return ""
}

// Parse lexes and parses a Cypher query string and converts the resulting
// parse tree into a typed AST node. It returns the first error encountered.
//
// Errors:
//   - [*ParseError] — syntax error from the ANTLR lexer/parser.
//   - [*SemaError]  — unsupported grammar rule encountered during tree walking.
func Parse(query string) (ast.Query, error) {
	// Validate string-literal escape sequences before any rewriting so that
	// `normalizeSingleQuotes` does not silently hide a malformed `\u…`
	// escape under a benign-looking double-quoted form.
	if err := validateUnicodeEscapes(query); err != nil {
		return nil, err
	}

	query = normalizeSingleQuotes(query)
	query = normalizeDoubleNot(query)
	query = normalizeCallNoParen(query)
	query = normalizeNegHexOct(query)
	query = normalizeFloatExpZeroPad(query)
	query = normalizeArithmeticMinus(query)
	// normalizeVarlenDotDot is intentionally NOT applied here: openCypher
	// requires a leading `*` on every variable-length relationship pattern
	// (`-[*]-`, `-[*..n]-`, `-[*n..m]-`), and the TCK Match4 [9] gates
	// against accepting `-[:T..]-` without the star. Keeping the helper
	// defined and unit-tested in this package documents the rewrite that
	// used to run but is no longer in the pipeline.
	query = normalizeVarlenBounds(query)
	query = normalizeZeroDotFloat(query)
	query = normalizeLeadingDotFloat(query)

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

// ParseStrict lexes and parses a Cypher query string and returns all syntax
// errors encountered rather than only the first. When the query is
// syntactically valid the AST is walked for semantic errors; a single
// [*SemaError] is returned in that case.
//
// This function is intended for tooling (editors, linters) that need the full
// error set. Application code should use [Parse].
//
// Errors:
//   - One or more [*ParseError] — syntax errors from lexer/parser.
//   - A single [*SemaError] — unsupported grammar rule or structural violation.
func ParseStrict(query string) (ast.Query, []error) {
	if err := validateUnicodeEscapes(query); err != nil {
		return nil, []error{err}
	}

	query = normalizeSingleQuotes(query)
	query = normalizeDoubleNot(query)
	query = normalizeCallNoParen(query)
	query = normalizeNegHexOct(query)
	query = normalizeFloatExpZeroPad(query)
	query = normalizeVarlenBounds(query)

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

	// Collect all errors: lex errors first, then parse errors.
	if n := len(lexErrListener.errs) + len(parseErrListener.errs); n > 0 {
		errs := make([]error, 0, n)
		for _, e := range lexErrListener.errs {
			errs = append(errs, e)
		}
		for _, e := range parseErrListener.errs {
			errs = append(errs, e)
		}
		return nil, errs
	}

	// Walk the parse tree.
	v := newVisitor()
	result := v.visit(tree)

	if se, ok := result.(*SemaError); ok {
		return nil, []error{se}
	}

	if q, ok := result.(ast.Query); ok {
		return q, nil
	}

	if sq, ok := result.(*ast.SingleQuery); ok {
		return sq, nil
	}

	err := &ParseError{Message: "visitor produced no AST node"}
	return nil, []error{err}
}

// AsParseErrors returns all [*ParseError] values from an error slice produced
// by [ParseStrict]. Non-ParseError values are included as-is.
//
// This is a convenience helper for callers that need to separate parse errors
// from sema errors.
func AsParseErrors(errs []error) ([]*ParseError, []error) {
	var pes []*ParseError
	var other []error
	for _, e := range errs {
		var pe *ParseError
		if errors.As(e, &pe) {
			pes = append(pes, pe)
		} else {
			other = append(other, e)
		}
	}
	return pes, other
}
