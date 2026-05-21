package parser

// normalizeArithmeticMinus inserts a space before a '-' that immediately
// follows an identifier character (a-z, A-Z, 0-9, _) and is itself
// immediately followed by a decimal digit.
//
// Background: the ANTLR DIGIT lexer rule matches an optional leading '-' so
// that negative integer literals tokenise as a single DIGIT token.  This is
// desirable for range bounds inside relationship patterns (e.g. [r*-1..-3])
// but causes incorrect tokenisation when a binary subtraction operator is
// written without spaces (e.g. numOfValues-1 → numOfValues + DIGIT(-1)).
// Inserting a space before the '-' separates the subtraction operator from
// the operand, causing the lexer to emit SUB followed by a separate
// ID/DIGIT token, which the parser can accept as a binary expression.
//
// The rewriter only fires when ALL of the following hold:
//  1. The character before '-' is an identifier character (a-z, A-Z, 0-9, _).
//  2. The character after '-' is a decimal digit (0-9).
//  3. The position is not inside a double-quoted string, single-quoted string,
//     backtick identifier, or comment.
//
// This is safe because:
//   - Bare identifier−integer subtraction (e.g. n-1, count-2) requires a
//     syntactic binary operator; inserting a space does not change semantics.
//   - The resulting token stream is identical to what the author intended.
//   - The normalizeVarlenBounds normalizer, which runs after this one, expects
//     its input to already have natural-number bounds in [r*N..M] form; this
//     normalizer does not affect those brackets because the digit after '-' in
//     "r*-1" is reached from '*', not from an identifier character.
//
// Fast path: return unchanged if q contains no '-' byte.
//
//nolint:gocyclo // byte-scanner with per-character branches; same pattern as normalizeVarlenBounds
func normalizeArithmeticMinus(q string) string {
	if !hasByte(q, '-') {
		return q
	}

	buf := make([]byte, 0, len(q)+4)
	i := 0
	n := len(q)

	skipStringOrComment := func() {
		ch := q[i]
		switch ch {
		case '"':
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\\' && i < n {
					buf = append(buf, q[i])
					i++
				} else if c == '"' {
					break
				}
			}
		case '\'':
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\'' {
					break
				}
			}
		case '`':
			buf = append(buf, ch)
			i++
			for i < n && q[i] != '`' {
				buf = append(buf, q[i])
				i++
			}
			if i < n {
				buf = append(buf, '`')
				i++
			}
		case '/':
			if i+1 < n && q[i+1] == '/' {
				for i < n && q[i] != '\n' {
					buf = append(buf, q[i])
					i++
				}
			} else if i+1 < n && q[i+1] == '*' {
				buf = append(buf, q[i], q[i+1])
				i += 2
				for i < n {
					if q[i] == '*' && i+1 < n && q[i+1] == '/' {
						buf = append(buf, '*', '/')
						i += 2
						break
					}
					buf = append(buf, q[i])
					i++
				}
			}
		}
	}

	isIdentChar := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '_'
	}

	for i < n {
		ch := q[i]

		// Guard: skip over lexical atoms that must not be rewritten.
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			skipStringOrComment()
			continue
		}

		if ch != '-' {
			buf = append(buf, ch)
			i++
			continue
		}

		// ch == '-': check for IdentChar - Digit pattern.
		if len(buf) > 0 && isIdentChar(buf[len(buf)-1]) &&
			i+1 < n && q[i+1] >= '0' && q[i+1] <= '9' {
			// Guard: do not rewrite scientific notation exponents such as
			// "1.5e-3" or ".1e-5". When the preceding character is 'e' or 'E'
			// (the exponent marker in a float literal), the '-' is part of the
			// exponent sign, not a binary subtraction operator.
			prev := buf[len(buf)-1]
			if prev == 'e' || prev == 'E' {
				buf = append(buf, ch)
				i++
				continue
			}
			// Binary subtraction written without spaces: insert spaces around '-'
			// so the lexer cannot consume '-N' as a single DIGIT token.
			buf = append(buf, ' ', '-', ' ')
			i++
			continue
		}

		buf = append(buf, ch)
		i++
	}

	return string(buf)
}

