package parser

import "testing"

// Tests for all pre-lex normalisers in normalize.go that go beyond
// normalizeSingleQuotes (already covered in normalize_test.go).
//
// Coverage targets:
//
//   - normalizeVarlenBounds   ([r*N..M] → [r*-N..-M] rewriter)
//   - normalizeVarlenDotDot   ([N..M] → [*N..M] insertion)
//   - normalizeArithmeticMinus (a-1 → a - 1 spacing)
//   - normalizeZeroDotFloat   (0.N  → .N)
//   - normalizeLeadingDotFloat (.0  → 0.0)
//   - normalizeNegHexOct      (-0x1A → (-26))
//   - normalizeDoubleNot      (NOT NOT x → x)
//   - normalizeCallNoParen    (CALL p YIELD x → CALL p() YIELD x)
//   - containsDoubleNot       (fast-path predicate)
//   - hasByte / hasSingleQuote (fast-path predicates)
//
// Each test exercises:
//   - the fast path (input has no trigger byte)
//   - the happy path (one rewrite)
//   - the escape / boundary path (rewrite suppressed inside strings/comments)
//   - rare branches (escape sequences, overflow, etc.)

// -----------------------------------------------------------------------------
// hasByte / hasSingleQuote
// -----------------------------------------------------------------------------

func TestHasByteFastPath(t *testing.T) {
	t.Parallel()
	if hasByte("", 'x') {
		t.Error("hasByte(\"\", 'x') = true; want false")
	}
	if !hasByte("abc", 'b') {
		t.Error("hasByte(\"abc\", 'b') = false; want true")
	}
	if hasByte("abc", 'z') {
		t.Error("hasByte(\"abc\", 'z') = true; want false")
	}
}

func TestHasSingleQuote(t *testing.T) {
	t.Parallel()
	if hasSingleQuote("") {
		t.Error("hasSingleQuote(\"\") = true; want false")
	}
	if hasSingleQuote("abc") {
		t.Error("hasSingleQuote(\"abc\") = true; want false")
	}
	if !hasSingleQuote("a'b") {
		t.Error("hasSingleQuote(\"a'b\") = false; want true")
	}
}

// -----------------------------------------------------------------------------
// normalizeVarlenBounds
// -----------------------------------------------------------------------------

