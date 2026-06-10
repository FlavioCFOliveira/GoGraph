package recovery

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRecovery_PromotesSnapshotBackupAfterInterruptedPublish is the gate
// test for the crash-atomic snapshot publish (audit task #1331).
//
// The snapshot writers used to publish with RemoveAll(live) followed by
// Rename(staging, live). A crash between those two calls left NO live
// snapshot directory while the checkpointer had ALREADY truncated the
// WAL at the previous checkpoint — recovery then found no manifest,
// rebuilt an empty graph, and returned nil: total silent loss of every
// checkpointed transaction. The fix archives the live snapshot to
// snapshot.bak before the publish rename, and recovery promotes a
// stranded backup back to the live name.
//
// The test builds the exact crash-window state by hand:
//
//  1. commit "pre" data and run a real checkpoint (self-sufficient
//     snapshot written, WAL truncated — the pre data now exists ONLY in
//     the snapshot);
//  2. commit "post" data, which lands in the WAL only;
//  3. simulate a crash between the archive rename and the publish rename
//     of a NEXT checkpoint: the live snapshot sits at snapshot.bak, the
//     live name is absent, and a stale snapshot.tmp staging directory is
//     stranded;
//  4. recovery must promote the backup, load it, replay the WAL, and
//     surface BOTH the pre and the post data with no error.
//
// Before the fix recovery has no fallback: SnapshotHit is false and the
// pre nodes are gone, so the test fails — the regression gate.
func TestRecovery_PromotesSnapshotBackupAfterInterruptedPublish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	snapDir := filepath.Join(dir, "snapshot")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// Phase 1: "pre" data, folded into the checkpoint below.
	preNodes := []string{"pre0", "pre1", "pre2", "pre3"}
	tx := store.Begin()
	for i, n := range preNodes {
		mustTx(t, tx.AddNode(n))
		mustTx(t, tx.SetNodeLabel(n, "Pre"))
		mustTx(t, tx.AddEdge(n, preNodes[(i+1)%len(preNodes)], int64(i)))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(pre): %v", err)
	}

	// Checkpoint: writes a self-sufficient snapshot, then truncates the
	// WAL. After this the pre data exists ONLY in snapshot/.
	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()

	// Phase 2: "post" data — WAL only.
	postNodes := []string{"post0", "post1"}
	txPost := store.Begin()
	for _, n := range postNodes {
		mustTx(t, txPost.AddNode(n))
		mustTx(t, txPost.SetNodeLabel(n, "Post"))
	}
	mustTx(t, txPost.AddEdge("pre0", "post0", 99))
	if err := txPost.Commit(); err != nil {
		t.Fatalf("Commit(post): %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Phase 3: simulate the crash window of a next checkpoint's publish —
	// the archive rename succeeded (live snapshot moved to snapshot.bak)
	// but the publish rename never ran, leaving a stale staging directory.
	snapBak := snapDir + ".bak"
	if err := os.Rename(snapDir, snapBak); err != nil {
		t.Fatalf("simulate crash: rename live snapshot to backup: %v", err)
	}
	staleTmp := snapDir + ".tmp"
	if err := os.MkdirAll(staleTmp, 0o750); err != nil {
		t.Fatalf("simulate crash: create stale staging dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staleTmp, "junk.bin"), []byte("stranded"), 0o600); err != nil {
		t.Fatalf("simulate crash: write stale staging file: %v", err)
	}

	// Phase 4: recovery must promote the backup and lose nothing.
	res, err := Open[string, int64](dir, OptionsFromTxn(opts))
	if err != nil {
		t.Fatalf("recovery.Open after interrupted publish: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true (backup snapshot must be promoted)")
	}

	// Every checkpointed pre node survives with its label and ring edge.
	for i, n := range preNodes {
		if !res.Graph.HasNodeLabel(n, "Pre") {
			t.Errorf("pre node %s missing label Pre after recovery", n)
		}
		next := preNodes[(i+1)%len(preNodes)]
		if !res.Graph.AdjList().HasEdge(n, next) {
			t.Errorf("pre edge %s->%s missing after recovery", n, next)
		}
	}
	// Every WAL-only post node survives too.
	for _, n := range postNodes {
		if !res.Graph.HasNodeLabel(n, "Post") {
			t.Errorf("post node %s missing label Post after recovery", n)
		}
	}
	if !res.Graph.AdjList().HasEdge("pre0", "post0") {
		t.Error("cross-boundary edge pre0->post0 missing after recovery")
	}

	// The repair must leave the directory in the published shape: live
	// snapshot present, backup consumed, stale staging removed.
	if _, err := os.Stat(filepath.Join(snapDir, "manifest.json")); err != nil {
		t.Errorf("live snapshot manifest missing after promotion: %v", err)
	}
	if _, err := os.Stat(snapBak); !os.IsNotExist(err) {
		t.Errorf("snapshot backup still present after promotion: stat err = %v", err)
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Errorf("stale staging dir still present after recovery: stat err = %v", err)
	}
}

// TestRecovery_LiveSnapshotWinsOverStaleBackup pins the non-promotion
// arm of the interrupted-publish repair: when the live snapshot is
// intact, a leftover snapshot.bak (crash after the publish rename but
// before the backup cleanup) must be ignored — recovery loads the LIVE
// snapshot, never the stale backup.
func TestRecovery_LiveSnapshotWinsOverStaleBackup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	snapDir := filepath.Join(dir, "snapshot")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// Checkpoint 1: only "old" committed.
	txOld := store.Begin()
	mustTx(t, txOld.AddNode("old"))
	mustTx(t, txOld.SetNodeLabel("old", "Old"))
	if err := txOld.Commit(); err != nil {
		t.Fatalf("Commit(old): %v", err)
	}
	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger(1): %v", err)
	}

	// Preserve checkpoint 1's snapshot as the stale backup-to-be.
	stale := filepath.Join(dir, "stale-copy")
	if err := os.Rename(snapDir, stale); err != nil {
		t.Fatalf("preserve checkpoint-1 snapshot: %v", err)
	}

	// Checkpoint 2: "new" added; its snapshot becomes the live one.
	txNew := store.Begin()
	mustTx(t, txNew.AddNode("new"))
	mustTx(t, txNew.SetNodeLabel("new", "New"))
	if err := txNew.Commit(); err != nil {
		t.Fatalf("Commit(new): %v", err)
	}
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger(2): %v", err)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Crash-after-publish state: live snapshot intact, stale backup left.
	if err := os.Rename(stale, snapDir+".bak"); err != nil {
		t.Fatalf("install stale backup: %v", err)
	}

	res, err := Open[string, int64](dir, OptionsFromTxn(opts))
	if err != nil {
		t.Fatalf("recovery.Open with live snapshot + stale backup: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}
	// The LIVE snapshot (checkpoint 2) carries both nodes; loading the
	// stale backup instead would lose "new".
	if !res.Graph.HasNodeLabel("old", "Old") {
		t.Error("node old missing after recovery")
	}
	if !res.Graph.HasNodeLabel("new", "New") {
		t.Error("node new missing after recovery (stale backup loaded instead of live snapshot?)")
	}
}