// normalizeVarlenBounds rewrites unsigned integer range bounds inside
// relationship patterns from the form [r*N..M] to [r*-N..-M] so that the
// ANTLR lexer tokenizes them as DIGIT (type 93) instead of ID (type 89).
//
// Background: the generated lexer places the ID rule before the DIGIT rule in
// the ATN, so a pure digit sequence like "1" or "42" always tokenizes as ID.
// The parser's rangeLit rule and its ATN prediction require DIGIT tokens; with
// ID tokens the adaptive-prediction engine rejects the input before rangeLit
// is even entered.  Negated integers (e.g. "-1") tokenize as DIGIT because the
// DIGIT grammar rule has the form SUB? (...), and the SUB prefix shifts the
// match away from the ID rule.
//
// This transformation is safe because:
//   - Negative range bounds have no valid semantics in Cypher (hop counts are
//     always ≥ 0), so no existing valid query contains -N in a range literal.
//   - visitRangeLit takes the absolute value of parsed bounds, so the semantic
//     result is identical to the original positive value.
//   - The scanner respects double-quoted strings, backtick identifiers, and
//     both line (//) and block (/* */) comments — the same guard used in
//     normalizeSingleQuotes.
//
// Only range-literal digit sequences are rewritten; identifiers, property
// values, and other uses of digits elsewhere in the query are untouched because
// the transform is scoped inside [...] brackets after a '*' (or '..' separator).
//
// Fast path: if q contains no '*' rune, q is returned unchanged.
//
//nolint:gocyclo // byte-scanner with per-character branches; same pattern as normalizeSingleQuotes
func normalizeVarlenBounds(q string) string {
	if !hasByte(q, '*') {
		return q
	}

	buf := make([]byte, 0, len(q))
	i := 0
	n := len(q)

	// skip advances i past a double-quoted string, backtick identifier, or
	// comment, copying all bytes verbatim into buf.
	skipStringOrComment := func() {
		ch := q[i]
		switch ch {
		case '"':
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\\' && i < n {
					buf = append(buf, q[i])
					i++
				} else if c == '"' {
					break
				}
			}
		case '\'':
			// Already normalised by normalizeSingleQuotes, but guard anyway.
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\'' {
					break
				}
			}
		case '`':
			buf = append(buf, ch)
			i++
			for i < n && q[i] != '`' {
				buf = append(buf, q[i])
				i++
			}
			if i < n {
				buf = append(buf, '`')
				i++
			}
		case '/':
			if i+1 < n && q[i+1] == '/' {
				for i < n && q[i] != '\n' {
					buf = append(buf, q[i])
					i++
				}
			} else if i+1 < n && q[i+1] == '*' {
				buf = append(buf, q[i], q[i+1])
				i += 2
				for i < n {
					if q[i] == '*' && i+1 < n && q[i+1] == '/' {
						buf = append(buf, '*', '/')
						i += 2
						break
					}
					buf = append(buf, q[i])
					i++
				}
			}
		}
	}

	// writeNegDigits reads a run of ASCII digits from q[i:] and emits "-<digits>"
	// so the lexer tokenizes the sequence as a DIGIT token instead of ID.
	// Returns true if any digits were emitted.
	writeNegDigits := func() bool {
		start := i
		for i < n && q[i] >= '0' && q[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
		buf = append(buf, '-')
		buf = append(buf, q[start:i]...)
		return true
	}

	for i < n {
		ch := q[i]

		// Guard: skip over lexical atoms that must not be rewritten.
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			skipStringOrComment()
			continue
		}

		// Only rewrite inside [...] brackets.
		if ch != '[' {
			buf = append(buf, ch)
			i++
			continue
		}

		// We are at '['. Copy until we find '*' (or ']' closes the bracket).
		buf = append(buf, ch)
		i++
		depth := 1
		for i < n && depth > 0 {
			c := q[i]
			switch {
			case c == '[':
				depth++
				buf = append(buf, c)
				i++
			case c == ']':
				depth--
				buf = append(buf, c)
				i++
			case c == '"' || c == '\'' || c == '`' ||
				(c == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')):
				skipStringOrComment()
			case c == '*':
				// Found a range-literal marker.
				buf = append(buf, c)
				i++
				// Optional lower bound: rewrite digits as negative.
				writeNegDigits()
				// Optional '..' followed by optional upper bound.
				if i+1 < n && q[i] == '.' && q[i+1] == '.' {
					buf = append(buf, '.', '.')
					i += 2
					// Optional upper bound.
					writeNegDigits()
				}
			default:
				buf = append(buf, c)
				i++
			}
		}
	}

	return string(buf)
}

