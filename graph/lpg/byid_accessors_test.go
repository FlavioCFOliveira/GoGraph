package lpg

import (
	"reflect"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestNodePropertiesByID_MatchesByKey verifies that the NodeID-keyed property
// accessor returns exactly what the external-key accessor returns, for nodes
// with zero, one and several properties. The Cypher result-materialisation
// path relies on this equivalence: it resolves the NodeID once for identity
// and then reads properties/labels by NodeID instead of by key.
// mustNoErr fails the test immediately if err is non-nil. Used to keep the
// fixture setup terse while still checking every error (errcheck).
func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func TestNodePropertiesByID_MatchesByKey(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	mustNoErr(t, g.AddNode("empty"))
	mustNoErr(t, g.AddNode("one"))
	mustNoErr(t, g.SetNodeProperty("one", "name", StringValue("Alice")))
	mustNoErr(t, g.AddNode("many"))
	mustNoErr(t, g.SetNodeProperty("many", "name", StringValue("Bob")))
	mustNoErr(t, g.SetNodeProperty("many", "age", Int64Value(42)))
	mustNoErr(t, g.SetNodeProperty("many", "active", BoolValue(true)))

	for _, key := range []string{"empty", "one", "many", "missing"} {
		id, ok := g.AdjList().Mapper().Lookup(key)
		if !ok {
			if key != "missing" {
				t.Fatalf("key %q not interned", key)
			}
			continue
		}
		byKey := g.NodeProperties(key)
		byID := g.NodePropertiesByID(id)
		if !reflect.DeepEqual(byKey, byID) {
			t.Fatalf("key %q: NodeProperties=%v NodePropertiesByID=%v", key, byKey, byID)
		}
	}

	// A NodeID never assigned must yield nil, not panic.
	if got := g.NodePropertiesByID(graph.NodeID(1 << 40)); got != nil {
		t.Fatalf("unknown NodeID: want nil, got %v", got)
	}
}

// TestNodePropertiesByIDFunc_MatchesByID verifies that the streaming visitor
// accessor visits exactly the same (name, value) pairs as the map-returning
// NodePropertiesByID, for nodes with zero, one and several properties, and that
// it visits nothing for an unknown NodeID. This equivalence is the contract the
// Cypher result-materialisation path relies on to build expr.MapValue directly
// without the intermediate map[string]PropertyValue (#1502).
func TestNodePropertiesByIDFunc_MatchesByID(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	mustNoErr(t, g.AddNode("empty"))
	mustNoErr(t, g.AddNode("one"))
	mustNoErr(t, g.SetNodeProperty("one", "name", StringValue("Alice")))
	mustNoErr(t, g.AddNode("many"))
	mustNoErr(t, g.SetNodeProperty("many", "name", StringValue("Bob")))
	mustNoErr(t, g.SetNodeProperty("many", "age", Int64Value(42)))
	mustNoErr(t, g.SetNodeProperty("many", "active", BoolValue(true)))

	for _, key := range []string{"empty", "one", "many"} {
		id, ok := g.AdjList().Mapper().Lookup(key)
		if !ok {
			t.Fatalf("key %q not interned", key)
		}
		want := g.NodePropertiesByID(id) // nil for the propertyless node
		got := make(map[string]PropertyValue)
		g.NodePropertiesByIDFunc(id, func(name string, pv PropertyValue) {
			if _, dup := got[name]; dup {
				t.Fatalf("key %q: visitor saw %q twice", key, name)
			}
			got[name] = pv
		})
		// Normalise: NodePropertiesByID returns nil (not an empty map) for a
		// node with no properties; the visitor simply never fires.
		if len(want) == 0 {
			if len(got) != 0 {
				t.Fatalf("key %q: want no visits, got %v", key, got)
			}
			continue
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("key %q: NodePropertiesByID=%v visitor=%v", key, want, got)
		}
	}

	// An unknown NodeID must never invoke the visitor and must not panic.
	calls := 0
	g.NodePropertiesByIDFunc(graph.NodeID(1<<40), func(string, PropertyValue) { calls++ })
	if calls != 0 {
		t.Fatalf("unknown NodeID: visitor fired %d times, want 0", calls)
	}
}

// TestNodeLabelsByID_MatchesByKey verifies the NodeID-keyed label accessor
// matches the external-key accessor (order-independent).
func TestNodeLabelsByID_MatchesByKey(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	mustNoErr(t, g.AddNode("none"))
	mustNoErr(t, g.AddNode("single"))
	mustNoErr(t, g.SetNodeLabel("single", "Person"))
	mustNoErr(t, g.AddNode("multi"))
	mustNoErr(t, g.SetNodeLabel("multi", "Person"))
	mustNoErr(t, g.SetNodeLabel("multi", "Admin"))

	for _, key := range []string{"none", "single", "multi"} {
		id, ok := g.AdjList().Mapper().Lookup(key)
		if !ok {
			t.Fatalf("key %q not interned", key)
		}
		byKey := append([]string(nil), g.NodeLabels(key)...)
		byID := append([]string(nil), g.NodeLabelsByID(id)...)
		sort.Strings(byKey)
		sort.Strings(byID)
		if !reflect.DeepEqual(byKey, byID) {
			t.Fatalf("key %q: NodeLabels=%v NodeLabelsByID=%v", key, byKey, byID)
		}
	}

	if got := g.NodeLabelsByID(graph.NodeID(1 << 40)); got != nil {
		t.Fatalf("unknown NodeID: want nil, got %v", got)
	}
}
