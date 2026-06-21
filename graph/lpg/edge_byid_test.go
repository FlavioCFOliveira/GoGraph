package lpg

import (
	"reflect"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestEdgeByID_EquivalentToKeyed asserts the lock-free NodeID-keyed edge
// accessors added for the snapshot-collector deadlock fix (#1648),
// [Graph.EdgeLabelsByID] and [Graph.EdgePropertiesByID], return results
// identical to their external-key counterparts. EdgeLabels and EdgeProperties
// now resolve both endpoint NodeIDs and delegate to the ByID variants, so the
// two paths must agree for every edge — including the multi-label overflow path
// and a parallel-edge (multigraph) pair whose properties coalesce latest-wins.
func TestEdgeByID_EquivalentToKeyed(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})

	// a->b: two labels (exercises the inline slot + overflow store) and two
	// properties.
	if err := g.AddEdgeLabeled("a", "b", 1, "KNOWS"); err != nil {
		t.Fatalf("AddEdgeLabeled(a,b,KNOWS): %v", err)
	}
	g.SetEdgeLabel("a", "b", "WORKS_WITH")
	if err := g.SetEdgeProperty("a", "b", "since", Int64Value(2020)); err != nil {
		t.Fatalf("SetEdgeProperty(since): %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "weight", Int64Value(5)); err != nil {
		t.Fatalf("SetEdgeProperty(weight): %v", err)
	}
	// a parallel a->b edge (multigraph): its property coalesces latest-wins into
	// the pair's EdgeProperties view.
	if err := g.AddEdgeLabeledWithProperty("a", "b", 1, "KNOWS", "since", Int64Value(2021)); err != nil {
		t.Fatalf("AddEdgeLabeledWithProperty(a,b): %v", err)
	}
	// a->c: single label, single property.
	if err := g.AddEdgeLabeledWithProperty("a", "c", 1, "LIKES", "rating", Int64Value(9)); err != nil {
		t.Fatalf("AddEdgeLabeledWithProperty(a,c): %v", err)
	}

	for _, e := range []struct{ src, dst string }{{"a", "b"}, {"a", "c"}} {
		srcID, ok := g.AdjList().Mapper().Lookup(e.src)
		if !ok {
			t.Fatalf("Lookup(%s) missing", e.src)
		}
		dstID, ok := g.AdjList().Mapper().Lookup(e.dst)
		if !ok {
			t.Fatalf("Lookup(%s) missing", e.dst)
		}

		keyedLabels := g.EdgeLabels(e.src, e.dst)
		byIDLabels := g.EdgeLabelsByID(srcID, dstID)
		sort.Strings(keyedLabels)
		sort.Strings(byIDLabels)
		if !reflect.DeepEqual(keyedLabels, byIDLabels) {
			t.Errorf("edge (%s,%s) labels: keyed=%v byID=%v", e.src, e.dst, keyedLabels, byIDLabels)
		}

		keyedProps := g.EdgeProperties(e.src, e.dst)
		byIDProps := g.EdgePropertiesByID(srcID, dstID)
		if !reflect.DeepEqual(keyedProps, byIDProps) {
			t.Errorf("edge (%s,%s) properties: keyed=%v byID=%v", e.src, e.dst, keyedProps, byIDProps)
		}
	}

	// An unknown endpoint NodeID resolves to no labels/properties, mirroring the
	// nil return of the key-based accessors for an absent edge.
	const absent = 1 << 40
	if got := g.EdgeLabelsByID(absent, absent); got != nil {
		t.Errorf("EdgeLabelsByID(absent) = %v, want nil", got)
	}
	if got := g.EdgePropertiesByID(absent, absent); got != nil {
		t.Errorf("EdgePropertiesByID(absent) = %v, want nil", got)
	}
}
