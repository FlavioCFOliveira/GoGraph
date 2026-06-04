package csv_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// TestReadInto_NilGraphOnParseError pins the uniform all-or-nothing
// contract: a stream that is valid for several records and then turns
// malformed must yield a nil graph alongside the typed parse error, so a
// caller cannot accidentally commit a half-imported graph.
func TestReadInto_NilGraphOnParseError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{
			// Two good rows, then a row with an unparseable weight.
			name:  "bad_weight",
			input: "a,b,1\nb,c,2\nc,d,not-a-number\n",
		},
		{
			// Two good rows, then a row with too few fields.
			name:  "too_few_fields",
			input: "a,b,1\nb,c,2\nlonely\n",
		},
		{
			// Two good rows, then a row whose quoting is malformed, which
			// the encoding/csv reader rejects with a parse error.
			name:  "bad_quote",
			input: "a,b,1\nb,c,2\n\"unterminated,d,4\n",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, _, err := csv.ReadInto(strings.NewReader(tc.input), csv.DefaultOptions())
			if err == nil {
				t.Fatalf("expected a parse error for %s, got nil", tc.name)
			}
			if a != nil {
				t.Errorf("graph = %v, want nil on parse error", a)
			}
		})
	}
}

// TestReadInto_NilGraphOnCancel confirms the same nil-on-error contract
// holds for context cancellation mid-stream: the partial graph is
// discarded and the context error is returned.
func TestReadInto_NilGraphOnCancel(t *testing.T) {
	t.Parallel()

	// Pre-cancel: the ctx check at rows=0 fires immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a, _, err := csv.ReadIntoCtx(ctx, strings.NewReader("a,b,1\nb,c,2\n"), csv.DefaultOptions())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on cancellation", a)
	}
}
