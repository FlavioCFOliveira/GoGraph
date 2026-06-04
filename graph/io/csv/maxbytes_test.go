package csv_test

import (
	"errors"
	"strings"
	"testing"

	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// TestReadInto_MaxBytesExceeded feeds a single CSV field far larger than
// a deliberately low MaxBytes ceiling and asserts the reader fails with
// the typed [csv.ErrInputTooLarge] rather than buffering the whole field.
func TestReadInto_MaxBytesExceeded(t *testing.T) {
	t.Parallel()

	const capBytes = 1024
	// One row whose first field alone is 8x the cap. The limit reader
	// must trip before the oversized field is fully read.
	huge := "a" + strings.Repeat("x", capBytes*8) + ",b\n"

	opts := csv.DefaultOptions()
	opts.MaxBytes = capBytes

	a, n, err := csv.ReadInto(strings.NewReader(huge), opts)
	if !errors.Is(err, csv.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on cap error", a)
	}
	if n != 0 {
		t.Errorf("rows = %d, want 0 on cap error", n)
	}
}

// TestReadInto_DefaultCapAllowsSmallInput is the control: a small input
// decodes cleanly through the default entry point, which applies the
// DefaultMaxBytes ceiling.
func TestReadInto_DefaultCapAllowsSmallInput(t *testing.T) {
	t.Parallel()

	a, n, err := csv.ReadInto(strings.NewReader("a,b,1\nb,c,2\n"), csv.DefaultOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	if a == nil {
		t.Fatal("graph is nil")
	}
}

// TestReadInto_MaxBytesDisabled confirms MaxBytes <= 0 opts out of the
// cap entirely: an input larger than DefaultMaxBytes would otherwise be
// rejected, but a small input with the cap disabled still succeeds.
func TestReadInto_MaxBytesDisabled(t *testing.T) {
	t.Parallel()

	opts := csv.DefaultOptions()
	opts.MaxBytes = 0 // disable the cap

	if _, n, err := csv.ReadInto(strings.NewReader("a,b,1\n"), opts); err != nil || n != 1 {
		t.Fatalf("ReadInto with cap disabled: n=%d err=%v, want n=1 err=nil", n, err)
	}
}

// TestReadInto_CapWithHeadroom confirms an input comfortably under the
// cap is accepted. The encoding/csv reader probes one read past the last
// record looking for EOF, so the cap must allow at least that probe; a
// few bytes of headroom is enough.
func TestReadInto_CapWithHeadroom(t *testing.T) {
	t.Parallel()

	input := "ab,cd\n" // 6 bytes
	opts := csv.DefaultOptions()
	opts.MaxBytes = int64(len(input)) + 16

	if _, n, err := csv.ReadInto(strings.NewReader(input), opts); err != nil || n != 1 {
		t.Fatalf("under cap: n=%d err=%v, want n=1 err=nil", n, err)
	}
}
