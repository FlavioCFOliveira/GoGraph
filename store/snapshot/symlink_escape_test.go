package snapshot

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// writeValidFullSnapshot builds a small string-keyed graph and writes a
// full snapshot to a fresh directory, returning that directory.
func writeValidFullSnapshot(t *testing.T) string {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("b", "c", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	return dir
}

// replaceWithSymlink removes the file at path and recreates it as a
// symlink pointing at target. The test is skipped when the platform
// cannot create symlinks (e.g. unprivileged Windows). Finding I4.
func replaceWithSymlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove %s: %v", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
}

// TestLoadSnapshotFull_RejectComponentSymlink replaces csr.bin in a valid
// snapshot directory with a symlink pointing at a secret file OUTSIDE the
// directory. LoadSnapshotFull must fail (O_NOFOLLOW) rather than reading
// the secret through the link. Finding I4.
func TestLoadSnapshotFull_RejectComponentSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW is a no-op on Windows; symlink escape is governed by separate OS controls")
	}
	t.Parallel()
	dir := writeValidFullSnapshot(t)

	// A secret file that lives outside the snapshot directory.
	secret := filepath.Join(t.TempDir(), "secret.bin")
	if err := os.WriteFile(secret, []byte("OUTSIDE-SECRET"), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}

	replaceWithSymlink(t, filepath.Join(dir, CSRFile), secret)

	if _, err := LoadSnapshotFull(dir); err == nil {
		t.Fatal("LoadSnapshotFull on a symlinked csr.bin = nil error, want rejection")
	}
}

// TestLoadSnapshotFull_RejectManifestSymlink does the same for
// manifest.json: a symlinked manifest must be rejected before any read.
// Finding I4.
func TestLoadSnapshotFull_RejectManifestSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW is a no-op on Windows; symlink escape is governed by separate OS controls")
	}
	t.Parallel()
	dir := writeValidFullSnapshot(t)

	secret := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(secret, []byte(`{"version":1,"files":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile secret manifest: %v", err)
	}

	replaceWithSymlink(t, filepath.Join(dir, "manifest.json"), secret)

	if _, err := LoadSnapshotFull(dir); err == nil {
		t.Fatal("LoadSnapshotFull on a symlinked manifest.json = nil error, want rejection")
	}
}

// TestLoadSnapshotFull_NormalSnapshotUnchanged confirms the O_NOFOLLOW
// guard does not perturb a legitimate snapshot whose component files are
// regular files. Finding I4.
func TestLoadSnapshotFull_NormalSnapshotUnchanged(t *testing.T) {
	t.Parallel()
	dir := writeValidFullSnapshot(t)
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull on a normal snapshot = %v, want nil", err)
	}
	if len(loaded.CSR.Vertices) == 0 {
		t.Fatal("loaded CSR has no vertices; snapshot did not round-trip")
	}
}