// normalizeVarlenDotDot rewrites varlen relationship patterns that use the
// ".." range syntax without a leading "*" to the equivalent "*" form, so the
// existing ANTLR grammar rule (which requires "*") can parse them.
//
// Background: the grammar accepts "-[r*N..M]->" (variable-length with explicit
// bounds) but not "-[r..M]->" or "-[r..]->". Adding "*" (with no lower bound)
// before the ".." makes the pattern equivalent and accepted by the grammar.
//
// Only the following forms inside "[...]" are rewritten:
//
//	[..]         →  [*..]
//	[..M]        →  [*..M]
//	[N..]        →  [*N..]
//	[N..M]       →  [*N..M]
//	[:T..]       →  [:T*..]
//	[:T..M]      →  [:T*..M]
//	[:TN..]      →  [:TN*..]    (type label followed by bound digits)
//	etc.
//
// Forms that already have "*" (e.g. [r*..], [r*N..M]) are left unchanged.
//
// The rewriter operates on the content inside "[...]" brackets. It inserts "*"
// immediately before the first ".." it finds if and only if there is no "*"
// already between the "[" opener and the "..".
//
// The scanner respects double-quoted strings, backtick identifiers, and both
// line (//) and block (/* */) comments.
//
// Fast path: return unchanged if q contains no '.' byte.
//
//nolint:gocyclo // byte-scanner with bracket-tracking state; same pattern as normalizeVarlenBounds
func normalizeVarlenDotDot(q string) string {
	if !hasByte(q, '.') {
		return q
	}

	buf := make([]byte, 0, len(q)+4)
	i := 0
	n := len(q)

	skipStringOrComment := func() {
		ch := q[i]
		switch ch {
		case '"':
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\\' && i < n {
					buf = append(buf, q[i])
					i++
				} else if c == '"' {
					break
				}
			}
		case '\'':
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\'' {
					break
				}
			}
		case '`':
			buf = append(buf, ch)
			i++
			for i < n && q[i] != '`' {
				buf = append(buf, q[i])
				i++
			}
			if i < n {
				buf = append(buf, '`')
				i++
			}
		case '/':
			if i+1 < n && q[i+1] == '/' {
				for i < n && q[i] != '\n' {
					buf = append(buf, q[i])
					i++
				}
			} else if i+1 < n && q[i+1] == '*' {
				buf = append(buf, q[i], q[i+1])
				i += 2
				for i < n {
					if q[i] == '*' && i+1 < n && q[i+1] == '/' {
						buf = append(buf, '*', '/')
						i += 2
						break
					}
					buf = append(buf, q[i])
					i++
				}
			}
		}
	}

	for i < n {
		ch := q[i]

		// Guard: skip over lexical atoms that must not be rewritten.
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			skipStringOrComment()
			continue
		}

		if ch != '[' {
			buf = append(buf, ch)
			i++
			continue
		}

		// We are at '['. Only apply the ".." → "*.." rewrite when this '[' is
		// part of a relationship pattern, i.e. when the immediately preceding
		// non-whitespace character is '-' (as in -(n)-[...]-).
		// List subscript brackets are preceded by an identifier character, ')',
		// or ']', and must not be modified.
		prevIsHyphen := len(buf) > 0 && buf[len(buf)-1] == '-'

		buf = append(buf, ch)
		i++

		if !prevIsHyphen {
			// Not a relationship pattern bracket — copy content verbatim until
			// the outer loop's next iteration picks up the remaining characters.
			continue
		}

		// Relationship pattern bracket: scan inside looking for ".." without "*".
		hasStar := false

		for i < n {
			c := q[i]

			// Guard inside bracket.
			if c == '"' || c == '\'' || c == '`' ||
				(c == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
				skipStringOrComment()
				continue
			}

			if c == ']' {
				buf = append(buf, c)
				i++
				break
			}

			if c == '*' {
				hasStar = true
				buf = append(buf, c)
				i++
				continue
			}

			if c == '.' && i+1 < n && q[i+1] == '.' && !hasStar {
				// Found ".." without a preceding "*": insert "*" before "..".
				buf = append(buf, '*', '.', '.')
				i += 2
				hasStar = true
				continue
			}

			buf = append(buf, c)
			i++
		}
	}

	return string(buf)
}

