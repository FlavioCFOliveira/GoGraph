package snapshot

// Regression lock-ins for the 2026-07-01 hostility+load audit finding F8
// (#1833): LoadManifest decoded manifest.json with no byte ceiling, so a
// hostile or corrupt snapshot directory (a giant JSON array or string field)
// could drive a multi-gigabyte transient allocation at store-open before any
// version or CRC check bounds it — the one untrusted store-file decode path
// lacking the DefaultMaxBytes-style ceiling every sibling loader has.
//
// The fix wraps the file-backed reader in a manifestLimitReader with the
// DefaultMaxManifestBytes ceiling and surfaces a typed ErrManifestTooLarge.
// These tests pin: (1) the reader's byte accounting + boundary error;
// (2) that LoadManifest over an oversized stream through the real
// DefaultMaxManifestBytes ceiling — the exact composition readManifestFileWith
// performs — fails with ErrManifestTooLarge rather than reading unboundedly;
// and (3) that a valid manifest below the ceiling round-trips unchanged through
// the same wrapping (below-cap transparency).

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// infiniteReader yields the same byte forever; it never returns io.EOF. It
// stands in for an unbounded/hostile manifest body so the decode must be
// stopped by the byte ceiling, not by end-of-input.
type infiniteReader struct{ b byte }

func (r infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

func TestManifestLimitReader_BoundaryAndAccounting(t *testing.T) {
	t.Parallel()
	// remaining=4: exactly 4 bytes permitted, the 5th read attempt errors.
	lr := &manifestLimitReader{r: bytes.NewReader([]byte("abcdef")), remaining: 4}
	buf := make([]byte, 3)

	n, err := lr.Read(buf)
	if n != 3 || err != nil {
		t.Fatalf("read 1: n=%d err=%v; want 3, nil", n, err)
	}
	// 1 byte of budget remains; a 3-byte request is capped to 1.
	n, err = lr.Read(buf)
	if n != 1 || err != nil {
		t.Fatalf("read 2: n=%d err=%v; want 1, nil", n, err)
	}
	// Budget exhausted: next read is ErrManifestTooLarge.
	if _, err = lr.Read(buf); !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("read 3: err=%v; want ErrManifestTooLarge", err)
	}
}

func TestLoadManifest_RejectsOversizedThroughRealCeiling(t *testing.T) {
	t.Parallel()
	// A whitespace stream that never ends: json.Decode keeps reading looking for
	// a value, so only the byte ceiling can stop it. Wrap with the SAME
	// composition readManifestFileWith uses (the real DefaultMaxManifestBytes).
	lr := &manifestLimitReader{r: infiniteReader{b: ' '}, remaining: DefaultMaxManifestBytes}
	_, err := LoadManifest(lr)
	if err == nil {
		t.Fatal("LoadManifest read an unbounded manifest without error; want ErrManifestTooLarge")
	}
	if !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("LoadManifest error = %v; want errors.Is(ErrManifestTooLarge)", err)
	}
}

func TestLoadManifest_ValidBelowCeilingRoundTrips(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	want := Manifest{Version: ManifestVersion, Order: 7, Size: 123}
	if err := WriteManifest(&buf, want); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	// A legitimate manifest is far below the ceiling: the limit reader is
	// transparent and the decode returns EOF normally.
	lr := &manifestLimitReader{r: io.Reader(bytes.NewReader(buf.Bytes())), remaining: DefaultMaxManifestBytes}
	got, err := LoadManifest(lr)
	if err != nil {
		t.Fatalf("LoadManifest of a valid below-ceiling manifest: %v", err)
	}
	if got.Version != want.Version || got.Order != want.Order || got.Size != want.Size {
		t.Fatalf("round-trip mismatch: got %+v; want %+v", got, want)
	}
}
