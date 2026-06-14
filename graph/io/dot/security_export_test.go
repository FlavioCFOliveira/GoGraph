package dot_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/dot"
)

// Security regression pin for the DOT writer's identifier neutralisation.
// The dot package is write-only (no reader), so the attack surface is the
// export path: a node id containing DOT metacharacters must not break out
// of its quoted string and inject graph structure or attributes into the
// emitted document. The writer encloses any non-simple id in double quotes
// and backslash-escapes embedded quotes and backslashes; these tests pin
// that so a future writer change cannot silently reintroduce an injection.

// TestSec_IO_DOTExportQuotesHostileIDs feeds node ids crafted to escape the
// DOT identifier grammar — embedded quotes, semicolons, braces, newlines,
// and attribute-like fragments — and asserts each appears inside a quoted,
// escaped string in the output, never as bare injected DOT tokens.
func TestSec_IO_DOTExportQuotesHostileIDs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		id   string
		// want is a substring that must appear: the id rendered inside a
		// quoted, properly-escaped DOT string.
		want string
	}{
		{
			name: "embedded_quote_and_attr",
			id:   `a" [color=red]; "b`,
			want: `"a\" [color=red]; \"b"`,
		},
		{
			name: "statement_breakout",
			id:   `x"; evil -> evil; "`,
			want: `"x\"; evil -> evil; \""`,
		},
		{
			name: "backslash",
			id:   `a\b`,
			want: `"a\\b"`,
		},
		{
			name: "newline_in_id",
			id:   "a\nb",
			want: "\"a\nb\"", // quoted; the newline stays inside the string
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := adjlist.New[string, int64](adjlist.Config{Directed: true})
			if err := a.AddEdge(tc.id, "safe", 0); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}

			var buf bytes.Buffer
			if err := dot.Write(&buf, a); err != nil {
				t.Fatalf("Write: %v", err)
			}
			out := buf.String()

			if !strings.Contains(out, tc.want) {
				t.Errorf("hostile id not quoted/escaped as expected.\nwant substring: %q\ngot:\n%s", tc.want, out)
			}
			// The injected "evil -> evil" must never appear as a bare
			// (unquoted) edge statement. It is only acceptable inside the
			// escaped string literal asserted above.
			if strings.Contains(tc.id, "evil") {
				// Count occurrences: the only legitimate one is inside the
				// quoted id. A breakout would add a second, bare occurrence.
				if strings.Count(out, "evil") != strings.Count(tc.id, "evil") {
					t.Errorf("possible DOT statement injection — 'evil' appears outside the quoted id:\n%s", out)
				}
			}
		})
	}
}
