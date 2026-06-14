package csv_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// Security regression battery for the CSV edge-list reader's behaviour on
// hostile input: a single row with a huge number of fields, embedded NUL
// bytes, an unterminated quoted field that an attacker might hope inflates
// allocation, and the MaxBytes cap at and just over its boundary.
//
// Crafted inputs are kept to a few megabytes at most, sized to prove the
// boundary (bounded by MaxBytes) rather than to allocate without limit.

// secIOReadCSV runs ReadInto with the given options and turns any panic
// into a fatal failure so every case shares the "never crashes the host"
// guarantee.
func secIOReadCSV(t *testing.T, in string, opts csv.Options) (rows int, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CSV reader panicked on hostile input: %v", r)
		}
	}()
	_, n, e := csv.ReadInto(strings.NewReader(in), opts)
	return n, e
}

// TestSec_IO_CSVReadManyFieldsBounded feeds one row with a very large number
// of comma-separated fields. encoding/csv parses it into a single record;
// the reader uses only the first three fields and the whole row is bounded
// by MaxBytes. The guard is "no panic, bounded by the cap" — the row is
// well-formed, so it is accepted, but a far larger version would trip
// ErrInputTooLarge rather than exhaust memory.
func TestSec_IO_CSVReadManyFieldsBounded(t *testing.T) {
	t.Parallel()

	// ~1 MiB row: src,dst, then ~500k empty trailing fields.
	const extra = 500_000
	var b strings.Builder
	b.Grow(extra + 8)
	b.WriteString("src,dst")
	for i := 0; i < extra; i++ {
		b.WriteByte(',')
	}
	b.WriteByte('\n')

	opts := csv.DefaultOptions() // 128 MiB cap — comfortably above ~1 MiB
	rows, err := secIOReadCSV(t, b.String(), opts)
	if err != nil {
		t.Fatalf("wide row rejected unexpectedly: err=%v", err)
	}
	if rows != 1 {
		t.Errorf("rows = %d, want 1 (only src,dst consumed)", rows)
	}
}

// TestSec_IO_CSVReadManyFieldsCapped confirms that the wide-row case is
// genuinely bounded by MaxBytes: with a small explicit cap, a row larger
// than the cap fails with ErrInputTooLarge instead of being read without
// end.
func TestSec_IO_CSVReadManyFieldsCapped(t *testing.T) {
	t.Parallel()

	const extra = 200_000 // ~200 KiB row
	var b strings.Builder
	b.Grow(extra + 8)
	b.WriteString("src,dst")
	for i := 0; i < extra; i++ {
		b.WriteByte(',')
	}
	b.WriteByte('\n')

	opts := csv.DefaultOptions()
	opts.MaxBytes = 4096 // far below the row size

	rows, err := secIOReadCSV(t, b.String(), opts)
	if !errors.Is(err, csv.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge (row exceeds cap)", err)
	}
	_ = rows
}

// TestSec_IO_CSVReadEmbeddedNUL pins that NUL bytes inside fields are
// accepted by encoding/csv without a panic. (The plain nul_bytes_test.go
// covers the default options; this adds the security-named regression pin
// with a NUL in every position of the triple.)
func TestSec_IO_CSVReadEmbeddedNUL(t *testing.T) {
	t.Parallel()

	cases := []string{
		"a\x00b,c,1\n",  // NUL mid-source
		"a,b\x00c,1\n",  // NUL mid-destination
		"\x00,\x00,1\n", // NUL-only ids
		"a,b,1\x002\n",  // NUL inside the weight field (parse error, no panic)
	}
	for _, in := range cases {
		in := in
		t.Run(strings.ReplaceAll(in, "\x00", "<NUL>"), func(t *testing.T) {
			t.Parallel()
			// Any return is acceptable; the contract is "no panic".
			_, _ = secIOReadCSV(t, in, csv.DefaultOptions())
		})
	}
}

// TestSec_IO_CSVReadStripsBOMSecurity pins BOM stripping under the
// security suite: a leading UTF-8 BOM must not become part of the first
// node id (which would let two logically-equal ids silently diverge).
func TestSec_IO_CSVReadStripsBOMSecurity(t *testing.T) {
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
	// "\ufeffsrc" is the id with a retained BOM prefix. The BOM must have
	// been stripped, so this lookup must miss. (A literal BOM in source is a
	// compile error, so the rune is written as an escape.)
	if _, ok := a.Mapper().Lookup("\ufeffsrc"); ok {
		t.Errorf("node id retains BOM prefix")
	}
}

