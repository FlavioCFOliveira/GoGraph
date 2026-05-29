package wal

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestParentDirFsync_ExistingDir confirms parentDirFsync runs without
// error against a real directory entry. On Windows the helper is a no-op
// by design (see parent_fsync_other.go) and still returns nil.
func TestParentDirFsync_ExistingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	child := filepath.Join(dir, "wal") // need not exist
	if err := parentDirFsync(child); err != nil {
		t.Fatalf("parentDirFsync(existing dir) returned %v; want nil", err)
	}
}

// TestParentDirFsync_MissingParent confirms parentDirFsync surfaces the
// underlying os.Open error when the parent directory is absent. The
// helper must not silently swallow the error: Open treats it as a hard
// failure so a WAL whose directory entry cannot be made durable is never
// returned to the caller.
//
// On Windows the helper is a no-op and always returns nil, so the
// assertion is skipped there.
func TestParentDirFsync_MissingParent(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("parentDirFsync is a no-op on Windows; missing-parent error not surfaced")
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist", "wal")
	if err := parentDirFsync(missing); err == nil {
		t.Fatalf("parentDirFsync(missing parent) returned nil; want error")
	}
}

// TestOpen_FreshCreateFsyncsParentAndReopens is the F4 end-to-end guard
// (docs/acid-audit.md). A fresh Open must succeed (the parent-directory
// fsync that makes the new file's directory entry durable runs inline and
// must not error on a normal directory), a committed frame must survive a
// Close, and a reopen of the now-existing file must also succeed (the
// parent fsync is skipped on the non-create path). The fsync itself is
// invisible from user space — as the mirrored snapshot test acknowledges —
// so the strongest portable assertion is that the create path executes
// without error and the data round-trips.
func TestOpen_FreshCreateFsyncsParentAndReopens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")

	// Fresh create: exercises the parent-dir fsync branch.
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open(fresh): %v", err)
	}
	if err := w.Append([]byte("frame-0")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("WAL file missing after fresh Open+Sync+Close: %v", err)
	}

	// Reopen the existing file: exercises the non-create path (no parent
	// fsync) and must append after the existing frame.
	w2, err := Open(path)
	if err != nil {
		t.Fatalf("Open(reopen): %v", err)
	}
	defer func() { _ = w2.Close() }()
	if err := w2.Append([]byte("frame-1")); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if err := w2.Sync(); err != nil {
		t.Fatalf("Sync after reopen: %v", err)
	}

	// Both frames must be readable back.
	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	var n int
	for range r.Frames() {
		n++
	}
	if err := r.TailError(); err != nil {
		t.Fatalf("TailError: %v", err)
	}
	if n != 2 {
		t.Fatalf("read %d frames, want 2", n)
	}
}
