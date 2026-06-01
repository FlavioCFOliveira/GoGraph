package extern

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// TestConcurrentReaders verifies that a single csrfile.Reader can be
// shared by 32 goroutines running extern.BFS concurrently. Each
// goroutine's depth map must match a sequential reference computed
// before the concurrent phase.
func TestConcurrentReaders(t *testing.T) {
	t.Parallel()

	// Build a 50×50 grid (2500 nodes) — undirected, no diagonal edges.
	shape := shapegen.Grid(50, 50, false)
	g, err := shape.Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Grid.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	const numGoroutines = 32
	if c.Order() < numGoroutines {
		t.Skipf("graph has %d nodes, need at least %d", c.Order(), numGoroutines)
	}

	path := filepath.Join(t.TempDir(), "grid50x50.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatalf("csrfile.WriteToFile: %v", err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	// Sequential reference pass.
	refs := make([]map[graph.NodeID]int, numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		src := graph.NodeID(i)
		refs[i] = make(map[graph.NodeID]int)
		if err := BFS(r, src, func(n graph.NodeID, d int) bool {
			refs[i][n] = d
			return true
		}); err != nil {
			t.Fatalf("reference BFS src=%d: %v", i, err)
		}
	}

	// Concurrent pass — each goroutine writes to its own result slot.
	results := make([]map[graph.NodeID]int, numGoroutines)
	errs := make([]error, numGoroutines)
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			src := graph.NodeID(i)
			m := make(map[graph.NodeID]int)
			errs[i] = BFS(r, src, func(n graph.NodeID, d int) bool {
				m[n] = d
				return true
			})
			results[i] = m
		}()
	}
	wg.Wait()

	// Compare.
	for i := 0; i < numGoroutines; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d BFS error: %v", i, errs[i])
			continue
		}
		if len(results[i]) != len(refs[i]) {
			t.Errorf("goroutine %d: depth map length %d != reference %d",
				i, len(results[i]), len(refs[i]))
			continue
		}
		for id, want := range refs[i] {
			if got := results[i][id]; got != want {
				t.Errorf("goroutine %d: node %d depth %d != reference %d", i, id, got, want)
			}
		}
	}
}
