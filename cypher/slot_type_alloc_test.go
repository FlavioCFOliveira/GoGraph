//go:build !race

package cypher

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// slot_type_alloc_test.go — rmp #2889: resolving one source's run allocates no
// per-source map. Excluded under -race, which changes allocation counts.

// hubRun returns the Hub's forward run of a freshly seeded n-spoke hub graph.
func hubRun(t *testing.T, n int) (eng *Engine, src graph.NodeID, edges []graph.NodeID, hs []uint64) {
	t.Helper()
	eng = newLiveTestEngine(t, false)
	seedHub(t, eng, n)
	view := eng.g.ReadAt(nil)
	fwd, _ := csrPairFromGraph(view)
	verts := fwd.VerticesSlice()
	for s := uint64(0); s < uint64(fwd.MaxNodeID()); s++ {
		if int(verts[s+1]-verts[s]) == n {
			return eng, graph.NodeID(s), fwd.EdgesSlice()[verts[s]:verts[s+1]], fwd.HandlesSlice()[verts[s]:verts[s+1]]
		}
	}
	t.Fatalf("no source with %d arcs", n)
	return nil, 0, nil, nil
}

// TestResolveSourceSlotTypes_NoPerSourceMaps bounds the allocations of resolving a
// Cypher-built hub's 512-slot run: every slot resolves by handle, which allocates
// its one types slice, and nothing else may be allocated per source. The two
// per-source counter maps the positional inference reads cost several more.
func TestResolveSourceSlotTypes_NoPerSourceMaps(t *testing.T) {
	const n = 512
	eng, src, edges, hs := hubRun(t, n)
	view := eng.g.ReadAt(nil)
	mapper := view.AdjList().Mapper()
	sc := newSlotTypeScratch()
	visited := 0
	visit := func(uint64, []string) { visited++ }
	resolveSourceSlotTypes(view, mapper, sc, src, edges, hs, 0, visit) // warm the scratch
	if visited != n {
		t.Fatalf("visited %d typed slots, want %d", visited, n)
	}
	allocs := testing.AllocsPerRun(20, func() {
		resolveSourceSlotTypes(view, mapper, sc, src, edges, hs, 0, visit)
	})
	if allocs > n {
		t.Fatalf("resolving a %d-slot run allocated %.0f times, want at most %d (one types slice per slot)", n, allocs, n)
	}
}
