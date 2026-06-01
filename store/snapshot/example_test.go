package snapshot_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// Example writes a full (v3) snapshot of a labelled graph to a directory
// and loads it back into a fresh process view, inspecting the manifest
// and the parsed CSR readback.
func Example() {
	dir, err := os.MkdirTemp("", "snapshot-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// A small labelled, weighted graph plus its frozen CSR snapshot.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("alice", "bob", 7); err != nil {
		panic(err)
	}
	if err := g.SetNodeLabel("alice", "Person"); err != nil {
		panic(err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	// WriteSnapshotFull lays out csr.bin + labels.bin + properties.bin +
	// a manifest, and (because the graph is string-keyed) a mapper.bin,
	// stamping the manifest at v3. Publication is atomic.
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, c, g); err != nil {
		panic(err)
	}

	// Load it back: LoadSnapshotFull verifies every component's CRC and
	// returns the parsed readbacks.
	loaded, err := snapshot.LoadSnapshotFull(snapDir)
	if err != nil {
		panic(err)
	}
	// The readback exposes the parsed edge array and the interned label
	// strings. (CSR.Vertices is the dense row-pointer array sized by the
	// largest interned NodeID, not the live-node count, so it is not
	// asserted here.)
	fmt.Printf("manifest version=%d\n", loaded.Manifest.Version)
	fmt.Printf("csr edges=%d\n", len(loaded.CSR.Edges))
	fmt.Printf("label strings=%d\n", len(loaded.Labels.Strings))

	// Output:
	// manifest version=3
	// csr edges=1
	// label strings=1
}

// ExampleWriteSnapshotCSR shows the lighter, CSR-only (v1) path: it
// writes just the adjacency and reads it straight back with Open.
func ExampleWriteSnapshotCSR() {
	dir, err := os.MkdirTemp("", "snapshot-csr-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		panic(err)
	}
	if err := a.AddEdge("a", "c", 2); err != nil {
		panic(err)
	}
	c := csr.BuildFromAdjList(a)

	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotCSR(snapDir, c); err != nil {
		panic(err)
	}

	loaded, err := snapshot.Open(snapDir)
	if err != nil {
		panic(err)
	}
	fmt.Printf("manifest version=%d\n", loaded.Manifest.Version)
	fmt.Printf("csr edges=%d\n", len(loaded.CSR.Edges))

	// Output:
	// manifest version=1
	// csr edges=2
}
