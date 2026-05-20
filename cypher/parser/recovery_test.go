package parser

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Bounded error recovery tests (task 210)
// ---------------------------------------------------------------------------

// TestTwoIndependentErrorsBothReported verifies that two syntactically
// independent errors in a single input are both surfaced by [ParseStrict].
// The semicolon acts as a statement boundary that prevents ANTLR's single-shot
// recovery from swallowing the second error.
func TestTwoIndependentErrorsBothReported(t *testing.T) {
	// "RETURN ," is missing an expression; "RETURN ," is another independent
	// instance after the ';' separator. ParseStrict should report ≥ 2 errors.
	const query = "RETURN , ; RETURN ,"
	_, errs := ParseStrict(query)
	if len(errs) < 2 {
		t.Fatalf("expected ≥2 errors for %q, got %d: %v", query, len(errs), errs)
	}
}

// TestCascadingErrorsCapped verifies that a pathological input producing many
// consecutive parse errors does not generate more than [maxParseErrors] errors.
func TestCascadingErrorsCapped(t *testing.T) {
	// Each "@ @ @ @" token is invalid; the lexer/parser generates an error for
	// every one of them but the cap must suppress all beyond maxParseErrors.
	//
	// We use invalid character sequences that the lexer reports as errors,
	// giving us a large burst of raw errors from which only maxParseErrors may
	// pass through.
	const query = "@ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @"
	_, errs := ParseStrict(query)
	if len(errs) > maxParseErrors {
		t.Fatalf("expected ≤%d errors for pathological input, got %d: %v",
			maxParseErrors, len(errs), errs)
	}
}

// TestCascadingKeywordFloodCapped verifies the cap on a parser-level cascade
// where an early structural mistake causes every subsequent keyword to be
// misidentified as erroneous.
func TestCascadingKeywordFloodCapped(t *testing.T) {
	// Missing WHERE predicate causes the parser to misparse the remainder.
	// The resulting cascade must be capped at maxParseErrors.
	const query = "MATCH (n) WHERE RETURN n UNION RETURN n UNION RETURN n UNION RETURN n UNION RETURN n"
	_, errs := ParseStrict(query)
	if len(errs) > maxParseErrors {
		t.Fatalf("expected ≤%d errors for cascading input, got %d: %v",
			maxParseErrors, len(errs), errs)
	}
}

// TestEmptyInputNoErrors verifies that an empty string is handled gracefully
// and does not panic.
func TestEmptyInputNoErrors(_ *testing.T) {
	// Empty input produces a parse error (missing expression) but must not panic.
	_, _ = ParseStrict("")
}

// ---------------------------------------------------------------------------
// Fuzz test (task 210)
// ---------------------------------------------------------------------------

// FuzzParse verifies that Parse never panics and always terminates on
// arbitrary byte sequences. It seeds the corpus with inputs known to trigger
// recovery paths.
func FuzzParse(f *testing.F) {
	// Seed with a selection of known-interesting inputs: valid queries, invalid
	// queries that exercise recovery, empty and binary-ish inputs.
	seeds := []string{
		"MATCH (n) RETURN n",
		"RETURN 1",
		"",
		"@@@@@@@@@@@@@@@@@@@@@",
		"RETURN , ; RETURN ,",
		"MATCH (n RETURN n",
		"THIS IS NOT CYPHER",
		"MATCH (n) WHERE RETURN n UNION RETURN n UNION RETURN n",
		"RETURN [1, 2",
		"RETURN {a: 1,}",
		"@ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @ @",
		"\x00\x01\x02\x03",
		"MATCH\x00(n)\x00RETURN\x00n",
		"RETURN CASE WHEN THEN 1 END",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		// Parse must not panic. It may return any combination of AST and errors.
		// We only assert termination (implicit) and no panic.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Parse panicked on input %q: %v", input, r)
			}
		}()

		_, _ = Parse(input)

		// Also exercise ParseStrict and verify the error cap is respected.
		_, errs := ParseStrict(input)
		if len(errs) > maxParseErrors {
			t.Errorf("ParseStrict returned %d errors (cap=%d) for input %q",
				len(errs), maxParseErrors, input)
		}
	})
}
