package parser

// shortestpath.go — pre-lex rewrite of shortestPath()/allShortestPaths()
// (rmp #1690).
//
// The vendored ANTLR grammar has no production for shortestPath /
// allShortestPaths, and regenerating the parser from the (drifted) grammar is
// not behaviour-preserving, so the proven TCK-green parser is left untouched.
// Instead, a named shortest-path binding in a MATCH clause —
//
//	MATCH p = shortestPath((a)-[*]-(b))
//	MATCH p = allShortestPaths((a)-[:T*1..3]->(b))
//
// — is rewritten before lexing into the ordinary named path it wraps —
//
//	MATCH p = (a)-[*]-(b)
//
// — and the stripped wrapper is recorded as a spMarker{pathVar, kind}. After
// the AST is built, [applyShortestMarkers] stamps the kind back onto the
// matching named [ast.PathPattern.Shortest], where the IR builder turns it into
// a shortest-path operator (rmp #1692). The wrapper's inner pattern (direction,
// relationship-type filter, variable-length bound, properties) is preserved
// verbatim, so no pattern semantics are lost in the rewrite.
//
// Scope: the NAMED MATCH form (`<var> = shortestPath(<pattern>)`). The scanner
// skips string/backtick/comment regions verbatim and only rewrites inside a
// MATCH / OPTIONAL MATCH clause, so a `=` equality against a shortestPath
// expression in WHERE/RETURN is never mistaken for a path assignment. An
// unnamed or expression-context shortestPath is left in place and rejected by
// semantic analysis with a clear message (rmp #1691).

import "github.com/FlavioCFOliveira/GoGraph/cypher/ast"

// spMarker records one stripped shortest-path wrapper: the path variable it was
// bound to and whether it was shortestPath (single) or allShortestPaths (all).
type spMarker struct {
	pathVar string
	kind    ast.ShortestKind
}

// rewriteShortestPath strips named shortestPath()/allShortestPaths() wrappers in
// MATCH clauses, returning the rewritten query and the ordered markers. When the
// query contains no such wrapper it returns q unchanged and a nil slice (fast
// path), so non-shortest-path queries pay only one case-insensitive scan.
func rewriteShortestPath(q string) (string, []spMarker) {
	if !containsShortestPath(q) {
		return q, nil
	}

	buf := make([]byte, 0, len(q))
	var markers []spMarker
	i, n := 0, len(q)
	inMatch := false // true while the most recent top-level clause is MATCH

	for i < n {
		ch := q[i]

		// Skip lexical atoms that must never be rewritten or scanned for
		// keywords, copying them verbatim.
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			i = copyStringOrComment(q, i, &buf)
			continue
		}

		if isIdentStart(ch) {
			idEnd := i + 1
			for idEnd < n && isIdentChar(q[idEnd]) {
				idEnd++
			}
			word := q[i:idEnd]

			// Track clause context so a WHERE/RETURN `=` equality against a
			// shortestPath expression is not treated as a path assignment.
			switch {
			case eqFold(word, "match"):
				inMatch = true
			case eqFold(word, "optional"):
				// OPTIONAL MATCH keeps inMatch driven by the following MATCH.
			case isClauseBreak(word):
				inMatch = false
			}

			// Try to match `<word> ws = ws (shortestPath|allShortestPaths) ws (`
			// as a named path assignment inside a MATCH clause.
			if inMatch {
				if rewritten, end, m, ok := tryShortestBinding(q, idEnd, word); ok {
					buf = append(buf, rewritten...)
					markers = append(markers, m)
					i = end
					continue
				}
			}

			buf = append(buf, word...)
			i = idEnd
			continue
		}

		buf = append(buf, ch)
		i++
	}
	return string(buf), markers
}

