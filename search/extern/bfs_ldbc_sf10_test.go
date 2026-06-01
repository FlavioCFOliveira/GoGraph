//go:build soak

package extern

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/ldbc"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/search"
	"github.com/FlavioCFOliveira/GoGraph/store/bulk"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// TestBFS_LDBCSf10_Soak verifies extern.BFS against in-memory
// search.BFS on a synthetic LDBC SF10-scale graph (~600k vertices,
// ~6M edges). Five source NodeIDs are cross-checked.
func TestBFS_LDBCSf10_Soak(t *testing.T) {
	testlayers.RequireSoak(t)

	path := filepath.Join(t.TempDir(), "ldbc_sf10.csr")
	loader := bulk.New(bulk.Options{OutputPath: path, Directed: true})
	ldbc.Synthetic(context.Background(), 600_000, 6_000_000, loader)
	_, c, err := loader.Finalise()
	if err != nil {
		t.Fatalf("loader.Finalise: %v", err)
	}

	if c.Order() < 5 {
		t.Skip("generated graph has fewer than 5 vertices")
	}

	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	sources := []graph.NodeID{0, 1, 2, 3, 4}
	for _, src := range sources {
		src := src
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
				t.Fatalf("extern.BFS src=%d: %v", src, err)
			}

			if !reflect.DeepEqual(tier1, tier2) {
				t.Errorf("src=%d: tier1 and tier2 depth maps differ (tier1 len=%d, tier2 len=%d)",
					src, len(tier1), len(tier2))
			}
		})
	}
}
