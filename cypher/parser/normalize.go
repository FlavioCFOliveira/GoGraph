package parser

import (
	"fmt"
	"strconv"
)

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
				// User-given negative sign: preserve it so the rewrite
				// emits `--<digits>` (an unparseable double-negative)
				// rather than `-<digits>` (which would collapse with the
				// visitor's abs-of-bound). The visitor's ParseInt then
				// fails and the query surfaces InvalidRelationshipPattern,
				// which is the canonical openCypher error for a negative
				// range bound (Match4 [10]).
				if i < n && q[i] == '-' {
					buf = append(buf, '-')
					i++
				}
				// Optional lower bound: rewrite digits as negative.
				writeNegDigits()
				// Optional '..' followed by optional upper bound.
				if i+1 < n && q[i] == '.' && q[i+1] == '.' {
					buf = append(buf, '.', '.')
					i += 2
					if i < n && q[i] == '-' {
						buf = append(buf, '-')
						i++
					}
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

// normalizeDoubleNot eliminates consecutive NOT pairs in a Cypher query by
// applying double-negation elimination: each pair of adjacent NOT tokens
// cancels out.  The result has zero or one NOT prepended to the operand
// expression, depending on the parity of the original NOT run.
//
// Background: the grammar for the NOT production (notExpression) uses
// NOT? comparisonExpression, which does not allow recursive NOT nesting.
// Double (or higher even-count) negation produces a parse error. Eliminating
// pairs before lexing resolves the grammar gap without any semantic change.
//
// Algorithm: locate runs of NOTs separated only by whitespace, count them,
// and emit either nothing (even count) or "NOT " (odd count) followed by the
// original content between the last NOT and the next non-NOT token.  The
// scanner is case-insensitive for NOT and skips over string literals, backtick
// identifiers, and comments so that "NOT" inside a string is not rewritten.
//
// Fast path: return early unless the input contains at least two consecutive
// NOT tokens (detected by the reDoubleNot-equivalent check on the raw string).
//
//nolint:gocyclo // state-machine scanner with per-state branches; complexity is inherent
func normalizeDoubleNot(q string) string {
	// Fast path: no double-NOT present.
	if !containsDoubleNot(q) {
		return q
	}

	buf := make([]byte, 0, len(q))
	i := 0
	n := len(q)

	// skipStringOrComment advances i past a string, backtick identifier, or
	// comment while copying all bytes verbatim into buf.
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

	// tryNOT checks whether q[i:] starts with a case-insensitive "NOT" token
	// that is not immediately followed by an identifier character. Returns the
	// index past the "NOT" text, or -1 if no match.
	tryNOT := func(pos int) int {
		if pos+3 > n {
			return -1
		}
		if (q[pos] != 'N' && q[pos] != 'n') ||
			(q[pos+1] != 'O' && q[pos+1] != 'o') ||
			(q[pos+2] != 'T' && q[pos+2] != 't') {
			return -1
		}
		// "NOT" must be followed by a non-identifier character (word boundary).
		if pos+3 < n {
			next := q[pos+3]
			if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') ||
				(next >= '0' && next <= '9') || next == '_' {
				return -1
			}
		}
		return pos + 3
	}

	for i < n {
		ch := q[i]

		// Guard: skip over lexical atoms that must not be rewritten.
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			skipStringOrComment()
			continue
		}

		// Try to match a run of NOTs separated by whitespace.
		end := tryNOT(i)
		if end < 0 {
			buf = append(buf, ch)
			i++
			continue
		}

		// Found a NOT at position i. Count consecutive NOTs (whitespace-separated).
		count := 1
		// ws holds the whitespace after each NOT that we may need to preserve.
		// We record each NOT's end position and the whitespace following it.
		type notEntry struct {
			afterNot int // index just past "NOT"
			afterWS  int // index just past whitespace following NOT
		}
		entries := []notEntry{{afterNot: end}}

		j := end
		// Skip whitespace after the first NOT.
		for j < n && (q[j] == ' ' || q[j] == '\t' || q[j] == '\r' || q[j] == '\n') {
			j++
		}
		entries[0] = notEntry{afterNot: end, afterWS: j}

		for {
			nextEnd := tryNOT(j)
			if nextEnd < 0 {
				break
			}
			count++
			wsS := nextEnd
			for wsS < n && (q[wsS] == ' ' || q[wsS] == '\t' || q[wsS] == '\r' || q[wsS] == '\n') {
				wsS++
			}
			entries = append(entries, notEntry{afterNot: nextEnd, afterWS: wsS})
			j = wsS
		}

		if count == 1 {
			// Single NOT — emit as-is including the original text and whitespace.
			buf = append(buf, q[i:entries[0].afterWS]...)
			i = entries[0].afterWS
			continue
		}

		// Multiple NOTs: apply double-negation elimination.
		// Emit "NOT " if odd count, nothing if even count.
		// Then continue from j (past all NOTs and their trailing whitespace).
		if count%2 == 1 {
			// Preserve the original capitalisation from the first NOT.
			buf = append(buf, q[i:entries[0].afterNot]...)
			buf = append(buf, ' ')
		}
		i = j
	}

	return string(buf)
}

