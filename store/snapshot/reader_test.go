package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestOpen_Roundtrip(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("a", "c", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("b", "c", 3); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(loaded.CSR.Vertices) != len(c.VerticesSlice()) {
		t.Fatalf("vertices mismatch: %d vs %d", len(loaded.CSR.Vertices), len(c.VerticesSlice()))
	}
	if len(loaded.CSR.Edges) != len(c.EdgesSlice()) {
		t.Fatalf("edges mismatch")
	}
}

func TestOpen_CorruptedCSR(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the CSR file to break the CRC.
	path := filepath.Join(dir, CSRFile)
	data, err := os.ReadFile(path) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // t.TempDir-rooted path
		t.Fatal(err)
	}
	_, err = Open(dir)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("expected ErrCorrupted, got %v", err)
	}
}

func TestOpen_MissingCSR(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatal(err)
	}
	// Remove csr.bin; manifest still references it.
	if err := os.Remove(filepath.Join(dir, CSRFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatalf("expected open to fail with missing csr.bin")
	}
}

func TestOpen_UnsupportedVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := `{"version": 9999, "files": []}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !errors.Is(err, ErrManifestUnsupported) {
		t.Fatalf("expected ErrManifestUnsupported, got %v", err)
	}
}
