package recovery

import (
	"errors"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestCommit_DurableButApplyFails_ReconciledByRecovery is the F5
// regression test (docs/acid-audit.md). A typed store whose graph was
// built with a shard-capacity cap can fail the in-memory apply AFTER the
// transaction is already durable (op frames + OpCommit marker fsynced). The
// commit must then:
//
//   - report ErrCommittedNotApplied (wrapping adjlist.ErrShardFull), so the
//     durable commit is never a silent, ambiguous failure; and
//   - remain fully recoverable: recovery rebuilds the graph WITHOUT a cap,
//     so it replays the whole transaction atomically and all nodes appear.
//
// AddNode alone never overflows a shard (it only interns in the mapper);
// AddEdge allocates the source node's outgoing slot, so with
// MaxShardCapacity == 1 and edges from > 256 distinct sources the
// pigeonhole principle guarantees at least one shard overflows during the
// apply phase.
func TestCommit_DurableButApplyFails_ReconciledByRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	const n = 400 // distinct edge sources > 256 shards => cap=1 overflows on apply
	// Cap each shard at a single node slot so the apply phase overflows.
	g := lpg.New[string, int64](adjlist.Config{Directed: true, MaxShardCapacity: 1})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	tx := store.Begin()
	srcs := make([]string, n)
	for i := range srcs {
		srcs[i] = "s" + itoa(i)
		if err := tx.AddEdge(srcs[i], "d"+itoa(i), int64(i+1)); err != nil {
			t.Fatalf("AddEdge(%s): %v", srcs[i], err)
		}
	}
	// The commit is durable (WAL synced) but the in-memory apply overflows a
	// capped shard, so Commit reports ErrCommittedNotApplied wrapping
	// ErrShardFull — never a plain error, and never a silent success.
	err = tx.Commit()
	if err == nil {
		t.Fatal("Commit returned nil; expected ErrCommittedNotApplied (capped shard must overflow on apply)")
	}
	if !errors.Is(err, txn.ErrCommittedNotApplied) {
		t.Fatalf("Commit error = %v; want errors.Is(..., ErrCommittedNotApplied)", err)
	}
	if !errors.Is(err, adjlist.ErrShardFull) {
		t.Fatalf("Commit error = %v; want it to wrap adjlist.ErrShardFull", err)
	}

	// The checkpointer is not involved; just close the WAL and recover.
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Recovery rebuilds the graph WITHOUT a shard cap, so the durable
	// transaction replays in full and atomically: all n nodes are present.
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if res.WALOps != n {
		t.Fatalf("WALOps = %d, want %d (whole durable transaction replays)", res.WALOps, n)
	}
	missing := 0
	for i, s := range srcs {
		if !res.Graph.AdjList().HasEdge(s, "d"+itoa(i)) {
			missing++
		}
	}
	if missing != 0 {
		t.Fatalf("%d/%d edges missing after recovery — durable transaction not fully reconciled", missing, n)
	}
}
