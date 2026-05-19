package wal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeNFrames(t *testing.T, path string, n int) {
	t.Helper()
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	for i := 0; i < n; i++ {
		if err := w.Append([]byte(fmt.Sprintf("frame-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestReader_Replay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	writeNFrames(t, path, 10)

	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	got := 0
	err = r.Replay(func(f Frame) error {
		want := fmt.Sprintf("frame-%d", got)
		if string(f.Payload) != want {
			return fmt.Errorf("frame %d payload = %q, want %q", got, f.Payload, want)
		}
		got++
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got != 10 {
		t.Fatalf("frames seen = %d, want 10", got)
	}
}

func TestReader_TornAtEveryOffset(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	writeNFrames(t, path, 5)
	full, err := os.ReadFile(path) //nolint:gosec // path from t.TempDir
	if err != nil {
		t.Fatal(err)
	}

	for cut := 0; cut <= len(full); cut++ {
		r := NewReader(bytes.NewReader(full[:cut]), nil)
		seen := 0
		err := r.Replay(func(_ Frame) error {
			seen++
			return nil
		})
		// Replay should never fail with a non-torn error on
		// systematic truncations; ErrTornFrame at the tail is fine,
		// other errors at the tail (CRCMismatch, BadMagic) are also
		// acceptable — they end iteration cleanly.
		if err != nil {
			if !errors.Is(err, ErrTornFrame) &&
				!errors.Is(err, ErrCRCMismatch) &&
				!errors.Is(err, ErrBadMagic) {
				t.Fatalf("cut=%d: unexpected error %v", cut, err)
			}
		}
		if seen > 5 {
			t.Fatalf("cut=%d: saw %d frames > 5", cut, seen)
		}
	}
}

func TestReader_TailErrorOnCorruption(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	writeNFrames(t, path, 3)

	full, err := os.ReadFile(path) //nolint:gosec // path from t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	full[HeaderSize+0] ^= 0xff // corrupt first byte of first payload
	r := NewReader(bytes.NewReader(full), nil)
	count := 0
	for range r.Frames() {
		count++
	}
	_ = count
	if !errors.Is(r.TailError(), ErrCRCMismatch) {
		t.Fatalf("TailError = %v, want ErrCRCMismatch", r.TailError())
	}
}

func TestReader_EarlyStop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	writeNFrames(t, path, 10)
	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	seen := 0
	for range r.Frames() {
		seen++
		if seen == 3 {
			break
		}
	}
	if seen != 3 {
		t.Fatalf("early break: seen=%d", seen)
	}
}

func BenchmarkReader_Replay(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 4096)
	const frames = 64
	for i := 0; i < frames; i++ {
		_ = w.Append(payload)
	}
	_ = w.Close()
	totalBytes := int64(frames * (HeaderSize + len(payload)))
	b.SetBytes(totalBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := OpenReader(path)
		if err != nil {
			b.Fatal(err)
		}
		_ = r.Replay(func(_ Frame) error { return nil })
		_ = r.Close()
	}
}
