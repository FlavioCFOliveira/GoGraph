package search

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// referenceFarthest is the pre-#3079 sweep: a full O(V) reset, the farthest
// vertex chosen by re-reading dist, and the eccentricity by an O(V) rescan.
// It is the oracle the touched-list sweep must match bit for bit.
func referenceFarthest(verts []uint64, edges []graph.NodeID, src graph.NodeID, n int) (graph.NodeID, int, []int) {
	dist := make([]int, n)
	for i := range dist {
		dist[i] = -1
	}
	dist[src] = 0
	queue := []graph.NodeID{src}
	far := src
	for qh := 0; qh < len(queue); qh++ {
		v := queue[qh]
		for k := verts[v]; k < verts[v+1]; k++ {
			nb := edges[k]
			if dist[nb] >= 0 {
				continue
			}
			dist[nb] = dist[v] + 1
			queue = append(queue, nb)
			if dist[nb] > dist[far] || (dist[nb] == dist[far] && nb < far) {
				far = nb
			}
		}
	}
	ecc := 0
	for _, d := range dist {
		ecc = max(ecc, d)
	}
	return far, ecc, dist
}

// TestBfsFarthest_TouchedResetMatchesFullReset sweeps every source of
// random, deliberately disconnected graphs through ONE reused (dist, queue)
// pair and checks each sweep against a freshly reset reference. A reset that
// missed a vertex of the previous sweep's component would leave a stale
// distance where the reference has -1.
func TestBfsFarthest_TouchedResetMatchesFullReset(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(7, 13)) //nolint:gosec // deterministic test RNG
	for trial := 0; trial < 40; trial++ {
		n := 2 + r.IntN(50)
		a := adjlist.New[int, struct{}](adjlist.Config{})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := r.IntN(2 * n); i > 0; i-- {
			u, v := r.IntN(n), r.IntN(n)
			if err := a.AddEdge(u, v, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a).BuildSymmetric()
		verts, edges := c.VerticesSlice(), c.EdgesSlice()
		m := int(c.MaxNodeID())
		dist := newBFSDist(m)
		queue := make([]graph.NodeID, 0, m)
		for _, s := range r.Perm(m) {
			var far graph.NodeID
			var ecc int
			var got []int
			far, ecc, got, queue = bfsFarthest(verts, edges, graph.NodeID(s), dist, queue)
			wantFar, wantEcc, want := referenceFarthest(verts, edges, graph.NodeID(s), m)
			if far != wantFar || ecc != wantEcc || !slices.Equal(got, want) {
				t.Fatalf("trial %d src %d: got (far=%d ecc=%d dist=%v), want (far=%d ecc=%d dist=%v)",
					trial, s, far, ecc, got, wantFar, wantEcc, want)
			}
		}
	}
}
