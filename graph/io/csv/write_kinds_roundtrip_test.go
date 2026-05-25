package csv_test

import (
	"bytes"
	"math"
	"testing"

	"gograph/graph/adjlist"
	csv "gograph/graph/io/csv"
)

// TestCSVWrite_SpecialStringRoundtrip verifies that node IDs that
// require CSV quoting/escaping survive a Write → ReadInto cycle intact.
func TestCSVWrite_SpecialStringRoundtrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		src    string
		dst    string
		weight int64
	}{
		{
			name:   "comma_in_name",
			src:    "alice,jr",
			dst:    "bob,jr",
			weight: 1,
		},
		{
			name:   "quote_in_name",
			src:    `al"ice`,
			dst:    `bo"b`,
			weight: 2,
		},
		{
			name:   "newline_in_name",
			src:    "line1\nline2",
			dst:    "line3\nline4",
			weight: 3,
		},
		{
			name:   "unicode_astral",
			src:    "node\U0001F600",
			dst:    "node\U0001F601",
			weight: 4,
		},
		{
			name:   "int64_min",
			src:    "x",
			dst:    "y",
			weight: math.MinInt64,
		},
		{
			name:   "int64_max",
			src:    "p",
			dst:    "q",
			weight: math.MaxInt64,
		},
		{
			name:   "int64_zero",
			src:    "u",
			dst:    "v",
			weight: 0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := adjlist.New[string, int64](adjlist.Config{Directed: true})
			if err := a.AddEdge(tc.src, tc.dst, tc.weight); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}

			var buf bytes.Buffer
			if _, err := csv.Write(&buf, a, csv.DefaultOptions()); err != nil {
				t.Fatalf("Write: %v", err)
			}

			b, _, err := csv.ReadInto(&buf, csv.DefaultOptions())
			if err != nil {
				t.Fatalf("ReadInto: %v", err)
			}
			if !b.HasEdge(tc.src, tc.dst) {
				t.Errorf("edge (%q -> %q) lost after roundtrip", tc.src, tc.dst)
			}
		})
	}
}