// TestSec_IO_CSVReadNoAttackerCountPrealloc confirms the reader never sizes
// an allocation from attacker-controlled structure. The CSV format carries
// no record-count header, so a stream that delivers a handful of rows then
// an unterminated quoted field (which an attacker might use hoping the
// parser buffers without bound) is bounded: with a small cap the
// unterminated field trips ErrInputTooLarge, with the default cap it trips
// a parse error at EOF — never an OOM, never a panic.
func TestSec_IO_CSVReadNoAttackerCountPrealloc(t *testing.T) {
	t.Parallel()

	// Real rows, then a field that opens a quote and never closes it.
	const head = "a,b,1\nc,d,2\n"
	unterminated := head + `e,"` + strings.Repeat("x", 4096)

	// Small cap: the unterminated field is bounded by the cap.
	opts := csv.DefaultOptions()
	opts.MaxBytes = 256
	rows, err := secIOReadCSV(t, unterminated, opts)
	if !errors.Is(err, csv.ErrInputTooLarge) {
		t.Fatalf("small cap: err = %v, want ErrInputTooLarge", err)
	}
	_ = rows

	// Default cap: a parse error at EOF (bare quote), still bounded, no panic.
	rows2, err2 := secIOReadCSV(t, unterminated, csv.DefaultOptions())
	if err2 == nil {
		t.Fatalf("default cap: unterminated quoted field accepted, rows=%d", rows2)
	}
}

// TestSec_IO_CSVReadByteCapBoundary pins the MaxBytes crossing precisely:
// a stream exactly at the cap is accepted; the same stream one byte over
// returns ErrInputTooLarge.
func TestSec_IO_CSVReadByteCapBoundary(t *testing.T) {
	t.Parallel()

	const doc = "a,b,1\n"
	capBytes := int64(len(doc))

	opts := csv.DefaultOptions()
	opts.MaxBytes = capBytes
	a, rows, err := csv.ReadInto(strings.NewReader(doc), opts)
	if err != nil {
		t.Fatalf("at-cap stream rejected: err=%v, want nil", err)
	}
	if a == nil || rows != 1 {
		t.Fatalf("at-cap: a=%v rows=%d, want non-nil, 1", a, rows)
	}

	over := doc + "x"
	a2, _, err2 := csv.ReadInto(strings.NewReader(over), opts)
	if !errors.Is(err2, csv.ErrInputTooLarge) {
		t.Fatalf("over-cap stream: err=%v, want ErrInputTooLarge", err2)
	}
	if a2 != nil {
		t.Errorf("graph = %v, want nil on cap error", a2)
	}
}

// TestSec_IO_CSVReadStreamingHostileBounded feeds an unbounded stream of
// comma bytes (a single ever-growing row) through a streaming reader under
// a small cap, and asserts the reader stops with ErrInputTooLarge rather
// than reading without end. This is the "never read a hostile stream to
// exhaustion" guarantee, proved without materialising the bytes.
func TestSec_IO_CSVReadStreamingHostileBounded(t *testing.T) {
	t.Parallel()

	r := &secIOEndlessCommaReader{prefix: "src,dst"}
	opts := csv.DefaultOptions()
	opts.MaxBytes = 64 << 10 // 64 KiB ceiling

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("endless-comma stream panicked: %v", rec)
		}
	}()
	a, _, err := csv.ReadIntoCtx(context.Background(), r, opts)
	if !errors.Is(err, csv.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on cap error", a)
	}
}

// secIOEndlessCommaReader emits prefix once, then an unbounded run of comma
// bytes, never reaching EOF — a single never-ending CSV row.
type secIOEndlessCommaReader struct {
	prefix string
	pos    int
}

func (r *secIOEndlessCommaReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && r.pos < len(r.prefix) {
		p[n] = r.prefix[r.pos]
		r.pos++
		n++
	}
	for ; n < len(p); n++ {
		p[n] = ','
	}
	return n, nil
}

// compile-time assertion that the streaming reader satisfies io.Reader.
var _ io.Reader = (*secIOEndlessCommaReader)(nil)
