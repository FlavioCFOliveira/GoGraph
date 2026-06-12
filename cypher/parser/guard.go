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

	// maxCASEKeywords is the maximum number of CASE keywords accepted in a single
	// query. Each CASE creates a recursive call in checkExpr so deep nesting —
	// possible without any brackets — can overflow the Go stack. 256 is far above
	// any legitimate Cypher query's needs.
	maxCASEKeywords = maxNestingDepth // 256

	// maxBinaryOpTokens is the maximum number of binary infix operator tokens
	// (AND, OR, XOR, NOT, +, /, %, ^) accepted in a single query. Long
	// left-recursive chains create a left-deep BinaryOp AST whose checkExpr
	// recursion depth equals the operator count. 512 is generous for any real query.
	// Note: '-' and '*' are excluded — they appear structurally in relationship
	// arrows and variable-length path patterns respectively.
	maxBinaryOpTokens = 512
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
// a [*ParseError] when the query exceeds [maxQueryBytes] in length, when its
// bracket nesting exceeds [maxNestingDepth], when its CASE keyword count exceeds
// [maxCASEKeywords], or when its binary operator token count exceeds
// [maxBinaryOpTokens]. It returns nil when the query is within all bounds.
//
// The nesting check is performed by [maxBracketDepth], the CASE check by
// [countCASEKeywords], and the operator check by [countBinaryOpTokens]. All
// three scanners ignore content inside string literals, escaped identifiers, and
// comments so that a legitimate query such as RETURN '(((' AS s is never
// rejected.
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
	if count := countCASEKeywords(query); count > maxCASEKeywords {
		return &ParseError{
			Line:   1,
			Column: 0,
			Message: "query nesting too deep: CASE keyword count " + itoa(count) +
				" exceeds the limit of " + itoa(maxCASEKeywords),
		}
	}
	if count := countBinaryOpTokens(query); count > maxBinaryOpTokens {
		return &ParseError{
			Line:   1,
			Column: 0,
			Message: "query expression too complex: binary operator count " + itoa(count) +
				" exceeds the limit of " + itoa(maxBinaryOpTokens),
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

// isIdentByte reports whether b is a letter, digit, or underscore — the
// characters that may appear inside a Cypher identifier. It is used by the
// keyword scanners to enforce word boundaries without any Unicode awareness:
// all Cypher ASCII keywords are bounded by non-identifier bytes, and multi-byte
// UTF-8 continuation bytes (>= 0x80) never collide with the ASCII ranges
// tested here.
func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

// countCASEKeywords returns the number of CASE keywords (case-insensitive) in
// query that appear in normal code context — outside string literals, escaped
// identifiers, and comments. Each CASE creates a recursive call in checkExpr,
// so a query with more than [maxCASEKeywords] CASE tokens can drive unbounded
// recursion without any bracket nesting.
//
// Word-boundary semantics: a four-byte sequence c/C a/A s/S e/E is counted
// only when the byte immediately before the 'C' is not an identifier character
// (or the 'C' is at position 0) and the byte immediately after the 'E' is not
// an identifier character (or 'E' is the last byte).
//
// The scan shares the same lexical-state machine as [maxBracketDepth] and runs
// in O(n) time with no heap allocation.
func countCASEKeywords(query string) int {
	state := stateNormal
	count := 0

	for i := 0; i < len(query); i++ {
		c := query[i]
		switch state {
		case stateNormal:
			switch c {
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
						i++
					case '*':
						state = stateBlockComment
						i++
					}
				}
			default:
				// Fast path: only inspect when current byte is 'C' or 'c'.
				if c != 'C' && c != 'c' {
					continue
				}
				// Need at least 4 bytes remaining (C A S E).
				if i+3 >= len(query) {
					continue
				}
				// Check 'A'/'a', 'S'/'s', 'E'/'e'.
				c1, c2, c3 := query[i+1], query[i+2], query[i+3]
				if (c1 != 'A' && c1 != 'a') ||
					(c2 != 'S' && c2 != 's') ||
					(c3 != 'E' && c3 != 'e') {
					continue
				}
				// Check left word boundary.
				if i > 0 && isIdentByte(query[i-1]) {
					continue
				}
				// Check right word boundary.
				if i+4 < len(query) && isIdentByte(query[i+4]) {
					continue
				}
				count++
				i += 3 // skip 'A', 'S', 'E'
			}
		case stateSingle:
			switch c {
			case '\\':
				i++
			case '\'':
				state = stateNormal
			}
		case stateDouble:
			switch c {
			case '\\':
				i++
			case '"':
				state = stateNormal
			}
		case stateBacktick:
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
				i++
			}
		}
	}
	return count
}

