package csv_test

import (
	"errors"
	"strings"
	"testing"

	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// TestReadInto_StripsUTF8BOM asserts that a leading UTF-8 BOM (EF BB BF),
// as emitted by Excel and other Windows tools, is stripped so the first
// node id is the clean logical id rather than one prefixed with U+FEFF
// (#1441).
func TestReadInto_StripsUTF8BOM(t *testing.T) {
	t.Parallel()

	const bom = "\xef\xbb\xbf"
	a, n, err := csv.ReadInto(strings.NewReader(bom+"src,dst,5\n"), csv.DefaultOptions())
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}

	if _, ok := a.Mapper().Lookup("src"); !ok {
		t.Errorf("clean node id %q not found — BOM not stripped", "src")
	}
	if _, ok := a.Mapper().Lookup("\ufeffsrc"); ok {
		t.Errorf("node id retains BOM prefix %q", "\ufeffsrc")
	}
}

// TestReadInto_NoBOMUnaffected confirms a stream with no BOM still
// interns the clean node id.
func TestReadInto_NoBOMUnaffected(t *testing.T) {
	t.Parallel()

	a, _, err := csv.ReadInto(strings.NewReader("src,dst,5\n"), csv.DefaultOptions())
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if _, ok := a.Mapper().Lookup("src"); !ok {
		t.Errorf("node id %q not found", "src")
	}
}

// TestReadInto_BOMProbeRespectsTinyCap confirms the BOM probe (a Peek(3))
// does not defeat a byte cap smaller than the 3-byte BOM: a 2-byte cap on
// a 2-byte input still fails with ErrInputTooLarge rather than slipping
// through the swallowed Peek error (#1441 / review m2).
func TestReadInto_BOMProbeRespectsTinyCap(t *testing.T) {
	t.Parallel()

	opts := csv.DefaultOptions()
	opts.MaxBytes = 2
	a, _, err := csv.ReadInto(strings.NewReader("a,b,c\n"), opts)
	if !errors.Is(err, csv.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on cap error", a)
	}
}
