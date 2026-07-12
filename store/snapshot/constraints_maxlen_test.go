package snapshot

// constraints_maxlen_test.go — regression for #1903: the constraints.bin writer
// must never emit a field the reader would reject as corrupt. Before the fix
// the writer used an unbounded uint32 length while the reader capped each field
// at constraintsMaxStringLen, so a > 64 KiB identifier written at checkpoint
// time turned the snapshot into a poison pill that failed to reopen.

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestWriteConstraints_RejectsOverLongField(t *testing.T) {
	over := strings.Repeat("x", constraintsMaxStringLen+1)
	specs := []ConstraintSpec{{Kind: 0, Label: "L", Property: "p", Name: over}}

	var buf bytes.Buffer
	if _, _, err := WriteConstraints(&buf, specs); !errors.Is(err, ErrConstraintsCorrupted) {
		t.Fatalf("expected ErrConstraintsCorrupted writing an over-long field, got: %v", err)
	}
}

func TestWriteConstraints_AtCapFieldRoundTrips(t *testing.T) {
	atCap := strings.Repeat("x", constraintsMaxStringLen)
	specs := []ConstraintSpec{{Kind: 1, Label: "L", Property: "p", Name: atCap}}

	var buf bytes.Buffer
	if _, _, err := WriteConstraints(&buf, specs); err != nil {
		t.Fatalf("at-cap field must be writable: %v", err)
	}
	rb, err := ReadConstraints(&buf)
	if err != nil {
		t.Fatalf("at-cap field must round-trip: %v", err)
	}
	if len(rb.Specs) != 1 || rb.Specs[0].Name != atCap {
		t.Fatalf("round-trip mismatch: got %+v", rb.Specs)
	}
}
