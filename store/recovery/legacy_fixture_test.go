package recovery

import (
	"bytes"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// logPayloads returns the payloads of every frame of the store log at walPath
// (legacy frames except the seal, then segment frames), in order.
func logPayloads(t testing.TB, walPath string) [][]byte {
	t.Helper()
	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	var out [][]byte
	for f := range r.Frames() {
		out = append(out, f.Payload)
	}
	if err := r.TailError(); err != nil {
		t.Fatalf("reading %s: %v", walPath, err)
	}
	return out
}

// writeLegacyLog replaces the store log at walPath with a legacy single-file
// log holding payloads as [wal.LegacyVersion] frames: the control file and the
// segments are removed. Tests whose subject is the replay state machine use it
// to hand-craft a frame sequence (a spliced-out marker, a corrupted body) that
// a segmented log would refuse earlier for its broken positions.
func writeLegacyLog(t testing.TB, walPath string, payloads [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, p := range payloads {
		if _, err := wal.Encode(&buf, wal.Frame{Payload: p}); err != nil {
			t.Fatalf("wal.Encode: %v", err)
		}
	}
	if err := os.RemoveAll(wal.SegmentDir(walPath)); err != nil {
		t.Fatalf("remove segments: %v", err)
	}
	if err := os.Remove(wal.ControlPath(walPath)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove control file: %v", err)
	}
	if err := os.WriteFile(walPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}
	return buf.Bytes()
}

// openSingleFileWAL opens a single-file WAL writer at path ([wal.OpenWith] over
// the file): no control file, no segments, positions equal to file offsets.
// Recovery reads such a directory as a legacy single-file log. It is the
// fixture writer of the tests that damage the log by byte offset.
func openSingleFileWAL(t testing.TB, path string) *wal.Writer {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	w, err := wal.OpenWith(f)
	if err != nil {
		t.Fatalf("wal.OpenWith: %v", err)
	}
	return w
}