func TestNormalizeVarlenBounds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "fast path no star",
			input: "MATCH (n) RETURN n",
			want:  "MATCH (n) RETURN n",
		},
		{
			name:  "no bracket no rewrite",
			input: "RETURN 1 * 2",
			want:  "RETURN 1 * 2",
		},
		{
			name:  "bare star unchanged",
			input: "MATCH (a)-[*]->(b) RETURN a",
			want:  "MATCH (a)-[*]->(b) RETURN a",
		},
		{
			name:  "fixed bound rewritten",
			input: "MATCH (a)-[*2]->(b) RETURN a",
			want:  "MATCH (a)-[*-2]->(b) RETURN a",
		},
		{
			name:  "lower bound only",
			input: "MATCH (a)-[*1..]->(b) RETURN a",
			want:  "MATCH (a)-[*-1..]->(b) RETURN a",
		},
		{
			name:  "upper bound only",
			input: "MATCH (a)-[*..3]->(b) RETURN a",
			want:  "MATCH (a)-[*..-3]->(b) RETURN a",
		},
		{
			name:  "both bounds",
			input: "MATCH (a)-[*1..3]->(b) RETURN a",
			want:  "MATCH (a)-[*-1..-3]->(b) RETURN a",
		},
		{
			name:  "multi-digit bounds",
			input: "MATCH (a)-[*10..200]->(b) RETURN a",
			want:  "MATCH (a)-[*-10..-200]->(b) RETURN a",
		},
		{
			name:  "string with bracket and star is not rewritten",
			input: `RETURN "[r*1..3]"`,
			want:  `RETURN "[r*1..3]"`,
		},
		{
			name:  "single-quoted string preserved",
			input: `RETURN '[r*5]'`,
			want:  `RETURN '[r*5]'`,
		},
		{
			name:  "backtick identifier preserved",
			input: "RETURN `[r*5]`",
			want:  "RETURN `[r*5]`",
		},
		{
			name:  "line comment preserved",
			input: "MATCH (a)-[*5]->(b) // [r*7]\nRETURN a",
			want:  "MATCH (a)-[*-5]->(b) // [r*7]\nRETURN a",
		},
		{
			name:  "block comment preserved",
			input: "MATCH (a)-[*5]->(b) /* [r*7] */ RETURN a",
			want:  "MATCH (a)-[*-5]->(b) /* [r*7] */ RETURN a",
		},
		{
			name:  "nested brackets",
			input: "MATCH (a)-[*2..4]->(b) WHERE b.x[0] = 1 RETURN a",
			want:  "MATCH (a)-[*-2..-4]->(b) WHERE b.x[0] = 1 RETURN a",
		},
		{
			name:  "unmatched escape inside double-quoted string",
			input: `RETURN "abc\""`,
			want:  `RETURN "abc\""`,
		},
		{
			name:  "double-dot only",
			input: "MATCH (a)-[*..]->(b) RETURN a",
			want:  "MATCH (a)-[*..]->(b) RETURN a",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeVarlenBounds(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// normalizeVarlenDotDot
// -----------------------------------------------------------------------------

func TestNormalizeVarlenDotDot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "fast path no dot",
			input: "RETURN 1 + 2",
			want:  "RETURN 1 + 2",
		},
		{
			name:  "no bracket no rewrite",
			input: "RETURN a.b.c",
			want:  "RETURN a.b.c",
		},
		{
			name:  "rel pattern bare double-dot",
			input: "MATCH (a)-[..]->(b) RETURN a",
			want:  "MATCH (a)-[*..]->(b) RETURN a",
		},
		{
			// normalizeVarlenDotDot inserts "*" immediately before the first ".."
			// (not at the opening bracket). When the bound is before "..", the
			// result is "[N*..]" rather than "[*N..]".
			name:  "rel pattern with upper bound only",
			input: "MATCH (a)-[..3]->(b) RETURN a",
			want:  "MATCH (a)-[*..3]->(b) RETURN a",
		},
		{
			name:  "rel pattern with lower bound only",
			input: "MATCH (a)-[1..]->(b) RETURN a",
			want:  "MATCH (a)-[1*..]->(b) RETURN a",
		},
		{
			name:  "rel pattern with both bounds",
			input: "MATCH (a)-[1..3]->(b) RETURN a",
			want:  "MATCH (a)-[1*..3]->(b) RETURN a",
		},
		{
			name:  "rel pattern with star already present unchanged",
			input: "MATCH (a)-[*1..3]->(b) RETURN a",
			want:  "MATCH (a)-[*1..3]->(b) RETURN a",
		},
		{
			name:  "list subscript bracket unchanged",
			input: "RETURN n.x[1..3]",
			want:  "RETURN n.x[1..3]",
		},
		{
			name:  "rel pattern with type label",
			input: "MATCH (a)-[:KNOWS..3]->(b) RETURN a",
			want:  "MATCH (a)-[:KNOWS*..3]->(b) RETURN a",
		},
		{
			name:  "string with bracket-and-dotdot preserved",
			input: `RETURN "-[..]-"`,
			want:  `RETURN "-[..]-"`,
		},
		{
			name:  "block comment preserved",
			input: "/* -[..]- */ MATCH (a)-[..]->(b) RETURN a",
			want:  "/* -[..]- */ MATCH (a)-[*..]->(b) RETURN a",
		},
		{
			name:  "line comment preserved",
			input: "MATCH (a)-[..]->(b) // -[..]-\nRETURN a",
			want:  "MATCH (a)-[*..]->(b) // -[..]-\nRETURN a",
		},
		{
			name:  "backtick identifier preserved",
			input: "RETURN `-[..]-`",
			want:  "RETURN `-[..]-`",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeVarlenDotDot(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// normalizeArithmeticMinus
// -----------------------------------------------------------------------------

func TestNormalizeArithmeticMinus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "fast path no minus",
			input: "RETURN a + b",
			want:  "RETURN a + b",
		},
		{
			name:  "ident minus digit gets spacing",
			input: "RETURN n-1",
			want:  "RETURN n - 1",
		},
		{
			name:  "scientific exponent preserved",
			input: "RETURN 1.5e-3",
			want:  "RETURN 1.5e-3",
		},
		{
			name:  "scientific upper E exponent preserved",
			input: "RETURN 1.5E-3",
			want:  "RETURN 1.5E-3",
		},
		{
			name:  "negative literal at start not rewritten",
			input: "RETURN -42",
			want:  "RETURN -42",
		},
		{
			name:  "spaced subtraction unchanged",
			input: "RETURN n - 1",
			want:  "RETURN n - 1",
		},
		{
			name:  "string with minus preserved",
			input: `RETURN "n-1"`,
			want:  `RETURN "n-1"`,
		},
		{
			name:  "single-quoted string with minus preserved",
			input: `RETURN 'n-1'`,
			want:  `RETURN 'n-1'`,
		},
		{
			name:  "backtick with minus preserved",
			input: "RETURN `n-1`",
			want:  "RETURN `n-1`",
		},
		{
			name:  "line comment preserved",
			input: "RETURN n-1 // n-1\n",
			want:  "RETURN n - 1 // n-1\n",
		},
		{
			name:  "block comment preserved",
			input: "RETURN n-1 /* n-1 */",
			want:  "RETURN n - 1 /* n-1 */",
		},
		{
			name:  "digit minus digit gets spacing",
			input: "RETURN 5-3",
			want:  "RETURN 5 - 3",
		},
		{
			name:  "trailing minus alone",
			input: "RETURN n - ",
			want:  "RETURN n - ",
		},
		{
			name:  "minus at end of input",
			input: "RETURN n -",
			want:  "RETURN n -",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeArithmeticMinus(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// normalizeZeroDotFloat
// -----------------------------------------------------------------------------

func TestNormalizeZeroDotFloat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "fast path no zero",
			input: "RETURN x + y",
			want:  "RETURN x + y",
		},
		{
			name:  "zero dot digit rewritten",
			input: "RETURN 0.5",
			want:  "RETURN .5",
		},
		{
			name:  "10.5 not rewritten (preceding ident char)",
			input: "RETURN 10.5",
			want:  "RETURN 10.5",
		},
		{
			name:  "underscore-prefixed not rewritten",
			input: "RETURN a_0.5",
			want:  "RETURN a_0.5",
		},
		{
			name:  "no digit after dot not rewritten",
			input: "RETURN 0.x",
			want:  "RETURN 0.x",
		},
		{
			name:  "bare 0 not rewritten",
			input: "RETURN 0",
			want:  "RETURN 0",
		},
		{
			name:  "string with 0.5 preserved",
			input: `RETURN "0.5"`,
			want:  `RETURN "0.5"`,
		},
		{
			name:  "single-quoted 0.5 preserved",
			input: `RETURN '0.5'`,
			want:  `RETURN '0.5'`,
		},
		{
			name:  "backtick 0.5 preserved",
			input: "RETURN `0.5`",
			want:  "RETURN `0.5`",
		},
		{
			name:  "line comment with 0.5 preserved",
			input: "RETURN 1 // 0.5\n",
			want:  "RETURN 1 // 0.5\n",
		},
		{
			name:  "block comment with 0.5 preserved",
			input: "RETURN 1 /* 0.5 */",
			want:  "RETURN 1 /* 0.5 */",
		},
		{
			name:  "negative zero dot digit",
			input: "RETURN -0.5",
			want:  "RETURN -.5",
		},
		{
			name:  "0.0 boundary",
			input: "RETURN 0.0",
			want:  "RETURN .0",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeZeroDotFloat(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// normalizeLeadingDotFloat
// -----------------------------------------------------------------------------

func TestNormalizeLeadingDotFloat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "fast path no dot",
			input: "RETURN a + b",
			want:  "RETURN a + b",
		},
		{
			name:  "leading dot zero digit prepends 0",
			input: "RETURN .05",
			want:  "RETURN 0.05",
		},
		{
			name:  "leading dot non-zero unchanged",
			input: "RETURN .5",
			want:  "RETURN .5",
		},
		{
			name:  "property access not rewritten",
			input: "RETURN n.05",
			want:  "RETURN n.05",
		},
		{
			name:  "subscript followed by dot not rewritten",
			input: "RETURN x[0].05",
			want:  "RETURN x[0].05",
		},
		{
			name:  "string with leading-dot zero preserved",
			input: `RETURN ".05"`,
			want:  `RETURN ".05"`,
		},
		{
			name:  "backtick preserved",
			input: "RETURN `.05`",
			want:  "RETURN `.05`",
		},
		{
			name:  "single-quoted preserved",
			input: `RETURN '.05'`,
			want:  `RETURN '.05'`,
		},
		{
			name:  "line comment preserved",
			input: "RETURN 1 // .05\n",
			want:  "RETURN 1 // .05\n",
		},
		{
			name:  "block comment preserved",
			input: "RETURN 1 /* .05 */",
			want:  "RETURN 1 /* .05 */",
		},
		{
			name:  "dot at end of input",
			input: "RETURN .",
			want:  "RETURN .",
		},
		{
			name:  "leading dot zero with multiple digits",
			input: "RETURN .00123",
			want:  "RETURN 0.00123",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeLeadingDotFloat(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// normalizeNegHexOct
// -----------------------------------------------------------------------------

func TestNormalizeNegHexOct(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "fast path no minus",
			input: "RETURN 1 + 2",
			want:  "RETURN 1 + 2",
		},
		{
			name:  "negative hex literal",
			input: "RETURN -0x1A",
			want:  "RETURN (-26)",
		},
		{
			name:  "negative hex uppercase",
			input: "RETURN -0X1a",
			want:  "RETURN (-26)",
		},
		{
			name:  "negative octal literal",
			input: "RETURN -0o17",
			want:  "RETURN (-15)",
		},
		{
			name:  "negative octal uppercase",
			input: "RETURN -0O17",
			want:  "RETURN (-15)",
		},
		{
			name:  "decimal minus preserved",
			input: "RETURN -42",
			want:  "RETURN -42",
		},
		{
			name:  "binary subtraction preserved",
			input: "RETURN a-0x10",
			want:  "RETURN a-0x10",
		},
		{
			name:  "hex overflow keeps original text",
			input: "RETURN -0xFFFFFFFFFFFFFFFFFF",
			want:  "RETURN -0xFFFFFFFFFFFFFFFFFF",
		},
		{
			name:  "octal overflow keeps original text",
			input: "RETURN -0o7777777777777777777777777777",
			want:  "RETURN -0o7777777777777777777777777777",
		},
		{
			name:  "hex INT64_MIN bit pattern",
			input: "RETURN -0x8000000000000000",
			want:  "RETURN (-9223372036854775808)",
		},
		{
			name:  "minus without hex/oct preserved",
			input: "RETURN -abc",
			want:  "RETURN -abc",
		},
		{
			name:  "minus with no following digits preserved",
			input: "RETURN -0xZ",
			want:  "RETURN -0xZ",
		},
		{
			name:  "minus followed by 0 alone preserved",
			input: "RETURN -0",
			want:  "RETURN -0",
		},
		{
			name:  "string with hex preserved",
			input: `RETURN "-0x1A"`,
			want:  `RETURN "-0x1A"`,
		},
		{
			name:  "backtick preserved",
			input: "RETURN `-0x1A`",
			want:  "RETURN `-0x1A`",
		},
		{
			name:  "single-quoted preserved",
			input: `RETURN '-0x1A'`,
			want:  `RETURN '-0x1A'`,
		},
		{
			name:  "line comment preserved",
			input: "RETURN 1 // -0x1A\n",
			want:  "RETURN 1 // -0x1A\n",
		},
		{
			name:  "block comment preserved",
			input: "RETURN 1 /* -0x1A */",
			want:  "RETURN 1 /* -0x1A */",
		},
		{
			name:  "octal INT64_MIN bit pattern",
			input: "RETURN -0o1000000000000000000000",
			want:  "RETURN (-9223372036854775808)",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeNegHexOct(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// normalizeDoubleNot / containsDoubleNot
// -----------------------------------------------------------------------------

func TestContainsDoubleNot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "empty", input: "", want: false},
		{name: "no N", input: "RETURN 1", want: false},
		{name: "single NOT", input: "WHERE NOT x", want: false},
		{name: "NOT NOT", input: "WHERE NOT NOT x", want: true},
		{name: "case insensitive not not", input: "where not not x", want: true},
		{name: "NOT followed by ident char", input: "WHERE NOTHING", want: false},
		{name: "Nope (NOT followed by ident)", input: "Nope NOT", want: false},
		{name: "triple NOT", input: "WHERE NOT NOT NOT x", want: true},
		{name: "NOT separated by tabs", input: "WHERE NOT\tNOT x", want: true},
		{name: "NOT at end (boundary)", input: "WHERE x NOT NOT", want: true},
		{name: "lone N at end", input: "N", want: false},
		{name: "NO at end (no T)", input: "NO", want: false},
		{name: "NOZ (T mismatch)", input: "NOZ", want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := containsDoubleNot(tc.input)
			if got != tc.want {
				t.Errorf("containsDoubleNot(%q) = %v; want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizeDoubleNot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "fast path no double NOT",
			input: "MATCH (n) WHERE n.x = 1 RETURN n",
			want:  "MATCH (n) WHERE n.x = 1 RETURN n",
		},
		{
			name:  "even NOT collapses to none",
			input: "WHERE NOT NOT x",
			want:  "WHERE x",
		},
		{
			name:  "odd NOT collapses to one",
			input: "WHERE NOT NOT NOT x",
			want:  "WHERE NOT x",
		},
		{
			name:  "case insensitive",
			input: "WHERE not not x",
			want:  "WHERE x",
		},
		{
			name:  "mixed case",
			input: "WHERE Not NoT x",
			want:  "WHERE x",
		},
		{
			name:  "string with NOT NOT preserved",
			input: `RETURN "NOT NOT" AS s`,
			want:  `RETURN "NOT NOT" AS s`,
		},
		{
			name:  "single-quoted NOT NOT preserved",
			input: `RETURN 'NOT NOT'`,
			want:  `RETURN 'NOT NOT'`,
		},
		{
			name:  "backtick NOT NOT preserved",
			input: "RETURN `NOT NOT`",
			want:  "RETURN `NOT NOT`",
		},
		{
			name:  "line comment NOT NOT preserved",
			input: "RETURN 1 // NOT NOT\n",
			want:  "RETURN 1 // NOT NOT\n",
		},
		{
			name:  "block comment NOT NOT preserved",
			input: "RETURN 1 /* NOT NOT */",
			want:  "RETURN 1 /* NOT NOT */",
		},
		{
			name:  "four NOTs collapse to zero",
			input: "WHERE NOT NOT NOT NOT x",
			want:  "WHERE x",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeDoubleNot(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// normalizeCallNoParen
// -----------------------------------------------------------------------------

func TestNormalizeCallNoParen(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "fast path no C",
			input: "RETURN 1",
			want:  "RETURN 1",
		},
		{
			name:  "no CALL keyword",
			input: "MATCH (n) RETURN n",
			want:  "MATCH (n) RETURN n",
		},
		{
			name:  "CALL with parens already unchanged",
			input: "CALL db.labels() YIELD label",
			want:  "CALL db.labels() YIELD label",
		},
		{
			name:  "CALL no parens with YIELD gets parens",
			input: "CALL db.labels YIELD label",
			want:  "CALL db.labels() YIELD label",
		},
		{
			name:  "CALL lowercase no parens with yield gets parens",
			input: "call db.labels yield label",
			want:  "call db.labels() yield label",
		},
		{
			name:  "CALL no parens no YIELD unchanged",
			input: "CALL db.labels",
			want:  "CALL db.labels",
		},
		{
			name:  "CALL with backtick name and YIELD",
			input: "CALL `db.labels` YIELD label",
			want:  "CALL `db.labels`() YIELD label",
		},
		{
			name:  "CALL inside string preserved",
			input: `RETURN "CALL db.labels YIELD x"`,
			want:  `RETURN "CALL db.labels YIELD x"`,
		},
		{
			name:  "single-quoted CALL preserved",
			input: `RETURN 'CALL db.labels YIELD x'`,
			want:  `RETURN 'CALL db.labels YIELD x'`,
		},
		{
			name:  "backtick CALL preserved",
			input: "RETURN `CALL db.labels YIELD x`",
			want:  "RETURN `CALL db.labels YIELD x`",
		},
		{
			name:  "line comment CALL preserved",
			input: "// CALL db.labels YIELD x\nRETURN 1",
			want:  "// CALL db.labels YIELD x\nRETURN 1",
		},
		{
			name:  "block comment CALL preserved",
			input: "/* CALL db.labels YIELD x */ RETURN 1",
			want:  "/* CALL db.labels YIELD x */ RETURN 1",
		},
		{
			name:  "CALL not at word boundary unchanged",
			input: "RECALL db.labels YIELD x",
			want:  "RECALL db.labels YIELD x",
		},
		{
			name:  "CALL followed by something other than YIELD unchanged",
			input: "CALL db.labels RETURN x",
			want:  "CALL db.labels RETURN x",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeCallNoParen(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Additional normalizeSingleQuotes edge cases that complement normalize_test.go
// -----------------------------------------------------------------------------

func TestNormalizeSingleQuotesEdges(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "escape sequence pass-through",
			input: `RETURN 'a\nb'`,
			want:  `RETURN "a\nb"`,
		},
		{
			name:  "trailing backslash inside literal pass-through",
			input: `RETURN 'a\tb'`,
			want:  `RETURN "a\tb"`,
		},
		{
			name:  "double-quote escape inside double-quoted preserved",
			input: `RETURN "a\"b"`,
			want:  `RETURN "a\"b"`,
		},
		{
			name:  "single quote in line comment preserved",
			input: "MATCH (n) // it's a node\nRETURN n",
			want:  "MATCH (n) // it's a node\nRETURN n",
		},
		{
			name:  "slash not start of comment",
			input: "RETURN 1 / 2",
			want:  "RETURN 1 / 2",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeSingleQuotes(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// normalizeFloatExpZeroPad
// -----------------------------------------------------------------------------

func TestNormalizeFloatExpZeroPad(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "fast path no e or E",
			input: "RETURN 42",
			want:  "RETURN 42",
		},
		{
			name:  "negative exponent single leading zero",
			input: "RETURN 2E-01",
			want:  "RETURN 2E-1",
		},
		{
			name:  "negative exponent lowercase e",
			input: "RETURN 2e-01",
			want:  "RETURN 2e-1",
		},
		{
			name:  "negative exponent multiple leading zeros",
			input: "RETURN 5E-001",
			want:  "RETURN 5E-1",
		},
		{
			name:  "positive exponent leading zero",
			input: "RETURN 2E+01",
			want:  "RETURN 2E+1",
		},
		{
			name:  "fractional mantissa with leading-zero exponent",
			input: "RETURN 2.5E-01",
			want:  "RETURN 2.5E-1",
		},
		{
			name:  "leading zero followed by another zero then non-zero",
			input: "RETURN 7E-010",
			want:  "RETURN 7E-10",
		},
		{
			name:  "no leading zero unchanged",
			input: "RETURN 7E-5",
			want:  "RETURN 7E-5",
		},
		{
			name:  "exponent normalises to zero leaves source unchanged",
			input: "RETURN 2E-0",
			want:  "RETURN 2E-0",
		},
		{
			name:  "exponent all zeros leaves source unchanged",
			input: "RETURN 2E-00",
			want:  "RETURN 2E-00",
		},
		{
			name:  "missing sign leaves source unchanged",
			input: "RETURN 2E01",
			want:  "RETURN 2E01",
		},
		{
			name:  "hex literal containing E is not touched",
			input: "RETURN 0x2E-01",
			want:  "RETURN 0x2E-01",
		},
		{
			name:  "hex with leading-zero suffix is not touched",
			input: "RETURN 0xAB-01",
			want:  "RETURN 0xAB-01",
		},
		{
			name:  "octal literal is not touched",
			input: "RETURN 0o7-01",
			want:  "RETURN 0o7-01",
		},
		{
			name:  "identifier containing E digits is not touched",
			input: "RETURN var2E01",
			want:  "RETURN var2E01",
		},
		{
			name:  "string with E-form preserved",
			input: `RETURN "2E-01"`,
			want:  `RETURN "2E-01"`,
		},
		{
			name:  "single-quoted string with E-form preserved",
			input: "RETURN '2E-01'",
			want:  "RETURN '2E-01'",
		},
		{
			name:  "backtick identifier preserved",
			input: "RETURN `2E-01`",
			want:  "RETURN `2E-01`",
		},
		{
			name:  "line comment preserved",
			input: "RETURN 1 // 2E-01\n",
			want:  "RETURN 1 // 2E-01\n",
		},
		{
			name:  "block comment preserved",
			input: "RETURN 1 /* 2E-01 */",
			want:  "RETURN 1 /* 2E-01 */",
		},
		{
			name:  "list with E-form inside",
			input: "RETURN [2E-01, 1]",
			want:  "RETURN [2E-1, 1]",
		},
		{
			name:  "multiple E-form floats in one query",
			input: "RETURN 2E-01, 3.5E-002, 7E+05",
			want:  "RETURN 2E-1, 3.5E-2, 7E+5",
		},
		{
			name:  "binary subtraction is not affected",
			input: "RETURN n - 1",
			want:  "RETURN n - 1",
		},
		{
			name:  "property access not affected",
			input: "RETURN n.prop",
			want:  "RETURN n.prop",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeFloatExpZeroPad(tc.input)
			if got != tc.want {
				t.Errorf("\n  input %q\n  got   %q\n  want  %q", tc.input, got, tc.want)
			}
		})
	}
}
