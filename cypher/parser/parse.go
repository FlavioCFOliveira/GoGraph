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

// ---------------------------------------------------------------------------
// Two-stage parsing (rmp #2850)
// ---------------------------------------------------------------------------

// bailErrorStrategy is antlr's [antlr.BailErrorStrategy] — the strategy the
// ANTLR runtime documents for the first stage of a two-stage parse — carrying
// its own sentinel and one override.
//
// # Why the first stage bails
//
// [antlr.DefaultErrorStrategy.Sync] runs at every decision point of every rule,
// on the ACCEPTING path as much as on an erroneous one, computing the
// recovery set it would need if the next token were wrong. The bail strategy's
// Sync is empty, so a statement that parses pays none of it. Measured over the
// examples corpus that is the whole of the −6.46% this change buys
// (docs/benchmarks/cypher-parse-lab-2026-09-16-spike2846/sll.benchstat.txt).
//
// The prediction mode is deliberately NOT changed. [antlr.PredictionModeSLL]
// was measured in the same spike at −0.49% (p=0.529, n=10), below the 1.05%
// noise floor, and was refuted: LL is full-power prediction, so the second
// stage below only ever has to reproduce a MESSAGE, never to rescue a valid
// statement the first stage wrongly rejected.
//
// # What "bail" means in antlr4-go v4.13.1, and why bailed is a field
//
// It does not mean the parse stops. Go has no exception to throw, so
// [antlr.BailErrorStrategy.Recover] only records a
// [antlr.ParseCancellationException] with SetError, and the generated rule's
// errorExit clears it on the very next line (`p.SetError(nil)`,
// cypher/parser/gen/cypher_parser.go). The first stage therefore returns to its
// caller and parses on to EOF, with no recovery and no further reporting.
//
// That is why the abort has to be latched here rather than read back from the
// parser, and why it is latched in THREE methods: ReportError, Recover and
// RecoverInline are the only entry points a parse can reach once it has left
// the accepting path. Sync is the fourth such entry point in the default
// strategy — it reports an unwanted token directly, touching none of the other
// three — and it is empty in BailErrorStrategy, which is what makes the set of
// three complete.
//
// Reading the abort from a recovered panic instead would not work either: the
// recovered value's text cannot distinguish an antlr abort from the GENUINE
// antlr4-go panic [recoverParseScript] exists to catch. The flag removes the
// question.
//
// # The override
//
// [antlr.DefaultErrorStrategy.ReportError], which BailErrorStrategy inherits,
// has no case for ParseCancellationException. It therefore falls to its default
// branch, which writes "unknown recognition error type: " to STDOUT and then
// calls the exception's GetMessage — a v4.13.1 stub whose body is
// panic("implement me"). A library must not write to stdout, so ReportError is
// overridden to record the abort and return. Nothing is lost: the first stage's
// diagnostics are discarded in every case, because they are not the ones
// [ParseStatement] reports — the second stage's are.
//
// # What the bail gives up
//
// [antlr.DefaultErrorStrategy] carries a failsafe that guarantees at least one
// token is consumed between two errors, which bounds recovery on malformed
// input. BailErrorStrategy has none, so nothing bounds the first stage's
// iteration on malformed input except [guardInput], which caps the input's
// length and nesting depth before any of this runs. No unbounded input has been
// observed — the corpus, every one-byte prefix of it, the TCK and FuzzParse all
// terminate — but the guarantee itself is gone, and that is a property of
// ANTLR's strategy, not of this file.
type bailErrorStrategy struct {
	*antlr.BailErrorStrategy

	// bailed reports that the parse left the accepting path. It is the ONLY
	// signal the caller acts on; the value recovered from a panic is never
	// inspected.
	bailed bool
}

// antlr.ErrorStrategy carries an unexported method, so bailErrorStrategy can
// only satisfy it by promotion from the embedded runtime type. Assert it where
// the embed is written rather than leaving SetErrorHandler to discover it.
var _ antlr.ErrorStrategy = (*bailErrorStrategy)(nil)

// ReportError records the abort and reports nothing. See [bailErrorStrategy].
func (b *bailErrorStrategy) ReportError(_ antlr.Parser, _ antlr.RecognitionException) {
	b.bailed = true
}

// Recover records the abort, then delegates to [antlr.BailErrorStrategy].
func (b *bailErrorStrategy) Recover(recognizer antlr.Parser, e antlr.RecognitionException) {
	b.bailed = true
	b.BailErrorStrategy.Recover(recognizer, e)
}

// RecoverInline records the abort, then delegates to [antlr.BailErrorStrategy].
// The embedded method calls the embedded Recover, not this type's, which is why
// the flag is set here as well.
func (b *bailErrorStrategy) RecoverInline(recognizer antlr.Parser) antlr.Token {
	b.bailed = true
	return b.BailErrorStrategy.RecoverInline(recognizer)
}

// lexNormalized lexes already-normalized text into a filled token stream and
// returns the lexical diagnostics, ERRCHAR promotions included.
//
// It is the lexing block of [ParseStatement] verbatim, factored out so that the
// two stages below run identical code and differ in exactly one input: the
// parser's error strategy.
func lexNormalized(normalized string) (*antlr.CommonTokenStream, *errorListener) {
	lexErrListener := &errorListener{}
	input := antlr.NewInputStream(normalized)
	lexer := gen.NewCypherLexer(input)
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrListener)

	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)

	// Lex the whole input before parsing so that the catch-all ERRCHAR rule can
	// be promoted to a syntax error instead of silently deleting a character.
	// See [collectErrCharErrors]. `script` is anchored at EOF, so an accepted
	// query consumes every token regardless: this fetches them eagerly rather
	// than adding work. Fill leaves the read index on the first
	// default-channel token, which is exactly where the parser expects it.
	stream.Fill()
	collectErrCharErrors(lexErrListener, stream)
	return stream, lexErrListener
}

