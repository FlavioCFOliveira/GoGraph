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

// TestForEachEdgeProperty_MatchesMap asserts that the streaming edge-property
// accessors added for M2 (#1662) — [Graph.ForEachEdgeProperty] and
// [Graph.ForEachEdgePropertyByID] — visit exactly the (name, value) pairs the
// map-returning [Graph.EdgeProperties] / [Graph.EdgePropertiesByID] contain,
// when the consumer applies the same last-write-wins a map build does. This is
// the value-identity contract the Cypher relationship-materialisation path
// relies on to build expr.MapValue directly without the intermediate
// map[string]PropertyValue (M2 double-allocation). It covers the empty,
// single-property, multi-property, parallel-edge-coalescing, and cross-kind
// same-key cases that exercise every coalescing branch.
func TestForEachEdgeProperty_MatchesMap(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})

	// a->b: two distinct properties of different kinds.
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge(a,b): %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "since", Int64Value(2020)); err != nil {
		t.Fatalf("SetEdgeProperty(since): %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "label", StringValue("first")); err != nil {
		t.Fatalf("SetEdgeProperty(label): %v", err)
	}
	// A parallel a->b edge carrying a newer `since` (latest-wins coalescing).
	if err := g.AddEdgeLabeledWithProperty("a", "b", 1, "KNOWS", "since", Int64Value(2021)); err != nil {
		t.Fatalf("AddEdgeLabeledWithProperty(a,b): %v", err)
	}
	// a->c: a single property whose key was first written as an int then
	// overwritten by a string — the cross-kind same-key path. The latest write
	// (string) must be the only value the coalesced view reports.
	if err := g.AddEdge("a", "c", 1); err != nil {
		t.Fatalf("AddEdge(a,c): %v", err)
	}
	if err := g.SetEdgeProperty("a", "c", "k", Int64Value(7)); err != nil {
		t.Fatalf("SetEdgeProperty(k,int): %v", err)
	}
	if err := g.SetEdgeProperty("a", "c", "k", StringValue("seven")); err != nil {
		t.Fatalf("SetEdgeProperty(k,string): %v", err)
	}
	// a->d: no properties (the streamer must never fire).
	if err := g.AddEdge("a", "d", 1); err != nil {
		t.Fatalf("AddEdge(a,d): %v", err)
	}

	for _, e := range []struct{ src, dst string }{{"a", "b"}, {"a", "c"}, {"a", "d"}} {
		srcID, ok := g.AdjList().Mapper().Lookup(e.src)
		if !ok {
			t.Fatalf("Lookup(%s) missing", e.src)
		}
		dstID, ok := g.AdjList().Mapper().Lookup(e.dst)
		if !ok {
			t.Fatalf("Lookup(%s) missing", e.dst)
		}

		want := g.EdgePropertiesByID(srcID, dstID) // nil for the propertyless pair

		// ForEachEdgePropertyByID, building a map with last-write-wins, must
		// reproduce the coalesced map byte-for-byte.
		gotByID := make(map[string]PropertyValue)
		g.ForEachEdgePropertyByID(srcID, dstID, func(name string, pv PropertyValue) {
			gotByID[name] = pv // last-write-wins, exactly as EdgePropertiesByID does
		})
		assertStreamedEqualsMap(t, e.src, e.dst, "ForEachEdgePropertyByID", want, gotByID)

		// The key-based ForEachEdgeProperty must agree with the ByID variant.
		gotByKey := make(map[string]PropertyValue)
		g.ForEachEdgeProperty(e.src, e.dst, func(name string, pv PropertyValue) {
			gotByKey[name] = pv
		})
		assertStreamedEqualsMap(t, e.src, e.dst, "ForEachEdgeProperty", want, gotByKey)
	}

	// An unknown endpoint must never invoke the visitor and must not panic,
	// through either entry point.
	const absent = 1 << 40
	calls := 0
	g.ForEachEdgePropertyByID(absent, absent, func(string, PropertyValue) { calls++ })
	g.ForEachEdgeProperty("nope", "nope", func(string, PropertyValue) { calls++ })
	if calls != 0 {
		t.Fatalf("unknown endpoint: visitor fired %d times, want 0", calls)
	}
}

// assertStreamedEqualsMap fails the test unless the map built by streaming
// (got) equals the map-returning accessor's result (want), normalising the
// propertyless case where the accessor returns nil and the streamer never fires.
func assertStreamedEqualsMap(t *testing.T, src, dst, who string, want, got map[string]PropertyValue) {
	t.Helper()
	if len(want) == 0 {
		if len(got) != 0 {
			t.Fatalf("edge (%s,%s) %s: want no visits, got %v", src, dst, who, got)
		}
		return
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("edge (%s,%s) %s: map=%v streamed=%v", src, dst, who, want, got)
	}
}
