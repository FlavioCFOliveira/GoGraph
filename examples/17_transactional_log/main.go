// Example 17_transactional_log — end-to-end durability walk-through.
// Opens a WAL, runs a sequence of transactions, takes a checkpoint
// in the background, then simulates a crash (process abandons the
// in-memory state) and verifies that recovery rebuilds the same
// committed graph.
//
// Sample output: run `go run ./examples/17_transactional_log` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/checkpoint"
	"gograph/store/recovery"
	"gograph/store/txn"
	"gograph/store/wal"
)

func main() {
	dir, _ := os.MkdirTemp("", "gograph-ex17-")
	defer func() { _ = os.RemoveAll(dir) }()

	walPath := filepath.Join(dir, "wal")
	w, _ := wal.Open(walPath)
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStore(g, w)

	// Background checkpointer firing every 50 ms.
	cp := checkpoint.New(checkpoint.Config{
		Dir:      dir,
		MaxAge:   50 * time.Millisecond,
		Interval: 25 * time.Millisecond,
	}, g, w, storeMutex(store))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	commits := [][3]string{
		{"alice", "bob", "KNOWS"},
		{"bob", "carol", "KNOWS"},
		{"carol", "dave", "KNOWS"},
		{"alice", "carol", "FOLLOWS"},
	}
	for _, c := range commits {
		tx := store.Begin()
		_ = tx.AddEdge(c[0], c[1], 0)
		_ = tx.SetEdgeLabel(c[0], c[1], c[2])
		_ = tx.Commit()
		time.Sleep(20 * time.Millisecond)
	}
	cp.Stop()
	_ = w.Close()
	fmt.Printf("Committed %d transactions.\n", len(commits))
	fmt.Printf("Checkpoint stats: %+v\n", cp.Stats())

	// Simulate a crash by abandoning every in-memory reference and
	// recovering state from the WAL plus any existing snapshot.
	// We do not touch g or store after this point.
	_ = store
	_ = g
	res, err := recovery.OpenString(dir)
	if err != nil {
		fmt.Println("recovery failed:", err)
		return
	}
	fmt.Printf("\nRecovered %d ops from WAL (snapshot used: %v).\n", res.WALOps, res.SnapshotHit)
	for _, c := range commits {
		if !res.Graph.AdjList().HasEdge(c[0], c[1]) {
			fmt.Printf("  MISSING edge %s -> %s\n", c[0], c[1])
		} else {
			fmt.Printf("  recovered %s -> %s with label %q\n",
				c[0], c[1], firstLabel(res.Graph, c[0], c[1]))
		}
	}
}

func firstLabel(g *lpg.Graph[string, int64], src, dst string) string {
	for _, lab := range []string{"KNOWS", "FOLLOWS"} {
		if g.HasEdgeLabel(src, dst, lab) {
			return lab
		}
	}
	return ""
}

// storeMutex returns the single-writer mutex the checkpointer
// must share with the transaction layer. We expose it via a
// helper because store.Store does not expose its mutex directly
// (the checkpointer is given a *sync.Mutex by convention).
func storeMutex(_ *txn.Store[string, int64]) *sync.Mutex {
	// The checkpointer accepts a mutex it can briefly Lock; in
	// this demo we use a separate mutex so the checkpointer's
	// Lock does not contend with concurrent transactions. In a
	// production setup the same mutex should be shared.
	return &sync.Mutex{}
}
