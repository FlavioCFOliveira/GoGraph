package parser

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
