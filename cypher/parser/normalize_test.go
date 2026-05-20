package parser

import "testing"

func TestNormalizeSingleQuotes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple single-quoted string",
			input: `RETURN 'Alice'`,
			want:  `RETURN "Alice"`,
		},
		{
			name:  "multi-word single-quoted string",
			input: `RETURN 'The Matrix'`,
			want:  `RETURN "The Matrix"`,
		},
		{
			name:  "temporal arg single-quoted string",
			input: `RETURN '2015-07-21'`,
			want:  `RETURN "2015-07-21"`,
		},
		{
			name:  "escaped single quote inside string",
			input: `RETURN 'it\'s fine'`,
			want:  `RETURN "it's fine"`,
		},
		{
			name:  "bare double-quote inside single-quoted string",
			input: `RETURN 'say "hi"'`,
			want:  `RETURN "say \"hi\""`,
		},
		{
			name:  "double-quoted string unchanged",
			input: `RETURN "hello world"`,
			want:  `RETURN "hello world"`,
		},
		{
			name:  "no single quotes — fast path",
			input: `MATCH (n) RETURN n`,
			want:  `MATCH (n) RETURN n`,
		},
		{
			name:  "mixed: property with single-quoted value",
			input: `MATCH (n {name: 'The Matrix'}) RETURN n`,
			want:  `MATCH (n {name: "The Matrix"}) RETURN n`,
		},
		{
			name:  "line comment with single quote — unchanged",
			input: "RETURN 1 // it's fine",
			want:  "RETURN 1 // it's fine",
		},
		{
			name:  "backtick identifier with single quote — unchanged",
			input: "RETURN `it's`",
			want:  "RETURN `it's`",
		},
		{
			name:  "block comment with single quote — unchanged",
			input: "/* it's here */ RETURN 1",
			want:  "/* it's here */ RETURN 1",
		},
		{
			name:  "escaped double-quote inside single-quoted string",
			input: `RETURN 'a\"b'`,
			want:  `RETURN "a\"b"`,
		},
		{
			name:  "multiple single-quoted strings",
			input: `RETURN 'Alice' AS a, 'Bob' AS b`,
			want:  `RETURN "Alice" AS a, "Bob" AS b`,
		},
		{
			name:  "empty single-quoted string",
			input: `RETURN ''`,
			want:  `RETURN ""`,
		},
		{
			name:  "temporal function call",
			input: `RETURN date('2015-07-21') AS d`,
			want:  `RETURN date("2015-07-21") AS d`,
		},
		{
			name:  "duration single-quoted",
			input: `RETURN duration('P5M1.5D') AS dur`,
			want:  `RETURN duration("P5M1.5D") AS dur`,
		},
	}

	for _, tc := range cases {
		tc := tc // capture
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeSingleQuotes(tc.input)
			if got != tc.want {
				t.Errorf("normalizeSingleQuotes(%q)\n  got  %q\n  want %q", tc.input, got, tc.want)
			}
		})
	}
}
