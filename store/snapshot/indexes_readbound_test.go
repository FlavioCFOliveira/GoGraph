package snapshot

// indexes_readbound_test.go — regression test for the snapshot-index OOM
// blocker (2026-07-13 production-readiness audit, security finding F2).
//
// readIndexFile historically used an unbounded io.ReadAll, so a tampered store
// directory whose manifest declared a tiny size for an indexes/<name>.bin that
// is actually multi-gigabyte on disk would allocate the whole file into memory
// on open — an out-of-memory crash before any CRC check (CWE-770 / CWE-789).
// Every sibling verified reader bounds its read to the manifest size; this test
// pins that readIndexFile now does too.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadIndexFile_BoundedToDeclaredSize proves the read is capped at the
// manifest-declared size: a 4 MiB on-disk file read with a 16-byte declared
// size yields at most 16 bytes, never the whole file. Before the fix this
// returned all 4 MiB (and, at attack scale, OOM'd the process).
func TestReadIndexFile_BoundedToDeclaredSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")

	const onDisk = 4 << 20 // 4 MiB actually on disk
	if err := os.WriteFile(path, make([]byte, onDisk), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	const declared = 16 // manifest lies: claims only 16 bytes
	buf, err := readIndexFile(osBackend{}, path, declared)
	if err != nil {
		t.Fatalf("readIndexFile: %v", err)
	}
	if int64(len(buf)) > declared {
		t.Fatalf("readIndexFile read %d bytes, want <= %d (bounded to declared size); "+
			"an unbounded read would return the full %d-byte file", len(buf), declared, onDisk)
	}
}

// TestReadIndexFile_ReadsHonestFileInFull confirms the bound does not truncate a
// legitimate snapshot, where the manifest size equals the real file size.
func TestReadIndexFile_ReadsHonestFileInFull(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "honest.bin")

	payload := []byte("a legitimate serialized index payload")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	buf, err := readIndexFile(osBackend{}, path, int64(len(payload)))
	if err != nil {
		t.Fatalf("readIndexFile: %v", err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("readIndexFile returned %q, want %q", buf, payload)
	}
}

// TestReadIndexFile_NonPositiveSizeUnbounded confirms the documented fallback:
// a non-positive declared size preserves the legacy unbounded read (for
// size-less legacy manifests), reading the file in full.
func TestReadIndexFile_NonPositiveSizeUnbounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.bin")

	payload := []byte("legacy manifest carried no size")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	buf, err := readIndexFile(osBackend{}, path, 0)
	if err != nil {
		t.Fatalf("readIndexFile: %v", err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("readIndexFile(size=0) returned %q, want full file %q", buf, payload)
	}
}
