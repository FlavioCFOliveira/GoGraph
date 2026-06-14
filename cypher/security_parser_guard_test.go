package cypher_test

// security_parser_guard_test.go — DEFENSE LOCK-IN for the pre-parse input guard
// (cypher/parser/guard.go), exercised through the PUBLIC engine entrypoints.
//
// The guard itself is unit-tested in-package (cypher/parser/guard_test.go) by
// calling parser.Parse / guardInput directly. Those tests prove the guard's
// internal logic. THIS file proves the property that matters operationally: the
// untrusted attack surface — cypher.Engine.Run and cypher.Engine.Explain, the
// two methods a Bolt client (or any embedder) can reach with arbitrary query
// text — repel each denial-of-service payload with a typed *parser.ParseError
// instead of driving the recursive ANTLR parser / visitor into a fatal Go
// "stack overflow" that recover() cannot catch.
//
// Each payload below, absent the guard, would either allocate unboundedly or
// overflow the goroutine stack and crash the whole process. With the guard the
// query is rejected in a single O(n) byte scan before any lexing or recursion.
//
// All cases pass today; this is a regression fence, not a finding demo.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// secCypherNewEngine builds a store-less in-memory engine over an empty graph.
// It is the minimal construction used by the read-path security tests.
func secCypherNewEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

// secCypherGuardPayload describes one denial-of-service input and the substring
// its rejection message must contain, so the test confirms the guard fired for
// the intended reason (depth vs length vs CASE vs operator chain) rather than
// some unrelated parse error.
type secCypherGuardPayload struct {
	name string
	// build returns the hostile query text. A builder keeps the multi-megabyte
	// strings out of the test source and lazily constructed.
	build func() string
	// wantMsg is a substring of parser.ParseError.Message proving the correct
	// guard branch rejected the input.
	wantMsg string
}

// secCypherGuardPayloads enumerates one payload per guard branch. The
// magnitudes are far past each cap so the intent is unambiguous, yet every
// payload is pure ASCII text (no allocation beyond the string itself) and is
// rejected before any recursion, so the test is cheap and cannot crash the host.
func secCypherGuardPayloads() []secCypherGuardPayload {
	const deepNest = 5000 // » maxNestingDepth (256); also » any real stack limit
	const caseChain = 300 // » maxCASEKeywords (256)
	const opChain = 600   // » maxBinaryOpTokens (512)
	return []secCypherGuardPayload{
		{
			name:    "deep_parens",
			build:   func() string { return "RETURN " + strings.Repeat("(", deepNest) + "1" + strings.Repeat(")", deepNest) },
			wantMsg: "nesting too deep",
		},
		{
			name:    "deep_lists",
			build:   func() string { return "RETURN " + strings.Repeat("[", deepNest) + strings.Repeat("]", deepNest) },
			wantMsg: "nesting too deep",
		},
		{
			name:    "deep_maps",
			build:   func() string { return "RETURN " + strings.Repeat("{", deepNest) + strings.Repeat("}", deepNest) },
			wantMsg: "nesting too deep",
		},
		{
			name: "case_keyword_chain",
			build: func() string {
				return "RETURN " + strings.Repeat("CASE WHEN true THEN ", caseChain) + "1" + strings.Repeat(" END", caseChain)
			},
			wantMsg: "CASE keyword count",
		},
		{
			name:    "binary_op_chain",
			build:   func() string { return "RETURN 1" + strings.Repeat(" + 1", opChain) },
			wantMsg: "binary operator count",
		},
		{
			name:    "over_length_query",
			build:   func() string { return "RETURN 1 AS n" + strings.Repeat(" ", 1<<20) }, // > 1 MiB
			wantMsg: "query too large",
		},
	}
}

// TestSec_Cypher_ParserGuard_RepelsViaRun asserts that every guard payload is
// rejected by Engine.Run with a typed *parser.ParseError carrying the expected
// branch message — and that the call returns promptly (a watchdog converts a
// hypothetical hang or stack overflow into a clean test failure instead of a
// process crash).
func TestSec_Cypher_ParserGuard_RepelsViaRun(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	for _, tc := range secCypherGuardPayloads() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := tc.build()
			secCypherRunRejectsWithin(t, eng, q, tc.wantMsg)
		})
	}
}

// TestSec_Cypher_ParserGuard_RepelsViaExplain asserts the same for Engine.Explain,
// which also routes through the parser (a planner-only path must be guarded too,
// because EXPLAIN of untrusted text is reachable from the same clients).
func TestSec_Cypher_ParserGuard_RepelsViaExplain(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	for _, tc := range secCypherGuardPayloads() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := tc.build()
			done := make(chan error, 1)
			go func() {
				_, err := eng.Explain(q, nil)
				done <- err
			}()
			select {
			case err := <-done:
				secCypherAssertParseErr(t, err, tc.wantMsg)
			case <-time.After(20 * time.Second):
				t.Fatalf("Explain(%s) did not return within 20s — the pre-parse guard failed to short-circuit the payload", tc.name)
			}
		})
	}
}

// secCypherRunRejectsWithin runs q on eng under a watchdog and asserts it was
// rejected with a *parser.ParseError whose message contains wantMsg. A Result
// is drained and closed defensively in the (unexpected) event Run succeeds.
func secCypherRunRejectsWithin(t *testing.T, eng *cypher.Engine, q, wantMsg string) {
	t.Helper()
	type outcome struct {
		res *cypher.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := eng.Run(context.Background(), q, nil)
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		if o.res != nil {
			_ = o.res.Close()
		}
		secCypherAssertParseErr(t, o.err, wantMsg)
	case <-time.After(20 * time.Second):
		t.Fatalf("Run did not return within 20s — the pre-parse guard failed to short-circuit a hostile payload (wantMsg=%q)", wantMsg)
	}
}

// secCypherAssertParseErr asserts err is a non-nil error that unwraps to a
// *parser.ParseError whose Message contains wantMsg. Engine.Run wraps the
// guard's typed *parser.ParseError as "cypher: parse: %w", so errors.As is the
// correct, wrapping-tolerant check.
func secCypherAssertParseErr(t *testing.T, err error, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a rejection error, got nil (the DoS payload was accepted)")
	}
	var pe *parser.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error %T (%v) does not wrap *parser.ParseError", err, err)
	}
	if !strings.Contains(pe.Message, wantMsg) {
		t.Fatalf("ParseError.Message = %q; want it to contain %q", pe.Message, wantMsg)
	}
}
