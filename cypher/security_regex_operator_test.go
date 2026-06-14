package cypher_test

// security_regex_operator_test.go — regression fence for the openCypher regex
// operator `=~` (finding #1479, SEC-2026-06-14 audit).
//
// Two defects were fixed together, because activating one without the other
// would have shipped a non-conformant, security-hazardous operator:
//
//  1. PARSER (cypher/parser/visitor.go): the vendored ANTLR grammar has no
//     `=~` token, so the lexer silently recovered the trailing `~` and the
//     visitor emitted a plain `=`. The whole regex execution path
//     (cypher/expr/eval.go, cypher/expr/regexcache.go) was therefore dead —
//     `'abc' =~ '[a-z]+'` evaluated as string equality (false). The visitor
//     now peeks the source character after the `=` token and emits the
//     dedicated "=~" operator when it is a contiguous `~`.
//
//  2. ANCHORING (cypher/expr): openCypher `=~` is an ANCHORED full-string
//     match equivalent to Java Matcher.matches(), but evalStringOp used Go's
//     unanchored regexp.MatchString (a substring find). That latent
//     non-conformance was invisible while the operator was dead; activating
//     the parser fix without anchoring would have made `role =~ 'admin'`
//     match 'superadmin' — a fail-open authorization hazard. The user pattern
//     is now anchored as \A(?:…)\z before compilation.
//
// These tests drive the public engine end to end. The openCypher TCK contains
// NO `=~` scenarios (String13/String14 are empty stubs), so a green TCK does
// not prove `=~` conformance — this file is the guard.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// asBool unwraps the engine's boxed boolean (expr.BoolValue) or a plain Go
// bool, reporting whether the value was a recognised boolean.
func asBool(v interface{}) (bool, bool) {
	switch b := v.(type) {
	case expr.BoolValue:
		return bool(b), true
	case bool:
		return b, true
	default:
		return false, false
	}
}

// runScalar executes a single-column RETURN query through the public engine and
// returns the value of column "m" of the first row (or nil if there is no row).
func runScalar(t *testing.T, eng *cypher.Engine, query string) interface{} {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunAny(%q) returned error: %v", query, err)
	}
	defer res.Close()
	var got interface{}
	if res.Next() {
		rec := res.Record()
		got = rec["m"]
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error for %q: %v", query, err)
	}
	return got
}

// TestSec_Cypher_RegexMatchOperator proves the `=~` operator is reachable
// through the public engine and behaves as an anchored full-string match per
// openCypher, while plain `=` equality is unchanged.
func TestSec_Cypher_RegexMatchOperator(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	tests := []struct {
		name  string
		query string
		want  interface{} // exec value; nil means an absent / NULL column
	}{
		// ── basic match ──────────────────────────────────────────────────────
		{"match_charclass", "RETURN 'abc' =~ '[a-z]+' AS m", true},
		{"match_dot", "RETURN 'abc' =~ 'a.c' AS m", true},

		// ── anchoring: a bare interior substring must NOT match ───────────────
		{"anchor_substring_b", "RETURN 'abc' =~ 'b' AS m", false},
		{"anchor_substring_ab", "RETURN 'abc' =~ 'ab' AS m", false},
		{"anchor_wildcards", "RETURN 'abc' =~ '.*b.*' AS m", true},
		{"anchor_full", "RETURN 'abc' =~ 'abc' AS m", true},

		// ── anchoring as a security predicate: must not over-match ───────────
		{"authz_no_overmatch", "RETURN 'superadmin' =~ 'admin' AS m", false},
		{"authz_exact", "RETURN 'admin' =~ 'admin' AS m", true},

		// ── no match ──────────────────────────────────────────────────────────
		{"no_match", "RETURN 'abc' =~ 'XYZ' AS m", false},

		// ── invalid pattern → NULL (openCypher) ──────────────────────────────
		{"invalid_pattern_null", "RETURN 'abc' =~ '[' AS m", nil},

		// ── NULL operands → NULL ──────────────────────────────────────────────
		{"null_left", "RETURN null =~ '[a-z]+' AS m", nil},
		{"null_right", "RETURN 'abc' =~ null AS m", nil},

		// ── `=~` is no longer identical to `=` ───────────────────────────────
		// Under the fix: ('abc' =~ '[a-z]+') = true, ('abc' = '[a-z]+') = false,
		// so the equality of the two is false. Before the fix both were false,
		// so the equality was true.
		{"not_equal_to_eq", "RETURN ('abc' =~ '[a-z]+') = ('abc' = '[a-z]+') AS m", false},

		// ── plain `=` equality is UNCHANGED ──────────────────────────────────
		{"plain_eq_true", "RETURN 'abc' = 'abc' AS m", true},
		{"plain_eq_false_literal", "RETURN 'abc' = '[a-z]+' AS m", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := runScalar(t, eng, tc.query)
			switch want := tc.want.(type) {
			case nil:
				// A Cypher NULL surfaces as the expr.Null sentinel (concrete
				// expr.nullValue), not a Go nil; recognise either.
				if got != nil {
					v, ok := got.(expr.Value)
					if !ok || !expr.IsNull(v) {
						t.Fatalf("%s: got %v (%T), want NULL/absent", tc.query, got, got)
					}
				}
			case bool:
				gotBool, ok := asBool(got)
				if !ok {
					t.Fatalf("%s: got %v (%T), want bool %v", tc.query, got, got, want)
				}
				if gotBool != want {
					t.Fatalf("%s: got %v, want %v", tc.query, gotBool, want)
				}
			default:
				t.Fatalf("unhandled want type %T", tc.want)
			}
		})
	}
}
