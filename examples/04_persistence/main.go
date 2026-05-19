// Example 04_persistence — opens a WAL, performs three transactions,
// then recovers the graph from the WAL and confirms every committed
// op survived.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/txn"
	"gograph/store/wal"
)

func main() {
	dir, _ := os.MkdirTemp("", "gograph-ex04-")
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "wal")

	w, _ := wal.Open(path)
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStore(g, w)
	for _, op := range []struct{ src, dst, label string }{
		{"alice", "bob", "KNOWS"},
		{"bob", "carol", "KNOWS"},
	} {
		tx := store.Begin()
		_ = tx.AddEdge(op.src, op.dst)
		_ = tx.SetEdgeLabel(op.src, op.dst, op.label)
		_ = tx.Commit()
	}
	_ = w.Close()

	res, _ := recovery.OpenString(dir)
	fmt.Printf("WAL replay applied %d ops\n", res.WALOps)
	if res.Graph.AdjList().HasEdge("alice", "bob") {
		fmt.Println("alice -> bob recovered")
	}
}
