package parser

// errchar_test.go — regression fence for the lexer's catch-all ERRCHAR rule
// (round-3 comparative audit, rmp #2167).
//
// `ERRCHAR : . -> channel(HIDDEN)` (CypherLexer.g4:157) matched any character
// no other lexer rule accepted and routed it to the hidden channel, where the
// parser never saw it. The character was therefore silently deleted and the
// remainder of the query parsed as if it had never been typed, so the engine
// answered a different question without reporting anything:
//
//	MATCH (n) WHERE n.v != 2 RETURN n  →  ... n.v = 2 ...   (exact negation)
//	MATCH (n:!A) RETURN n              →  MATCH (n:A) ...   (exact complement)
//	WHERE n.name = "unterminated       →  bare identifiers  (opening quote gone)
//
// `:!A` is valid Neo4j 5 syntax, so a ported query returned the precise
// complement of the intended result. None of this is visible to the openCypher
// TCK, which only executes syntactically valid queries — hence this file.
//
// The one character that legitimately reaches ERRCHAR is the `~` of the
// openCypher regex-match operator `=~`, which the vendored grammar has no token
// for; TestErrChar_RegexOperatorStillAccepted fences that exemption.

import (
	"errors"
	"strings"
	"testing"
)

// TestErrChar_RejectsUnrecognisedCharacters proves that every character outside
// the grammar is now reported instead of discarded. Each case parsed without
// error before the fix.
func TestErrChar_RejectsUnrecognisedCharacters(t *testing.T) {
	tests := []struct {
		name  string
		query string
		// wantMsg is a substring the reported message must contain. Empty means
		// only that the query must be rejected, whichever phase reports it.
		wantMsg string
	}{
		// ── the audit's two reproduction queries ─────────────────────────────
		// Both previously parsed cleanly and inverted their own predicate.
		{"not_equal_bang", "MATCH (n) WHERE n.v != 2 RETURN n", "unrecognised character"},
		{"label_negation", "MATCH (n:!A) RETURN n", "unrecognised character"},

		// ── unterminated literals, found while reproducing the above ─────────
		// The opening quote was dropped and its content lexed as identifiers,
		// so the query became a reference to an undeclared variable.
		{"unterminated_double", `MATCH (n) WHERE n.name = "abc RETURN n`, "unterminated string literal"},
		{"unterminated_single", `MATCH (n) WHERE n.name = 'abc RETURN n`, "unterminated string literal"},
		// An unterminated backtick identifier was already rejected, by the ESC
		// token rather than ERRCHAR; asserted here so the set is exhaustive.
		{"unterminated_backtick", "MATCH (n:`Lab RETURN n", ""},

		// ── every remaining ERRCHAR character, in a position where it would
		//    otherwise have been silently deleted ───────────────────────────
		{"hash", "MATCH (n) RETURN n # trailing", "unrecognised character"},
		{"ampersand", "MATCH (n:A&B) RETURN n", "unrecognised character"},
		{"question", "MATCH (n) WHERE n.v ? 2 RETURN n", "unrecognised character"},
		{"at_sign", "MATCH (n) RETURN n@", "unrecognised character"},
		{"backslash", `MATCH (n) RETURN n\`, "unrecognised character"},

		// ── a stray `~` is still an error; only a contiguous `=~` is not ─────
		{"tilde_alone", "MATCH (n) RETURN ~n", "unrecognised character"},
		{"tilde_spaced_from_eq", "RETURN 'abc' = ~ '[a-z]+' AS m", "unrecognised character"},
		{"tilde_before_eq", "RETURN 'abc' ~= '[a-z]+' AS m", "unrecognised character"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.query)
			if err == nil {
				t.Fatalf("Parse(%q) returned no error (AST %T); the character was silently discarded", tc.query, got)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("Parse(%q) error is %T, want *ParseError: %v", tc.query, err, err)
			}
			if tc.wantMsg != "" {
				if !strings.Contains(pe.Message, tc.wantMsg) {
					t.Fatalf("Parse(%q) message = %q, want it to contain %q", tc.query, pe.Message, tc.wantMsg)
				}
				if pe.OffendingToken == "" {
					t.Errorf("Parse(%q) reported no offending token", tc.query)
				}
			}

			// ParseStrict must agree; tooling relies on it seeing the same defect.
			if _, errs := ParseStrict(tc.query); len(errs) == 0 {
				t.Fatalf("ParseStrict(%q) returned no errors", tc.query)
			}
		})
	}
}

// TestErrChar_RegexOperatorStillAccepted fences the single exemption: the `~`
// of `=~`. The vendored grammar has no REGMATCH token, so `=~` lexes as ASSIGN
// followed by a hidden ERRCHAR `~` that cypher/parser/visitor.go recovers by
// peeking the source. Reporting that `~` would break a valid openCypher
// operator, and the TCK would not catch it — it has no `=~` scenarios.
func TestErrChar_RegexOperatorStillAccepted(t *testing.T) {
	queries := []string{
		"RETURN 'abc' =~ '[a-z]+' AS m",
		"MATCH (n) WHERE n.name =~ '.*x.*' RETURN n",
		"MATCH (n) WHERE n.a =~ 'p' AND n.b =~ 'q' RETURN n",
		// No whitespace anywhere around the operator.
		"RETURN 'abc'=~'[a-z]+' AS m",
	}
	for _, q := range queries {
		if _, err := Parse(q); err != nil {
			t.Errorf("Parse(%q) returned error, want accepted: %v", q, err)
		}
	}
}

// TestErrChar_AcceptedInsideLiterals proves the fix did not narrow the accepted
// language: an ERRCHAR character inside a string literal, a backtick-quoted
// identifier or a comment is part of that token and never reaches ERRCHAR.
func TestErrChar_AcceptedInsideLiterals(t *testing.T) {
	queries := []string{
		`RETURN "a!b" AS s`,
		`RETURN "a?b@c#d&e~f" AS s`,
		`RETURN 'a!b' AS s`,
		`RETURN "back\\slash" AS s`,
		"MATCH (`we!rd`:`A?B`) RETURN 1 AS n",
		"MATCH (n) RETURN n // trailing ! ? @ # & ~ comment",
		"MATCH (n) /* inline ! ? @ # & ~ */ RETURN n",
		// Non-ASCII is a Letter, not an ERRCHAR.
		"MATCH (ação) RETURN ação",
	}
	for _, q := range queries {
		if _, err := Parse(q); err != nil {
			t.Errorf("Parse(%q) returned error, want accepted: %v", q, err)
		}
	}
}

// TestErrChar_UnterminatedSingleQuoteIsRejected fences the companion defect
// found while reproducing the ERRCHAR cases: normalizeSingleQuotes rewrote a
// single-quoted literal to a double-quoted one and appended the closing quote
// unconditionally, so an unterminated literal was *completed* before lexing.
// The query then parsed cleanly with the rest of itself absorbed into the
// invented string — `RETURN 'abc AS s` returned the string "abc AS s".
//
// This is a separate defect from ERRCHAR: it happens in the pre-lex rewrite, so
// promoting ERRCHAR to an error does not cover it. Both must hold.
func TestErrChar_UnterminatedSingleQuoteIsRejected(t *testing.T) {
	queries := []string{
		"RETURN 'abc",
		"RETURN 'abc AS s",
		"MATCH (n) RETURN 'x",
		"RETURN 1 AS a, 'oops",
		"MATCH (n) WHERE n.name = 'abc RETURN n",
	}
	for _, q := range queries {
		got, err := Parse(q)
		if err == nil {
			t.Errorf("Parse(%q) returned no error (AST %T); the closing quote was invented", q, got)
			continue
		}
		// The normalizer must leave the literal alone so the stray quote is
		// reported at its true position, not swallowed into a rewritten string.
		if norm := applyNormalizers(q); !strings.Contains(norm, "'") {
			t.Errorf("applyNormalizers(%q) = %q, want the unterminated quote preserved", q, norm)
		}
	}

	// A *terminated* single-quoted literal must still be rewritten and accepted.
	for _, q := range []string{
		`RETURN 'abc' AS s`,
		`RETURN 'it\'s' AS s`,
		`RETURN 'say "hi"' AS s`,
		`RETURN '' AS s`,
		`MATCH (n) WHERE n.a = 'x' AND n.b = 'y' RETURN n`,
	} {
		if _, err := Parse(q); err != nil {
			t.Errorf("Parse(%q) returned error, want accepted: %v", q, err)
		}
	}
}

// TestErrChar_ErrorCapIsPerParse verifies that the combined lexer+parser error
// set honours maxParseErrors. Before ERRCHAR was reported, the lexer listener
// was always empty and the cap could only ever be reached by one phase; a
// pathological input can now fill both.
func TestErrChar_ErrorCapIsPerParse(t *testing.T) {
	const query = "@ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @"
	_, errs := ParseStrict(query)
	if len(errs) == 0 {
		t.Fatalf("ParseStrict(%q) returned no errors", query)
	}
	if len(errs) > maxParseErrors {
		t.Fatalf("ParseStrict(%q) returned %d errors, cap is %d: %v",
			query, len(errs), maxParseErrors, errs)
	}
}
