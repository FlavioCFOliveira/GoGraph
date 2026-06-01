package search

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestTransitiveClosure_Chain(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	tc := TransitiveClosure(c)
	m := a.Mapper()
	// 0 reaches 0..4; 4 reaches only 4.
	for u := 0; u < 5; u++ {
		for v := 0; v < 5; v++ {
			uid, _ := m.Lookup(u)
			vid, _ := m.Lookup(v)
			want := v >= u
			if got := tc.Reachable(uid, vid); got != want {
				t.Fatalf("Reachable(%d, %d) = %v, want %v", u, v, got, want)
			}
		}
	}
}

// TestTransitiveClosure_VsBFS fuzzes random directed graphs and
// asserts the oracle matches a BFS-based reachability reference.
func TestTransitiveClosure_VsBFS(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(181, 191)) //nolint:gosec // deterministic
	for seed := 0; seed < 5; seed++ {
		const n = 16
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := 0; i < 3*n; i++ {
			if err := a.AddEdge(r.IntN(n), r.IntN(n), struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		tc := TransitiveClosure(c)
		m := a.Mapper()
		maxID := int(c.MaxNodeID())
		ids := make([]graph.NodeID, n)
		live := make([]bool, n)
		for i := 0; i < n; i++ {
			if id, ok := m.Lookup(i); ok {
				ids[i] = id
				live[i] = true
			}
		}
		for u := 0; u < n; u++ {
			if !live[u] {
				continue
			}
			ref := bfsReachable(c, ids[u], maxID)
			for v := 0; v < n; v++ {
				if !live[v] {
					continue
				}
				got := tc.Reachable(ids[u], ids[v])
				want := ref[uint64(ids[v])]
				if got != want {
					t.Fatalf("seed=%d: Reachable(%d, %d) = %v, BFS = %v",
						seed, u, v, got, want)
				}
			}
		}
	}
}

// bfsReachable computes a per-destination reachability bitmap from
// src using a plain BFS — the cheap reference used to validate the
// bitset closure oracle. The returned slice is indexed by raw
// NodeID (size = maxID).
func bfsReachable(c *csr.CSR[struct{}], src graph.NodeID, maxID int) []bool {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	reach := make([]bool, maxID)
	reach[uint64(src)] = true
	queue := []graph.NodeID{src}
	for qh := 0; qh < len(queue); qh++ {
		v := queue[qh]
		for k := verts[uint64(v)]; k < verts[uint64(v)+1]; k++ {
			nb := edges[k]
			if int(nb) >= maxID || reach[uint64(nb)] {
				continue
			}
			reach[uint64(nb)] = true
			queue = append(queue, nb)
		}
	}
	return reach
}
