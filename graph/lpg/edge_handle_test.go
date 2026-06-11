package lpg

import (
	"sort"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestGraph_AddEdgeH_Monotone verifies handles are strictly increasing and
// start at 1 (0 is the no-handle sentinel).
func TestGraph_AddEdgeH_Monotone(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	var prev uint64
	for i := 0; i < 8; i++ {
		h, err := g.AddEdgeH("a", "b", 0)
		if err != nil {
			t.Fatalf("AddEdgeH #%d: %v", i, err)
		}
		if h == 0 {
			t.Fatalf("AddEdgeH #%d returned handle 0 (reserved sentinel)", i)
		}
		if h <= prev {
			t.Fatalf("AddEdgeH #%d handle %d not strictly greater than previous %d", i, h, prev)
		}
		prev = h
	}
	if prev != 8 {
		t.Fatalf("after 8 AddEdgeH, last handle = %d, want 8 (1-based monotone)", prev)
	}
}

// TestGraph_EdgeHandle_NeverReusedAfterDelete verifies that deleting an
// edge does not free its handle: a subsequent AddEdgeH yields a strictly
// larger value, never the deleted edge's handle.
func TestGraph_EdgeHandle_NeverReusedAfterDelete(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	h1, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH #1: %v", err)
	}
	h2, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH #2: %v", err)
	}

	// Fully remove the pair (two RemoveEdge calls drop both parallels).
	g.RemoveEdge("a", "b")
	g.RemoveEdge("a", "b")

	h3, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH #3 (post-delete): %v", err)
	}
	if h3 == h1 || h3 == h2 {
		t.Fatalf("post-delete handle %d reused a deleted handle (h1=%d h2=%d)", h3, h1, h2)
	}
	if h3 <= h2 {
		t.Fatalf("post-delete handle %d not strictly greater than last %d", h3, h2)
	}
}

// TestGraph_EdgeLabelsByHandle_RoundTrip verifies that a label set under a
// handle reads back under the same handle, and is independent of a sibling
// parallel edge's handle.
func TestGraph_EdgeLabelsByHandle_RoundTrip(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	h1, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH #1: %v", err)
	}
	h2, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH #2: %v", err)
	}

	g.SetEdgeLabelByHandle("a", "b", h1, "USES")
	g.SetEdgeLabelByHandle("a", "b", h2, "CALLS")

	if got := g.EdgeLabelsByHandle("a", "b", h1); len(got) != 1 || got[0] != "USES" {
		t.Fatalf("EdgeLabelsByHandle(h1) = %v, want [USES]", got)
	}
	if got := g.EdgeLabelsByHandle("a", "b", h2); len(got) != 1 || got[0] != "CALLS" {
		t.Fatalf("EdgeLabelsByHandle(h2) = %v, want [CALLS]", got)
	}

	// Unknown handle and the 0 sentinel return nil.
	if got := g.EdgeLabelsByHandle("a", "b", 99999); got != nil {
		t.Fatalf("EdgeLabelsByHandle(unknown) = %v, want nil", got)
	}
	if got := g.EdgeLabelsByHandle("a", "b", 0); got != nil {
		t.Fatalf("EdgeLabelsByHandle(0) = %v, want nil", got)
	}
}

// TestGraph_EdgeLabelsByHandle_SurvivesSiblingDelete is the core Stage-1
// invariant at the lpg layer: deleting one parallel edge leaves the
// survivor's per-handle labels intact and resolvable by its original
// handle, even though the adjacency slot was compacted.
func TestGraph_EdgeLabelsByHandle_SurvivesSiblingDelete(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	h1, _ := g.AddEdgeH("a", "b", 0)
	h2, _ := g.AddEdgeH("a", "b", 0)
	g.SetEdgeLabelByHandle("a", "b", h1, "USES")
	g.SetEdgeLabelByHandle("a", "b", h2, "CALLS")

	// Remove the FIRST parallel (handle h1). The pair still has one edge,
	// so the per-handle store is NOT cleared. The survivor (h2) keeps its
	// label.
	g.RemoveEdge("a", "b")

	if got := g.EdgeLabelsByHandle("a", "b", h2); len(got) != 1 || got[0] != "CALLS" {
		t.Fatalf("survivor EdgeLabelsByHandle(h2) = %v, want [CALLS]", got)
	}
}

// TestGraph_EdgePropertiesByHandle_RoundTrip verifies the property analogue.
func TestGraph_EdgePropertiesByHandle_RoundTrip(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	h, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	if err := g.SetEdgePropertyByHandle("a", "b", h, "weight", Int64Value(7)); err != nil {
		t.Fatalf("SetEdgePropertyByHandle: %v", err)
	}

	props := g.EdgePropertiesByHandle("a", "b", h)
	if props == nil {
		t.Fatal("EdgePropertiesByHandle = nil after a write")
	}
	pv, ok := props["weight"]
	if !ok {
		t.Fatalf("property 'weight' missing; got %v", props)
	}
	if i, ok := pv.Int64(); !ok || i != 7 {
		t.Fatalf("property 'weight' = %v, want int64(7)", pv)
	}
}