// errParserBailed reports that the first stage did not accept, and nothing
// more. It is never returned to a caller of this package: [ParseStatement]
// answers it by running [parseDiagnostic], whose diagnostic is the one the
// caller sees.
var errParserBailed = errors.New("parser: first stage did not accept")

// parseAccepting is the FIRST stage: it lexes normalized and parses it under
// [bailErrorStrategy].
//
// It returns the parse tree when the statement parsed with no diagnostic at all
// — the fast path, which never constructs the default strategy, never re-lexes
// and never runs the second stage. Otherwise it returns either a lexical
// diagnostic, which the bail cannot affect because the whole input is lexed
// before the parser is created, or [errParserBailed], which says only that the
// caller must ask [parseDiagnostic] what went wrong.
//
// A parse that did not accept returns no tree. It leaves one, with exceptions
// stamped on its contexts, and it is never walked.
func parseAccepting(normalized string) (gen.IScriptContext, error) {
	stream, lexErrListener := lexNormalized(normalized)
	if len(lexErrListener.errs) > 0 {
		return nil, lexErrListener.errs[0]
	}

	p := gen.NewCypherParser(stream)
	// No parse-error listener is attached: the first stage's diagnostics are
	// discarded in every case, and RemoveErrorListeners is still required to
	// drop the runtime's own console listener.
	p.RemoveErrorListeners()
	p.BuildParseTrees = true

	handler := &bailErrorStrategy{BailErrorStrategy: antlr.NewBailErrorStrategy()}
	p.SetErrorHandler(handler)

	// The recovered panic VALUE is deliberately discarded: its text cannot tell
	// an antlr abort from a genuine antlr4-go v4.13.1 panic (see
	// [bailErrorStrategy]), and the first stage does not need to know. Both mean
	// the same thing and take the same action — [parseDiagnostic] reproduces
	// whichever it was, because it is the unchanged pre-change code running over
	// the same text.
	tree, panicErr := recoverParseScript(p)
	if panicErr != nil || handler.bailed {
		return nil, errParserBailed
	}
	return tree, nil
}

// parseDiagnostic is the SECOND stage: the front end exactly as it stood before
// two-stage parsing, from a FRESH input stream, lexer and token stream.
//
// The stream is rebuilt rather than rewound. The first stage's parser consumed
// from it, so it cannot simply be handed over; antlr4-go's
// CommonTokenStream.Seek(0) would rewind it exactly — lazyInit is a no-op once
// the stream is initialised, and adjustSeekIndex(0) reproduces what Fill's setup
// left — so this is a CHOICE, not a necessity. It is made because a fresh stream
// makes this function literally the pre-change code, which is what the
// byte-identical-diagnostics obligation rests on. Reusing the filled stream
// would delete one input stream, one lexer, one lex and one ERRCHAR pass per
// failing statement, and is recorded as a follow-up rather than folded in here.
//
// It runs only when [parseAccepting] did not accept, so every diagnostic
// [ParseStatement] returns — its type, Message, Line, Column, OffendingToken
// and Expected set, and the order the three sources are checked in — is
// produced by this function, which is the unchanged code. The error path costs
// one extra lex and one extra parse; the accepting path costs neither.
func parseDiagnostic(normalized string) (gen.IScriptContext, error) {
	stream, lexErrListener := lexNormalized(normalized)
	if len(lexErrListener.errs) > 0 {
		return nil, lexErrListener.errs[0]
	}

	parseErrListener := &errorListener{}
	p := gen.NewCypherParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(parseErrListener)
	p.BuildParseTrees = true

	tree, panicErr := recoverParseScript(p)
	if panicErr != nil {
		return nil, panicErr
	}

	// Report lex errors first.
	if len(lexErrListener.errs) > 0 {
		return nil, lexErrListener.errs[0]
	}
	if len(parseErrListener.errs) > 0 {
		return nil, parseErrListener.errs[0]
	}
	return tree, nil
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

	// Two-stage parse. The first stage bails at the first sign of trouble and
	// so pays nothing for the recovery machinery a statement that parses never
	// needs; the second reproduces every diagnostic by re-parsing under the
	// default strategy. See [bailErrorStrategy], [parseAccepting] and
	// [parseDiagnostic].
	tree, err := parseAccepting(query)
	if errors.Is(err, errParserBailed) {
		// The first stage rejected the statement but says nothing about why.
		// Everything the caller sees from here — including the case where the
		// full pass finds no diagnostic at all and its tree is walked, exactly
		// as the pre-change code walked it — comes from the second stage.
		tree, err = parseDiagnostic(query)
	}
	if err != nil {
		return nil, PlanModeNone, err
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

	// ParseStrict keeps the DefaultErrorStrategy unconditionally, and does NOT
	// use the two-stage path [ParseStatement] takes. Its contract is the FULL
	// error set: a bailing first stage stops at the first diagnostic, so every
	// input with more than one error would have to be parsed twice to satisfy
	// it, and every input with none is already served by [ParseStatement]. The
	// bail would therefore cost on the multi-error path and save on no path
	// this function is called for — it is tooling, not the execution path.
	//
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
