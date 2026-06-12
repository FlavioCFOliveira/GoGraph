//go:build soak || nightly

package csrfile_test

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/rmat"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/store/bulk"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// TestReader_LargeFile_RMAT generates a ~8M-edge RMAT graph (scale=20,
// edgeFactor=8), writes it to a csrfile via bulk.Loader, opens it with
// csrfile.Open, and exercises a BFS-style traversal from vertex 0 to
// confirm the file is fully readable without errors.
func TestReader_LargeFile_RMAT(t *testing.T) {
	testlayers.RequireSoak(t)

	outPath := filepath.Join(t.TempDir(), "rmat20.csr")
	loader := bulk.New(bulk.Options{OutputPath: outPath, Directed: true})
	rmat.Generate(rmat.Spec{Scale: 20, EdgeFactor: 8, Seed: 42}, loader)
	_, _, err := loader.Finalise()
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}

	r, err := csrfile.Open(outPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	h := r.Header()
	// Scale=20, EdgeFactor=8 → target ~2^20*8 = 8_388_608 edges before dedup.
	// After RMAT dedup the real count is lower; assert it is at least 1M.
	const minExpectedEdges = 1_000_000
	if h.NEdges < minExpectedEdges {
		t.Fatalf("NEdges=%d, want >= %d", h.NEdges, minExpectedEdges)
	}

	// BFS from vertex 0 across the file reader's slices.
	verts := r.Vertices()
	edges := r.Edges()
	if len(verts) == 0 {
		t.Fatal("empty vertices slice")
	}

	visited := make([]bool, len(verts))
	queue := make([]graph.NodeID, 0, 1024)
	queue = append(queue, 0)
	visited[0] = true
	seen := 0
	for len(queue) > 0 {
		src := queue[0]
		queue = queue[1:]
		seen++
		if int(src)+1 >= len(verts) {
			continue
		}
		start := verts[src]
		end := verts[src+1]
		for i := start; i < end; i++ {
			dst := edges[i]
			if int(dst) < len(visited) && !visited[dst] {
				visited[dst] = true
				queue = append(queue, dst)
			}
		}
	}
	if seen == 0 {
		t.Fatal("BFS visited zero nodes")
	}
	t.Logf("BFS from vertex 0: visited %d nodes out of %d (NEdges=%d)",
		seen, len(verts), h.NEdges)
}
