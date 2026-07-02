package snapshot

// csr_lying_manifest_size_test.go — security engagement 2026-07-02 R2 (#1850).
//
// Regression for finding B1 (CWE-789 / CWE-770). The csr.bin reader's precise
// allocation bound was derived from the manifest FileEntry.Size — an
// attacker-controlled JSON field. The sibling csr_manifest_bound_test.go only
// exercises a TRUTHFUL manifest size (== the real file length); it never
// covered a manifest that LIES about the size. A poisoned store directory
// (adopted backup / shared filesystem) whose manifest declares size:0 collapsed
// readCSRLimited's precise bound to the 128 GiB backstop (maxCSRCount), so a
// tiny csr.bin declaring a huge nVertices drove a multi-GiB (up to ~128 GiB)
// eager make() and an OOM fatal crash on recovery — before any CRC check.
//
// The fix (safeCSRAllocBound) bounds the allocation by min(manifestSize,
// REAL on-disk size): a legitimate snapshot (manifest == real) is unchanged,
// while a lying manifest can only make the bound tighter, never exceed the real
// bytes. These tests prove both the size:0 and the inflated-size vectors are
// rejected with the typed corruption sentinel and WITHOUT the giant allocation.
// nV is 1<<28 (a 2 GiB []uint64 if the guard failed) — large enough to trip the
// 64 MiB assertBoundedAlloc budget, small enough not to endanger the test host.

import (
	"bytes"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLyingSnapshot lays out a snapshot directory whose csr.bin holds only an
// 18-byte header and whose manifest FileEntry.Size is manifestSize — a value
// that need not match the real file length. The manifest CRC32C is computed
// over the real file bytes, so the only thing standing between Open /
// LoadSnapshotFull and a giant allocation is the allocation bound: with the fix
// it derives from the REAL size, not the lie.
func writeLyingSnapshot(t *testing.T, hdr []byte, manifestSize int64) string {
	t.Helper()
	dir := t.TempDir()
	csrPath := filepath.Join(dir, CSRFile)
	if err := os.WriteFile(csrPath, hdr, 0o600); err != nil {
		t.Fatalf("WriteFile(csr.bin): %v", err)
	}
	crc := crc32.Checksum(hdr, castagnoli)
	m := Manifest{
		Version:   manifestVersionLegacy,
		CreatedAt: time.Now().UTC(),
		Order:     0,
		Size:      0,
		Files: []FileEntry{
			// Size is the ATTACKER-CONTROLLED lie; the real file is len(hdr).
			{Name: CSRFile, Size: manifestSize, CRC32C: crc},
		},
	}
	var buf bytes.Buffer
	if err := WriteManifest(&buf, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile(manifest.json): %v", err)
	}
	return dir
}

// TestOpen_LyingManifestSizeZeroRejectsHugeVertexCount is the B1 PoC on the
// legacy Open path: manifest size:0 forced the old reader to the 128 GiB
// backstop, so nV=1<<28 passed and a ~2 GiB make() ran. With the real-size
// bound (18 bytes) it must be rejected before allocating.
func TestOpen_LyingManifestSizeZeroRejectsHugeVertexCount(t *testing.T) {
	// Not t.Parallel: assertBoundedAlloc forces a process-wide runtime.GC and
	// reads global MemStats — see TestOpen_ManifestSizeBoundRejectsHugeVertexCount.
	hdr := csrHeader(1<<28, 0, 0, 0)
	dir := writeLyingSnapshot(t, hdr, 0)
	assertBoundedAlloc(t, func() {
		_, err := Open(dir)
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("Open = %v, want ErrCSRCorrupted (size:0 must not reach the backstop)", err)
		}
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("Open = %v, want it wrapped under ErrCorrupted", err)
		}
	})
}

// TestLoadSnapshotFull_LyingManifestSizeZeroRejectsHugeVertexCount is the
// recovery-path twin (store/recovery flows through readVerifiedCSR).
func TestLoadSnapshotFull_LyingManifestSizeZeroRejectsHugeVertexCount(t *testing.T) {
	// Not t.Parallel — see assertBoundedAlloc.
	hdr := csrHeader(1<<28, 0, 0, 0)
	dir := writeLyingSnapshot(t, hdr, 0)
	assertBoundedAlloc(t, func() {
		_, err := LoadSnapshotFull(dir)
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("LoadSnapshotFull = %v, want ErrCSRCorrupted (size:0 must not reach the backstop)", err)
		}
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("LoadSnapshotFull = %v, want it wrapped under ErrCorrupted", err)
		}
	})
}

// TestOpen_InflatedManifestSizeRejectsHugeEdgeCount covers the dual lie: an
// INFLATED manifest size (1<<40) would set an enormous precise bound, letting
// nE=1<<28 pass. min(manifestSize, realSize) clamps it back to the 18-byte
// reality, so the edge count is rejected before allocation.
func TestOpen_InflatedManifestSizeRejectsHugeEdgeCount(t *testing.T) {
	// Not t.Parallel — see assertBoundedAlloc.
	hdr := csrHeader(0, 1<<28, 0, 0)
	dir := writeLyingSnapshot(t, hdr, 1<<40)
	assertBoundedAlloc(t, func() {
		_, err := Open(dir)
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("Open = %v, want ErrCSRCorrupted (inflated manifest size must be clamped to real size)", err)
		}
	})
}

// TestLoadSnapshotFull_InflatedManifestSizeRejectsHugeVertexCount is the
// recovery-path twin of the inflated-size vector.
func TestLoadSnapshotFull_InflatedManifestSizeRejectsHugeVertexCount(t *testing.T) {
	// Not t.Parallel — see assertBoundedAlloc.
	hdr := csrHeader(1<<28, 0, 0, 0)
	dir := writeLyingSnapshot(t, hdr, 1<<40)
	assertBoundedAlloc(t, func() {
		_, err := LoadSnapshotFull(dir)
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("LoadSnapshotFull = %v, want ErrCSRCorrupted (inflated manifest size must be clamped to real size)", err)
		}
	})
}
