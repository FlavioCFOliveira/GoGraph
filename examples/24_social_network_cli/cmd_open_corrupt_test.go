package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// firstFramePayloadOffset returns the byte offset of the first payload byte
// of the first WAL frame in raw (i.e. just past the first frame header).
// The WAL header layout is magic(4) | version(2) | length(4) | crc(4); the
// length field at offset 6 is not needed here because the payload of frame
// 0 always begins immediately after the fixed-size header.
func firstFramePayloadOffset(t *testing.T, raw []byte) int {
	t.Helper()
	if len(raw) < wal.HeaderSize+1 {
		t.Fatalf("WAL too small to corrupt: %d bytes", len(raw))
	}
	return wal.HeaderSize
}

// TestOpenStore_RefusesCorruptWAL is the example-level guard for task
// #1289: after a committed write, flipping a byte inside a durable
// (non-tail) WAL frame makes recovery fail-stop, and the example's
// openStore helper must refuse to open the data directory for append
// rather than silently building on the damaged prefix and dropping every
// committed op past the corruption.
//
// The assertion exercises the real recovery path used by every subcommand
// (openStore -> recovery.OpenCtx -> res.IsClean() guard -> wal.Open). A
// clean (uncorrupted) directory is opened as a positive control so the
// harness is proven to accept a healthy WAL.
func TestOpenStore_RefusesCorruptWAL(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	// Seed the canonical fixture: this commits a populated transaction to
	// the WAL through the example's own store, then closes it.
	var seedOut bytes.Buffer
	if err := runSeed(context.Background(), dir, &seedOut); err != nil {
		t.Fatalf("runSeed: %v", err)
	}

	walPath := filepath.Join(dir, "wal")
	raw, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("seed produced an empty WAL; nothing to corrupt")
	}

	// Positive control: the un-corrupted directory must open cleanly.
	o, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore on a clean WAL: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("close clean store: %v", err)
	}

	// Flip a payload byte inside the first (durable) frame so its CRC32C no
	// longer validates. This is genuine corruption, not a torn tail.
	corrupt := append([]byte(nil), raw...)
	corrupt[firstFramePayloadOffset(t, corrupt)] ^= 0xFF
	if err := os.WriteFile(walPath, corrupt, 0o600); err != nil { //nolint:gosec // path under t.TempDir
		t.Fatalf("write corrupt WAL: %v", err)
	}

	_, err = openStore(context.Background(), dir)
	if err == nil {
		t.Fatal("openStore opened a corrupt WAL; it must refuse to append")
	}
	// recovery.OpenCtx now returns the corruption as a hard error, so
	// openStore's first error check refuses before ever reaching wal.Open
	// for append. (The explicit res.IsClean() guard that follows is
	// defence-in-depth for a caller that ignores the error.) Either way the
	// returned error must wrap the underlying CRC sentinel so callers can
	// branch on it with errors.Is.
	if !errors.Is(err, wal.ErrCRCMismatch) {
		t.Fatalf("openStore error = %v, want it to wrap wal.ErrCRCMismatch", err)
	}
	if !strings.Contains(err.Error(), "open:") {
		t.Fatalf("openStore error = %q, want it scoped to the open path", err.Error())
	}

	// The corrupt WAL on disk must be left untouched by the refused open:
	// no new frame may have been appended.
	after, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("re-read WAL: %v", err)
	}
	if len(after) != len(corrupt) {
		t.Fatalf("WAL length changed after a refused open: before=%d after=%d (must not append to a corrupt WAL)",
			len(corrupt), len(after))
	}
}
