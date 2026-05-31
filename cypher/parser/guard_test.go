package parser

import (
	"errors"
	"strings"
	"testing"
)

// asParseError asserts that err is a non-nil *ParseError and returns it.
func asParseError(t *testing.T, err error) *ParseError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a *ParseError, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ParseError, got %T: %v", err, err)
	}
	return pe
}

// TestGuardRejectsDeepNesting verifies that a query nested past maxNestingDepth
// is rejected with a *ParseError and does NOT crash the process. The three
// bracket families — '(', '[' and '{' — are each exercised independently.
//
// Without the guard the recursive parser/visitor overflows the goroutine stack,
// which is a fatal error that recover() cannot catch; the test would therefore
// abort the whole `go test` process rather than fail cleanly.
func TestGuardRejectsDeepNesting(t *testing.T) {
	// One level beyond the cap is enough to trip the guard; we use a large
	// multiple to make the intent unambiguous and to stay well clear of any
	// real stack-overflow threshold even if the guard were removed.
	const n = 150000

	cases := []struct {
		name   string
		prefix string // run before the brackets
		open   byte
		close  byte
		inner  string // innermost token between open and close runs
	}{
		{name: "parens", prefix: "RETURN ", open: '(', close: ')', inner: "1"},
		{name: "lists", prefix: "RETURN ", open: '[', close: ']', inner: ""},
		{name: "maps", prefix: "RETURN ", open: '{', close: '}', inner: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString(tc.prefix)
			b.WriteString(strings.Repeat(string(tc.open), n))
			b.WriteString(tc.inner)
			b.WriteString(strings.Repeat(string(tc.close), n))
			q := b.String()

			_, err := Parse(q)
			pe := asParseError(t, err)
			if !strings.Contains(pe.Message, "nesting too deep") {
				t.Fatalf("expected a nesting-depth error, got: %v", pe)
			}
		})
	}
}

// TestGuardDeepNestingParseStrict verifies the guard fires for ParseStrict too,
// returning the depth error as the sole error in the slice.
func TestGuardDeepNestingParseStrict(t *testing.T) {
	const n = 150000
	q := "RETURN " + strings.Repeat("(", n) + "1" + strings.Repeat(")", n)

	_, errs := ParseStrict(q)
	if len(errs) != 1 {
		t.Fatalf("expected exactly one error, got %d: %v", len(errs), errs)
	}
	pe := asParseError(t, errs[0])
	if !strings.Contains(pe.Message, "nesting too deep") {
		t.Fatalf("expected a nesting-depth error, got: %v", pe)
	}
}

// TestGuardBracketsInStringNotCounted is the false-positive guard: a query whose
// string literal contains many '(' (or other brackets) must NOT be rejected,
// because brackets inside a string carry no structural meaning. This proves the
// scanner skips string content correctly.
func TestGuardBracketsInStringNotCounted(t *testing.T) {
	// A string literal far deeper than maxNestingDepth, wrapped in benign
	// structure. The whole query nests only one real level (the RETURN).
	bigParens := strings.Repeat("(", maxNestingDepth*4)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "single_quoted",
			query: "RETURN '" + bigParens + "' AS s",
		},
		{
			name:  "double_quoted",
			query: `RETURN "` + bigParens + `" AS s`,
		},
		{
			name: "mixed_brackets_in_string",
			query: "RETURN '" +
				strings.Repeat("([{", maxNestingDepth*2) + "' AS s",
		},
		{
			name:  "escaped_quote_inside_string",
			query: `RETURN '` + bigParens + `\'` + bigParens + `' AS s`,
		},
		{
			name:  "brackets_in_line_comment",
			query: "RETURN 1 AS n // " + bigParens,
		},
		{
			name:  "brackets_in_block_comment",
			query: "RETURN /* " + bigParens + " */ 1 AS n",
		},
		{
			name:  "brackets_in_backtick_identifier",
			query: "RETURN 1 AS `" + bigParens + "`",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The guard itself must not reject it.
			if err := guardInput(tc.query); err != nil {
				t.Fatalf("guard wrongly rejected a legitimate query: %v", err)
			}
			// And the full parse must succeed (these are valid Cypher).
			if _, err := Parse(tc.query); err != nil {
				t.Fatalf("Parse failed on a legitimate query: %v", err)
			}
		})
	}
}

// TestGuardWithinBoundParses verifies a query nested right at the limit still
// parses unchanged — the guard must not be off-by-one against legitimate input.
func TestGuardWithinBoundParses(t *testing.T) {
	// maxNestingDepth nested parentheses around a literal. Exactly at the cap
	// must be accepted (the guard rejects only depth > maxNestingDepth).
	q := "RETURN " + strings.Repeat("(", maxNestingDepth) + "1" +
		strings.Repeat(")", maxNestingDepth)

	if err := guardInput(q); err != nil {
		t.Fatalf("guard rejected a query exactly at the depth cap: %v", err)
	}
	if _, err := Parse(q); err != nil {
		t.Fatalf("Parse failed on a query at the depth cap: %v", err)
	}
}

// TestGuardOverLengthRejected verifies an over-length query is rejected with a
// *ParseError before any parsing work is done.
func TestGuardOverLengthRejected(t *testing.T) {
	// Pad a trivially-valid query with whitespace past the byte cap.
	q := "RETURN 1 AS n" + strings.Repeat(" ", maxQueryBytes)

	_, err := Parse(q)
	pe := asParseError(t, err)
	if !strings.Contains(pe.Message, "too large") {
		t.Fatalf("expected an over-length error, got: %v", pe)
	}
}

// TestGuardInputBoundaries exercises maxBracketDepth directly on small inputs to
// pin down the counting and skipping behaviour.
func TestMaxBracketDepth(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		depth int
	}{
		{name: "empty", in: "", depth: 0},
		{name: "no_brackets", in: "RETURN 1 AS n", depth: 0},
		{name: "flat_pairs", in: "()()()", depth: 1},
		{name: "nested_three", in: "((()))", depth: 3},
		{name: "mixed_families", in: "([{}])", depth: 3},
		{name: "interleaved", in: "(a[b{c}])", depth: 3},
		{name: "unbalanced_close_ignored", in: ")))(((", depth: 3},
		{name: "single_quoted_skipped", in: "'((((('", depth: 0},
		{name: "double_quoted_skipped", in: `"((((("`, depth: 0},
		{name: "backtick_skipped", in: "`(((((`", depth: 0},
		{name: "line_comment_skipped", in: "// (((((\n", depth: 0},
		{name: "block_comment_skipped", in: "/* ((((( */", depth: 0},
		{name: "escaped_quote_keeps_string", in: `'\'((' `, depth: 0},
		{name: "structure_around_string", in: "( '(((' )", depth: 1},
		{name: "structure_after_comment", in: "// x\n((", depth: 2},
		{name: "div_not_comment", in: "(a/b)", depth: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxBracketDepth(tc.in); got != tc.depth {
				t.Fatalf("maxBracketDepth(%q) = %d, want %d", tc.in, got, tc.depth)
			}
		})
	}
}

// TestItoa pins the dependency-free integer formatter used in guard messages.
func TestItoa(t *testing.T) {
	cases := map[int]string{
		0:       "0",
		7:       "7",
		256:     "256",
		1 << 20: "1048576",
	}
	for n, want := range cases {
		if got := itoa(n); got != want {
			t.Fatalf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}