// tryShortestBinding tests whether the identifier word that ends at idEnd
// begins a `word = shortestPath(<pattern>)` / `allShortestPaths(<pattern>)`
// binding. On success it returns the replacement text (`word = <pattern>`), the
// index in q just past the wrapper's closing paren, the marker, and true.
// Otherwise ok is false.
func tryShortestBinding(q string, idEnd int, word string) (string, int, spMarker, bool) {
	n := len(q)
	k := skipWS(q, idEnd)
	// A single '=' (ASSIGN), not '==', '=~', '>=', '<=' (the preceding char is
	// the identifier, so only the trailing char can extend it).
	if k >= n || q[k] != '=' || (k+1 < n && (q[k+1] == '=' || q[k+1] == '~')) {
		return "", 0, spMarker{}, false
	}
	k = skipWS(q, k+1)
	kind, klen := matchShortestKeyword(q, k)
	if kind == ast.ShortestNone {
		return "", 0, spMarker{}, false
	}
	k2 := skipWS(q, k+klen)
	if k2 >= n || q[k2] != '(' {
		return "", 0, spMarker{}, false
	}
	closeIdx := matchCloseParen(q, k2)
	if closeIdx < 0 {
		return "", 0, spMarker{}, false
	}
	// Wrapper is KEYWORD '(' INNER ')'; INNER = q[k2+1:closeIdx] already carries
	// its own node/relationship parentheses, e.g. "(a)-[*]-(b)".
	inner := q[k2+1 : closeIdx]
	return word + " = " + inner, closeIdx + 1, spMarker{pathVar: word, kind: kind}, true
}

// matchShortestKeyword reports whether q[pos:] starts with a case-insensitive
// "shortestPath" or "allShortestPaths" token at a word boundary, returning the
// kind and the matched length, or (ShortestNone, 0).
func matchShortestKeyword(q string, pos int) (ast.ShortestKind, int) {
	if hasWordAt(q, pos, "allShortestPaths") {
		return ast.ShortestAll, len("allShortestPaths")
	}
	if hasWordAt(q, pos, "shortestPath") {
		return ast.ShortestSingle, len("shortestPath")
	}
	return ast.ShortestNone, 0
}

// hasWordAt reports whether q[pos:] equals word (case-insensitive) terminated by
// a non-identifier character (word boundary).
func hasWordAt(q string, pos int, word string) bool {
	if pos+len(word) > len(q) {
		return false
	}
	if !eqFold(q[pos:pos+len(word)], word) {
		return false
	}
	if next := pos + len(word); next < len(q) && isIdentChar(q[next]) {
		return false
	}
	return true
}

