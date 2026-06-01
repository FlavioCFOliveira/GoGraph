package txn_test

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

// ExampleStore runs one atomic transaction end-to-end: open a store
// over a graph and a WAL, begin a transaction, buffer a few mutations,
// commit them, and read the committed state back. A second transaction
// is rolled back to show the all-or-nothing nature: rolled-back work
// leaves no trace in the graph.
func ExampleStore() {
	dir, err := os.MkdirTemp("", "txn-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		panic(err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	// A committed transaction: a labelled node and a weighted edge become
	// visible together when Commit returns.
	tx := s.Begin()
	if err := tx.AddNode("alice"); err != nil {
		panic(err)
	}
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		panic(err)
	}
	if err := tx.AddEdge("alice", "bob", 7); err != nil {
		panic(err)
	}
	if err := tx.Commit(); err != nil {
		panic(err)
	}

	committed := s.Graph()
	fmt.Printf("after commit: edge=%t label=%t\n",
		committed.AdjList().HasEdge("alice", "bob"),
		committed.HasNodeLabel("alice", "Person"))

	// A rolled-back transaction: the buffered op never reaches the graph.
	tx2 := s.Begin()
	if err := tx2.AddEdge("alice", "carol", 1); err != nil {
		panic(err)
	}
	if err := tx2.Rollback(); err != nil {
		panic(err)
	}
	fmt.Printf("after rollback: edge=%t\n", committed.AdjList().HasEdge("alice", "carol"))

	// Output:
	// after commit: edge=true label=true
	// after rollback: edge=false
}

// ExampleStore_recover shows the durability half of the contract:
// Commit writes to the WAL before applying in memory, so after the
// store is closed (a simulated restart) recovery replays the WAL and
// the committed state is fully rebuilt.
func ExampleStore_recover() {
	dir, err := os.MkdirTemp("", "txn-recover-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Open, commit, close — the lifetime of one process.
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
	if err := tx.SetEdgeLabel("alice", "bob", "KNOWS"); err != nil {
		panic(err)
	}
	if err := tx.Commit(); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}

	// A fresh process reopens the directory and replays the WAL.
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		panic(err)
	}
	rg := res.Graph
	fmt.Printf("recovered: edge=%t label=%t\n",
		rg.AdjList().HasEdge("alice", "bob"),
		rg.HasEdgeLabel("alice", "bob", "KNOWS"))

	// Output:
	// recovered: edge=true label=true
}
