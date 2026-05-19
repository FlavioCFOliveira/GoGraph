package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCompat_V1FixtureReplays asserts the frozen v1 WAL fixture
// committed at store/wal/testdata/v1/sample.wal still decodes
// cleanly under the current build. Regenerate with:
//
//	go run ./cmd/fmtfixture -pkg wal
//
// Any mismatch flags an unintended on-disk-format change.
func TestCompat_V1FixtureReplays(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "v1", "sample.wal")
	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader(%s): %v", path, err)
	}
	defer func() { _ = r.Close() }()
	var seen []string
	err = r.Replay(func(f Frame) error {
		seen = append(seen, string(f.Payload))
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	want := []string{"alpha", "beta", "gamma", "delta", "omega"}
	if len(seen) != len(want) {
		t.Fatalf("seen %d frames, want %d", len(seen), len(want))
	}
	for i, w := range want {
		if seen[i] != w {
			t.Fatalf("frame %d = %q, want %q", i, seen[i], w)
		}
	}
}

// TestCompat_FutureVersionRejected synthesises a v1 frame whose
// version field is bumped past CurrentVersion and verifies that the
// current decoder returns ErrUnsupportedVersion instead of silently
// mis-parsing or panicking.
func TestCompat_FutureVersionRejected(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "v1", "sample.wal")
	data, err := os.ReadFile(path) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < HeaderSize {
		t.Fatalf("fixture shorter than HeaderSize")
	}
	bumped := append([]byte(nil), data...)
	// Header layout: magic (4) | version (2 LE) | length (4 LE) | crc (4)
	binary.LittleEndian.PutUint16(bumped[4:6], CurrentVersion+1)
	_, err = Decode(bytes.NewReader(bumped))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("future-version Decode = %v, want ErrUnsupportedVersion", err)
	}
}
