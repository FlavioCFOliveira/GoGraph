package csv

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestFieldGuard_BareCommaFloodRejected confirms the headline fix: a single
// record made of far more delimiters than the per-record cap is rejected with
// ErrTooManyFields rather than accepted (which would let encoding/csv allocate
// ~40 bytes per field → multi-GiB OOM). #1844 (CWE-789/1284).
func TestFieldGuard_BareCommaFloodRejected(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("src,dst")
	for i := 0; i < maxFieldsPerRecord+10; i++ {
		b.WriteByte(',')
	}
	b.WriteByte('\n')

	_, _, err := ReadInto(strings.NewReader(b.String()), DefaultOptions())
	if !errors.Is(err, ErrTooManyFields) {
		t.Fatalf("bare-comma flood: err = %v, want ErrTooManyFields", err)
	}
}

// TestFieldGuard_QuotedCommasNotCounted is the critical quote-awareness test:
// a single QUOTED field containing more commas than the cap is still one field,
// so the record (that field plus one more) must be ACCEPTED. A guard that
// naively counted every comma would wrongly reject legitimate quoted data.
func TestFieldGuard_QuotedCommasNotCounted(t *testing.T) {
	t.Parallel()
	// First field is a quoted blob holding cap+100 commas; then a second field.
	blob := strings.Repeat(",", maxFieldsPerRecord+100)
	input := "\"" + blob + "\",dst\n"

	a, rows, err := ReadInto(strings.NewReader(input), DefaultOptions())
	if err != nil {
		t.Fatalf("quoted-comma record rejected unexpectedly: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}
	if a == nil {
		t.Fatal("graph is nil")
	}
}

// TestFieldGuard_ModerateWideRecordAccepted confirms a record wider than the
// importer's three columns but within the field cap is still accepted, with the
// surplus columns ignored — the pre-existing behaviour is preserved.
func TestFieldGuard_ModerateWideRecordAccepted(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("src,dst,5") // src,dst,weight=5
	for i := 0; i < 1000; i++ {  // 1000 surplus columns, well under the cap
		b.WriteString(",x")
	}
	b.WriteByte('\n')

	_, rows, err := ReadInto(strings.NewReader(b.String()), DefaultOptions())
	if err != nil {
		t.Fatalf("moderate wide row rejected unexpectedly: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}
}

// TestFieldGuard_ManyNormalRowsNotAccumulated confirms the field count resets at
// every record boundary: a file of many short rows (whose TOTAL delimiter count
// dwarfs the cap) parses cleanly, proving the guard bounds per-record, not
// cumulative, fields.
func TestFieldGuard_ManyNormalRowsNotAccumulated(t *testing.T) {
	t.Parallel()
	const nRows = 200_000 // 400k total delimiters, but only 2 per record
	var b strings.Builder
	for i := 0; i < nRows; i++ {
		fmt.Fprintf(&b, "%d,%d,1\n", i, i+1)
	}
	_, rows, err := ReadInto(strings.NewReader(b.String()), DefaultOptions())
	if err != nil {
		t.Fatalf("many normal rows rejected unexpectedly: %v", err)
	}
	if rows != nRows {
		t.Fatalf("rows = %d, want %d", rows, nRows)
	}
}

// TestFieldGuard_feed exercises the quote/field state machine directly on the
// subtle transitions: CRLF terminators, escaped quotes (""), and delimiters and
// newlines that live inside a quoted field.
func TestFieldGuard_feed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		// wantFields is the field count of the LAST record after feeding input.
		wantFields int
		wantErr    bool
	}{
		{"plain three fields", "a,b,c", 3, false},
		{"crlf resets record", "a,b\r\nc,d", 2, false},
		{"lf resets record", "a,b,c\nd", 1, false},
		{"quoted delimiter is literal", "\"a,b,c\",d", 2, false},
		{"quoted newline does not reset", "\"a\nb\",c", 2, false},
		{"escaped quote stays in field", "\"a\"\"b\",c", 2, false},
		{"empty quoted field", "\"\",\"\",x", 3, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newFieldGuardReader(strings.NewReader(tc.input), ',')
			var err error
			for i := 0; i < len(tc.input); i++ {
				if e := g.feed(tc.input[i]); e != nil {
					err = e
					break
				}
			}
			if tc.wantErr && err == nil {
				t.Fatalf("input %q: want error, got none", tc.input)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("input %q: unexpected error %v", tc.input, err)
			}
			if !tc.wantErr && g.fields != tc.wantFields {
				t.Fatalf("input %q: fields = %d, want %d", tc.input, g.fields, tc.wantFields)
			}
		})
	}
}
