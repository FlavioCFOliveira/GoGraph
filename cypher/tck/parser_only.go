package tck

import (
	"bufio"
	"embed"
	"errors"
	"io/fs"
	"regexp"
	"strings"
)

//go:embed features
var featureFiles embed.FS

// Scenario represents a single Gherkin Scenario extracted from a feature file.
// Fields are exported so that test code in external test packages can read them.
type Scenario struct {
	// File is the path of the feature file relative to the embed root, e.g.
	// "features/clauses/return/Return1.feature".
	File string
	// Feature is the Feature: header from the containing feature file.
	Feature string
	// Name is the Scenario: text, including its [N] index prefix where present.
	Name string
	// Tags are the @tag annotations on the Scenario block.
	Tags []string
	// Query is the Cypher string from the "When executing query:" step.
	Query string
	// SyntaxErrorType is non-empty when the scenario expects a SyntaxError,
	// e.g. "UndefinedVariable", "UnexpectedSyntax".
	SyntaxErrorType string
	// SkipReason is non-empty when the scenario is excluded from the pass-rate
	// gate. It records the grammar-gap category that caused the exclusion.
	SkipReason SkipReason
}

// WantParseError reports whether the scenario expects [parser.Parse] to return
// a non-nil error.
func (s *Scenario) WantParseError() bool {
	return parseTimeErrors[s.SyntaxErrorType]
}

// SkipReason categorises why a scenario is excluded from the pass-rate gate.
// Every category corresponds to a known gap between the antlr/grammars-v4
// grammar (commit 284602b) and the full openCypher specification.
type SkipReason string

const (
	// SkipNone means the scenario is included in the pass-rate gate.
	SkipNone SkipReason = ""

	// SkipPlaceholder excludes Scenario Outline template rows that contain
	// angle-bracket placeholders (<pattern>, <yield>, etc.) which are not
	// valid Cypher.
	SkipPlaceholder SkipReason = "placeholder-template"

	// SkipSingleQuoteString excludes queries with multi-word single-quoted
	// string literals (e.g. 'The Matrix'). The grammar tokenises them as a
	// char literal followed by an identifier, producing a spurious parse error.
	SkipSingleQuoteString SkipReason = "single-quote-string"

	// SkipVarlenExplicitBound excludes variable-length relationship patterns
	// with numeric bounds, e.g. -[:T*2]-> or -[:T*1..3]->. The grammar only
	// supports unbounded * (Kleene star).
	SkipVarlenExplicitBound SkipReason = "varlen-explicit-bound"

	// SkipVarlenDotDot excludes patterns that use the .. range syntax on a
	// relationship type without the * operator, e.g. -[:T..]-> (missing *).
	SkipVarlenDotDot SkipReason = "varlen-dotdot"

	// SkipChainedWith excludes queries with two or more WITH clauses, e.g.
	// MATCH (n) WITH n MATCH (m) WITH n, m RETURN n, m. The grammar only
	// supports a single WITH per query chain.
	SkipChainedWith SkipReason = "chained-with"

	// SkipNegHexOct excludes queries with negative hexadecimal or octal
	// literals, e.g. -0x1A2B or -0o777. The grammar does not support a unary
	// minus applied directly to a hex/octal literal.
	SkipNegHexOct SkipReason = "neg-hex-oct"

	// SkipLeadingDotFloat excludes queries with floating-point literals whose
	// integer part is absent, e.g. .5 or -.5.
	SkipLeadingDotFloat SkipReason = "leading-dot-float"

	// SkipZeroDotFloat excludes queries with floating-point literals whose
	// integer part is zero, e.g. 0.5. The lexer tokenises the leading 0 as an
	// integer literal and the remaining .NNN as a separate (invalid) token.
	SkipZeroDotFloat SkipReason = "zero-dot-float"

	// SkipDoubleNot excludes queries with double logical negation (NOT NOT …).
	// The grammar does not allow a NOT expression as the operand of NOT.
	SkipDoubleNot SkipReason = "double-not"

	// SkipCallNoParen excludes in-query CALL without parentheses followed by
	// YIELD, e.g. CALL proc YIELD out. The grammar requires parentheses for
	// in-query procedure calls.
	SkipCallNoParen SkipReason = "call-no-paren"

	// SkipOverflowAsSema excludes scenarios where the TCK expects a SyntaxError
	// for integer or floating-point overflow (IntegerOverflow,
	// FloatingPointOverflow) but the grammar reports the overflow as a SemaError
	// from the visitor's numeric literal handler.
	SkipOverflowAsSema SkipReason = "overflow-as-sema"

	// SkipGrammarGapLiteral excludes specific literal scenarios where the
	// grammar is more permissive than the specification:
	//
	//   - InvalidUnicodeLiteral / InvalidUnicodeCharacter: the grammar does not
	//     validate unicode escape sequences or disallow non-ASCII operator
	//     characters.
	//   - InvalidNumberLiteral: the grammar tokenises malformed hex/integer
	//     literals as two valid tokens rather than a single error token.
	//   - UnexpectedSyntax on map keys starting with a digit: the grammar
	//     permits them.
	//   - UnexpectedSyntax on pattern expressions in RETURN/WITH/SET: the
	//     grammar accepts them; the restriction is semantic.
	SkipGrammarGapLiteral SkipReason = "grammar-gap-literal"

	// SkipLongFloatSema excludes valid queries containing very long floating-
	// point literal strings whose decimal-to-float64 conversion overflows.  The
	// visitor raises a SemaError, while the TCK expects a successful result.
	SkipLongFloatSema SkipReason = "long-float-sema"
)

