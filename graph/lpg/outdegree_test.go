package lpg_test

// outdegree_test.go — the degree primitive is a substitute for enumeration
// (task #2218).
//
// A degree is only useful to the planner if it equals the number of rows the
// equivalent expansion produces. So these tests never compare against a
// hand-computed constant: they compare against the traversal itself. If the two
// ever disagree, the primitive is not substitutable and the tests say so.
//
// The cases that matter are the ones where a naive implementation diverges:
//
//   - a MULTIGRAPH, where parallel edges each occupy their own slot;
//   - an UNDIRECTED graph, where insertion is mirrored so the adjacency already
//     holds every incident edge;
//   - a TOMBSTONED endpoint, which RemoveNode leaves in other nodes' adjacency
//     — a raw slot count would include an edge the query layer treats as absent;
//   - a SELF-LOOP, which must count exactly as the traversal counts it;
//   - a node with no edges, and a node that was never interned.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// enumerateOutDegree counts src's out-neighbours the way a traversal would: by
// walking them and skipping any whose endpoint is tombstoned. This is the
// oracle — the number the primitive must reproduce.
func enumerateOutDegree(g *lpg.Graph[string, float64], src string) int {
	n := 0
	for dst := range g.AdjList().Neighbours(src) {
		id, ok := g.AdjList().Mapper().Lookup(dst)
		if !ok {
			continue
		}
		if g.IsTombstoned(id) {
			continue
		}
		n++
	}
	return n
}

// enumerateOutDegreeByType is the oracle for the type-filtered count: it walks
// src's edge INSTANCES and counts those whose own relationship type is relType.
//
// Per-instance is the only correct granularity here, and the property test
// proved it: Graph.HasEdgeLabel is a PER-PAIR check, so on a multigraph holding
// one typed and one untyped edge between the same two nodes it reports the type
// for BOTH. Using it as the oracle claimed a degree of 2 where 1 is right. The
// engine matches relationship type per instance (see the parallel-edge work in
// #1685 and #2016), so a degree that is substitutable for an expansion must too.
//
// The walk decodes the adjacency's stored slot label through the lpg codec, which
// is exactly the boundary the first implementation got wrong: it compared a raw
// LabelID against a column holding encodeSlotLabel(id).
func enumerateOutDegreeByType(g *lpg.Graph[string, float64], src, relType string) int {
	want, known := g.Registry().Lookup(relType)
	if !known {
		return 0
	}
	n := 0
	_, _ = g.AdjList().OutDegreeFunc(src, func(dst graph.NodeID, slotLabel uint32) bool {
		if g.IsTombstoned(dst) {
			return false
		}
		lid, hasLabel := lpg.DecodeSlotLabelForTest(slotLabel)
		if hasLabel && lid == want {
			n++
		}
		return false // counting is done here; the return value is unused
	})
	return n
}

func newDegreeGraph(t *testing.T, cfg adjlist.Config) *lpg.Graph[string, float64] {
	t.Helper()
	return lpg.New[string, float64](cfg)
}

// TestOutDegreeMatchesEnumeration is the core differential, run across every
// graph configuration.
func TestOutDegreeMatchesEnumeration(t *testing.T) {
	t.Parallel()

	configs := []struct {
		name string
		cfg  adjlist.Config
	}{
		{"directed", adjlist.Config{Directed: true}},
		{"directed-multigraph", adjlist.Config{Directed: true, Multigraph: true}},
		{"undirected", adjlist.Config{}},
		{"undirected-multigraph", adjlist.Config{Multigraph: true}},
	}

	for _, c := range configs {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			g := newDegreeGraph(t, c.cfg)

			// A hub with several distinct neighbours, an isolated node, a
			// self-loop, and a node reached by two parallel edges.
			for _, n := range []string{"hub", "a", "b", "c", "lonely", "loop"} {
				if err := g.AddNode(n); err != nil {
					t.Fatalf("AddNode %q: %v", n, err)
				}
			}
			for _, dst := range []string{"a", "b", "c"} {
				if err := g.AddEdge("hub", dst, 1); err != nil {
					t.Fatalf("AddEdge hub->%s: %v", dst, err)
				}
			}
			if err := g.AddEdge("loop", "loop", 1); err != nil {
				t.Fatalf("AddEdge self-loop: %v", err)
			}
			if c.cfg.Multigraph {
				// A second hub->a edge: in a multigraph it is its own slot.
				if err := g.AddEdge("hub", "a", 2); err != nil {
					t.Fatalf("AddEdge parallel: %v", err)
				}
			}

			for _, node := range []string{"hub", "a", "b", "c", "lonely", "loop"} {
				want := enumerateOutDegree(g, node)
				got, ok := g.OutDegree(node)
				if !ok {
					t.Errorf("%s: OutDegree reported not-interned for an interned node", node)
					continue
				}
				if got != want {
					t.Errorf("%s: OutDegree = %d, enumeration = %d", node, got, want)
				}
			}
		})
	}
}

