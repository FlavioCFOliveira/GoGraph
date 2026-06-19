package lpg

// edge_instance_tier_test.go — regression gate for sprint 221 / #1633: the
// per-CREATE-instance and per-handle edge property/label stores now hold their
// innermost set as the tiered propBag/labelBag. This verifies membership and
// read-back are complete across the singleton/small/map tier transitions for
// all four stores. Layer: short.

import (
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestEdgeInstanceAndHandleTiering(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddEdge("a", "b", 1.0); err != nil {
		t.Fatal(err)
	}

	const n = 12 // crosses the smallLabelMax/smallBagMax=8 promotion threshold
	wantLabels := make([]string, n)
	wantProps := map[string]bool{}
	for i := 0; i < n; i++ {
		lbl := "L" + string(rune('a'+i))
		wantLabels[i] = lbl
		pk := "p" + string(rune('a'+i))
		wantProps[pk] = true
		// per-instance (idx-keyed)
		g.SetEdgeLabelAt("a", "b", 1, lbl)
		if err := g.SetEdgePropertyAt("a", "b", 1, pk, Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
		// per-handle
		g.SetEdgeLabelByHandle("a", "b", 7, lbl)
		if err := g.SetEdgePropertyByHandle("a", "b", 7, pk, Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
	}

	sort.Strings(wantLabels)
	checkLabels := func(name string, got []string) {
		sort.Strings(got)
		if len(got) != n {
			t.Fatalf("%s: got %d labels, want %d (%v)", name, len(got), n, got)
		}
		for i := range got {
			if got[i] != wantLabels[i] {
				t.Fatalf("%s: label %d = %q, want %q", name, i, got[i], wantLabels[i])
			}
		}
	}
	checkProps := func(name string, got map[string]PropertyValue) {
		if len(got) != n {
			t.Fatalf("%s: got %d props, want %d", name, len(got), n)
		}
		for k := range wantProps {
			if _, ok := got[k]; !ok {
				t.Fatalf("%s: missing prop %q", name, k)
			}
		}
	}

	checkLabels("EdgeLabelsAt", g.EdgeLabelsAt("a", "b", 1))
	checkProps("EdgePropertiesAt", g.EdgePropertiesAt("a", "b", 1))
	checkLabels("EdgeLabelsByHandle", g.EdgeLabelsByHandle("a", "b", 7))
	checkProps("EdgePropertiesByHandle", g.EdgePropertiesByHandle("a", "b", 7))

	// RemoveEdgeInstance / RemoveEdgeInstanceByHandle clear the stores.
	g.RemoveEdgeInstance("a", "b", 1)
	if got := g.EdgeLabelsAt("a", "b", 1); got != nil {
		t.Fatalf("EdgeLabelsAt after RemoveEdgeInstance = %v, want nil", got)
	}
	if got := g.EdgePropertiesAt("a", "b", 1); got != nil {
		t.Fatalf("EdgePropertiesAt after RemoveEdgeInstance = %v, want nil", got)
	}
	g.RemoveEdgeInstanceByHandle("a", "b", 7)
	if got := g.EdgeLabelsByHandle("a", "b", 7); got != nil {
		t.Fatalf("EdgeLabelsByHandle after remove = %v, want nil", got)
	}
	if got := g.EdgePropertiesByHandle("a", "b", 7); got != nil {
		t.Fatalf("EdgePropertiesByHandle after remove = %v, want nil", got)
	}
}