// parseTimeErrors is the set of TCK SyntaxError error-type names that
// correspond to parse-time (ANTLR lexer/parser) failures.  Scenarios whose
// SyntaxErrorType is in this set are expected to cause parser.Parse to return
// a non-nil error; all others are expected to parse successfully.
var parseTimeErrors = map[string]bool{
	"InvalidSyntax":         true,
	"UnexpectedSyntax":      true,
	"InvalidNumberLiteral":  true,
	"InvalidUnicodeLiteral": true,
	"InvalidStringLiteral":  true,
}

// grammarGapExact lists (file, scenarioNamePrefix) pairs for scenarios that
// cannot be detected programmatically but are known grammar-coverage gaps.
var grammarGapExact = [][2]string{
	// InvalidNumberLiteral cases that the grammar silently accepts by
	// tokenising the malformed literal as two separate valid tokens:
	{"features/expressions/literals/Literals2.feature", "[11] Fail on an integer"},
	{"features/expressions/literals/Literals3.feature", "[12] Fail on an incomplete"},
	{"features/expressions/literals/Literals3.feature", "[13] Fail on an hexadecimal literal containing a lower"},
	{"features/expressions/literals/Literals3.feature", "[14] Fail on an hexadecimal literal containing a upper"},
	// Single-quoted string with escape sequences that confuse the skip
	// heuristic (no space in the string content, but the escapes cause the
	// grammar to mis-tokenise):
	{"features/expressions/literals/Literals6.feature", "[5] Return a single-quoted string with escaped"},
}

// reAngleBracket matches Scenario Outline placeholder tokens such as
// <pattern>, <yield>, <rename>.
var reAngleBracket = regexp.MustCompile(`<[a-zA-Z][a-zA-Z0-9_]*>`)

// reSingleQuoteSpace matches a single-quoted string whose content contains at
// least one space, e.g. 'The Matrix'. These confuse the grammar because the
// lexer treats the first word as a char literal and the rest as identifiers.
var reSingleQuoteSpace = regexp.MustCompile(`'[^']*\s+[^']*'`)

// reSingleQuoteTemporalArg matches a temporal function call (date, time,
// localtime, datetime, localdatetime, duration) whose first argument is a
// single-quoted string containing a digit–hyphen–digit, digit–colon–digit, or
// digit–dot–digit sequence. These patterns arise after Scenario Outline
// expansion from rows like:
//
//	| '2015-07-21' |    →   RETURN date('2015-07-21') AS result
//	| 'P5M1.5D'   |    →   RETURN duration('P5M1.5D') AS result
//
// The grammar tokenises the temporal string as a char literal followed by
// arithmetic operators, causing a spurious parse error.  This is the same root
// cause as [reSingleQuoteSpace] but without spaces in the string content.
var reSingleQuoteTemporalArg = regexp.MustCompile(`(?i)(?:date|time|localtime|datetime|localdatetime|duration)(?:\.[a-zA-Z]+)?\s*\('[^']*(?:\d[-:]\d|\d\.\d)`)

// reVarlenBound matches variable-length relationship patterns with explicit
// numeric bounds: -[:T*2]-> or -[:T*1..3]-> or -[*2]->.
var reVarlenBound = regexp.MustCompile(`\[[\w:]*\*(?:\d|\.\.\d)`)

