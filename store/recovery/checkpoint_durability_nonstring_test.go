package recovery

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/checkpoint"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestCheckpointDurability_NonStringKeysNotLost is the F2 safety-guard
// regression test for the NO-CODEC fallback on non-string node keys
// (docs/acid-audit.md).
//
// WriteSnapshotFull (the no-codec writer used here, via a checkpointer
// constructed WITHOUT checkpoint.WithMapperCodec) emits mapper.bin only
// for string keys; for any other key type it writes a v2 snapshot
// without a mapper, which CANNOT reconstruct the graph on its own. If
// the checkpointer truncated the WAL after such a snapshot, the
// NodeID->key mapping — and therefore every edge/label/property keyed by
// it — would be permanently lost.
//
// runCheckpoint guards against this: it truncates the WAL only when the
// snapshot is self-sufficient (carries mapper.bin). In the no-codec
// mode exercised here the int64 snapshot is NOT self-sufficient, so the
// checkpointer SKIPS truncation, retains the WAL, and replays it at
// recovery. This test commits int64-keyed edges + a label + a property,
// checkpoints WITHOUT a codec, and asserts:
//
//   - the WAL was NOT truncated (the guard engaged): the file still holds
//     bytes and recovery replays them (WALOps > 0);
//   - every committed edge, label, and property survives recovery.
//
// The codec-aware counterpart — where WithMapperCodec makes the int64
// snapshot self-sufficient so the WAL IS truncated and recovery reads
// from the snapshot alone — is
// [TestCheckpointDurability_NonStringKeysCodecTruncates] (audit gap F3).
func TestCheckpointDurability_NonStringKeysNotLost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	opts := txn.Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[int64, int64](g, w, opts)

	tx := store.Begin()
	mustTx(t, tx.AddEdge(1, 2, 100))
	mustTx(t, tx.AddEdge(2, 3, 200))
	mustTx(t, tx.SetNodeLabel(1, "Root"))
	mustTx(t, tx.SetNodeProperty(2, "weight", lpg.Int64Value(42)))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Checkpoint. For int64 keys the snapshot is NOT self-sufficient, so
	// the checkpointer must SKIP truncation to avoid data loss.
	var mu sync.Mutex
	cp := checkpoint.New[int64, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()

	// The WAL must NOT have been truncated (guard engaged).
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("WAL was truncated after a non-self-sufficient snapshot — committed data would be lost")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Recover: the retained WAL replays on top of the v2 snapshot.
	res, err := Open[int64, int64](dir, Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if res.WALOps == 0 {
		t.Fatal("WALOps = 0 — the WAL was not replayed; non-string state would be lost")
	}
	rg := res.Graph
	if !rg.AdjList().HasEdge(1, 2) {
		t.Error("edge 1->2 lost across non-string checkpoint+recovery")
	}
	if !rg.AdjList().HasEdge(2, 3) {
		t.Error("edge 2->3 lost across non-string checkpoint+recovery")
	}
	if !rg.HasNodeLabel(1, "Root") {
		t.Error("node label Root lost across non-string checkpoint+recovery")
	}
	if v, ok := rg.GetNodeProperty(2, "weight"); !ok {
		t.Error("node property weight lost across non-string checkpoint+recovery")
	} else if got, _ := v.Int64(); got != 42 {
		t.Errorf("node property weight = %d, want 42", got)
	}
}
