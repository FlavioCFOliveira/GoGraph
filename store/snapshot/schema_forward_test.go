package snapshot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshot_SchemaVersionForwardRejected verifies that a snapshot
// whose manifest.json carries a schema_version beyond [ManifestVersion]
// is rejected by [LoadSnapshotFull] with [ErrManifestUnsupported].
//
// The test mutates a freshly written snapshot in a staging directory
// so the reference snapshot committed in testdata is never touched.
// The test uses [LoadSnapshotFull] (rather than the CSR-only [Open])
// because the forward-rejection contract must hold on the full
// snapshot load path.
//
// Coverage note: [TestCompat_FutureManifestRejected] already pins
// this for [Open]; [TestOpen_UnsupportedVersion] covers the raw
// manifest path. This test additionally exercises the
// [LoadSnapshotFull] entry point so every high-level snapshot reader
// is anchored.
func TestSnapshot_SchemaVersionForwardRejected(t *testing.T) {
	t.Parallel()

	// Write a valid snapshot to get well-formed segment files.
	goodDir := buildFullSnapshot(t)

	// Stage a copy in a fresh directory with the manifest patched to a
	// future version.
	dir := copySnapshotDir(t, goodDir)

	manifestPath := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(manifestPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile(manifest.json): %v", err)
	}

	// Decode, bump version to 999, and re-encode.
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	m.Version = 999
	patched, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(manifestPath, patched, 0o600); err != nil { //nolint:gosec // path under t.TempDir
		t.Fatalf("WriteFile(manifest.json): %v", err)
	}

	_, err = LoadSnapshotFull(dir)
	if !errors.Is(err, ErrManifestUnsupported) {
		t.Fatalf("LoadSnapshotFull(version=999) = %v, want ErrManifestUnsupported", err)
	}
}