// containsDoubleNot reports whether q contains at least two consecutive NOT
// tokens (case-insensitive). Used as a fast path guard for normalizeDoubleNot.
//
//nolint:gocyclo // byte-scanner with per-character branches; same pattern as normalizeDoubleNot
func containsDoubleNot(q string) bool {
	n := len(q)
	for i := 0; i < n; {
		// Skip non-N/n characters quickly.
		if q[i] != 'N' && q[i] != 'n' {
			i++
			continue
		}
		// Try to match "NOT".
		if i+3 > n {
			break
		}
		if (q[i+1] != 'O' && q[i+1] != 'o') || (q[i+2] != 'T' && q[i+2] != 't') {
			i++
			continue
		}
		// Word-boundary check after "NOT".
		if i+3 < n {
			next := q[i+3]
			if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') ||
				(next >= '0' && next <= '9') || next == '_' {
				i++
				continue
			}
		}
		// Found a NOT at i. Skip past it and any whitespace, then look for another.
		j := i + 3
		for j < n && (q[j] == ' ' || q[j] == '\t' || q[j] == '\r' || q[j] == '\n') {
			j++
		}
		// Check for another NOT.
		if j+3 <= n &&
			(q[j] == 'N' || q[j] == 'n') &&
			(q[j+1] == 'O' || q[j+1] == 'o') &&
			(q[j+2] == 'T' || q[j+2] == 't') {
			// Boundary after second NOT.
			if j+3 >= n {
				return true // end-of-string is a valid boundary
			}
			next := q[j+3]
			if (next < 'a' || next > 'z') && (next < 'A' || next > 'Z') &&
				(next < '0' || next > '9') && next != '_' {
				return true
			}
		}
		i = j
	}
	return false
}

