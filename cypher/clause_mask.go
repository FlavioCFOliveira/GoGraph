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
// path and register as a writer to do it.
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
// writingKeywords are the six clause keywords [QueryHasWritingClause] routes on,
// grouped by length so the comparison starts from a length test that rejects
// almost every word in a query outright.
//
// They are the exact set the regexp this replaced encoded, and they carry the
// same meaning: a standalone word, matched without regard to case.
const (
	kwSET    = "SET"
	kwMERGE  = "MERGE"
	kwCREATE = "CREATE"
	kwREMOVE = "REMOVE"
	kwDELETE = "DELETE"
	kwDETACH = "DETACH"
)

// containsWritingKeyword reports whether any standalone word in text is a
// writing-clause keyword, comparing without regard to case and WITHOUT
// allocating (rmp #2240).
//
// # Why this is not a regexp
//
// It replaces `(?i)\b(CREATE|MERGE|SET|REMOVE|DELETE|DETACH)\b`, which cost
// 2.69-2.73 us and one 80-byte allocation on every RunAny / RunInTxAny dispatch —
// measured against an indexed point lookup of roughly 5 us end to end, so
// classifying a query could cost a third of running it. The expense was the
// regexp engine, not the mask: the mask's own fast path allocates nothing.
//
// # Word boundaries
//
// A word is a maximal run of ASCII letters, digits and underscore, which is
// exactly the class Go's regexp `\b` anchors on. That equivalence is what keeps
// "PRESET" and "NOMERGE" classified as reads, and it is pinned by the
// pre-existing table-driven cases rather than assumed.
//
// # Case folding
//
// The comparison folds ASCII only. That is not a narrowing: Go's `(?i)` did not
// fold beyond ASCII for this pattern either — verified before the change, with
// the regexp rejecting "ſet" (U+017F) exactly as this does.
func containsWritingKeyword(text string) bool {
	for i := 0; i < len(text); {
		if !isWordByte(text[i]) {
			i++
			continue
		}
		start := i
		for i < len(text) && isWordByte(text[i]) {
			i++
		}
		if isWritingKeyword(text[start:i]) {
			return true
		}
	}
	return false
}

// isWordByte reports whether c belongs to the ASCII word class [0-9A-Za-z_] that
// delimits a keyword, matching the class Go's regexp `\b` uses.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' ||
		c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' ||
		c == '_'
}

// isWritingKeyword reports whether word is one of the six keywords, ignoring
// ASCII case. The length switch runs first because it rejects the great majority
// of words in a query — MATCH, WHERE, RETURN, every variable and every property
// name — without inspecting a single byte.
func isWritingKeyword(word string) bool {
	switch len(word) {
	case 3:
		return equalFoldASCII(word, kwSET)
	case 5:
		return equalFoldASCII(word, kwMERGE)
	case 6:
		return equalFoldASCII(word, kwCREATE) ||
			equalFoldASCII(word, kwREMOVE) ||
			equalFoldASCII(word, kwDELETE) ||
			equalFoldASCII(word, kwDETACH)
	default:
		return false
	}
}

// equalFoldASCII reports whether s equals upper, an ASCII UPPERCASE literal,
// ignoring case. The caller has already established len(s) == len(upper).
//
// It is not [strings.EqualFold], which decodes runes and applies Unicode simple
// folding: that costs more and would admit characters the regexp this replaces
// did not.
func equalFoldASCII(s, upper string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != upper[i] {
			return false
		}
	}
	return true
}

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
