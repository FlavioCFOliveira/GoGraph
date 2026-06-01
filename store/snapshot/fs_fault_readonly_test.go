package snapshot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSnapshot_ReadOnly verifies that [WriteSnapshotFull] returns a
// non-nil error when the target parent directory is not writable.
//
// The test is skipped when running as root because root can write to
// a 0o555 directory, making the permission check unreliable.
func TestSnapshot_ReadOnly(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("running as root: read-only permission tests are unreliable")
	}

	// Build a small graph and a CSR to pass to WriteSnapshotFull.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AdjList().AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	// Create a read-only parent directory. WriteSnapshotFull must fail
	// because it cannot create the staging <dir>.tmp inside it.
	readonlyParent := t.TempDir()
	if err := os.Chmod(readonlyParent, 0o555); err != nil { //nolint:gosec // G302: test intentionally sets 0o555 to simulate a read-only directory
		t.Fatalf("Chmod 0o555: %v", err)
	}
	// Restore write permission so t.Cleanup can remove the temp dir.
	t.Cleanup(func() {
		_ = os.Chmod(readonlyParent, 0o755) //nolint:gosec // G302: restoring normal directory permissions for cleanup
	})

	snapDir := filepath.Join(readonlyParent, "snap")
	err := WriteSnapshotFull(snapDir, c, g)
	if err == nil {
		t.Fatal("WriteSnapshotFull into read-only parent: got nil error, want OS permission error")
	}
}