// normalizeCallNoParen rewrites in-query CALL statements that omit the
// argument parentheses — e.g. "CALL proc YIELD out" — to the equivalent form
// with an explicit empty argument list: "CALL proc() YIELD out".
//
// Background: the generated ANTLR grammar requires parentheses for in-query
// CALL (queryCallSt : CALL invocationName parenExpressionChain …), whereas
// the standalone CALL allows them to be omitted.  Inserting "()" before YIELD
// makes both forms syntactically equivalent without changing semantics.
//
// The rewrite fires when ALL of the following hold:
//  1. The keyword CALL (case-insensitive) appears as a whole word.
//  2. It is followed by a dotted identifier (the procedure name).
//  3. The dotted identifier is immediately followed by whitespace then YIELD
//     (case-insensitive) — i.e. there is no '(' already present.
//
// The scanner respects double-quoted strings, single-quoted strings (after
// [normalizeSingleQuotes] has run), backtick identifiers, and both line (//)
// and block (/* */) comments.
//
// Fast path: return early if q contains no 'C' or 'c' byte (no CALL possible).
//
//nolint:gocyclo // byte-scanner with per-character branches; same pattern as normalizeArithmeticMinus
func normalizeCallNoParen(q string) string {
	if !hasByte(q, 'C') && !hasByte(q, 'c') {
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

	isWS := func(b byte) bool {
		return b == ' ' || b == '\t' || b == '\r' || b == '\n'
	}

	// tryCALL checks whether q[pos:] starts with a case-insensitive "CALL" keyword
	// (not followed by an identifier character). Returns the index past CALL or -1.
	tryCALL := func(pos int) int {
		if pos+4 > n {
			return -1
		}
		if (q[pos] != 'C' && q[pos] != 'c') ||
			(q[pos+1] != 'A' && q[pos+1] != 'a') ||
			(q[pos+2] != 'L' && q[pos+2] != 'l') ||
			(q[pos+3] != 'L' && q[pos+3] != 'l') {
			return -1
		}
		if pos+4 < n && isIdentChar(q[pos+4]) {
			return -1
		}
		return pos + 4
	}

	// tryYIELD checks whether q[pos:] starts with a case-insensitive "YIELD"
	// keyword (not preceded by ident char check not needed — caller ensures it).
	tryYIELD := func(pos int) bool {
		if pos+5 > n {
			return false
		}
		return (q[pos] == 'Y' || q[pos] == 'y') &&
			(q[pos+1] == 'I' || q[pos+1] == 'i') &&
			(q[pos+2] == 'E' || q[pos+2] == 'e') &&
			(q[pos+3] == 'L' || q[pos+3] == 'l') &&
			(q[pos+4] == 'D' || q[pos+4] == 'd') &&
			(pos+5 >= n || !isIdentChar(q[pos+5]))
	}

	for i < n {
		ch := q[i]

		// Guard: skip over lexical atoms that must not be rewritten.
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			skipStringOrComment()
			continue
		}

		// Try to match CALL keyword.
		callEnd := tryCALL(i)
		if callEnd < 0 {
			buf = append(buf, ch)
			i++
			continue
		}

		// Found CALL. Check that it is preceded by a word boundary (not ident char).
		if len(buf) > 0 && isIdentChar(buf[len(buf)-1]) {
			buf = append(buf, ch)
			i++
			continue
		}

		// Emit CALL.
		buf = append(buf, q[i:callEnd]...)
		i = callEnd

		// Skip mandatory whitespace after CALL.
		if i >= n || !isWS(q[i]) {
			// No whitespace — not a valid CALL statement; continue.
			continue
		}
		// Emit whitespace.
		wsStart := i
		for i < n && isWS(q[i]) {
			i++
		}
		buf = append(buf, q[wsStart:i]...)

		// Consume the procedure invocation name (identifier.identifier.…).
		// Also handle backtick-quoted components.
		if i >= n {
			continue
		}
		nameStart := i
		for i < n {
			if q[i] == '`' {
				// Backtick-quoted name component.
				i++
				for i < n && q[i] != '`' {
					i++
				}
				if i < n {
					i++ // closing backtick
				}
			} else if isIdentChar(q[i]) {
				// Consume the full run of identifier characters.
				for i < n && isIdentChar(q[i]) {
					i++
				}
			} else {
				break
			}
			// After a name component, allow DOT to continue to next component.
			if i < n && q[i] == '.' {
				i++ // consume the dot
			} else {
				break
			}
		}
		if i == nameStart {
			// Nothing consumed — not a valid CALL.
			continue
		}
		buf = append(buf, q[nameStart:i]...)

		// Peek at what follows: skip whitespace.
		j := i
		for j < n && isWS(q[j]) {
			j++
		}

		// If the next non-whitespace char is '(' there are already parens.
		if j < n && q[j] == '(' {
			// Already has parentheses; continue without rewriting.
			buf = append(buf, q[i:j]...)
			i = j
			continue
		}

		// If the next non-whitespace token is YIELD, insert "()".
		if tryYIELD(j) {
			buf = append(buf, '(', ')')
			// Emit the whitespace between name and YIELD.
			buf = append(buf, q[i:j]...)
			i = j
			continue
		}

		// Otherwise no rewrite needed.
	}

	return string(buf)
}