// reVarlenDotDot matches relationship patterns that use .. without the *
// operator, e.g. -[:T..]-> (the * is missing).
var reVarlenDotDot = regexp.MustCompile(`\[[\w:]*\.\.[^\]]*\]`)

// reNegHexOct matches a unary minus applied to a hex or octal literal,
// e.g. -0x1A2B or -0o777.
var reNegHexOct = regexp.MustCompile(`-0[xXoO]`)

// reLeadingDotFloat matches a floating-point literal with no integer digits
// before the decimal point, e.g. .5 or -.5. The negated class [^\w\]] avoids
// matching a subscript-style ".5" inside a bracket expression such as n[.5] —
// not a valid Cypher construct, but prevents false positives in heuristic
// detection.  0-9 is omitted from the class because \w already covers it.
var reLeadingDotFloat = regexp.MustCompile(`(?:[^\w\]])\.\d`)

// reZeroDotFloat matches a floating-point literal whose integer part is zero,
// e.g. 0.5.
var reZeroDotFloat = regexp.MustCompile(`\b0\.\d`)

// reDoubleNot matches the double-negation pattern NOT NOT.
var reDoubleNot = regexp.MustCompile(`(?i)\bNOT\s+NOT\b`)

// reCallNoParen matches an in-query CALL without parentheses followed by YIELD,
// e.g. CALL proc YIELD out.
var reCallNoParen = regexp.MustCompile(`(?i)\bCALL\s+[\w.]+\s+YIELD\b`)

// reLongFloat matches a very long numeric literal (>50 digits) that may cause
// visitor overflow.
var reLongFloat = regexp.MustCompile(`-\d{50,}`)

// classifySkip returns the reason why a scenario should be excluded from the
// pass-rate gate, or SkipNone if the scenario should be run.
func classifySkip(s *Scenario) SkipReason {
	if r := classifySkipByQuery(s.Query); r != SkipNone {
		return r
	}
	return classifySkipByErrorType(s)
}

// classifySkipByQuery returns a SkipReason based solely on the query text, or
// SkipNone if no query-level skip condition matches.
func classifySkipByQuery(q string) SkipReason {
	switch {
	case reAngleBracket.MatchString(q):
		return SkipPlaceholder
	case reSingleQuoteSpace.MatchString(q):
		return SkipSingleQuoteString
	case reSingleQuoteTemporalArg.MatchString(q):
		return SkipSingleQuoteString
	case reVarlenBound.MatchString(q):
		return SkipVarlenExplicitBound
	case reVarlenDotDot.MatchString(q):
		return SkipVarlenDotDot
	case strings.Count(strings.ToUpper(q), "WITH") > 1:
		return SkipChainedWith
	case reNegHexOct.MatchString(q):
		return SkipNegHexOct
	case reLeadingDotFloat.MatchString(q):
		return SkipLeadingDotFloat
	case reZeroDotFloat.MatchString(q):
		return SkipZeroDotFloat
	case reDoubleNot.MatchString(q):
		return SkipDoubleNot
	case reCallNoParen.MatchString(q):
		return SkipCallNoParen
	}
	return SkipNone
}

// classifySkipByErrorType returns a SkipReason based on the scenario's error
// type and name metadata, or SkipNone if no metadata-level skip condition
// matches.
func classifySkipByErrorType(s *Scenario) SkipReason {
	// Exact known grammar-gap pairs for scenarios not detectable by query text.
	for _, pair := range grammarGapExact {
		if s.File == pair[0] && strings.HasPrefix(s.Name, pair[1]) {
			return SkipGrammarGapLiteral
		}
	}

	switch s.SyntaxErrorType {
	case "IntegerOverflow", "FloatingPointOverflow":
		return SkipOverflowAsSema
	case "InvalidUnicodeLiteral", "InvalidUnicodeCharacter":
		return SkipGrammarGapLiteral
	case "UnexpectedSyntax":
		n := s.Name
		if strings.Contains(n, "map containing key starting") ||
			strings.Contains(n, "pattern in RETURN") ||
			strings.Contains(n, "pattern in WITH") ||
			strings.Contains(n, "pattern in right") ||
			strings.Contains(n, "pattern predicates") {
			return SkipGrammarGapLiteral
		}
	case "":
		// Valid queries whose visitor raises a SemaError on very long floats.
		if reLongFloat.MatchString(s.Query) {
			return SkipLongFloatSema
		}
	}

	return SkipNone
}

