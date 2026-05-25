package recovery

import (
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestRecovery_UnreadableWAL verifies that a WAL file whose
// permissions deny read access causes [OpenString] to return a
// non-nil error rather than panicking or silently ignoring the
// permission failure.
//
// The test differs from [TestOpenString_NonExistentWALPathBubblesError]
// which revokes access on the *parent directory* of the WAL. Here
// we create a valid snapshot so recovery proceeds past the snapshot
// phase, then place a readable WAL, and then revoke read permission
// on the WAL file itself. This exercises the distinct code branch
// where the snapshot load succeeds but the subsequent WAL open fails.
//
// Skipped when running as root because root ignores file permissions.
func TestRecovery_UnreadableWAL(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("running as root: file permission checks are ineffective")
	}

	dir := t.TempDir()

	// 1. Build a minimal snapshot so recovery has something to restore.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	adj := g.AdjList()
	if err := adj.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(adj)
	if err := snapshot.WriteSnapshotFull(filepath.Join(dir, "snapshot"), c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// 2. Write a valid single-frame WAL.
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)
	tx := s.Begin()
	if err := tx.SetNodeLabel("a", "Node"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	// 3. Revoke all permissions on the WAL file.
	walPath := filepath.Join(dir, "wal")
	if err := os.Chmod(walPath, 0o000); err != nil {
		t.Fatalf("Chmod(wal, 0): %v", err)
	}
	// Always restore permissions so t.TempDir cleanup can remove the file.
	defer func() { _ = os.Chmod(walPath, 0o600) }() //nolint:gosec // test cleanup

	// 4. Recovery must fail with a non-nil error.
	_, err = OpenString(dir)
	if err == nil {
		t.Fatal("OpenString with unreadable WAL: want error, got nil")
	}
}