// matchKeywordOp reports whether query[i:] begins with the keyword operator kw
// (case-insensitive) at a word boundary on both sides, and returns the number
// of extra bytes to skip (len(kw)-1) so the caller can advance i past the
// keyword. It returns -1 when there is no match.
//
// prevIsIdent must be true when query[i-1] is an identifier byte (i.e. the
// left boundary is NOT clear); the caller passes this to avoid re-computing it.
func matchKeywordOp(query string, i int, prevIsIdent bool, kw string) int {
	n := len(kw)
	if i+n-1 >= len(query) {
		return -1
	}
	// Match kw case-insensitively.
	for j := 0; j < n; j++ {
		b := query[i+j]
		k := kw[j]
		// Lowercase b for comparison.
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if b != k {
			return -1
		}
	}
	// Left boundary.
	if prevIsIdent {
		return -1
	}
	// Right boundary.
	if i+n < len(query) && isIdentByte(query[i+n]) {
		return -1
	}
	return n - 1 // bytes to advance beyond the first char
}

// countBinaryOpTokens returns the number of binary infix operator tokens in
// query that appear in normal code context — outside string literals, escaped
// identifiers, and comments. Counted tokens are:
//
//   - Keyword operators: AND, OR, XOR, NOT (case-insensitive, word-bounded)
//   - Symbol operators:  +  /  %  ^
//
// Note: '-' and '*' are deliberately excluded. In Cypher '-' is overwhelmingly
// used in relationship arrows '(a)-[r]->(b)' and '*' is used in variable-length
// path patterns '[:REL*1..n]'. Counting them would produce false positives on
// legitimate large CREATE/MATCH queries, breaking TCK conformance. The recursion
// risk targeted here is expression-level BinaryOp chains (AND/OR/XOR/NOT/+/%/^),
// which do not suffer from this ambiguity.
//
// Long left-recursive chains of these operators build left-deep BinaryOp AST
// nodes whose checkExpr recursion depth equals the operator count. A query with
// more than [maxBinaryOpTokens] such tokens can therefore drive unbounded
// recursion with no bracket nesting.
//
// The scan shares the same lexical-state machine as [maxBracketDepth] and runs
// in O(n) time with no heap allocation.
func countBinaryOpTokens(query string) int {
	state := stateNormal
	count := 0

	for i := 0; i < len(query); i++ {
		c := query[i]
		switch state {
		case stateNormal:
			count, i, state = countBinaryOpNormal(query, i, c, count, state)
		case stateSingle:
			switch c {
			case '\\':
				i++
			case '\'':
				state = stateNormal
			}
		case stateDouble:
			switch c {
			case '\\':
				i++
			case '"':
				state = stateNormal
			}
		case stateBacktick:
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
				i++
			}
		}
	}
	return count
}

// countBinaryOpNormal handles one byte in stateNormal for [countBinaryOpTokens].
// It returns updated (count, i, state).
func countBinaryOpNormal(query string, i int, c byte, count int, state scanState) (int, int, scanState) {
	prevIsIdent := i > 0 && isIdentByte(query[i-1])
	switch c {
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
				i++
				return count, i, state
			case '*':
				state = stateBlockComment
				i++
				return count, i, state
			}
		}
		// '/' not followed by '/' or '*' — divide operator.
		count++
	case '+', '%', '^':
		count++
	case 'A', 'a':
		if skip := matchKeywordOp(query, i, prevIsIdent, "and"); skip >= 0 {
			count++
			i += skip
		}
	case 'O', 'o':
		if skip := matchKeywordOp(query, i, prevIsIdent, "or"); skip >= 0 {
			count++
			i += skip
		}
	case 'X', 'x':
		if skip := matchKeywordOp(query, i, prevIsIdent, "xor"); skip >= 0 {
			count++
			i += skip
		}
	case 'N', 'n':
		if skip := matchKeywordOp(query, i, prevIsIdent, "not"); skip >= 0 {
			count++
			i += skip
		}
	}
	return count, i, state
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
