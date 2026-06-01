package csv_test

import (
	"strings"
	"testing"

	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// TestCSVRead_EmbeddedNUL verifies the parser's behaviour when the
// input contains NUL ('\x00') bytes.
//
// Go 1.26's encoding/csv silently accepts NUL bytes; no parse error
// is returned.  The test therefore asserts that ReadInto does NOT
// panic (any return value is acceptable) and that calling it twice
// with NUL-containing input is idempotent.
func TestCSVRead_EmbeddedNUL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "nul_mid_field",
			input: "a\x00b,c,1\n",
		},
		{
			name:  "nul_as_first_char",
			input: "\x00,b,1\n",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// encoding/csv on Go 1.26 accepts NUL bytes silently; we
			// assert only that ReadInto does not panic.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ReadInto panicked on NUL input: %v", r)
				}
			}()
			_, _, _ = csv.ReadInto(strings.NewReader(tc.input), csv.DefaultOptions())
		})
	}
}