// normalizeZeroDotFloat rewrites zero-prefixed floating-point literals of the
// form 0.N (where N is one or more digits) to the equivalent leading-dot form
// .N so that the ANTLR lexer produces a single FLOAT token.
//
// Background: the generated ANTLR lexer tokenises "0" as an ID token (because
// the ID rule precedes DIGIT in the ATN) and the subsequent ".NNN" as a
// separate FLOAT token, yielding two tokens where the parser expects one
// numeric literal expression.  The leading-dot form ".NNN" is tokenised
// correctly as a single FLOAT token (for non-zero first digits) or handled by
// normalizeLeadingDotFloat (for zero first digits).
//
// Semantically 0.5 == .5, so the rewrite is safe.  visitRangeLit and the
// visitor's literal handler are unaffected because they operate on the parsed
// token text, not the original source.
//
// The rewriter scopes the transform tightly: it only rewrites a "0" that is
//   - immediately followed by "." then at least one digit,
//   - NOT preceded by an identifier character (a-z, A-Z, 0-9, _) — which
//     would indicate a longer integer literal such as "10.5" (valid and handled
//     correctly by the lexer as two adjacent floats/integers) or a hex literal.
//
// The scanner respects double-quoted strings, backtick identifiers, and both
// line (//) and block (/* */) comments.
//
// Fast path: return unchanged if q contains no '0' byte.
//
//nolint:gocyclo // byte-scanner with per-character branches; same pattern as normalizeVarlenBounds
func normalizeZeroDotFloat(q string) string {
	if !hasByte(q, '0') {
		return q
	}

	buf := make([]byte, 0, len(q))
	i := 0
	n := len(q)

	skipStringOrComment := func() {
		ch := q[i]
		switch ch {
		case '"':
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\\' && i < n {
					buf = append(buf, q[i])
					i++
				} else if c == '"' {
					break
				}
			}
		case '\'':
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\'' {
					break
				}
			}
		case '`':
			buf = append(buf, ch)
			i++
			for i < n && q[i] != '`' {
				buf = append(buf, q[i])
				i++
			}
			if i < n {
				buf = append(buf, '`')
				i++
			}
		case '/':
			if i+1 < n && q[i+1] == '/' {
				for i < n && q[i] != '\n' {
					buf = append(buf, q[i])
					i++
				}
			} else if i+1 < n && q[i+1] == '*' {
				buf = append(buf, q[i], q[i+1])
				i += 2
				for i < n {
					if q[i] == '*' && i+1 < n && q[i+1] == '/' {
						buf = append(buf, '*', '/')
						i += 2
						break
					}
					buf = append(buf, q[i])
					i++
				}
			}
		}
	}

	isIdentChar := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '_'
	}

	for i < n {
		ch := q[i]

		// Guard: skip over lexical atoms that must not be rewritten.
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			skipStringOrComment()
			continue
		}

		if ch != '0' {
			buf = append(buf, ch)
			i++
			continue
		}

		// ch == '0': check for "0.digit" pattern.
		if i+2 < n && q[i+1] == '.' && q[i+2] >= '0' && q[i+2] <= '9' {
			// Guard: if preceded by an ident char, this is part of a larger
			// number literal (e.g. "10.5") — do not strip the leading zero.
			if len(buf) > 0 && isIdentChar(buf[len(buf)-1]) {
				buf = append(buf, ch)
				i++
				continue
			}
			// Drop the leading '0'; the remaining ".NNN" will be emitted as-is
			// and then handled as a leading-dot float by normalizeLeadingDotFloat
			// (which runs after this function in the pipeline — but since .NNN
			// with non-zero first digit is already a valid FLOAT token, and .0NNN
			// is handled by normalizeLeadingDotFloat, the combined pipeline is
			// correct).  We do NOT emit '0' here.
			i++ // skip '0'
			continue
		}

		buf = append(buf, ch)
		i++
	}

	return string(buf)
}

