package parser

import (
	"errors"
	"fmt"

	"github.com/antlr4-go/antlr/v4"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser/gen"
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

// applyNormalizers applies the full text-rewriting pipeline to query before
// lexing. Both [Parse] and [ParseStrict] call this helper so that neither
// diverges from the other on valid input.
//
// The pipeline order is load-bearing: each rewrite produces output that
// subsequent rewrites consume. Do not reorder without verifying the TCK suite.
func applyNormalizers(query string) string {
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
	return query
}

// collectErrCharErrors appends a [ParseError] to l for every ERRCHAR token in
// stream, honouring the same [maxParseErrors] cap the ANTLR listener applies.
// stream must already be filled.
//
// ERRCHAR is the lexer's catch-all rule (`ERRCHAR : . -> channel(HIDDEN)` at
// CypherLexer.g4:157). It matches any single character no other lexer rule
// accepts and routes it to the hidden channel, where the parser never sees it.
// Left alone, the character is therefore silently deleted and the rest of the
// query parses as though it had never been typed, so the engine answers a
// question the caller did not ask:
//
//   - `MATCH (n) WHERE n.v != 2` becomes `... n.v = 2` — the exact negation of
//     the requested predicate.
//   - `MATCH (n:!A)` becomes `MATCH (n:A)` — the exact complement of the
//     requested label set. `:!A` is valid Neo4j 5 syntax, so a ported query
//     inverts silently.
//   - `WHERE n.name = "unterminated` drops the opening quote, leaving the
//     literal's content to lex as bare identifiers.
//
// None of those raised an error before this check existed, and none is visible
// to the openCypher TCK, which only executes syntactically valid queries.
//
// The character is promoted to a syntax error here rather than by deleting the
// grammar rule, because deleting it would renumber the token vocabulary and
// force a full ATN regeneration, re-applying the hand patches CypherLexer.g4
// documents. Reporting from the token stream is behaviourally equivalent for
// every input — ERRCHAR matches exactly the characters the grammar does not
// know — at no cost to the accepting path.
//
// The single exception is the `~` of the openCypher regex-match operator `=~`,
// which the grammar has no token for and which [comparisonOp] recovers by
// peeking past the ASSIGN token. That `~` is a legitimate part of a valid
// query, so it is exempted here. See [isRegexCombiner].
func collectErrCharErrors(l *errorListener, stream *antlr.CommonTokenStream) {
	toks := stream.GetAllTokens()
	for i, tok := range toks {
		if tok.GetTokenType() != gen.CypherLexerERRCHAR {
			continue
		}
		if isRegexCombiner(toks, i) {
			continue
		}
		if len(l.errs) >= maxParseErrors {
			return
		}
		text := tok.GetText()
		msg := "unrecognised character"
		// A lone quote reaches ERRCHAR only when its literal is never closed,
		// which is a materially different diagnosis worth reporting as such.
		if text == `"` || text == `'` {
			msg = "unterminated string literal"
		}
		l.errs = append(l.errs, &ParseError{
			OffendingToken: text,
			Message:        msg,
			Line:           tok.GetLine(),
			Column:         tok.GetColumn(),
		})
	}
}

// isRegexCombiner reports whether toks[i] is the `~` that completes the
// openCypher regex-match operator `=~`, i.e. an ERRCHAR `~` immediately
// preceded, with no intervening character, by an ASSIGN token.
//
// The vendored grammar has no REGMATCH token, so `=~` lexes as ASSIGN followed
// by a hidden ERRCHAR `~`, and [comparisonOp] reconstructs the operator by
// peeking the character after the `=`. This predicate is the exact mirror of
// that peek, applied from the token side: it exempts precisely the `~` that
// [isRegexMatchAssign] will go on to claim, so a valid `=~` query is accepted
// while a stray `~` anywhere else is reported. Adjacency is checked on
// character offsets, so `= ~` — which is not the operator — is still an error.
//
// Replacing both halves with a real REGMATCH lexer rule remains the clean fix
// and is gated on regenerating the ANTLR ATN.
func isRegexCombiner(toks []antlr.Token, i int) bool {
	if toks[i].GetText() != "~" || i == 0 {
		return false
	}
	prev := toks[i-1]
	return prev.GetTokenType() == gen.CypherLexerASSIGN &&
		prev.GetStop()+1 == toks[i].GetStart()
}

// recoverParseScript calls p.Script() and converts any runtime panic into a
// *ParseError. Incomplete WITH clauses and certain pipe-in-arg expressions
// drive ANTLR's DefaultErrorStrategy into an unchecked type assertion in
// antlr4-go v4.13.1; without this guard the process crashes.
func recoverParseScript(p *gen.CypherParser) (tree gen.IScriptContext, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &ParseError{Message: fmt.Sprintf("parser panic: %v", r)}
		}
	}()
	return p.Script(), nil
}

// Parse lexes and parses a Cypher query string and converts the resulting
// parse tree into a typed AST node. It returns the first error encountered.
//
// A statement written with an EXPLAIN or PROFILE prefix parses here exactly as
// the same statement without one, and the prefix is discarded. Callers that must
// honour it — the engine does, because EXPLAIN may not execute — call
// [ParseStatement] instead.
//
// Errors:
//   - [*ParseError] — syntax error from the ANTLR lexer/parser.
//   - [*SemaError]  — unsupported grammar rule encountered during tree walking.
func Parse(query string) (ast.Query, error) {
	q, _, err := ParseStatement(query)
	return q, err
}

