package snapshot

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSnapshotFiles_Mode0600 writes a full snapshot — CSR, manifest,
// labels, properties, mapper, and one persisted index — then walks the
// snapshot directory and asserts every regular file was created with
// mode 0o600. Directories are exempt (they remain 0o750). The Unix
// permission check is skipped on Windows, whose ACL model does not map
// onto POSIX mode bits. Finding L2.
func TestSnapshotFiles_Mode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not apply on Windows")
	}
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	mgr := index.NewManager()
	g.SetIndexManager(mgr)
	bt := btree.New[string]()
	if err := mgr.CreateIndex("btree.age", bt); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("b", "c", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	bt.Insert("k1", 0)

	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	fileCount := 0
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("file %s has mode %o, want 600", path, perm)
		}
		fileCount++
		return nil
	})
	if walkErr != nil {
		t.Fatalf("WalkDir: %v", walkErr)
	}
	if fileCount == 0 {
		t.Fatal("no files found under the snapshot dir; nothing was asserted")
	}
}

// TestSnapshotFiles_RoundTripAfterTightening confirms the 0o600 files are
// still fully readable by the loader — the permission change does not
// break the write+read round-trip. Finding L2.
func TestSnapshotFiles_RoundTripAfterTightening(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("x", "y", 7); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if len(loaded.CSR.Vertices) == 0 {
		t.Fatal("loaded CSR has no vertices; round-trip failed after permission tightening")
	}
}

// TestCreateSnapshotFile_Mode0600 exercises the helper directly: a file it
// creates must have mode 0o600 regardless of the process umask. Finding L2.
func TestCreateSnapshotFile_Mode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not apply on Windows")
	}
	t.Parallel()
	path := filepath.Join(t.TempDir(), "component.bin")
	f, err := createSnapshotFile(path)
	if err != nil {
		t.Fatalf("createSnapshotFile: %v", err)
	}
	if _, werr := f.WriteString("payload"); werr != nil {
		t.Fatalf("WriteString: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("createSnapshotFile mode = %o, want 600", perm)
	}
}
