package parser

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