// normalizeNegHexOct rewrites negated hexadecimal and octal literals to their
// signed decimal representation so the ANTLR parser can accept them.
//
// Background: the grammar does not support a unary minus applied directly to a
// hex (0x…) or octal (0o…) literal.  Converting the value to its signed decimal
// form (e.g. -0x1A → -26) is the safest approach: the ANTLR DIGIT rule accepts
// negative decimal integers (SUB? Digits), and the decimal form avoids the
// ambiguity around 0x8000000000000000 (= INT64_MIN when negated).
//
// For literals that overflow int64, the rewrite is not performed (the original
// text is kept as-is so that the visitor can detect and report the overflow).
//
// The rewrite fires only when the '-' is NOT preceded by an identifier
// character (a–z, A–Z, 0–9, _), which would indicate binary subtraction
// (already handled by [normalizeArithmeticMinus]).
//
// The scanner respects double-quoted strings, single-quoted strings (after
// [normalizeSingleQuotes] has run), backtick identifiers, and both line (//)
// and block (/* */) comments.
//
// Fast path: return early if q contains no '-' byte.
//
//nolint:gocyclo // byte-scanner with per-character branches; same pattern as normalizeArithmeticMinus
func normalizeNegHexOct(q string) string {
	if !hasByte(q, '-') {
		return q
	}

	buf := make([]byte, 0, len(q)+8)
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

	isHexDigit := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
	}

	isOctDigit := func(b byte) bool {
		return b >= '0' && b <= '7'
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

		// ch == '-': check if this is a unary minus before a hex/octal literal.
		// Skip if preceded by an identifier character (binary subtraction).
		if len(buf) > 0 && isIdentChar(buf[len(buf)-1]) {
			buf = append(buf, ch)
			i++
			continue
		}

		// Look ahead for 0x<hexdigit> or 0o<octdigit>.
		// Require at least one valid digit after the prefix.
		j := i + 1 // position of '0'
		if j >= n || q[j] != '0' {
			buf = append(buf, ch)
			i++
			continue
		}

		switch {
		case j+2 < n && (q[j+1] == 'x' || q[j+1] == 'X') && isHexDigit(q[j+2]):
			// -0x<hexdigits> → decimal representation of the negated value.
			k := j + 2
			for k < n && isHexDigit(q[k]) {
				k++
			}
			digits := q[j+2 : k]
			// Attempt signed parse first; for exactly 0x8000000000000000 (INT64_MIN
			// bit pattern) also accept via unsigned parse.
			val, parseErr := strconv.ParseInt(digits, 16, 64)
			if parseErr != nil {
				u, uerr := strconv.ParseUint(digits, 16, 64)
				if uerr == nil && u == 1<<63 {
					val = int64(u) //nolint:gosec // INT64_MIN: int64(1<<63) = -9223372036854775808
					parseErr = nil
				}
			}
			if parseErr != nil {
				// Overflow: emit the original text unchanged; the visitor will
				// detect and report the overflow.
				buf = append(buf, ch) // '-'
				buf = append(buf, q[j:k]...)
			} else {
				buf = append(buf, []byte(fmt.Sprintf("(%d)", -val))...)
			}
			i = k

		case j+2 < n && (q[j+1] == 'o' || q[j+1] == 'O') && isOctDigit(q[j+2]):
			// -0o<octdigits> → decimal representation of the negated value.
			k := j + 2
			for k < n && isOctDigit(q[k]) {
				k++
			}
			digits := q[j+2 : k]
			val, parseErr := strconv.ParseInt(digits, 8, 64)
			if parseErr != nil {
				// For exactly 0o1000000000000000000000 (= 2^63 = INT64_MIN when
				// negated) allow the unsigned value and reinterpret as INT64_MIN.
				u, uerr := strconv.ParseUint(digits, 8, 64)
				if uerr == nil && u == 1<<63 {
					val = int64(u) //nolint:gosec // INT64_MIN: int64(1<<63) = -9223372036854775808
					parseErr = nil
				}
			}
			if parseErr != nil {
				// Overflow: emit the original text unchanged.
				buf = append(buf, ch) // '-'
				buf = append(buf, q[j:k]...)
			} else {
				buf = append(buf, []byte(fmt.Sprintf("(%d)", -val))...)
			}
			i = k

		default:
			buf = append(buf, ch)
			i++
		}
	}

	return string(buf)
}

