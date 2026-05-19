package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestOpen_MissingManifest(t *testing.T) {
	t.Parallel()
	// Directory exists but contains no manifest.json.
	dir := t.TempDir()
	_, err := Open(dir)
	if err == nil {
		t.Fatalf("Open without manifest should error")
	}
	if !os.IsNotExist(err) {
		// Acceptable that the underlying error is a wrapped "not exist".
		var pe *os.PathError
		if !errors.As(err, &pe) {
			t.Fatalf("Open without manifest = %v, want a path/not-exist error", err)
		}
	}
}

func TestOpen_CorruptedManifestJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !errors.Is(err, ErrManifestCorrupted) {
		t.Fatalf("Open corrupted JSON = %v, want ErrManifestCorrupted", err)
	}
}

func TestOpen_EmptyManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Empty manifest decodes (version=0) but has no csr.bin entry, so
	// Open should report a corrupted directory.
	_, err := Open(dir)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Open empty manifest = %v, want ErrCorrupted", err)
	}
}

func TestOpen_TruncatedCSRFile(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	a.AddEdge("a", "b", 1)
	a.AddEdge("a", "c", 2)
	c := csr.BuildFromAdjList(a)
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatal(err)
	}
	// Truncate csr.bin so the reader fails with a short-read /
	// CRC-mismatch / corrupted-directory error (all are surfaced as
	// ErrCorrupted by Open).
	csrPath := filepath.Join(dir, CSRFile)
	if err := os.Truncate(csrPath, 4); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Open truncated csr.bin = %v, want ErrCorrupted", err)
	}
}

func TestReadManifestFile_PropagatesOSError(t *testing.T) {
	t.Parallel()
	bogus := filepath.Join(t.TempDir(), "definitely-missing", "manifest.json")
	_, err := ReadManifestFile(bogus)
	if err == nil {
		t.Fatalf("ReadManifestFile on missing file should error")
	}
}
