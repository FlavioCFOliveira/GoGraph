package cypher

import "strings"

// maskNonClauseRegions returns query with every region that cannot contain a
// clause blanked out: line comments, block comments, and the three quoted forms.
// Everything else is returned byte-for-byte, and masked bytes become spaces so
// token boundaries either side are preserved rather than glued together.
//
// # Why this exists
//
// [QueryHasWritingClause] is a textual heuristic by design, and that is not the
// problem. The problem was that it inspected regions of the text which cannot
// hold a clause at all, so
//
//	MATCH (n:Person) WHERE n.name = 'CREATE' RETURN count(n)
//	// CREATE nothing here
//	CALL db.labels() YIELD label
//
// were both classified as writes. A misrouted read runs inside a write
// transaction and therefore SERIALISES on the store's single writer, throttling
// exactly the concurrent read throughput the engine is built for — silently,
// with nothing surfaced. It was found because adding a comment to a working
// `CALL db.labels()` query broke it outright (rmp #2230).
//
// # The regions, and where they come from
//
// The four forms are taken from the lexer grammar rather than from memory
// (cypher/parser/grammar/CypherLexer.g4):
//
//   - LINE_COMMENT — two slashes to end of line.
//   - COMMENT — slash-star to star-slash, NON-GREEDY, so it ends at the first
//     closing pair rather than the last.
//   - CHAR_LITERAL and STRING_LITERAL — single- and double-quoted, both with
//     backslash EscapeSequence, so an escaped delimiter does not close the
//     literal and the scan must skip it as one unit.
//   - ESC_LITERAL — backtick-quoted identifier, non-greedy and with NO escape
//     sequence at all, so a backslash inside one is an ordinary byte.
//
// # Unterminated regions
//
// An unterminated comment or string masks to the end of the input. That is the
// conservative direction for this caller: the tail is a lexical error the parser
// will reject anyway, and masking it can only make the classifier say "read",
// which routes the statement to the read path where it fails cleanly — whereas
// leaving it unmasked could route a genuinely unparseable statement to the write
// path and take the single-writer lock to do it.
//
// # Cost
//
// One left-to-right pass, no regexp, and NO ALLOCATION at all when the query
// contains none of the four opening characters — the overwhelmingly common case,
// which matters because this runs on every RunAny / RunInTxAny dispatch.
func maskNonClauseRegions(query string) string {
	// Fast path: nothing that can open a maskable region, so there is nothing to
	// mask and no copy to make.
	if !strings.ContainsAny(query, "/'\"`") {
		return query
	}

	var b strings.Builder
	b.Grow(len(query))
	for i := 0; i < len(query); {
		c := query[i]
		switch {
		case c == '/' && i+1 < len(query) && query[i+1] == '/':
			i = maskLineComment(&b, query, i)
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			i = maskBlockComment(&b, query, i)
		case c == '\'' || c == '"':
			i = maskQuoted(&b, query, i, c, true)
		case c == '`':
			i = maskQuoted(&b, query, i, c, false)
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// maskLineComment blanks `//` and the rest of the line, and returns the index of
// the newline (or len(q) when the comment runs to the end of the input). The
// newline itself is left for the main loop to copy, so line structure survives.
func maskLineComment(b *strings.Builder, q string, i int) int {
	for ; i < len(q) && q[i] != '\n' && q[i] != '\r'; i++ {
		b.WriteByte(' ')
	}
	return i
}

// maskBlockComment blanks a block comment inclusive of both delimiters, ending at
// the FIRST closing pair to match
// the grammar's non-greedy rule. Newlines inside are preserved so a following
// line comment still terminates correctly.
func maskBlockComment(b *strings.Builder, q string, i int) int {
	b.WriteString("  ") // the opening "/*"
	i += 2
	for ; i < len(q); i++ {
		if q[i] == '*' && i+1 < len(q) && q[i+1] == '/' {
			b.WriteString("  ") // the closing "*/"
			return i + 2
		}
		if q[i] == '\n' || q[i] == '\r' {
			b.WriteByte(q[i]) // keep line structure
			continue
		}
		b.WriteByte(' ')
	}
	return i // unterminated: masked to the end
}

// maskQuoted blanks a quoted region opened at i by the delimiter quote, including
// both delimiters, and returns the index just past the closing one. When escapes
// is true a backslash escapes the next byte, so an escaped delimiter does not end
// the region; when false (a backtick identifier) the first delimiter closes it.
func maskQuoted(b *strings.Builder, q string, i int, quote byte, escapes bool) int {
	b.WriteByte(' ') // the opening delimiter
	i++
	for ; i < len(q); i++ {
		if escapes && q[i] == '\\' {
			// Blank the backslash and whatever it escapes, as one unit, so an
			// escaped delimiter cannot be mistaken for the closing one.
			b.WriteByte(' ')
			if i+1 < len(q) {
				b.WriteByte(' ')
				i++
			}
			continue
		}
		if q[i] == quote {
			b.WriteByte(' ') // the closing delimiter
			return i + 1
		}
		if q[i] == '\n' || q[i] == '\r' {
			b.WriteByte(q[i]) // keep line structure
			continue
		}
		b.WriteByte(' ')
	}
	return i // unterminated: masked to the end
}