// matchCloseParen returns the index of the ')' that matches the '(' at q[open],
// skipping string/backtick/comment regions so a ')' inside a property string
// literal is not mistaken for the closer. Returns -1 if unbalanced.
func matchCloseParen(q string, open int) int {
	depth := 0
	n := len(q)
	for i := open; i < n; {
		ch := q[i]
		if ch == '"' || ch == '\'' || ch == '`' ||
			(ch == '/' && i+1 < n && (q[i+1] == '/' || q[i+1] == '*')) {
			var scratch []byte
			i = copyStringOrComment(q, i, &scratch)
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// copyStringOrComment advances past the string literal, backtick identifier, or
// comment beginning at q[i], appending the consumed bytes to *buf, and returns
// the index just past it. Mirrors the guard used by the other normalizers.
func copyStringOrComment(q string, i int, buf *[]byte) int {
	n := len(q)
	ch := q[i]
	switch ch {
	case '"':
		*buf = append(*buf, ch)
		i++
		for i < n {
			c := q[i]
			*buf = append(*buf, c)
			i++
			if c == '\\' && i < n {
				*buf = append(*buf, q[i])
				i++
			} else if c == '"' {
				break
			}
		}
	case '\'':
		*buf = append(*buf, ch)
		i++
		for i < n {
			c := q[i]
			*buf = append(*buf, c)
			i++
			if c == '\\' && i < n {
				*buf = append(*buf, q[i])
				i++
			} else if c == '\'' {
				break
			}
		}
	case '`':
		*buf = append(*buf, ch)
		i++
		for i < n && q[i] != '`' {
			*buf = append(*buf, q[i])
			i++
		}
		if i < n {
			*buf = append(*buf, '`')
			i++
		}
	case '/':
		if i+1 < n && q[i+1] == '/' {
			for i < n && q[i] != '\n' {
				*buf = append(*buf, q[i])
				i++
			}
		} else if i+1 < n && q[i+1] == '*' {
			*buf = append(*buf, q[i], q[i+1])
			i += 2
			for i < n {
				if q[i] == '*' && i+1 < n && q[i+1] == '/' {
					*buf = append(*buf, '*', '/')
					i += 2
					break
				}
				*buf = append(*buf, q[i])
				i++
			}
		} else {
			*buf = append(*buf, ch)
			i++
		}
	}
	return i
}

// applyShortestMarkers stamps the recorded shortest-path kinds onto the matching
// named path patterns of the built query. Markers are matched to PathPatterns by
// path-variable name (a path variable is bound exactly once in a valid query, so
// the match is unambiguous). It walks every MATCH/OPTIONAL MATCH pattern in the
// query, including those inside subqueries and UNION branches.
func applyShortestMarkers(q ast.Query, markers []spMarker) {
	if len(markers) == 0 {
		return
	}
	byVar := make(map[string]ast.ShortestKind, len(markers))
	for _, m := range markers {
		byVar[m.pathVar] = m.kind
	}
	walkPathPatterns(q, func(pp *ast.PathPattern) {
		if pp.Variable == nil {
			return
		}
		if kind, ok := byVar[*pp.Variable]; ok {
			pp.Shortest = kind
		}
	})
}

// walkPathPatterns invokes fn on every PathPattern reachable through a
// MATCH / OPTIONAL MATCH clause of q, descending into UNION branches. That is
// the full set of contexts in which a named shortest-path binding is supported;
// a shortestPath() in an EXISTS{}/COUNT{} subquery is not rewritten by
// [rewriteShortestPath] into a named binding and so is not reached here.
func walkPathPatterns(q ast.Query, fn func(*ast.PathPattern)) {
	switch qq := q.(type) {
	case *ast.SingleQuery:
		walkSingleQueryPatterns(qq, fn)
	case *ast.MultiQuery:
		for _, p := range qq.Parts {
			walkSingleQueryPatterns(p, fn)
		}
	}
}

func walkSingleQueryPatterns(sq *ast.SingleQuery, fn func(*ast.PathPattern)) {
	if sq == nil {
		return
	}
	for _, rc := range sq.ReadingClauses {
		switch c := rc.(type) {
		case *ast.Match:
			walkPatternPaths(c.Pattern, fn)
		case *ast.OptionalMatch:
			walkPatternPaths(c.Pattern, fn)
		case *ast.Union:
			walkSingleQueryPatterns(c.Query, fn)
		}
	}
}

func walkPatternPaths(p *ast.Pattern, fn func(*ast.PathPattern)) {
	if p == nil {
		return
	}
	for _, pp := range p.Paths {
		if pp != nil {
			fn(pp)
		}
	}
}

// --- small character/word helpers (ASCII; identifiers are ASCII in Cypher) ---

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func skipWS(q string, i int) int {
	for i < len(q) && (q[i] == ' ' || q[i] == '\t' || q[i] == '\r' || q[i] == '\n') {
		i++
	}
	return i
}

// eqFold reports case-insensitive ASCII equality of two equal-length-agnostic
// strings (a is a slice of the query, b a lowercase literal).
func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// isClauseBreak reports whether word is a top-level clause keyword that ends a
// MATCH clause's pattern region, so a subsequent `=` is no longer a path
// assignment. OPTIONAL and MATCH are handled by the caller (they open, not
// break, a MATCH context).
func isClauseBreak(word string) bool {
	switch {
	case eqFold(word, "where"), eqFold(word, "return"), eqFold(word, "with"),
		eqFold(word, "create"), eqFold(word, "merge"), eqFold(word, "delete"),
		eqFold(word, "detach"), eqFold(word, "set"), eqFold(word, "remove"),
		eqFold(word, "unwind"), eqFold(word, "call"), eqFold(word, "yield"),
		eqFold(word, "foreach"), eqFold(word, "union"), eqFold(word, "order"),
		eqFold(word, "skip"), eqFold(word, "limit"):
		return true
	}
	return false
}

// containsShortestPath is the fast-path guard: a case-insensitive substring
// scan for "shortestpath" (the suffix common to both keywords). When absent,
// rewriteShortestPath returns the input untouched.
func containsShortestPath(q string) bool {
	const needle = "shortestpath"
	n, m := len(q), len(needle)
	if n < m {
		return false
	}
	for i := 0; i+m <= n; i++ {
		if eqFold(q[i:i+m], needle) {
			return true
		}
	}
	return false
}