// ParseStatement is [Parse] with the statement's EXPLAIN / PROFILE prefix
// reported alongside the AST.
//
// The AST is identical either way: the prefix is a statement-level instruction
// to the engine, not a clause, so it changes nothing the scope analyser or the
// IR translator sees. What it changes is whether the engine is allowed to
// execute the statement at all — see [PlanMode] and cypher/plan_prefix.go.
//
// The prefix is recognised by the GRAMMAR (`script` in
// cypher/parser/grammar/CypherParser.g4), so `RETURN explain` — where `explain`
// is an ordinary identifier — still parses as a plain statement with
// [PlanModeNone], which a textual scan for a leading keyword could not
// distinguish reliably.
//
// Errors: as [Parse].
func ParseStatement(query string) (ast.Query, PlanMode, error) {
	// Reject over-length or excessively nested input before any lexing or
	// parsing. Deep bracket nesting drives unbounded parser/visitor recursion
	// into a fatal Go stack overflow that recover() cannot catch, so the guard
	// must run first — once the stack has overflowed there is no recovery path.
	// See guard.go.
	if err := guardInput(query); err != nil {
		return nil, PlanModeNone, err
	}

	// Validate string-literal escape sequences before any rewriting so that
	// `normalizeSingleQuotes` does not silently hide a malformed `\u…`
	// escape under a benign-looking double-quoted form.
	if err := validateUnicodeEscapes(query); err != nil {
		return nil, PlanModeNone, err
	}

	// Strip shortestPath()/allShortestPaths() wrappers from named MATCH path
	// bindings into the plain patterns they wrap, recording each as a marker to
	// stamp back onto the AST after the build (rmp #1690). Runs before the other
	// normalizers so the unwrapped inner pattern's variable-length bounds are
	// normalized as usual. A query with no such wrapper is returned untouched.
	query, spMarkers := rewriteShortestPath(query)

	query = applyNormalizers(query)

	// Lex.
	lexErrListener := &errorListener{}
	input := antlr.NewInputStream(query)
	lexer := gen.NewCypherLexer(input)
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrListener)

	// Parse.
	parseErrListener := &errorListener{}
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)

	// Lex the whole input before parsing so that the catch-all ERRCHAR rule can
	// be promoted to a syntax error instead of silently deleting a character.
	// See [collectErrCharErrors]. `script` is anchored at EOF, so an accepted
	// query consumes every token regardless: this fetches them eagerly rather
	// than adding work. Fill leaves the read index on the first
	// default-channel token, which is exactly where the parser expects it.
	stream.Fill()
	collectErrCharErrors(lexErrListener, stream)
	if len(lexErrListener.errs) > 0 {
		return nil, PlanModeNone, lexErrListener.errs[0]
	}

	p := gen.NewCypherParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(parseErrListener)
	p.BuildParseTrees = true

	tree, panicErr := recoverParseScript(p)
	if panicErr != nil {
		return nil, PlanModeNone, panicErr
	}

	// Report lex errors first.
	if len(lexErrListener.errs) > 0 {
		return nil, PlanModeNone, lexErrListener.errs[0]
	}
	if len(parseErrListener.errs) > 0 {
		return nil, PlanModeNone, parseErrListener.errs[0]
	}

	// Walk the parse tree.
	v := newVisitor()
	result := v.visit(tree)

	if se, ok := result.(*SemaError); ok {
		return nil, PlanModeNone, se
	}

	// The arithmetic visitors (VisitAddSubExpression / VisitMultDivExpression
	// / VisitPowerExpression) lift list/string predicates (IN, CONTAINS,
	// STARTS WITH, ENDS WITH) above each arithmetic level as the tree is
	// built, so no post-pass is required here. See cypher/parser/rebalance.go
	// for the rationale.
	if q, ok := result.(ast.Query); ok {
		applyShortestMarkers(q, spMarkers)
		return q, v.planMode, nil
	}
	if sq, ok := result.(*ast.SingleQuery); ok {
		applyShortestMarkers(sq, spMarkers)
		return sq, v.planMode, nil
	}

	return nil, PlanModeNone, &ParseError{Message: "visitor produced no AST node"}
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
	// Reject over-length or excessively nested input before any lexing or
	// parsing, for the same stack-overflow reason as [Parse]. See guard.go.
	if err := guardInput(query); err != nil {
		return nil, []error{err}
	}

	if err := validateUnicodeEscapes(query); err != nil {
		return nil, []error{err}
	}

	query = applyNormalizers(query)

	// Lex.
	lexErrListener := &errorListener{}
	input := antlr.NewInputStream(query)
	lexer := gen.NewCypherLexer(input)
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrListener)

	// Parse.
	parseErrListener := &errorListener{}
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)

	// See [Parse] for why the stream is filled before parsing. ParseStrict
	// reports the full error set, so parsing continues after an ERRCHAR has
	// been recorded rather than short-circuiting on the first one.
	stream.Fill()
	collectErrCharErrors(lexErrListener, stream)

	p := gen.NewCypherParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(parseErrListener)
	p.BuildParseTrees = true

	tree, panicErr := recoverParseScript(p)
	if panicErr != nil {
		return nil, []error{panicErr}
	}

	// Collect all errors: lex errors first, then parse errors. Each listener
	// caps its own slice at maxParseErrors, so the concatenation is capped
	// again here — the documented contract is a maximum per parse, not per
	// phase, and a query can now produce errors in both phases.
	if n := len(lexErrListener.errs) + len(parseErrListener.errs); n > 0 {
		if n > maxParseErrors {
			n = maxParseErrors
		}
		errs := make([]error, 0, n)
		for _, e := range lexErrListener.errs {
			if len(errs) == n {
				break
			}
			errs = append(errs, e)
		}
		for _, e := range parseErrListener.errs {
			if len(errs) == n {
				break
			}
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
