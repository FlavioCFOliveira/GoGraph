// Example 04_persistence — opens a WAL, performs a few transactions
// that include both node and edge labels, attaches typed properties
// directly on the in-memory graph, then takes a v2 snapshot (CSR +
// labels.bin + properties.bin) and demonstrates that labels and
// typed properties survive a restart. The flow mirrors what a
// production durability path looks like:
//
//  1. Transactions append framed ops to the WAL and apply them to
//     the in-memory LPG.
//  2. Typed properties (currently not WAL-logged) are set directly
//     on the graph.
//  3. snapshot.WriteSnapshotFull persists the CSR view, labels.bin,
//     and properties.bin atomically alongside the WAL.
//  4. The process "restarts" — every in-memory reference is dropped
//     and recovery.OpenString rebuilds the graph from disk. The WAL
//     replay re-populates the mapper; labels.bin re-attaches the
//     snapshot-time label set; properties.bin re-attaches the
//     snapshot-time typed property set.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

//nolint:gocyclo // example walk-through: setup + commits + property writes + snapshot + recovery + per-kind assertions
func main() {
	dir, err := os.MkdirTemp("", "gograph-ex04-")
	if err != nil {
		log.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
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

	// Attach typed properties before snapshotting. These travel
	// through properties.bin only; the WAL records labels and edges
	// today, not property writes.
	joined := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	if err := g.SetNodeProperty("alice", "name", lpg.StringValue("Alice")); err != nil {
		log.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("alice", "age", lpg.Int64Value(30)); err != nil {
		log.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("alice", "joined", lpg.TimeValue(joined)); err != nil {
		log.Fatalf("SetNodeProperty: %v", err)
	}
	g.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026"))
	g.SetEdgeProperty("alice", "bob", "weight", lpg.Int64Value(7))
	fmt.Println("Typed properties set on alice and edge alice->bob.")

	// Persist a v2 snapshot (CSR + labels.bin + properties.bin)
	// alongside the WAL.
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		fmt.Println("snapshot.WriteSnapshotFull:", err)
		return
	}
	fmt.Println("v2 snapshot persisted: csr.bin + labels.bin + properties.bin + manifest.json.")
	_ = w.Close()

	// "Restart": drop all in-memory references and rebuild from disk.
	_ = store
	_ = g
	res, err := recovery.OpenString(dir)
	if err != nil {
		fmt.Println("recovery.OpenString:", err)
		return
	}
	fmt.Printf("Recovered: WAL ops=%d, snapshot hit=%v, snapshot label records=%d, snapshot property records=%d.\n",
		res.WALOps, res.SnapshotHit, res.SnapshotLabels, res.SnapshotProperties)
	for _, c := range commits {
		if res.Graph.HasNodeLabel(c.src, c.nodeLabel) &&
			res.Graph.HasEdgeLabel(c.src, c.dst, c.edgeLabel) {
			fmt.Printf("  recovered %s -[%s]-> %s (src carries %q)\n",
				c.src, c.edgeLabel, c.dst, c.nodeLabel)
		} else {
			fmt.Printf("  MISSING label data for %s -> %s\n", c.src, c.dst)
		}
	}

	// Assert typed-property survival.
	if v, ok := res.Graph.GetNodeProperty("alice", "name"); !ok {
		fmt.Println("  MISSING property alice.name")
	} else if s, _ := v.String(); s != "Alice" {
		fmt.Printf("  property alice.name mismatch: %q\n", s)
	} else {
		fmt.Printf("  recovered alice.name = %q\n", s)
	}
	if v, ok := res.Graph.GetNodeProperty("alice", "age"); !ok {
		fmt.Println("  MISSING property alice.age")
	} else if i, _ := v.Int64(); i != 30 {
		fmt.Printf("  property alice.age mismatch: %d\n", i)
	} else {
		fmt.Printf("  recovered alice.age = %d\n", i)
	}
	if v, ok := res.Graph.GetNodeProperty("alice", "joined"); !ok {
		fmt.Println("  MISSING property alice.joined")
	} else if tm, _ := v.Time(); !tm.Equal(joined) {
		fmt.Printf("  property alice.joined mismatch: %v\n", tm)
	} else {
		fmt.Printf("  recovered alice.joined = %s\n", tm.Format(time.RFC3339))
	}
	if v, ok := res.Graph.GetEdgeProperty("alice", "bob", "since"); !ok {
		fmt.Println("  MISSING edge property since")
	} else if s, _ := v.String(); s != "2026" {
		fmt.Printf("  edge(alice,bob).since mismatch: %q\n", s)
	} else {
		fmt.Printf("  recovered edge(alice,bob).since = %q\n", s)
	}
	if v, ok := res.Graph.GetEdgeProperty("alice", "bob", "weight"); !ok {
		fmt.Println("  MISSING edge property weight")
	} else if i, _ := v.Int64(); i != 7 {
		fmt.Printf("  edge(alice,bob).weight mismatch: %d\n", i)
	} else {
		fmt.Printf("  recovered edge(alice,bob).weight = %d\n", i)
	}
}
