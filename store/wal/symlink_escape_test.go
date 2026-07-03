package wal

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// replaceWithSymlink creates path as a symlink pointing at target, skipping
// the test when the platform cannot create symlinks (e.g. unprivileged
// Windows). Mirrors store/snapshot/symlink_escape_test.go.
func replaceWithSymlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
}

// writeVictim writes a sentinel file outside the WAL directory whose content
// must survive every symlink-escape attempt below.
func writeVictim(t *testing.T) (path string, want []byte) {
	t.Helper()
	want = []byte("OUTSIDE-VICTIM-CONTENT")
	path = filepath.Join(t.TempDir(), "victim.bin")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}
	return path, want
}

// assertUntouched fails if the victim file's content changed (an append would
// grow it; an O_TRUNC would empty it).
func assertUntouched(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile victim: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("victim file was modified through the symlink: got %q, want %q", got, want)
	}
}

func skipIfNoNoFollow(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW is a no-op on Windows; symlink escape is governed by separate OS controls")
	}
}

// TestOpen_RejectSymlinkedWAL confirms wal.Open refuses a WAL data-file path
// that is a symlink (O_NOFOLLOW → ELOOP) rather than appending the mutation
// stream to the linked victim file. Security finding #1843 (CWE-59).
func TestOpen_RejectSymlinkedWAL(t *testing.T) {
	skipIfNoNoFollow(t)
	t.Parallel()
	victim, want := writeVictim(t)
	walPath := filepath.Join(t.TempDir(), "wal")
	replaceWithSymlink(t, walPath, victim)

	w, err := Open(walPath)
	if err == nil {
		_ = w.Close()
		t.Fatal("Open on a symlinked WAL path = nil error, want rejection")
	}
	assertUntouched(t, victim, want)
}

// TestOpenReader_RejectSymlinkedWAL confirms the recovery read path also
// refuses to follow a symlinked WAL path. #1843 (CWE-59).
func TestOpenReader_RejectSymlinkedWAL(t *testing.T) {
	skipIfNoNoFollow(t)
	t.Parallel()
	victim, want := writeVictim(t)
	walPath := filepath.Join(t.TempDir(), "wal")
	replaceWithSymlink(t, walPath, victim)

	r, err := OpenReader(walPath)
	if err == nil {
		_ = r.Close()
		t.Fatal("OpenReader on a symlinked WAL path = nil error, want rejection")
	}
	assertUntouched(t, victim, want)
}

// TestOSWALFS_OpenFile_RejectSymlinkTruncate confirms the production WAL
// filesystem backend refuses to open a symlinked path with O_TRUNC — the
// suffix-temp write in Writer.writeSuffixTmp and the post-rename reopen both
// route through this call, so the arbitrary-file truncate/append primitive is
// closed. #1843 (CWE-59).
func TestOSWALFS_OpenFile_RejectSymlinkTruncate(t *testing.T) {
	skipIfNoNoFollow(t)
	t.Parallel()
	victim, want := writeVictim(t)
	tmpPath := filepath.Join(t.TempDir(), "wal.tmp")
	replaceWithSymlink(t, tmpPath, victim)

	f, err := osWALFS{}.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err == nil {
		_ = f.Close()
		t.Fatal("osWALFS.OpenFile(O_TRUNC) on a symlinked path = nil error, want rejection")
	}
	assertUntouched(t, victim, want)
}

// TestOpen_NormalWALUnchanged confirms the O_NOFOLLOW guard is
// behaviour-preserving: a regular (non-symlink) WAL path opens, closes, and
// reopens for reading without error. #1843.
func TestOpen_NormalWALUnchanged(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")

	w, err := Open(walPath)
	if err != nil {
		t.Fatalf("Open on a regular WAL path = %v, want nil", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The created path must be a regular file, not a symlink.
	fi, err := os.Lstat(walPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("created WAL path is a symlink; want a regular file")
	}
	r, err := OpenReader(walPath)
	if err != nil {
		t.Fatalf("OpenReader on a regular WAL = %v, want nil", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("reader Close: %v", err)
	}
}
