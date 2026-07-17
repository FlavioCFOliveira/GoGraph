package csv_test

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// TestCSVWrite_CommentPrefixedIDRoundtrip is the regression guard for rmp
// #2042: a source id whose first rune is the active comment rune was written
// as a bare cell (encoding/csv.Writer never quotes a cell that merely begins
// with the comment rune), so on read the line was treated as a comment and the
// edge was silently dropped. The writer now force-quotes such a cell, so the
// edge survives a Write -> ReadInto cycle intact.
func TestCSVWrite_CommentPrefixedIDRoundtrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts func() csv.Options
		// src of the comment-prefixed edge; the guard row. A plain second
		// edge (c -> d) checks the surrounding rows are unaffected.
		src string
		dst string
	}{
		{
			name: "hash_default_options",
			opts: csv.DefaultOptions,
			src:  "#a",
			dst:  "b",
		},
		{
			// Options built by hand with Comment left zero must still protect
			// the default '#', exactly as the reader defaults it.
			name: "hash_zero_comment_defaulted",
			opts: func() csv.Options { return csv.Options{Delimiter: ',', Directed: true} },
			src:  "#a",
			dst:  "b",
		},
		{
			// A configured (non-default) comment rune must be honoured too.
			name: "configured_comment_rune",
			opts: func() csv.Options { o := csv.DefaultOptions(); o.Comment = '%'; return o },
			src:  "%row",
			dst:  "b",
		},
		{
			// The comment rune in the destination column also round-trips.
			name: "hash_in_destination",
			opts: csv.DefaultOptions,
			src:  "a",
			dst:  "#b",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := adjlist.New[string, int64](adjlist.Config{Directed: true})
			if err := a.AddEdge(tc.src, tc.dst, 1); err != nil {
				t.Fatalf("AddEdge guard edge: %v", err)
			}
			if err := a.AddEdge("c", "d", 2); err != nil {
				t.Fatalf("AddEdge plain edge: %v", err)
			}

			var buf bytes.Buffer
			n, err := csv.Write(&buf, a, tc.opts())
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if n != 2 {
				t.Fatalf("rows written = %d, want 2", n)
			}

			b, rows, err := csv.ReadInto(&buf, tc.opts())
			if err != nil {
				t.Fatalf("ReadInto: %v", err)
			}
			if rows != 2 {
				t.Fatalf("rows read = %d, want 2 (comment-prefixed edge dropped)", rows)
			}
			if !b.HasEdge(tc.src, tc.dst) {
				t.Errorf("edge (%q -> %q) lost after roundtrip", tc.src, tc.dst)
			}
			if !b.HasEdge("c", "d") {
				t.Errorf("plain edge (c -> d) lost after roundtrip")
			}
		})
	}
}
