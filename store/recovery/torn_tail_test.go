package recovery

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRecovery_TornTailDropsLastOp_WithSnapshot writes K=10 v2 WAL
// frames (SetNodeLabel) on top of an existing snapshot, then truncates
// the file to a position INSIDE the last frame (mid-payload), and
// asserts:
//
//  1. [Open] returns no error (torn tail is tolerated).
//  2. The recovered graph reflects exactly the first K-1 frames —
//     the label from the torn last frame must NOT be present.
//
// The snapshot is present in the directory so the test exercises the
// "snapshot + partial WAL" recovery path rather than the WAL-only
// path covered by [TestRecovery_TornTailDropsLastOp] and
// [TestCrashInjection_TruncateEveryFrameBoundary].
//
//nolint:gocyclo // test: write K frames + snapshot + truncate + assert K-1 state
func TestRecovery_TornTailDropsLastOp_WithSnapshot(t *testing.T) {
	t.Parallel()

	const K = 10

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// Phase 1: build a base graph and snapshot it. The snapshot node
	// "base" must survive regardless of WAL truncation.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// Commit one pre-snapshot node so the snapshot is non-trivial.
	tx := s.Begin()
	if err := tx.AddNode("base"); err != nil {
		t.Fatalf("AddNode(base): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	c := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(filepath.Join(dir, "snapshot"), c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Phase 2: append K=10 post-snapshot SetNodeLabel frames.
	// Node names are "n0".."n9"; each commit is exactly one op so
	// WALOps == K in the ideal case.
	nodes := make([]string, K)
	for i := range nodes {
		nodes[i] = "n" + itoa(i)
	}
	for _, name := range nodes {
		tx := s.Begin()
		if err := tx.SetNodeLabel(name, "Label"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	// Phase 3: locate the last frame boundary and truncate inside it.
	boundaries := frameBoundaries(t, walPath)
	if len(boundaries) < K+1 {
		// boundaries includes offset=0 plus one entry per frame end
		t.Fatalf("expected at least %d boundaries, got %d", K+1, len(boundaries))
	}
	// The last complete frame ends at boundaries[len-1]. The
	// second-to-last complete frame ends at boundaries[len-2].
	// Truncate to the midpoint between them to land inside the last
	// frame's body.
	secondLast := boundaries[len(boundaries)-2]
	last := boundaries[len(boundaries)-1]
	tearOff := secondLast + (last-secondLast)/2
	if tearOff == secondLast {
		tearOff = secondLast + 1 // ensure we are inside the frame
	}
	rawWAL, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile(wal): %v", err)
	}
	if err := os.WriteFile(walPath, rawWAL[:tearOff], 0o600); err != nil { //nolint:gosec // path under t.TempDir
		t.Fatalf("WriteFile(truncated wal): %v", err)
	}

	// Phase 4: recover and assert.
	res, err := Open[string, int64](dir, Options[string, int64](opts))
	if err != nil {
		t.Fatalf("Open with torn tail: %v", err)
	}

	// The snapshot node must be present regardless.
	if _, ok := res.Graph.AdjList().Mapper().Lookup("base"); !ok {
		t.Fatal("snapshot node 'base' missing after recovery")
	}

	// The last node's label must NOT be applied (its frame was torn).
	lastName := nodes[K-1]
	if res.Graph.HasNodeLabel(lastName, "Label") {
		t.Fatalf("torn last frame must not apply: node %q has label", lastName)
	}

	// The first K-1 frames must all be present. Each SetNodeLabel
	// interns the node implicitly, so we check HasNodeLabel.
	for _, name := range nodes[:K-1] {
		if !res.Graph.HasNodeLabel(name, "Label") {
			t.Errorf("node %q should have label Label (frame %d was complete)", name, K-1)
		}
	}

	_ = binary.LittleEndian // imported for frameBoundaries helper
}
