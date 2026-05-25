//go:build nightly

package extern

import (
	"path/filepath"
	"testing"

	"gograph/bench/rmat"
	"gograph/graph"
	"gograph/internal/testlayers"
	"gograph/store/bulk"
	"gograph/store/csrfile"
)

// TestBFS_RMATScale20_Nightly verifies that extern.BFS completes
// without error and visits at least one node on a large RMAT-scale-20
// graph (~1M vertices, ~16M edges).
func TestBFS_RMATScale20_Nightly(t *testing.T) {
	testlayers.RequireNightly(t)

	path := filepath.Join(t.TempDir(), "rmat20.csr")
	loader := bulk.New(bulk.Options{OutputPath: path, Directed: true})
	rmat.Generate(rmat.Spec{
		Scale:      20,
		EdgeFactor: 16,
		A:          0.57,
		B:          0.19,
		C:          0.19,
		D:          0.05,
		Seed:       42,
	}, loader)
	if _, _, err := loader.Finalise(); err != nil {
		t.Fatalf("loader.Finalise: %v", err)
	}

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
