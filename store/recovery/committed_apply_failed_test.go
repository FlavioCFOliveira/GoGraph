package recovery

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCommit_ApplyRefusalLeavesNothingDurable descends from the F5 regression
// test (docs/acid-audit.md). A typed store whose graph was built with a
// shard-capacity cap fails the in-memory apply with adjlist.ErrShardFull. Until
// the ACID audit of rmp #2965 that failure came AFTER the fsync, so Commit
// reported ErrCommittedNotApplied and recovery replayed the transaction. Commit
// now applies before it writes the WAL record, so the commit must:
//
//   - return adjlist.ErrShardFull, and NOT ErrCommittedNotApplied;
//   - leave nothing durable: recovery replays no op; and
//   - leave nothing visible: the apply is aborted as a whole, so none of the
//     transaction's edges is in the live graph.
//
// AddNode alone never overflows a shard (it only interns in the mapper);
// AddEdge allocates the source node's outgoing slot, so with
// MaxShardCapacity == 1 and edges from > 256 distinct sources the
// pigeonhole principle guarantees at least one shard overflows during the
// apply phase.
func TestCommit_ApplyRefusalLeavesNothingDurable(t *testing.T) {
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
	// The in-memory apply overflows a capped shard BEFORE anything is written to
	// the WAL, so Commit returns ErrShardFull and nothing is durable.
	err = tx.Commit()
	if err == nil {
		t.Fatal("Commit returned nil; expected adjlist.ErrShardFull (capped shard must overflow on apply)")
	}
	if errors.Is(err, txn.ErrCommittedNotApplied) {
		t.Fatalf("Commit error = %v; a refused apply must not be reported as durable", err)
	}
	if !errors.Is(err, adjlist.ErrShardFull) {
		t.Fatalf("Commit error = %v; want it to wrap adjlist.ErrShardFull", err)
	}
	visible := 0
	for i, s := range srcs {
		if g.AdjList().HasEdge(s, "d"+itoa(i)) {
			visible++
		}
	}
	if visible != 0 {
		t.Fatalf("ATOMICITY: %d/%d edges of the refused transaction are in the live graph", visible, n)
	}

	// The checkpointer is not involved; just close the WAL and recover.
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (the refused transaction must not be durable)", res.WALOps)
	}
	for i, s := range srcs {
		if res.Graph.AdjList().HasEdge(s, "d"+itoa(i)) {
			t.Fatalf("recovered graph holds edge %s->d%d of the refused transaction", s, i)
		}
	}
}
