//go:build nightly

package extern

import (
	"context"
	"path/filepath"
	"testing"

	"gograph/bench/dimacs9"
	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/testlayers"
	"gograph/store/csrfile"
)

// TestBFS_Dimacs9Full_Nightly verifies that extern.BFS completes
// without error on a full DIMACS9-USA-scale synthetic graph
// (24M vertices, 60M edges).
func TestBFS_Dimacs9Full_Nightly(t *testing.T) {
	testlayers.RequireNightly(t)

	a, err := dimacs9.Synthetic(context.Background(), 24_000_000, 60_000_000)
	if err != nil {
		t.Fatalf("dimacs9.Synthetic: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	path := filepath.Join(t.TempDir(), "dimacs9_full.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatalf("csrfile.WriteToFile: %v", err)
	}
	// Release the in-memory CSR before opening the file to reduce peak
	// memory usage at this scale.
	c = nil
	a = nil

	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	visited := 0
	if err := BFS(r, graph.NodeID(0), func(_ graph.NodeID, _ int) bool {
		visited++
		return true
	}); err != nil {
		t.Fatalf("extern.BFS: %v", err)
	}

	if visited < 1 {
		t.Fatalf("BFS visited %d nodes, want at least 1", visited)
	}
}