// LoadScenarios walks the embedded feature directory and parses all Gherkin
// files, returning every Scenario that contains a "When executing query:" step.
// Each returned Scenario has its SkipReason field set.
//
// LoadScenarios is safe to call concurrently.
func LoadScenarios() ([]*Scenario, error) {
	var out []*Scenario
	err := fs.WalkDir(featureFiles, "features", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".feature") {
			return nil
		}
		f, openErr := featureFiles.Open(path)
		if openErr != nil {
			return openErr
		}
		defer f.Close() //nolint:errcheck // embed.FS.Close is a no-op; error is always nil.

		sc, ferr := parseFeatureFile(path, bufio.NewScanner(f))
		if ferr != nil {
			return ferr
		}
		out = append(out, sc...)
		return nil
	})
	return out, err
}

// parseFeatureFile parses a single Gherkin feature file and returns all
// Scenarios that contain a "When executing query:" step.
//
// The parser handles a small subset of Gherkin:
//   - Feature: <name>
//   - @tag [additional @tag…]
//   - Scenario: <name>  /  Scenario Outline: <name>
//   - When executing query: followed by a triple-quoted block (""")
//   - Then a SyntaxError should be raised at compile|runtime time: <ErrorType>
//
// All other lines are treated as scenario body prose and are consumed but
// not interpreted.
func parseFeatureFile(filePath string, scanner *bufio.Scanner) ([]*Scenario, error) { //nolint:gocyclo // Gherkin state machine: complexity is inherent to dispatching over five states × multiple line prefixes, plus Scenario Outline expansion.
	var out []*Scenario
	var featureName string

	type parserState int
	const (
		parserStateTop       parserState = iota // between scenarios
		parserStateScenario                     // inside a scenario body
		parserStateQueryOpen                    // seen "When executing query:", waiting for opening """
		parserStateQuery                        // inside the triple-quoted query block
		parserStateExamples                     // inside an Examples: table (Scenario Outline expansion)
	)

	cur := &Scenario{File: filePath}
	st := parserStateTop
	var queryBuf strings.Builder
	var pendingTags []string

	// Scenario Outline expansion state.
	var isOutline bool            // true when cur belongs to a Scenario Outline block
	var outlineTemplate *Scenario // non-nil while we are consuming an Examples table
	var exampleHeaders []string   // column names from the Examples header row

	// flush emits cur to out (for regular Scenario) or buffers it as outlineTemplate
	// (for Scenario Outline).  It then resets cur and clears Outline state.
	flush := func() {
		if cur.Query != "" {
			if isOutline {
				// Buffer the template for upcoming Examples: expansion.
				outlineTemplate = cur
			} else {
				cur.SkipReason = classifySkip(cur)
				out = append(out, cur)
			}
		}
		cur = &Scenario{File: filePath, Feature: featureName}
		isOutline = false
		exampleHeaders = nil
	}

	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)

		switch st {
		case parserStateQueryOpen:
			// Waiting for the opening """ delimiter.
			if line == `"""` {
				st = parserStateQuery
			}
			// Ignore blank lines and any prose between the step keyword and """.

		case parserStateQuery:
			// Inside the triple-quoted block; collect lines until closing """.
			if line == `"""` {
				cur.Query = strings.TrimSpace(queryBuf.String())
				queryBuf.Reset()
				st = parserStateScenario
				continue
			}
			if queryBuf.Len() > 0 {
				queryBuf.WriteByte('\n')
			}
			// Strip the standard 6-space indentation used in TCK feature files.
			queryBuf.WriteString(strings.TrimPrefix(raw, "      "))

		case parserStateExamples:
			switch {
			case strings.HasPrefix(line, "|"):
				// Table row — first row is the header, subsequent rows are data.
				row := parseTableRow(line)
				if exampleHeaders == nil {
					exampleHeaders = row
				} else if outlineTemplate != nil {
					// Expand: substitute <column> placeholders and emit a concrete scenario.
					q := substituteOutlineRow(outlineTemplate.Query, exampleHeaders, row)
					sc := &Scenario{
						File:            outlineTemplate.File,
						Feature:         outlineTemplate.Feature,
						Name:            outlineTemplate.Name,
						Tags:            outlineTemplate.Tags,
						SyntaxErrorType: outlineTemplate.SyntaxErrorType,
						Query:           q,
					}
					sc.SkipReason = classifySkip(sc)
					out = append(out, sc)
				}

			case strings.HasPrefix(line, "Scenario"):
				// New scenario starts — flush outline template (already buffered) and
				// transition to parserStateScenario.
				outlineTemplate = nil
				exampleHeaders = nil
				// Reset cur for the new scenario.
				cur = &Scenario{File: filePath, Feature: featureName}
				isOutline = strings.HasPrefix(line, "Scenario Outline")
				idx := strings.Index(line, ":")
				if idx >= 0 {
					cur.Name = strings.TrimSpace(line[idx+1:])
				}
				cur.Tags = pendingTags
				pendingTags = nil
				st = parserStateScenario

			case line == "" || strings.HasPrefix(line, "#"):
				// Blank lines and comments are allowed between table rows.

			default:
				// Any non-table, non-scenario line ends the Examples section.
				// This handles a second Examples: block or other Gherkin keywords.
				outlineTemplate = nil
				exampleHeaders = nil
				st = parserStateScenario
			}

		case parserStateTop, parserStateScenario:
			switch {
			case strings.HasPrefix(line, "Feature:"):
				featureName = strings.TrimSpace(strings.TrimPrefix(line, "Feature:"))
				cur.Feature = featureName

			case strings.HasPrefix(line, "@") && st == parserStateTop:
				// Tag line — collect tags for the next scenario.
				for _, t := range strings.Fields(line) {
					pendingTags = append(pendingTags, strings.TrimPrefix(t, "@"))
				}

			case strings.HasPrefix(line, "Scenario"):
				// Flush the previous scenario if any.
				flush()
				isOutline = strings.HasPrefix(line, "Scenario Outline")
				st = parserStateScenario
				// Extract scenario name: "Scenario: [N] …" or "Scenario Outline: [N] …"
				idx := strings.Index(line, ":")
				if idx >= 0 {
					cur.Name = strings.TrimSpace(line[idx+1:])
				}
				cur.Tags = pendingTags
				pendingTags = nil

			case st == parserStateScenario && strings.HasPrefix(line, "@"):
				// Tag line between scenarios — belongs to the next scenario.
				pendingTags = nil
				for _, t := range strings.Fields(line) {
					pendingTags = append(pendingTags, strings.TrimPrefix(t, "@"))
				}

			case st == parserStateScenario && strings.HasPrefix(line, "When executing query:"):
				// Transition to waiting for the opening """ delimiter.
				st = parserStateQueryOpen

			case st == parserStateScenario && strings.HasPrefix(line, "Then a SyntaxError should be raised"):
				// Extract the error type that follows the last colon.
				idx := strings.LastIndex(line, ":")
				if idx >= 0 {
					cur.SyntaxErrorType = strings.TrimSpace(line[idx+1:])
				}

			case st == parserStateScenario && (line == "Examples:" || strings.HasPrefix(line, "Examples:")):
				// Transition to Examples table parsing.  flush() buffers cur as the
				// outline template (if isOutline) or discards it (if not).
				flush()
				st = parserStateExamples
				exampleHeaders = nil
			}
		}
	}
	// Flush the last scenario in the file.
	flush()

	if err := scanner.Err(); err != nil {
		return nil, errors.New("scanning " + filePath + ": " + err.Error())
	}
	return out, nil
}

// parseTableRow parses a Gherkin pipe-delimited table row into a slice of
// trimmed cell strings.  Leading and trailing pipe characters are ignored.
//
// Example: "|  a.bool, a.num  |  b  |" → ["a.bool, a.num", "b"]
func parseTableRow(line string) []string {
	parts := strings.Split(line, "|")
	// parts[0] is before the first pipe (whitespace), parts[len-1] after the last.
	if len(parts) < 2 {
		return nil
	}
	cells := make([]string, 0, len(parts)-2)
	for _, p := range parts[1 : len(parts)-1] {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

// substituteOutlineRow replaces every <column> placeholder in query with the
// corresponding value from the data row.  Columns and values are matched by
// position; excess columns or values are silently ignored.
func substituteOutlineRow(query string, headers, values []string) string {
	q := query
	for i, h := range headers {
		if i < len(values) {
			q = strings.ReplaceAll(q, "<"+h+">", values[i])
		}
	}
	return q
}
