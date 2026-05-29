package checkpoint_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/checkpoint"
	"gograph/store/recovery"
	"gograph/store/txn"
	"gograph/store/wal"
)

// ExampleCheckpointer commits a transaction, folds the WAL tail into a
// self-sufficient on-disk snapshot with one forced checkpoint, then
// restores the whole graph from that snapshot alone. Because the
// checkpoint truncates the WAL after writing the snapshot, recovery
// reports zero replayed WAL ops: every byte of state came from the
// snapshot.
func ExampleCheckpointer() {
	dir, err := os.MkdirTemp("", "checkpoint-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		panic(err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", 7); err != nil {
		panic(err)
	}
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		panic(err)
	}
	if err := tx.Commit(); err != nil {
		panic(err)
	}

	// The checkpointer shares the store's mutex so it can take a
	// consistent snapshot. Trigger forces one synchronous checkpoint;
	// Stop tears the goroutine down (no leaks).
	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		panic(err)
	}
	fmt.Printf("checkpoints=%d\n", cp.Stats().Checkpoints)
	cp.Stop()
	if err := w.Close(); err != nil {
		panic(err)
	}

	// Restore from the directory: the snapshot is self-sufficient, so
	// WALOps is zero and the committed state is fully present.
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		panic(err)
	}
	rg := res.Graph
	fmt.Printf("snapshot hit=%t wal ops=%d\n", res.SnapshotHit, res.WALOps)
	fmt.Printf("edge=%t label=%t\n",
		rg.AdjList().HasEdge("alice", "bob"),
		rg.HasNodeLabel("alice", "Person"))

	// Output:
	// checkpoints=1
	// snapshot hit=true wal ops=0
	// edge=true label=true
}
