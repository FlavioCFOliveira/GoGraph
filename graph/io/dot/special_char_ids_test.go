package dot_test

import (
	"bytes"
	"strings"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/io/dot"
)

// TestDOTWrite_SpecialCharIDs verifies that node IDs containing characters
// outside the DOT simple-ID alphabet are enclosed in double quotes.
func TestDOTWrite_SpecialCharIDs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		src, dst      string
		expectedQuote string // substring that must appear in the output
	}{
		{
			name:          "space_in_id",
			src:           "alice smith",
			dst:           "bob jones",
			expectedQuote: `"alice smith"`,
		},
		{
			name:          "hyphen_in_id",
			src:           "node-1",
			dst:           "node-2",
			expectedQuote: `"node-1"`,
		},
		{
			name:          "dot_in_id",
			src:           "node.1",
			dst:           "node.2",
			expectedQuote: `"node.1"`,
		},
		{
			name:          "unicode",
			src:           "café",
			dst:           "naïve",
			expectedQuote: `"café"`,
		},
		{
			name:          "empty_string",
			src:           "",
			dst:           "b",
			expectedQuote: `""`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := adjlist.New[string, int64](adjlist.Config{Directed: true})
			if err := a.AddEdge(tc.src, tc.dst, 0); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			var buf bytes.Buffer
			if err := dot.Write(&buf, a); err != nil {
				t.Fatalf("Write: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, tc.expectedQuote) {
				t.Errorf("output does not contain %q:\n%s", tc.expectedQuote, out)
			}
		})
	}
}