// normalizeFloatExpZeroPad strips redundant leading zeros from a signed
// floating-point exponent. The transform rewrites tokens of the form
//
//	<digits>[.<digits>] (e|E) (+|-) 0+ <digits>
//
// to the equivalent form without the leading zero padding in the exponent:
//
//	2E-01      → 2E-1
//	2E+01      → 2E+1
//	5e-001     → 5e-1
//	2.5E-001   → 2.5E-1
//	7E-010     → 7E-10        (only the leading zero is stripped)
//	1.0E+0     → 1.0E+0        (unchanged: exponent has no leading-zero pad)
//
// Background: the ANTLR lexer's FLOAT rule expects the exponent's digit run
// to match the `Digits` fragment, which requires `[1-9]` as the first digit.
// A leading zero (e.g. `01` in `2E-01`) does not match `Digits`, so the
// tokenizer splits `2E-01` into the three tokens `ID("2E")`, `DIGIT("-0")`,
// `ID("1")` (or, depending on the trailing digits, into other invalid
// sequences). Stripping the leading zeros produces a single FLOAT token that
// the parser accepts without any grammar change.
//
// The transform fires only when ALL of the following hold:
//  1. The exponent has an explicit sign (`+` or `-`). Without a sign the
//     resulting token (e.g. `2E1`) is still lexed as `ID`, leaving the latent
//     "no-sign positive exponent" gap untouched.
//  2. The exponent digit run has at least one leading zero AND at least one
//     trailing non-zero digit. `2E-0`, `2E-00` are left unchanged because the
//     resulting `2E0` would still lex as `ID`.
//  3. The digit before `e`/`E` is part of a decimal literal — i.e. not part
//     of a hexadecimal (`0x...`) or octal (`0o...`) literal. Hex literals are
//     skipped entirely so that `0x2E-01` is preserved verbatim.
//  4. The position is not inside a double-quoted string, single-quoted string,
//     backtick identifier, or comment.
//
// Fast path: return unchanged if q contains no 'e' or 'E' byte.
//
//nolint:gocyclo // byte-scanner with per-character branches; same pattern as normalizeArithmeticMinus
func normalizeFloatExpZeroPad(q string) string {
	if !hasByte(q, 'e') && !hasByte(q, 'E') {
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

	isLetterOrUnderscore := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
	}

	isDigit := func(b byte) bool { return b >= '0' && b <= '9' }
	isHexDigit := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
	}

	for i < n {
		ch := q[i]

		// Guard: skip over lexical atoms that must not be rewritten.
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			skipStringOrComment()
			continue
		}

		// Skip identifiers (letters or underscore followed by any LetterOrDigit
		// sequence). This preserves variable names such as "var2E01" verbatim.
		if isLetterOrUnderscore(ch) {
			buf = append(buf, ch)
			i++
			for i < n {
				c := q[i]
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
					(c >= '0' && c <= '9') || c == '_' {
					buf = append(buf, c)
					i++
					continue
				}
				break
			}
			continue
		}

		// Only digits can begin a numeric literal that we may rewrite.
		if !isDigit(ch) {
			buf = append(buf, ch)
			i++
			continue
		}

		// Guard: must not be preceded by an identifier character (would mean we
		// are mid-identifier). The loop above already consumes identifiers, but
		// a digit can also follow a `.` (property access) or `]` (subscript),
		// in which case it is still a numeric literal — those cases are fine.
		// However, an identifier character immediately preceding a digit can
		// only happen if the leading-letter branch above did not run, which is
		// impossible here.

		// Hex / octal prefix: `0x`, `0X`, `0o`, `0O`. Skip the entire literal
		// verbatim so that we do not mis-classify the `E` inside `0x2E`.
		if ch == '0' && i+1 < n && (q[i+1] == 'x' || q[i+1] == 'X') {
			// Hex literal: emit `0x` then consume all hex digits.
			buf = append(buf, q[i], q[i+1])
			i += 2
			for i < n && isHexDigit(q[i]) {
				buf = append(buf, q[i])
				i++
			}
			continue
		}
		if ch == '0' && i+1 < n && (q[i+1] == 'o' || q[i+1] == 'O') {
			// Octal literal: emit `0o` then consume octal digits.
			buf = append(buf, q[i], q[i+1])
			i += 2
			for i < n && q[i] >= '0' && q[i] <= '7' {
				buf = append(buf, q[i])
				i++
			}
			continue
		}

		// Decimal literal mantissa. Emit the integer-part digit run.
		for i < n && isDigit(q[i]) {
			buf = append(buf, q[i])
			i++
		}
		// Optional fractional part: `.` followed by digits.
		if i+1 < n && q[i] == '.' && isDigit(q[i+1]) {
			buf = append(buf, '.')
			i++
			for i < n && isDigit(q[i]) {
				buf = append(buf, q[i])
				i++
			}
		}
		// Optional exponent: `e` or `E` followed by required sign followed by
		// digits. We only rewrite when the sign is present.
		if i < n && (q[i] == 'e' || q[i] == 'E') &&
			i+1 < n && (q[i+1] == '+' || q[i+1] == '-') &&
			i+2 < n && isDigit(q[i+2]) {
			// Find the run of leading zeros and the trailing non-zero suffix.
			signedExpStart := i + 2 // position of first digit after sign
			zeroEnd := signedExpStart
			for zeroEnd < n && q[zeroEnd] == '0' {
				zeroEnd++
			}
			digEnd := zeroEnd
			for digEnd < n && isDigit(q[digEnd]) {
				digEnd++
			}
			leadingZeros := zeroEnd - signedExpStart
			trailingDigits := digEnd - zeroEnd

			// Rewrite only when there is at least one leading zero AND at least
			// one trailing non-zero digit. Otherwise leave the source unchanged.
			if leadingZeros > 0 && trailingDigits > 0 {
				buf = append(buf, q[i], q[i+1]) // e|E and sign
				buf = append(buf, q[zeroEnd:digEnd]...)
				i = digEnd
				continue
			}
			// No rewrite: emit the exponent verbatim.
			buf = append(buf, q[i:digEnd]...)
			i = digEnd
		}
	}

	return string(buf)
}