// normalizeLeadingDotFloat rewrites leading-dot float literals (e.g. .0, .05,
// .00123) to their equivalent 0-prefixed form (0.0, 0.05, 0.00123) so the
// ANTLR lexer tokenises them as a single FLOAT token rather than splitting them
// into a DOT token followed by an ID/DIGIT token.
//
// Background: the generated ANTLR lexer only produces a FLOAT token for `.NNN`
// when the first digit after the dot is non-zero (e.g. `.5` → FLOAT). When the
// first post-dot digit is `0` (e.g. `.0`, `.05`, `.00`) the lexer emits a DOT
// token followed by an ID token, which the parser cannot accept as a numeric
// literal.  Prepending `0` is semantically equivalent and consistently produces
// a valid `0.NNN` FLOAT token for all digit sequences.
//
// The rewriter recognises a leading-dot float as a `.` that is:
//   - NOT preceded by an identifier character (a-z, A-Z, 0-9, _) — which would
//     indicate a property access such as n.prop.
//   - NOT preceded by `]` — which would indicate a subscript.
//   - Immediately followed by one or more ASCII digits.
//
// The scanner respects double-quoted strings, backtick identifiers, and both
// line (//) and block (/* */) comments — rewriting is suppressed inside these.
//
// Fast path: return unchanged if q contains no '.' byte.
//
//nolint:gocyclo // byte-scanner with per-character branches; same pattern as normalizeVarlenBounds
func normalizeLeadingDotFloat(q string) string {
	if !hasByte(q, '.') {
		return q
	}

	buf := make([]byte, 0, len(q)+4)
	i := 0
	n := len(q)

	skipStringOrComment := func() {
		ch := q[i]
		switch ch {
		case '"':
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\\' && i < n {
					buf = append(buf, q[i])
					i++
				} else if c == '"' {
					break
				}
			}
		case '\'':
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\'' {
					break
				}
			}
		case '`':
			buf = append(buf, ch)
			i++
			for i < n && q[i] != '`' {
				buf = append(buf, q[i])
				i++
			}
			if i < n {
				buf = append(buf, '`')
				i++
			}
		case '/':
			if i+1 < n && q[i+1] == '/' {
				for i < n && q[i] != '\n' {
					buf = append(buf, q[i])
					i++
				}
			} else if i+1 < n && q[i+1] == '*' {
				buf = append(buf, q[i], q[i+1])
				i += 2
				for i < n {
					if q[i] == '*' && i+1 < n && q[i+1] == '/' {
						buf = append(buf, '*', '/')
						i += 2
						break
					}
					buf = append(buf, q[i])
					i++
				}
			}
		}
	}

	// isIdentChar reports whether b is a character that can appear in a Cypher
	// identifier: a-z, A-Z, 0-9, or _. Used to distinguish property-access dots
	// from leading-dot float literals.
	isIdentChar := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '_'
	}

	for i < n {
		ch := q[i]

		// Guard: skip over lexical atoms that must not be rewritten.
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			skipStringOrComment()
			continue
		}

		if ch != '.' {
			buf = append(buf, ch)
			i++
			continue
		}

		// ch == '.'
		// Check: is the next character a zero digit?
		// Only .0NNN forms fail to lex; .1NNN, .5NNN, .1e-5, etc. are handled
		// correctly by the ANTLR FLOAT rule and must not be rewritten.
		if i+1 >= n || q[i+1] != '0' {
			buf = append(buf, ch)
			i++
			continue
		}

		// Check: is the preceding character an identifier char or ']'?
		// If so, this is a property access (n.prop) — do not rewrite.
		if len(buf) > 0 {
			prev := buf[len(buf)-1]
			if isIdentChar(prev) || prev == ']' {
				buf = append(buf, ch)
				i++
				continue
			}
		}

		// Leading-dot float starting with zero (e.g. .0, .00, .05): insert '0' prefix.
		// The resulting 0.0NNN form tokenises as a valid FLOAT in all cases.
		buf = append(buf, '0', '.')
		i++ // consume '.'
		// Copy the digit run.
		for i < n && q[i] >= '0' && q[i] <= '9' {
			buf = append(buf, q[i])
			i++
		}
	}

	return string(buf)
}