// TestOutDegreeAfterTombstone is the case a raw slot count gets wrong.
// RemoveNode tombstones the node and strips its label bitmaps but leaves the
// edges other nodes hold to it, so the adjacency slot survives while the
// traversal stops yielding it.
func TestOutDegreeAfterTombstone(t *testing.T) {
	t.Parallel()

	g := newDegreeGraph(t, adjlist.Config{Directed: true})
	for _, n := range []string{"hub", "a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for _, dst := range []string{"a", "b", "c"} {
		if err := g.AddEdge("hub", dst, 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}

	if got, _ := g.OutDegree("hub"); got != 3 {
		t.Fatalf("before tombstone: OutDegree = %d, want 3", got)
	}

	g.RemoveNode("b")

	// The adjacency still holds hub->b, so the raw slot count is stale.
	rawSlots, _ := g.AdjList().OutDegree("hub")
	if rawSlots != 3 {
		t.Logf("adjacency slots after tombstone = %d (implementation may strip eagerly)", rawSlots)
	}

	want := enumerateOutDegree(g, "hub")
	if want != 2 {
		t.Fatalf("oracle says %d live out-neighbours, expected 2 after tombstoning one", want)
	}
	got, ok := g.OutDegree("hub")
	if !ok {
		t.Fatal("OutDegree reported not-interned")
	}
	if got != want {
		t.Errorf("OutDegree = %d, enumeration = %d; a tombstoned endpoint must not be counted", got, want)
	}
}

// TestOutDegreeByTypeMatchesEnumeration pins the type-filtered count against the
// traversal, which also settles which label store is authoritative.
func TestOutDegreeByTypeMatchesEnumeration(t *testing.T) {
	t.Parallel()

	g := newDegreeGraph(t, adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"hub", "a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := g.AddEdgeLabeled("hub", "a", 1, "KNOWS"); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}
	if err := g.AddEdgeLabeled("hub", "b", 1, "KNOWS"); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}
	if err := g.AddEdgeLabeled("hub", "c", 1, "FOLLOWS"); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}

	knows, ok := g.Registry().Lookup("KNOWS")
	if !ok {
		t.Fatal("KNOWS was not interned")
	}
	follows, ok := g.Registry().Lookup("FOLLOWS")
	if !ok {
		t.Fatal("FOLLOWS was not interned")
	}

	for _, tc := range []struct {
		name    string
		relType lpg.LabelID
		label   string
	}{
		{"KNOWS", knows, "KNOWS"},
		{"FOLLOWS", follows, "FOLLOWS"},
	} {
		want := enumerateOutDegreeByType(g, "hub", tc.label)
		got, ok := g.OutDegreeByType("hub", tc.relType)
		if !ok {
			t.Errorf("%s: OutDegreeByType reported not-interned", tc.name)
			continue
		}
		if got != want {
			t.Errorf("%s: OutDegreeByType = %d, enumeration = %d", tc.name, got, want)
		}
	}

	// The total must still agree with the unfiltered count.
	total, _ := g.OutDegree("hub")
	if want := enumerateOutDegree(g, "hub"); total != want {
		t.Errorf("OutDegree = %d, enumeration = %d", total, want)
	}
}

// TestOutDegreeUninternedNode pins the ok contract: a node the graph has never
// seen is distinguishable from a node with no edges.
func TestOutDegreeUninternedNode(t *testing.T) {
	t.Parallel()

	g := newDegreeGraph(t, adjlist.Config{Directed: true})
	if err := g.AddNode("known"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	if got, ok := g.OutDegree("known"); !ok || got != 0 {
		t.Errorf("interned node with no edges: got (%d, %v), want (0, true)", got, ok)
	}
	if got, ok := g.OutDegree("never-seen"); ok || got != 0 {
		t.Errorf("uninterned node: got (%d, %v), want (0, false)", got, ok)
	}
	if got, ok := g.AdjList().OutDegree("never-seen"); ok || got != 0 {
		t.Errorf("AdjList uninterned: got (%d, %v), want (0, false)", got, ok)
	}
}

// TestOutDegreeFuncAgreesWithOutDegree pins that the predicate form reduces to
// the O(1) form when the predicate admits everything, which is the property the
// lpg wrapper relies on.
func TestOutDegreeFuncAgreesWithOutDegree(t *testing.T) {
	t.Parallel()

	g := newDegreeGraph(t, adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < 8; i++ {
		if err := g.AddEdge("hub", fmt.Sprintf("n%d", i), 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	fast, ok1 := g.AdjList().OutDegree("hub")
	slow, ok2 := g.AdjList().OutDegreeFunc("hub", func(graph.NodeID, uint32) bool { return true })
	if !ok1 || !ok2 {
		t.Fatal("both forms must report interned")
	}
	if fast != slow {
		t.Errorf("OutDegree = %d, OutDegreeFunc(always-true) = %d; the two must agree", fast, slow)
	}
}
