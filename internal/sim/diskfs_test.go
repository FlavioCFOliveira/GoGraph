package sim

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// TestDiskFS_CheckpointOptionAccepts asserts at compile + run time that
// simCheckpointBackend satisfies the checkpoint package's (unexported) snapshot
// backend interface: WithSnapshotFS would not compile otherwise. This pins the
// cross-package seam so a drift in either side fails the build here.
func TestDiskFS_CheckpointOptionAccepts(t *testing.T) {
	disk := NewSimDisk(NewSeed(1), 0)
	// The option is generic on the store's key/weight; the simulator store is
	// string/float64. Constructing it proves the adapter satisfies the seam.
	_ = checkpoint.WithSnapshotFS[string, float64](simCheckpointBackend{disk: disk})
}

// TestDiskFS_CSRFileRoundTripOnSimDisk exercises the csrfile write+read seam on
// the in-memory backend: WriteToFileWith publishes a bulk CSR through SimDisk,
// and OpenWith reads it back into an 8-byte-aligned buffer (no mmap), proving
// the read-into-[]byte fallback is sound and byte-faithful.
func TestDiskFS_CSRFileRoundTripOnSimDisk(t *testing.T) {
	disk := NewSimDisk(NewSeed(2), 0)
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < 5; i++ {
		_ = g.AddNode(nodeKey(i))
	}
	for i := 0; i+1 < 5; i++ {
		if err := g.AddEdge(nodeKey(i), nodeKey(i+1), float64(i)); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	cs := csr.BuildFromAdjList(g.AdjList())

	fsys := simCSRFS{disk: disk}
	wantHeader, err := csrfile.WriteToFileWith(fsys, "bulk/graph.csr", cs)
	if err != nil {
		t.Fatalf("WriteToFileWith: %v", err)
	}
	// The publish rename's parent dirent must be durable across a crash, since
	// csrfile is the bulk loader's sole durability mechanism.
	disk.Crash()

	r, err := csrfile.OpenWith(fsys, "bulk/graph.csr")
	if err != nil {
		t.Fatalf("OpenWith after crash: %v", err)
	}
	defer func() { _ = r.Close() }()
	if r.Header() != wantHeader {
		t.Fatalf("header mismatch after SimDisk round-trip: got %+v want %+v", r.Header(), wantHeader)
	}
	// Sanity-check the edge count survives the heap-reinterpret read path.
	if got := len(r.Edges()); got != 4 {
		t.Fatalf("edges after round-trip = %d, want 4", got)
	}
}
