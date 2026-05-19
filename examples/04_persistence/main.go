// Example 04_persistence — opens a WAL, performs a few transactions
// that include both node and edge labels, then takes a v2 snapshot
// (CSR + labels.bin) and demonstrates that labels survive a restart.
// The flow mirrors what a production durability path looks like:
//
//  1. Transactions append framed ops to the WAL and apply them to
//     the in-memory LPG.
//  2. snapshot.WriteSnapshotFull persists the CSR view and the
//     labels.bin component atomically alongside the WAL.
//  3. The process "restarts" — every in-memory reference is dropped
//     and recovery.OpenString rebuilds the graph from disk. The WAL
//     replay re-populates the mapper; labels.bin then re-attaches
//     the snapshot-time label set to the matching NodeIDs.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

func main() {
	dir, err := os.MkdirTemp("", "gograph-ex04-")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		panic(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStore(g, w)

	commits := []struct{ src, dst, nodeLabel, edgeLabel string }{
		{"alice", "bob", "Person", "KNOWS"},
		{"bob", "carol", "Person", "KNOWS"},
		{"carol", "dave", "Person", "FOLLOWS"},
	}
	for _, c := range commits {
		tx := store.Begin()
		_ = tx.SetNodeLabel(c.src, c.nodeLabel)
		_ = tx.SetNodeLabel(c.dst, c.nodeLabel)
		_ = tx.AddEdge(c.src, c.dst, 0)
		_ = tx.SetEdgeLabel(c.src, c.dst, c.edgeLabel)
		_ = tx.Commit()
	}
	fmt.Printf("Committed %d transactions to the WAL.\n", len(commits))

	// Persist a v2 snapshot (CSR + labels.bin) alongside the WAL.
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		fmt.Println("snapshot.WriteSnapshotFull:", err)
		return
	}
	fmt.Println("v2 snapshot persisted: csr.bin + labels.bin + manifest.json.")
	_ = w.Close()

	// "Restart": drop all in-memory references and rebuild from disk.
	_ = store
	_ = g
	res, err := recovery.OpenString(dir)
	if err != nil {
		fmt.Println("recovery.OpenString:", err)
		return
	}
	fmt.Printf("Recovered: WAL ops=%d, snapshot hit=%v, snapshot label records=%d.\n",
		res.WALOps, res.SnapshotHit, res.SnapshotLabels)
	for _, c := range commits {
		if res.Graph.HasNodeLabel(c.src, c.nodeLabel) &&
			res.Graph.HasEdgeLabel(c.src, c.dst, c.edgeLabel) {
			fmt.Printf("  recovered %s -[%s]-> %s (src carries %q)\n",
				c.src, c.edgeLabel, c.dst, c.nodeLabel)
		} else {
			fmt.Printf("  MISSING label data for %s -> %s\n", c.src, c.dst)
		}
	}
}
