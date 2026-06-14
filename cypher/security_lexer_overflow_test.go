package cypher_test

// security_lexer_overflow_test.go — DEFENSE LOCK-IN for numeric-literal overflow.
//
// A query that embeds a numeric literal too large for the target type —
// integer (> MaxInt64), hexadecimal (>= 2^63 when unsigned-wrapping), or
// floating-point (> MaxFloat64) — must be rejected with a typed *parser.SemaError
// whose message signals "out of range" / "overflow", NOT
//
//   - silently misinterpreted (e.g. 1e309 becoming +Inf or a variable named
//     "1e309" — the historical bug behind cypher/parser/float_overflow_test.go), and
//   - NOT a Go panic that unwinds past the engine.
//
// This is checked at both layers an untrusted query crosses: the parser package
// directly (parser.Parse) and the public engine (cypher.Engine.Run). At the
// engine layer the typed *parser.SemaError is wrapped as "cypher: parse: %w", so
// errors.As is the wrapping-tolerant assertion.
//
// All cases pass today; this is a regression fence.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
)

// secCypherOverflowLiterals lists the over-range numeric literals and a label
// describing which numeric domain each one overflows. Every entry must be
// rejected; none may parse to a usable value.
var secCypherOverflowLiterals = []struct {
	name    string
	literal string
}{
	{"integer_decimal_20digit", "99999999999999999999"},   // > MaxInt64 (19 digits)
	{"integer_maxint64_plus_pad", "92233720368547758080"}, // MaxInt64 with a trailing 0
	{"hex_2pow63", "0x8000000000000000"},                  // 2^63 — first value past MaxInt64
	{"hex_all_ones_64", "0xFFFFFFFFFFFFFFFF"},             // 2^64-1
	{"float_1e309", "1e309"},                              // > MaxFloat64 (~1.8e308)
	{"float_2e308", "2e308"},                              // > MaxFloat64
	{"float_1e400", "1e400"},                              // far past MaxFloat64
}

// TestSec_Cypher_LiteralOverflow_RejectedByParser asserts the parser package
// rejects each over-range literal with a typed *parser.SemaError that signals
// overflow — never a *ast.Variable with a numeric name and never a panic.
func TestSec_Cypher_LiteralOverflow_RejectedByParser(t *testing.T) {
	t.Parallel()
	for _, tc := range secCypherOverflowLiterals {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := "RETURN " + tc.literal
			_, err := parser.Parse(q)
			secCypherAssertSemaOverflow(t, err, q)
		})
	}
}

// TestSec_Cypher_LiteralOverflow_RejectedByEngine asserts the public engine
// surfaces the same typed *parser.SemaError (wrapped) from Run, so an embedder
// cannot smuggle an over-range literal past the public boundary into a silent
// wrong value or a crash.
func TestSec_Cypher_LiteralOverflow_RejectedByEngine(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	for _, tc := range secCypherOverflowLiterals {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := "RETURN " + tc.literal + " AS n"
			res, err := eng.Run(context.Background(), q, nil)
			if res != nil {
				_ = res.Close()
			}
			secCypherAssertSemaOverflow(t, err, q)
		})
	}
}

// secCypherAssertSemaOverflow asserts err is non-nil, unwraps to a
// *parser.SemaError, and its message contains "out of range" or "overflow".
func secCypherAssertSemaOverflow(t *testing.T, err error, q string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%q: expected a SemaError (over-range literal), got nil — the literal was silently accepted", q)
	}
	var se *parser.SemaError
	if !errors.As(err, &se) {
		t.Fatalf("%q: error %T (%v) does not wrap *parser.SemaError", q, err, err)
	}
	if !strings.Contains(se.Message, "out of range") && !strings.Contains(se.Message, "overflow") {
		t.Fatalf("%q: SemaError.Message = %q; want it to signal 'out of range' or 'overflow'", q, se.Message)
	}
}
