package recovery_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ExampleOpen rebuilds graph state after a simulated restart. A store
// commits two transactions to the WAL and the process "crashes" (the
// WAL is simply closed). recovery.Open then reopens the directory,
// replays the WAL into a fresh graph, and reports how many ops it
// applied.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "recovery-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// --- process 1: write and commit, then close (simulated crash) ---
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		panic(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	tx1 := s.Begin()
	if err := tx1.AddEdge("alice", "bob", 7); err != nil {
		panic(err)
	}
	if err := tx1.Commit(); err != nil {
		panic(err)
	}

	tx2 := s.Begin()
	if err := tx2.SetNodeLabel("alice", "Person"); err != nil {
		panic(err)
	}
	if err := tx2.Commit(); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}

	// --- process 2: recover from the directory on disk ---
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		panic(err)
	}

	rg := res.Graph
	fmt.Printf("snapshot hit=%t\n", res.SnapshotHit)
	fmt.Printf("edge=%t label=%t\n",
		rg.AdjList().HasEdge("alice", "bob"),
		rg.HasNodeLabel("alice", "Person"))

	// Output:
	// snapshot hit=false
	// edge=true label=true
}
