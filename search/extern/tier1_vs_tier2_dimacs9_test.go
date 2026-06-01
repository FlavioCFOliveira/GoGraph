package extern

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/dimacs9"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// TestTier1VsTier2_Dimacs9 compares in-memory search.BFS (Tier 1)
// against extern.BFS (Tier 2) for 10 deterministic source vertices
// on a 10k-vertex, 30k-edge synthetic DIMACS9-style graph.
func TestTier1VsTier2_Dimacs9(t *testing.T) {
	t.Parallel()

	a, err := dimacs9.Synthetic(context.Background(), 10_000, 30_000)
	if err != nil {
		t.Fatalf("dimacs9.Synthetic: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	path := filepath.Join(t.TempDir(), "tier12_dimacs9.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatalf("csrfile.WriteToFile: %v", err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	numSources := 10
	if int(c.Order()) < numSources {
		numSources = int(c.Order())
	}

	for i := 0; i < numSources; i++ {
		src := graph.NodeID(i)
		i := i
		t.Run("src="+nodeIDStr(src), func(t *testing.T) {
			tier1 := make(map[graph.NodeID]int)
			search.BFS(c, src, func(n graph.NodeID, d int) bool {
				tier1[n] = d
				return true
			})

			tier2 := make(map[graph.NodeID]int)
			if err := BFS(r, src, func(n graph.NodeID, d int) bool {
				tier2[n] = d
				return true
			}); err != nil {
				t.Fatalf("extern.BFS src=%d: %v", i, err)
			}

			if !reflect.DeepEqual(tier1, tier2) {
				t.Errorf("src=%d: depth maps differ (tier1 len=%d, tier2 len=%d)",
					i, len(tier1), len(tier2))
			}
		})
	}
}
