package snapshot

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestWriteReadCSR_Roundtrip(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	a.AddEdge("a", "b", 1)
	a.AddEdge("a", "c", 2)
	a.AddEdge("b", "c", 3)
	c := csr.BuildFromAdjList(a)

	var buf bytes.Buffer
	size, csum, err := WriteCSR(&buf, c)
	if err != nil {
		t.Fatalf("WriteCSR: %v", err)
	}
	if size <= 0 || csum == 0 {
		t.Fatalf("size=%d csum=%d", size, csum)
	}
	back, err := ReadCSR(&buf)
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	if uint64(len(back.Vertices)) != uint64(len(c.VerticesSlice())) {
		t.Fatalf("vertices length mismatch")
	}
	if len(back.Edges) != len(c.EdgesSlice()) {
		t.Fatalf("edges length mismatch")
	}
}

func TestWriteSnapshotCSR_AtomicPublish(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < 32; i++ {
		a.AddEdge("origin", string(rune('a'+i%26)), int64(i))
	}
	c := csr.BuildFromAdjList(a)

	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatalf("WriteSnapshotCSR: %v", err)
	}

	manifestPath := filepath.Join(dir, "manifest.json")
	m, err := ReadManifestFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadManifestFile: %v", err)
	}
	if m.Version != ManifestVersion {
		t.Fatalf("Version = %d, want %d", m.Version, ManifestVersion)
	}
	if len(m.Files) != 1 || m.Files[0].Name != CSRFile {
		t.Fatalf("Files = %v", m.Files)
	}

	// Verify the CSR file exists and matches the manifest entry.
	info, err := os.Stat(filepath.Join(dir, CSRFile))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != m.Files[0].Size {
		t.Fatalf("file size %d != manifest %d", info.Size(), m.Files[0].Size)
	}

	// The .tmp directory must be gone.
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp dir should be gone, stat err: %v", err)
	}
}

func TestManifest_UnsupportedVersion(t *testing.T) {
	t.Parallel()
	m := Manifest{Version: ManifestVersion + 10}
	var buf bytes.Buffer
	if err := WriteManifest(&buf, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if _, err := LoadManifest(&buf); err == nil {
		t.Fatalf("expected error for unsupported version")
	}
}
