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

// TestBFS_Dimacs9NY_CorrectVsInMemory verifies that extern.BFS
// produces the same depth map as in-memory search.BFS on a
// synthetic DIMACS9-NY-scale graph (24k vertices, 60k edges).
// Three source vertices (NodeIDs 0, 1, 2) are exercised.
func TestBFS_Dimacs9NY_CorrectVsInMemory(t *testing.T) {
	t.Parallel()

	a, err := dimacs9.Synthetic(context.Background(), 24_000, 60_000)
	if err != nil {
		t.Fatalf("dimacs9.Synthetic: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	if c.Order() < 3 {
		t.Skip("graph has fewer than 3 vertices")
	}

	path := filepath.Join(t.TempDir(), "dimacs9_ny.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatalf("csrfile.WriteToFile: %v", err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	for _, src := range []graph.NodeID{0, 1, 2} {
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
				t.Fatalf("extern.BFS: %v", err)
			}

			if !reflect.DeepEqual(tier1, tier2) {
				t.Errorf("src=%d: tier1 and tier2 depth maps differ (tier1 len=%d, tier2 len=%d)",
					src, len(tier1), len(tier2))
			}
		})
	}
}

// nodeIDStr converts a NodeID to a decimal string for subtest names.
func nodeIDStr(id graph.NodeID) string {
	const digits = "0123456789"
	v := uint64(id)
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v%10]
		v /= 10
	}
	return string(buf[i:])
}
