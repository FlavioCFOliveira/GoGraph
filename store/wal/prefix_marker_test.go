package wal

// prefix_marker_test.go — rmp #2990 (storage audit F1, F3): every operation that
// discards WAL history makes the prefix-truncation marker durable first, and a
// failure while doing so leaves the log intact and the Writer usable.
//
// Layer: short.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestTruncate_WritesPrefixMarkerBeforeEmptying pins F1: the whole-file Truncate
// discards history exactly as TruncatePrefix does, so a non-empty log leaves the
// marker behind. Without it, losing the snapshot Truncate relies on reopens as an
// empty, clean store.
func TestTruncate_WritesPrefixMarkerBeforeEmptying(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Append([]byte("folded")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n, err := w.Truncate(); err != nil || n == 0 {
		t.Fatalf("Truncate = (%d, %v), want a non-zero size and nil", n, err)
	}
	if _, err := os.Stat(PrefixTruncatedMarkerPath(path)); err != nil {
		t.Fatalf("prefix marker after emptying a non-empty WAL: %v", err)
	}
}

// TestTruncate_EmptyLogWritesNoMarker is the boundary: emptying a log that holds
// nothing discards no history, so no marker is written and a store that never
// held data does not demand a snapshot.
func TestTruncate_EmptyLogWritesNoMarker(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	if _, err := w.Truncate(); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if _, err := os.Stat(PrefixTruncatedMarkerPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prefix marker after emptying an empty WAL: %v, want not-exist", err)
	}
}

// TestTruncatePrefix_MarkerDirFsyncFailureLeavesWALIntact pins F3: the marker's
// directory fsync runs through the dirFsync seam, and a failure there happens
// before any byte is discarded. The call returns the error, the Writer is NOT
// poisoned, the WAL still holds every frame, and a retry succeeds.
func TestTruncatePrefix_MarkerDirFsyncFailureLeavesWALIntact(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	for _, p := range []string{"fold-0", "fold-1"} {
		if err := w.Append([]byte(p)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	watermark := w.DurableOffset()

	marker := PrefixTruncatedMarkerPath(path)
	injErr := errors.New("injected marker dir fsync failure")
	var markerCalls int
	w.dirFsync = func(p string) error {
		if p == marker {
			markerCalls++
			return injErr
		}
		return parentDirFsync(p)
	}
	if _, err := w.TruncatePrefix(watermark); !errors.Is(err, injErr) {
		t.Fatalf("TruncatePrefix = %v, want the injected error %v", err, injErr)
	}
	if markerCalls != 1 {
		t.Fatalf("marker dir fsync reached the seam %d time(s), want 1", markerCalls)
	}
	if perr := w.Poisoned(); perr != nil {
		t.Fatalf("Writer poisoned by a pre-truncation marker failure: %v", perr)
	}
	if err := w.Append([]byte("after")); err != nil {
		t.Fatalf("Append after the marker failure: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync after the marker failure: %v", err)
	}
	got := collectPayloads(t, path)
	if len(got) != 3 || string(got[0]) != "fold-0" || string(got[1]) != "fold-1" || string(got[2]) != "after" {
		t.Fatalf("WAL after the marker failure = %q, want [fold-0 fold-1 after]", got)
	}

	// The fault clears; the retry writes the marker and truncates.
	w.dirFsync = parentDirFsync
	if n, err := w.TruncatePrefix(watermark); err != nil || n != watermark {
		t.Fatalf("retry TruncatePrefix = (%d, %v), want (%d, nil)", n, err, watermark)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("prefix marker after the retry: %v", err)
	}
	if got := collectPayloads(t, path); len(got) != 1 || string(got[0]) != "after" {
		t.Fatalf("WAL after the retry = %q, want [after]", got)
	}
}
