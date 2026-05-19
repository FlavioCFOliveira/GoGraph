package snapshot

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCompat_V1FixtureLoads asserts the frozen v1 snapshot fixture
// committed at store/snapshot/testdata/v1/sample loads under the
// current build with the expected CSR shape. Regenerate with:
//
//	go run ./cmd/fmtfixture -pkg snapshot
//
// Any mismatch flags an unintended on-disk-format change.
func TestCompat_V1FixtureLoads(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "v1", "sample")
	loaded, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	// The fixture is the frozen v1 sample committed at
	// testdata/v1/sample; its on-disk manifest carries version 1
	// verbatim. The build's ManifestVersion has since advanced to 2
	// (current shipped highest), and LoadManifest accepts both
	// transparently: a v1 directory loaded here returns Version=1.
	if loaded.Manifest.Version != 1 {
		t.Fatalf("Manifest.Version = %d, want 1 (v1 fixture)", loaded.Manifest.Version)
	}
	if len(loaded.Manifest.Files) != 1 || loaded.Manifest.Files[0].Name != CSRFile {
		t.Fatalf("Files = %v, want exactly one entry for %q", loaded.Manifest.Files, CSRFile)
	}
	// The fixture encodes a 3-edge graph (0->1, 0->2, 1->2). Vertex
	// count = 3 + 1 sentinel head; edge count = 3.
	if loaded.CSR.Edges == nil || len(loaded.CSR.Edges) != 3 {
		t.Fatalf("Edges length = %d, want 3", len(loaded.CSR.Edges))
	}
	if loaded.CSR.HasWeights == false {
		t.Fatalf("HasWeights = false, want true (int64 weights)")
	}
	if loaded.CSR.WeightSize != 8 {
		t.Fatalf("WeightSize = %d, want 8 (int64)", loaded.CSR.WeightSize)
	}
}

// TestCompat_FutureManifestRejected mutates the on-disk fixture in
// memory by bumping the manifest version past ManifestVersion and
// verifies the loader returns ErrManifestUnsupported instead of
// silently accepting the file.
func TestCompat_FutureManifestRejected(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "v1", "sample")
	manifestPath := filepath.Join(dir, "manifest.json")
	orig, err := os.ReadFile(manifestPath) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	// Stage a future-version manifest in a TempDir so we don't touch
	// the committed fixture.
	tmp := t.TempDir()
	// Replace the version "1" with a version above ManifestVersion.
	mutated := []byte(`{"version": 9999, "files": [{"name": "csr.bin", "size": 0, "crc32c": 0}]}`)
	if err := os.WriteFile(filepath.Join(tmp, "manifest.json"), mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	// Copy csr.bin so the loader can find both files in the temp dir.
	csrBytes, err := os.ReadFile(filepath.Join(dir, CSRFile)) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, CSRFile), csrBytes, 0o600); err != nil { //nolint:gosec // tmp is from t.TempDir
		t.Fatal(err)
	}
	if _, err := Open(tmp); !errors.Is(err, ErrManifestUnsupported) {
		t.Fatalf("future-version manifest = %v, want ErrManifestUnsupported", err)
	}
	// Sanity: original fixture still loads cleanly.
	if _, err := Open(dir); err != nil {
		t.Fatalf("original fixture should still load: %v", err)
	}
	// And we never mutated the file on disk.
	got, _ := os.ReadFile(manifestPath) //nolint:gosec // testdata
	if !bytes.Equal(got, orig) {
		t.Fatalf("on-disk manifest mutated by the test")
	}
}
