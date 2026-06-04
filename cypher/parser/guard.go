package parser

// This file implements the pre-parse input guard. A Cypher query is parsed by
// building an ANTLR parse tree and walking it with a recursive visitor (see
// visitor.go). Each level of bracket/paren/brace nesting consumes one Go stack
// frame both in ANTLR's recursive-descent parser and in the visitor. A query
// with sufficiently deep nesting — e.g. RETURN ((((( … ))))) or RETURN [[[ … ]]]
// — therefore drives unbounded recursion and triggers a Go `fatal error: stack
// overflow`. A stack overflow is fatal and CANNOT be caught by recover(), so it
// kills the host process. This is reachable from any untrusted query text
// (Engine.Run, and remotely via the Bolt server), making it a denial-of-service
// vector.
//
// The guard runs once, before lexing/parsing begins, because once recursion has
// overflowed the stack there is no way to recover. It performs a single O(n)
// linear pass over the raw query bytes with no heap allocation.

const (
	// maxQueryBytes is the maximum accepted length, in bytes, of a query string.
	// Queries longer than this are rejected outright with a [*ParseError]. The
	// limit is a sanity ceiling, not a functional constraint: 1 MiB is far above
	// any legitimate Cypher query — including the largest scenario in the
	// openCypher TCK — yet bounds the work a single untrusted request can force.
	maxQueryBytes = 1 << 20 // 1 MiB

	// maxNestingDepth is the maximum accepted depth of bracket nesting — the
	// combined nesting of '(', '[' and '{'. Queries that nest deeper are
	// rejected with a [*ParseError] before any recursion begins, which is the
	// actual fix for the stack-overflow DoS (the byte-length cap alone is
	// insufficient, as even a small query can nest tens of thousands of levels).
	//
	// The bound is generous: legitimate Cypher — and every openCypher TCK
	// scenario — nests only a handful of levels. 256 leaves ample head-room for
	// any real query while keeping the worst-case parse recursion well within
	// the goroutine stack.
	maxNestingDepth = 256
)

// CheckQueryLength returns a [*ParseError] when query exceeds the maximum
// accepted query length (1 MiB), and nil otherwise. It is the byte-length half
// of [guardInput], exported so the DDL parse path — which routes through
// [github.com/FlavioCFOliveira/GoGraph/cypher/ir.ParseDDL] rather than this
// package's [Parse] — enforces the SAME cap with the SAME error message,
// keeping the limit defined in exactly one place ([maxQueryBytes]). The DDL
// tokeniser is iterative, so the nesting half of the guard is not needed there.
//
// The returned error is a non-nil *ParseError when the limit is exceeded; the
// typed nil is deliberately avoided (callers compare the interface against nil).
func CheckQueryLength(query string) *ParseError {
	if len(query) > maxQueryBytes {
		return &ParseError{
			Line:   1,
			Column: 0,
			Message: "query too large: " + itoa(len(query)) +
				" bytes exceeds the limit of " + itoa(maxQueryBytes) + " bytes",
		}
	}
	return nil
}

// guardInput validates a raw query string before lexing and parsing. It returns
// a [*ParseError] when the query exceeds [maxQueryBytes] in length or when its
// bracket nesting exceeds [maxNestingDepth]. It returns nil when the query is
// within both bounds.
//
// The nesting check is performed by [maxBracketDepth], which ignores brackets
// that appear inside string literals, escaped identifiers, and comments so that
// a legitimate query such as RETURN '(((' AS s is never rejected.
//
// guardInput runs in O(n) time over the query bytes and allocates nothing on
// the success path beyond the returned error value on rejection.
func guardInput(query string) *ParseError {
	if err := CheckQueryLength(query); err != nil {
		return err
	}
	if depth := maxBracketDepth(query); depth > maxNestingDepth {
		return &ParseError{
			Line:   1,
			Column: 0,
			Message: "query nesting too deep: bracket depth " + itoa(depth) +
				" exceeds the limit of " + itoa(maxNestingDepth),
		}
	}
	return nil
}

// scanState enumerates the lexical contexts the bracket scanner tracks. Brackets
// are only counted in [stateNormal]; in every other state the scanner is
// skipping content where '(', '[' and '{' carry no structural meaning.
type scanState uint8

const (
	stateNormal       scanState = iota // outside any string or comment
	stateSingle                        // inside a '...' single-quoted string
	stateDouble                        // inside a "..." double-quoted string
	stateBacktick                      // inside a `...` escaped identifier
	stateLineComment                   // inside a // ... line comment
	stateBlockComment                  // inside a /* ... */ block comment
)

// maxBracketDepth returns the maximum simultaneous nesting depth of '(', '[' and
// '{' in the query, counting opens and closes but ignoring any that appear
// inside Cypher string literals, escaped identifiers, or comments.
//
// String, identifier, and comment syntax follow the Cypher lexer grammar:
//
//   - '...'  single-quoted string; '\' escapes the next byte (CHAR_LITERAL).
//   - "..."  double-quoted string; '\' escapes the next byte (STRING_LITERAL).
//   - `...`  escaped identifier; no escapes — the next backtick closes it
//     (ESC_LITERAL).
//   - // ... line comment, terminated by a newline (LINE_COMMENT).
//   - /* ... */ block comment (COMMENT).
//
// The function iterates over raw bytes. All delimiter and bracket characters are
// ASCII (single-byte in UTF-8); multi-byte runes only ever contribute
// continuation bytes >= 0x80, which never collide with the ASCII bytes inspected
// here, so a byte-level scan is correct without decoding runes. The scan runs in
// O(n) time and allocates nothing.
func maxBracketDepth(query string) int {
	state := stateNormal
	depth := 0
	maxDepth := 0

	for i := 0; i < len(query); i++ {
		c := query[i]
		switch state {
		case stateNormal:
			switch c {
			case '(', '[', '{':
				depth++
				if depth > maxDepth {
					maxDepth = depth
				}
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			case '\'':
				state = stateSingle
			case '"':
				state = stateDouble
			case '`':
				state = stateBacktick
			case '/':
				if i+1 < len(query) {
					switch query[i+1] {
					case '/':
						state = stateLineComment
						i++ // consume the second '/'
					case '*':
						state = stateBlockComment
						i++ // consume the '*'
					}
				}
			}
		case stateSingle:
			switch c {
			case '\\':
				i++ // skip the escaped byte (incl. an escaped closing quote)
			case '\'':
				state = stateNormal
			}
		case stateDouble:
			switch c {
			case '\\':
				i++ // skip the escaped byte (incl. an escaped closing quote)
			case '"':
				state = stateNormal
			}
		case stateBacktick:
			// ESC_LITERAL has no escape sequence: the next backtick closes it.
			if c == '`' {
				state = stateNormal
			}
		case stateLineComment:
			if c == '\n' || c == '\r' {
				state = stateNormal
			}
		case stateBlockComment:
			if c == '*' && i+1 < len(query) && query[i+1] == '/' {
				state = stateNormal
				i++ // consume the '/'
			}
		}
	}
	return maxDepth
}

// itoa renders a non-negative int as decimal without importing strconv, keeping
// the guard's dependency surface minimal. Negative values are not expected here
// (lengths and depths are always >= 0).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte // enough for a 64-bit int
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
