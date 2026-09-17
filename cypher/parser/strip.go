package parser

import (
	"strconv"
	"strings"
)

// AutoParamPrefix is the reserved name prefix [StripLiterals] gives the
// parameters it extracts. It contains spaces, and the rewritten text always
// quotes the name in backticks, so it cannot collide with a user parameter: an
// unquoted openCypher parameter name is an identifier or a decimal index, and
// neither can contain a space.
const AutoParamPrefix = "  auto_"

// IsAutoParam reports whether name was produced by [StripLiterals].
//
// It exists because an auto-parameter must behave like the LITERAL it replaced,
// not like a parameter the caller supplied. The engine deliberately surfaces a
// type-incompatible user parameter as a typed error rather than silently
// matching nothing, while openCypher says a type-incompatible literal simply
// compares false and yields zero rows. Hoisting must not convert the second
// behaviour into the first, so the parameter type check skips these names.
func IsAutoParam(name string) bool { return strings.HasPrefix(name, AutoParamPrefix) }

// StripLiterals rewrites query so that the string literals it can safely hoist
// become parameter references, and returns the rewritten text together with the
// values those parameters must be bound to. ok reports whether anything was
// hoisted; when it is false the caller must use query unchanged.
//
// # Why
//
// The plan cache is keyed on query text, so `{sk: 'a'}` and `{sk: 'b'}` are two
// different queries: each distinct literal re-parses and re-plans. Measured
// against the same query written with a parameter, that cost 65% more processor
// time per query (docs/cpu-vs-neo4j-memgraph-2026-08-11.md §6). Hoisting the
// literal collapses every spelling onto one cache entry.
//
// It pays only because scanning is far cheaper than parsing: on the audit's
// point-lookup query, parsing costs 15.0 µs and analysing and translating a
// further ~1 µs, so avoiding the parse is nearly the whole saving. This scanner
// is a single pass over the bytes and costs ~330 ns.
//
// # Why a hand-written scanner
//
// The generated ANTLR lexer cannot be used for this. Its tokenisation is
// context-dependent in exactly the places that matter: `'p42'` arrives as
// ERRCHAR, ID, ERRCHAR rather than as a string token, and a bare `40` is
// sometimes ID and sometimes DIGIT, with the parser reinterpreting both later.
// Reproducing that reinterpretation here would be a second implementation of a
// subtle rule. Memgraph reached the same conclusion and hand-wrote its stripper
// rather than reuse its ANTLR lexer.
//
// # What it hoists, and what it will not
//
// Only STRING literals, and only inside a MATCH pattern or a WHERE predicate.
// Both limits were set by measurement rather than caution:
//
//   - Numbers are left alone. A number can appear where a parameter is invalid
//     or plan-changing — the bounds of a variable-length pattern (`[r*1..3]`),
//     SKIP and LIMIT — and has forms (hexadecimal, octal, exponent) the parser
//     reinterprets from ID tokens.
//   - Every clause other than MATCH and WHERE is skipped. An earlier version
//     hoisted everywhere except projections and regressed five TCK scenarios: a
//     procedure argument (`CALL test.my.proc('Stefan', 1)`) and a map literal fed
//     to `SET r += {...}` are both positions where a literal is not simply an
//     expression whose value may arrive by parameter. RETURN and WITH are
//     skipped too, because an unaliased projection is named after its own source
//     text — `RETURN 'x'` yields a column called `'x'`.
//
// Skipping a hoistable literal costs one cache entry. Hoisting one that should
// not be hoisted changes what the query means, so every uncertainty here
// resolves towards skipping.
func StripLiterals(query string) (stripped string, params map[string]string, ok bool) {
	type repl struct {
		start, end int // byte offsets, end exclusive
		name       string
	}
	// Fast reject. A query with no quote byte anywhere cannot contain a string
	// literal, so there is nothing to hoist and the scan below would walk the
	// whole text to conclude that. This matters because StripLiterals runs on
	// every cache miss, including the fully parameterised queries that have
	// nothing to gain from it: a quote-free query resolves in one IndexByte pass
	// instead of a full scan.
	if strings.IndexByte(query, '\'') < 0 && strings.IndexByte(query, '"') < 0 {
		return query, nil, false
	}

	var repls []repl

	// hoistable is true only while the scan is inside a MATCH pattern or a
	// WHERE predicate. It is a clause state machine over the byte stream, not a
	// parse, so it can only make the scanner more conservative.
	hoistable := false

	i, n := 0, len(query)
	for i < n {
		c := query[i]
		switch {
		case c == '/' && i+1 < n && query[i+1] == '/':
			for i < n && query[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && query[i+1] == '*':
			i += 2
			for i+1 < n && (query[i] != '*' || query[i+1] != '/') {
				i++
			}
			i = min(i+2, n)
		case c == '`':
			// A backtick-quoted identifier. Its contents are not Cypher, and a
			// doubled backtick is an escaped one.
			i++
			for i < n {
				if query[i] == '`' {
					if i+1 < n && query[i+1] == '`' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '\'' || c == '"':
			start := i
			quote := c
			i++
			closed := false
			for i < n {
				if query[i] == '\\' {
					i += 2
					continue
				}
				if query[i] == quote {
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				// An unterminated string: the query will not parse anyway, and
				// the scanner has no idea where the literal ends. Refuse.
				return query, nil, false
			}
			if !hoistable {
				continue
			}
			repls = append(repls, repl{start: start, end: i, name: AutoParamPrefix + strconv.Itoa(len(repls))})
		case isIdentStart(c):
			start := i
			for i < n && isIdentChar(query[i]) {
				i++
			}
			// The keyword test folds case DURING the comparison instead of
			// materialising an upper-cased copy of the token. The copy was the
			// scanner's whole allocation cost: `strings.ToUpper` returns a new
			// string for every identifier whose token is not already upper
			// case, once per identifier in the statement, only to compare it
			// against 23 ASCII literals and throw it away.
			//
			// The fold is EXACTLY equivalent, not nearly. `isIdentStart` and
			// `isIdentChar` (shortestpath.go) accept ASCII bytes and nothing
			// else, so any non-ASCII byte terminates the identifier scan above
			// before this test is reached: every token that arrives here is
			// pure ASCII, and on pure ASCII an ASCII fold and Unicode
			// upper-casing agree by definition. `strings.ToUpper`'s Unicode
			// branch — the special-casing that makes it more than an ASCII
			// fold — is unreachable from this call site.
			switch classifyClauseKeyword(query[start:i]) {
			case clauseHoistOn:
				hoistable = true
			case clauseHoistOff:
				hoistable = false
			case clauseOther:
				// Not a clause keyword: the region is unchanged.
			}
		default:
			i++
		}
	}

	if len(repls) == 0 {
		return query, nil, false
	}

	params = make(map[string]string, len(repls))
	var b strings.Builder
	b.Grow(len(query) + len(repls)*(len(AutoParamPrefix)+6))
	prev := 0
	for _, r := range repls {
		params[r.name] = unquoteString(query[r.start:r.end])
		b.WriteString(query[prev:r.start])
		b.WriteString("$`")
		b.WriteString(r.name)
		b.WriteString("`")
		prev = r.end
	}
	b.WriteString(query[prev:])
	return b.String(), params, true
}

// clauseKind classifies an identifier token the [StripLiterals] scanner found,
// against the clause keywords that open and close a hoistable region.
type clauseKind uint8

const (
	// clauseOther is every identifier that is not one of the 23 keywords.
	clauseOther clauseKind = iota
	// clauseHoistOn is MATCH or WHERE: the two clauses whose literals may be
	// hoisted.
	clauseHoistOn
	// clauseHoistOff is every other clause keyword: it ends a hoistable region.
	clauseHoistOff
)

// The 23 classified keywords span 2 bytes (ON) to 8 (OPTIONAL). A token outside
// that range cannot be one of them, so it is rejected before any byte of it is
// read.
const (
	clauseKeywordMinLen = 2
	clauseKeywordMaxLen = 8
)

// classifyClauseKeyword is the allocation-free equivalent of
//
//	switch strings.ToUpper(tok) {
//	case "MATCH", "WHERE":  return clauseHoistOn
//	case "RETURN", ...:     return clauseHoistOff
//	}
//
// It is correct only for a pure-ASCII tok; see the reasoning at its one call
// site in [StripLiterals], which is the only place a token reaches it.
//
// The order of the tests is length, then first byte, then the whole token:
//
//   - length first, because it rejects most identifiers without reading a byte
//     of them, and because a keyword and a longer word starting with it —
//     `set` and `setting`, `for` and `forall`, `match` and `matches`, `order`
//     and `ordering`, `set` and `sets` — must never be confused. Length is
//     re-checked inside [eqFoldUpperASCII] for the same reason: the comparison
//     is of the WHOLE token, never of a prefix.
//   - first byte next, folded, so the 23 candidates collapse to at most three.
//   - then the whole token, folded byte by byte.
func classifyClauseKeyword(tok string) clauseKind {
	if len(tok) < clauseKeywordMinLen || len(tok) > clauseKeywordMaxLen {
		return clauseOther
	}
	switch tok[0] &^ asciiCaseBit {
	case 'M':
		if eqFoldUpperASCII(tok, "MATCH") {
			return clauseHoistOn
		}
		if eqFoldUpperASCII(tok, "MERGE") {
			return clauseHoistOff
		}
	case 'W':
		if eqFoldUpperASCII(tok, "WHERE") {
			return clauseHoistOn
		}
		if eqFoldUpperASCII(tok, "WITH") {
			return clauseHoistOff
		}
	case 'R':
		if eqFoldUpperASCII(tok, "RETURN") || eqFoldUpperASCII(tok, "REMOVE") {
			return clauseHoistOff
		}
	case 'C':
		if eqFoldUpperASCII(tok, "CREATE") || eqFoldUpperASCII(tok, "CALL") {
			return clauseHoistOff
		}
	case 'D':
		if eqFoldUpperASCII(tok, "DELETE") || eqFoldUpperASCII(tok, "DETACH") ||
			eqFoldUpperASCII(tok, "DROP") {
			return clauseHoistOff
		}
	case 'S':
		if eqFoldUpperASCII(tok, "SET") || eqFoldUpperASCII(tok, "SKIP") {
			return clauseHoistOff
		}
	case 'U':
		if eqFoldUpperASCII(tok, "UNWIND") || eqFoldUpperASCII(tok, "UNION") {
			return clauseHoistOff
		}
	case 'Y':
		if eqFoldUpperASCII(tok, "YIELD") {
			return clauseHoistOff
		}
	case 'F':
		if eqFoldUpperASCII(tok, "FOREACH") || eqFoldUpperASCII(tok, "FOR") {
			return clauseHoistOff
		}
	case 'O':
		if eqFoldUpperASCII(tok, "OPTIONAL") || eqFoldUpperASCII(tok, "ON") ||
			eqFoldUpperASCII(tok, "ORDER") {
			return clauseHoistOff
		}
	case 'L':
		if eqFoldUpperASCII(tok, "LIMIT") {
			return clauseHoistOff
		}
	case 'A':
		if eqFoldUpperASCII(tok, "ADD") {
			return clauseHoistOff
		}
	}
	return clauseOther
}

// asciiCaseBit is the bit that separates an ASCII letter's two cases: clearing
// it upper-cases a letter and leaves an already-upper-case one alone.
const asciiCaseBit = byte(0x20)

// eqFoldUpperASCII reports whether tok equals the ALL-UPPERCASE ASCII literal
// kw, case-insensitively, without allocating.
//
// Only tok is folded, and only by clearing [asciiCaseBit], because kw is known
// upper case at every call site: that is one operation per byte, where a
// two-sided fold such as [eqFold] costs two branches per byte. [eqFold] is kept
// as it is — it serves shortestpath.go, whose literals are lower case.
//
// Clearing the bit is safe for every byte the scanner can put in tok, not only
// for letters:
//
//	'a'-'z' (0x61-0x7A) -> 'A'-'Z' (0x41-0x5A)   the fold itself
//	'A'-'Z'                                       already clear, unchanged
//	'_'     (0x5F)      -> 0x5F                   bit already clear
//	'0'-'9' (0x30-0x39) -> 0x10-0x19              outside 'A'-'Z'
//
// so neither an underscore nor a digit can be folded onto a letter of kw, and
// no identifier containing one can be mistaken for a keyword.
func eqFoldUpperASCII(tok, kw string) bool {
	if len(tok) != len(kw) {
		return false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i]&^asciiCaseBit != kw[i] {
			return false
		}
	}
	return true
}
