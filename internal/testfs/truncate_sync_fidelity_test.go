package testfs_test

// truncate_sync_fidelity_test.go — regression gate for #1808 (sprint 253): a
// firing sync fault used to re-extend a file that had been Truncated below the
// recorded durable size, fabricating zero-filled "durable" bytes that were
// never written. Truncate now clamps syncedSize so the fault can only shrink.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
)

func TestFailSync_AfterTruncateBelowSyncedSize_1808(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.bin")
	ff, err := testfs.New(path, testfs.Faults{FailSyncAfter: 1})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	defer ff.Close()

	if _, err := ff.Write(make([]byte, 200)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := ff.Sync(); err != nil { // 1st Sync succeeds; syncedSize = 200
		t.Fatalf("first Sync: %v", err)
	}
	if err := ff.Truncate(50); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// 2nd Sync fires the fault; it must NOT re-extend the file past 50.
	if err := ff.Sync(); !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("second Sync: want ErrSyncFailed, got %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() > 50 {
		t.Fatalf("sync fault re-extended a truncated file to %d bytes (fabricated %d durable bytes); want <= 50",
			fi.Size(), fi.Size()-50)
	}
}

// TestFailSync_SuffixOnlyModel_1809 pins the documented suffix-only fault model:
// an in-prefix overwrite (seek back, rewrite within the synced prefix) is
// INTENTIONALLY retained after a firing sync fault (a real fsync failure would
// also lose it, but the model only discards the suffix). Documented in
// failSyncLocked; this gate ensures the limitation is a deliberate, observed
// contract rather than a silent surprise.
func TestFailSync_SuffixOnlyModel_1809(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.bin")
	ff, err := testfs.New(path, testfs.Faults{FailSyncAfter: 1})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	defer ff.Close()
	if _, err := ff.Write([]byte("AAAAAAAAAA")); err != nil { // 10 bytes
		t.Fatalf("write: %v", err)
	}
	if err := ff.Sync(); err != nil { // syncedSize = 10
		t.Fatalf("first Sync: %v", err)
	}
	if _, err := ff.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, err := ff.Write([]byte("BB")); err != nil { // overwrite within prefix
		t.Fatalf("overwrite: %v", err)
	}
	if err := ff.Sync(); !errors.Is(err, testfs.ErrSyncFailed) { // fault fires
		t.Fatalf("second Sync: want ErrSyncFailed, got %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Suffix-only model: size stays at the synced 10; the in-prefix overwrite
	// is retained (NOT reverted). This pins the documented limitation.
	if fi.Size() != 10 {
		t.Fatalf("size = %d, want 10 (suffix-only fault model)", fi.Size())
	}
}