// TestGraph_RemoveEdgeInstanceByHandle verifies a single handle's metadata
// is dropped while a sibling handle's survives.
func TestGraph_RemoveEdgeInstanceByHandle(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	h1, _ := g.AddEdgeH("a", "b", 0)
	h2, _ := g.AddEdgeH("a", "b", 0)
	g.SetEdgeLabelByHandle("a", "b", h1, "USES")
	g.SetEdgeLabelByHandle("a", "b", h2, "CALLS")

	g.RemoveEdgeInstanceByHandle("a", "b", h1)

	if got := g.EdgeLabelsByHandle("a", "b", h1); got != nil {
		t.Fatalf("after RemoveEdgeInstanceByHandle(h1), labels = %v, want nil", got)
	}
	if got := g.EdgeLabelsByHandle("a", "b", h2); len(got) != 1 || got[0] != "CALLS" {
		t.Fatalf("sibling h2 labels = %v, want [CALLS]", got)
	}
}

// TestGraph_PairFullDelete_ClearsHandleStore verifies that once the LAST
// edge between a pair is gone, clearEdgePairState drops the handle store so
// a re-created edge between the same endpoints does not resurrect a removed
// edge's per-handle type.
func TestGraph_PairFullDelete_ClearsHandleStore(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	h1, _ := g.AddEdgeH("a", "b", 0)
	g.SetEdgeLabelByHandle("a", "b", h1, "USES")

	g.RemoveEdge("a", "b") // last edge gone → pair state cleared

	if got := g.EdgeLabelsByHandle("a", "b", h1); got != nil {
		t.Fatalf("after full pair delete, labels = %v, want nil (store cleared)", got)
	}
}

// TestGraph_FirstEdgeHandle reports the handle of the FIRST src→dst slot —
// the one RemoveEdge would remove — and tracks the compaction: after the
// first parallel is removed, FirstEdgeHandle returns the next survivor's
// handle.
func TestGraph_FirstEdgeHandle(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	// Unknown pair: not present.
	if h, ok := g.FirstEdgeHandle("a", "b"); ok || h != 0 {
		t.Fatalf("FirstEdgeHandle on empty graph = (%d, %v), want (0, false)", h, ok)
	}

	h1, _ := g.AddEdgeH("a", "b", 0)
	h2, _ := g.AddEdgeH("a", "b", 0)

	// The first slot carries h1 (insertion order).
	if h, ok := g.FirstEdgeHandle("a", "b"); !ok || h != h1 {
		t.Fatalf("FirstEdgeHandle = (%d, %v), want (%d, true)", h, ok, h1)
	}

	// RemoveEdge drops the first slot (h1); the survivor h2 becomes first.
	g.RemoveEdge("a", "b")
	if h, ok := g.FirstEdgeHandle("a", "b"); !ok || h != h2 {
		t.Fatalf("after one delete, FirstEdgeHandle = (%d, %v), want (%d, true)", h, ok, h2)
	}

	// Remove the last edge: pair empty again.
	g.RemoveEdge("a", "b")
	if h, ok := g.FirstEdgeHandle("a", "b"); ok || h != 0 {
		t.Fatalf("after full delete, FirstEdgeHandle = (%d, %v), want (0, false)", h, ok)
	}
}

// TestGraph_FirstEdgeHandle_NoHandleSlot verifies the 0-sentinel case: a
// plain AddEdge (no handle) makes FirstEdgeHandle report (0, false).
func TestGraph_FirstEdgeHandle_NoHandleSlot(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if h, ok := g.FirstEdgeHandle("a", "b"); ok || h != 0 {
		t.Fatalf("FirstEdgeHandle on no-handle edge = (%d, %v), want (0, false)", h, ok)
	}
}

// TestGraph_AddEdgeH_Concurrent verifies handle allocation is safe and
// produces a contiguous, unique set under concurrent writers.
func TestGraph_AddEdgeH_Concurrent(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	const writers, perWriter = 8, 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	handles := make([]uint64, 0, writers*perWriter)

	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			local := make([]uint64, 0, perWriter)
			for i := 0; i < perWriter; i++ {
				h, err := g.AddEdgeH("a", "b", 0)
				if err != nil {
					t.Errorf("AddEdgeH: %v", err)
					return
				}
				local = append(local, h)
			}
			mu.Lock()
			handles = append(handles, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(handles) != writers*perWriter {
		t.Fatalf("collected %d handles, want %d", len(handles), writers*perWriter)
	}
	sort.Slice(handles, func(i, j int) bool { return handles[i] < handles[j] })
	for i, h := range handles {
		want := uint64(i + 1) // 1-based, contiguous, unique
		if h != want {
			t.Fatalf("sorted handle[%d] = %d, want %d (unique & contiguous)", i, h, want)
		}
	}
}