// validateUnicodeEscapes scans q for malformed `\u` escape sequences inside
// double- or single-quoted string literals. The openCypher specification
// requires every `\u` to be followed by exactly four hexadecimal digits
// (the grammar fragment is `'\\' 'u'+ HexDigit HexDigit HexDigit HexDigit`,
// so additional `u` characters are also permitted). If any `\u` escape is
// followed by fewer than four hex digits, validateUnicodeEscapes returns a
// non-nil error pinpointing the offending position.
//
// This runs before the pre-processor pipeline so that
// `normalizeSingleQuotes` does not silently rewrite a malformed escape
// into a benign-looking double-quoted string that the ANTLR lexer would
// otherwise accept by hiding the malformed bytes via ERRCHAR.
//
// Fast path: return nil if q contains no backslash byte.
//
//nolint:gocyclo // byte-scanner with string-state tracking; per-character branches
func validateUnicodeEscapes(q string) error {
	if !hasByte(q, '\\') {
		return nil
	}

	n := len(q)
	isHex := func(b byte) bool {
		return (b >= '0' && b <= '9') ||
			(b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
	}

	// Track 1-based line/column for diagnostic positions.
	line, col := 1, 1
	advance := func(i int) {
		if q[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}

	i := 0
	for i < n {
		ch := q[i]

		// Skip backtick identifiers verbatim (no escape semantics inside).
		if ch == '`' {
			advance(i)
			i++
			for i < n && q[i] != '`' {
				advance(i)
				i++
			}
			if i < n {
				advance(i)
				i++
			}
			continue
		}
		// Skip line and block comments verbatim.
		if ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*') {
			if q[i+1] == '/' {
				for i < n && q[i] != '\n' {
					advance(i)
					i++
				}
			} else {
				advance(i)
				i++
				advance(i)
				i++
				for i < n {
					if q[i] == '*' && i+1 < n && q[i+1] == '/' {
						advance(i)
						i++
						advance(i)
						i++
						break
					}
					advance(i)
					i++
				}
			}
			continue
		}
		if ch != '"' && ch != '\'' {
			advance(i)
			i++
			continue
		}

		// Enter a string literal. Track the opening quote so we know where
		// the string ends. Scan until the matching unescaped quote.
		quote := ch
		startLine, startCol := line, col
		advance(i)
		i++
		for i < n {
			c := q[i]
			if c == '\\' {
				if i+1 >= n {
					// dangling backslash at EOF — leave to ANTLR.
					advance(i)
					i++
					break
				}
				next := q[i+1]
				if next == 'u' {
					// Validate the Unicode escape sequence.
					// Pre-quoteCol points at the '\\' itself.
					escLine, escCol := line, col
					// Consume the '\\' and any run of 'u' characters.
					advance(i)
					i++ // past '\\'
					uCount := 0
					for i < n && q[i] == 'u' {
						uCount++
						advance(i)
						i++
					}
					_ = uCount // grammar accepts u+ but we only require >=1.
					// Now require exactly four hex digits.
					if i+4 > n || !isHex(q[i]) || !isHex(q[i+1]) ||
						!isHex(q[i+2]) || !isHex(q[i+3]) {
						return &ParseError{
							Line:    escLine,
							Column:  escCol,
							Message: "invalid unicode escape sequence: \\u must be followed by exactly four hexadecimal digits",
						}
					}
					// Consume the four hex digits.
					for k := 0; k < 4; k++ {
						advance(i)
						i++
					}
					continue
				}
				// Other backslash escape: skip the escaped byte verbatim.
				advance(i)
				i++ // past '\\'
				advance(i)
				i++ // past escaped char
				continue
			}
			if c == quote {
				advance(i)
				i++
				// Closed string.
				break
			}
			if c == '\n' {
				// Unterminated string at end of line — leave to ANTLR to
				// surface its own error.
				advance(i)
				i++
				_ = startLine
				_ = startCol
				break
			}
			advance(i)
			i++
		}
	}

	return nil
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