// hasByte reports whether s contains the byte b.
func hasByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}

// normalizeSingleQuotes rewrites all single-quoted string literals in a Cypher
// query to double-quoted string literals before ANTLR lexing.
//
// This is safe because:
//   - ANTLR already handles double-quoted strings correctly.
//   - Single quotes are only valid in Cypher as string delimiters.
//   - The rewriter skips over double-quoted strings, backtick identifiers, and
//     both line (//) and block (/* */) comments.
//
// Fast path: if q contains no single-quote rune, q is returned unchanged.
//
// Escape handling inside a single-quoted literal:
//   - \' (escaped single quote) → ' (unescape; no longer needed as delimiter)
//   - \" (escaped double quote) → \" (preserved; valid in the output)
//   - \<other> → \<other> (pass through)
//   - unescaped " → \" (must escape for the double-quoted output)
//   - unescaped ' → end of string
//
//nolint:gocyclo // byte-scanner with one branch per character class (6 outer) and one per escape class (4 inner); extracting further helpers would add indirection without reducing actual complexity
func normalizeSingleQuotes(q string) string {
	// Fast path: nothing to rewrite.
	if !hasSingleQuote(q) {
		return q
	}

	// Pre-allocate slightly more than the input to avoid re-alloc in the common
	// case where a handful of single-quoted strings are present.
	buf := make([]byte, 0, len(q)+8)

	i := 0
	n := len(q)
	for i < n {
		ch := q[i]
		switch ch {
		case '"':
			// Double-quoted string: copy verbatim until closing '"', respecting
			// backslash escapes.
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				buf = append(buf, c)
				i++
				if c == '\\' && i < n {
					// Consume the escaped character as-is.
					buf = append(buf, q[i])
					i++
				} else if c == '"' {
					break
				}
			}

		case '`':
			// Backtick identifier: copy verbatim until closing '`'.
			buf = append(buf, ch)
			i++
			for i < n && q[i] != '`' {
				buf = append(buf, q[i])
				i++
			}
			if i < n {
				buf = append(buf, '`')
				i++
			}

		case '/':
			if i+1 < n && q[i+1] == '/' {
				// Line comment: copy to end of line.
				for i < n && q[i] != '\n' {
					buf = append(buf, q[i])
					i++
				}
			} else if i+1 < n && q[i+1] == '*' {
				// Block comment: copy until closing */.
				buf = append(buf, q[i], q[i+1])
				i += 2
				for i < n {
					if q[i] == '*' && i+1 < n && q[i+1] == '/' {
						buf = append(buf, '*', '/')
						i += 2
						break
					}
					buf = append(buf, q[i])
					i++
				}
			} else {
				buf = append(buf, ch)
				i++
			}

		case '\'':
			// Single-quoted string: rewrite as double-quoted.
			buf = append(buf, '"')
			i++ // consume opening '
			for i < n {
				c := q[i]
				if c == '\\' && i+1 < n {
					next := q[i+1]
					switch next {
					case '\'':
						// \' inside single-quoted string → emit ' (unescape)
						buf = append(buf, '\'')
					case '"':
						// \" → keep as \" (already valid in double-quoted context)
						buf = append(buf, '\\', '"')
					default:
						// Pass through all other escape sequences unchanged.
						buf = append(buf, '\\', next)
					}
					i += 2
				} else if c == '\'' {
					// Closing single quote.
					i++
					break
				} else if c == '"' {
					// Bare double-quote inside single-quoted string: must escape.
					buf = append(buf, '\\', '"')
					i++
				} else {
					buf = append(buf, c)
					i++
				}
			}
			buf = append(buf, '"')

		default:
			buf = append(buf, ch)
			i++
		}
	}

	return string(buf)
}

// hasSingleQuote reports whether s contains any single-quote byte.
// It is a fast-path guard before the full scanner runs.
func hasSingleQuote(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			return true
		}
	}
	return false
}
