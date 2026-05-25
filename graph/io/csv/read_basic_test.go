package csv_test

import (
	"strings"
	"testing"

	csv "gograph/graph/io/csv"
)

// TestCSVRead_HeaderSkipCommentsBadWeightShortRow covers the common
// read-path branches: header skip, comment stripping, weight parse
// failure, and short rows.
func TestCSVRead_HeaderSkipCommentsBadWeightShortRow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		input       string
		opts        csv.Options
		wantEdges   int
		wantErr     bool
		errContains []string
	}{
		{
			name:      "header_skip",
			input:     "src,dst,weight\na,b,1\nc,d,2\n",
			opts:      func() csv.Options { o := csv.DefaultOptions(); o.HasHeader = true; return o }(),
			wantEdges: 2,
		},
		{
			name:      "comments_skipped",
			input:     "# comment\na,b,1\n# another\nb,c,2\n",
			opts:      csv.DefaultOptions(),
			wantEdges: 2,
		},
		{
			name:        "valid_before_bad_weight",
			input:       "a,b,1\nc,d,not-a-number\n",
			opts:        csv.DefaultOptions(),
			wantErr:     true,
			errContains: []string{"row 2", "weight"},
		},
		{
			name:        "short_row",
			input:       "a,b,1\nx\n",
			opts:        csv.DefaultOptions(),
			wantErr:     true,
			errContains: []string{"row 2"},
		},
		{
			name:      "comment_then_valid",
			input:     "# hdr\na,b,42\n",
			opts:      csv.DefaultOptions(),
			wantEdges: 1,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, n, err := csv.ReadInto(strings.NewReader(tc.input), tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				msg := err.Error()
				for _, want := range tc.errContains {
					if !strings.Contains(msg, want) {
						t.Errorf("error %q does not contain %q", msg, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n != tc.wantEdges {
				t.Fatalf("edge count = %d, want %d", n, tc.wantEdges)
			}
			_ = a
		})
	}
}
