package lpg_test

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// sortedSet returns a sorted copy of in so set equality can be asserted
// order-independently.
func sortedSet(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa, sb := sortedSet(a), sortedSet(b)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// TestInUseEnumerators_Empty asserts the empty-graph contract: a non-nil,
// empty slice from every enumerator.
func TestInUseEnumerators_Empty(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	for _, tc := range []struct {
		name string
		got  []string
	}{
		{"NodeLabelsInUse", g.NodeLabelsInUse()},
		{"RelationshipTypesInUse", g.RelationshipTypesInUse()},
		{"PropertyKeysInUse", g.PropertyKeysInUse()},
	} {
		if tc.got == nil {
			t.Errorf("%s on empty graph = nil, want non-nil empty slice", tc.name)
		}
		if len(tc.got) != 0 {
			t.Errorf("%s on empty graph = %v, want empty", tc.name, tc.got)
		}
	}
}

// buildSampleGraph wires a small graph exercising multiple node labels
// (some shared across nodes), several edge types, and node + edge
// properties.
func buildSampleGraph(t *testing.T) *lpg.Graph[string, int64] {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
	}

	// Node labels: Person on a and b (shared), Company on c, Admin on a only.
	mustSetNodeLabel(t, g, "a", "Person")
	mustSetNodeLabel(t, g, "b", "Person")
	mustSetNodeLabel(t, g, "c", "Company")
	mustSetNodeLabel(t, g, "a", "Admin")

	// Edges with distinct relationship types.
	mustAddEdge(t, g, "a", "b")
	mustAddEdge(t, g, "a", "c")
	g.SetEdgeLabel("a", "b", "KNOWS")
	g.SetEdgeLabel("a", "c", "WORKS_AT")

	// Node properties.
	if err := g.SetNodeProperty("a", "name", lpg.StringValue("Alice")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("a", "age", lpg.Int64Value(30)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("c", "founded", lpg.Int64Value(1999)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	// Edge property (key disjoint from node keys to test the union).
	if err := g.SetEdgeProperty("a", "b", "since", lpg.Int64Value(2020)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

	return g
}

func mustSetNodeLabel(t *testing.T, g *lpg.Graph[string, int64], n, name string) {
	t.Helper()
	if err := g.SetNodeLabel(n, name); err != nil {
		t.Fatalf("SetNodeLabel(%q, %q): %v", n, name, err)
	}
}

func mustAddEdge(t *testing.T, g *lpg.Graph[string, int64], src, dst string) {
	t.Helper()
	if err := g.AddEdge(src, dst, 1); err != nil {
		t.Fatalf("AddEdge(%q, %q): %v", src, dst, err)
	}
}

// TestInUseEnumerators_Populated checks exact expected sets on a populated
// graph.
func TestInUseEnumerators_Populated(t *testing.T) {
	t.Parallel()
	g := buildSampleGraph(t)

	if got, want := g.NodeLabelsInUse(), []string{"Person", "Company", "Admin"}; !equalSet(got, want) {
		t.Errorf("NodeLabelsInUse = %v, want set %v", got, want)
	}
	if got, want := g.RelationshipTypesInUse(), []string{"KNOWS", "WORKS_AT"}; !equalSet(got, want) {
		t.Errorf("RelationshipTypesInUse = %v, want set %v", got, want)
	}
	if got, want := g.PropertyKeysInUse(), []string{"name", "age", "founded", "since"}; !equalSet(got, want) {
		t.Errorf("PropertyKeysInUse = %v, want set %v", got, want)
	}
}

// TestInUseEnumerators_TombstoneFiltering verifies that removing a node
// retires the names it solely bore, while names still carried by a
// survivor remain.
func TestInUseEnumerators_TombstoneFiltering(t *testing.T) {
	t.Parallel()
	g := buildSampleGraph(t)

	// Remove c: it solely bore label Company, property key "founded", and
	// it is the dst endpoint of the only WORKS_AT edge. After removal those
	// three names must disappear; shared names (Person via a/b, "name"/"age"
	// via a, KNOWS via a->b) must survive.
	g.RemoveNode("c")

	t.Run("node-labels", func(t *testing.T) {
		got := g.NodeLabelsInUse()
		want := []string{"Person", "Admin"}
		if !equalSet(got, want) {
			t.Errorf("NodeLabelsInUse after remove = %v, want set %v", got, want)
		}
		for _, n := range got {
			if n == "Company" {
				t.Errorf("Company should be retired after removing its sole bearer")
			}
		}
	})

	t.Run("rel-types", func(t *testing.T) {
		got := g.RelationshipTypesInUse()
		want := []string{"KNOWS"}
		if !equalSet(got, want) {
			t.Errorf("RelationshipTypesInUse after remove = %v, want set %v", got, want)
		}
	})

	t.Run("property-keys", func(t *testing.T) {
		got := g.PropertyKeysInUse()
		// "founded" gone (sole bearer c removed); a's keys + the edge key survive.
		want := []string{"name", "age", "since"}
		if !equalSet(got, want) {
			t.Errorf("PropertyKeysInUse after remove = %v, want set %v", got, want)
		}
	})
}

// TestInUseEnumerators_EdgeRetiredWhenEndpointRemoved confirms an edge
// type / edge property key is retired when only one endpoint is removed
// (both endpoints must be live).
func TestInUseEnumerators_EdgeRetiredWhenEndpointRemoved(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	mustAddEdge(t, g, "x", "y")
	g.SetEdgeLabel("x", "y", "LINK")
	if err := g.SetEdgeProperty("x", "y", "w", lpg.Float64Value(1.5)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

	if got := g.RelationshipTypesInUse(); !equalSet(got, []string{"LINK"}) {
		t.Fatalf("pre-remove RelationshipTypesInUse = %v, want [LINK]", got)
	}

	g.RemoveNode("y") // dst endpoint gone

	if got := g.RelationshipTypesInUse(); len(got) != 0 {
		t.Errorf("RelationshipTypesInUse after endpoint removal = %v, want empty", got)
	}
	if got := g.PropertyKeysInUse(); len(got) != 0 {
		t.Errorf("PropertyKeysInUse after endpoint removal = %v, want empty", got)
	}
}

// TestInUseEnumerators_Concurrency is a short-layer race smoke test: it
// runs the three enumerators concurrently with mutations under -race. No
// assertion on results — only that the run is race-clean.
func TestInUseEnumerators_Concurrency(t *testing.T) {
	t.Parallel()
	g := lpg.New[int, int64](adjlist.Config{Directed: true})

	const (
		writers  = 4
		readers  = 4
		perGroup = 200
	)
	stop := make(chan struct{})
	var writersWG, readersWG sync.WaitGroup

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(base int) {
			defer writersWG.Done()
			for i := 0; i < perGroup; i++ {
				n := base*perGroup + i
				_ = g.SetNodeLabel(n, "Person")
				_ = g.SetNodeProperty(n, "k", lpg.Int64Value(int64(i)))
				if i > 0 {
					_ = g.AddEdge(n, n-1, 1)
					g.SetEdgeLabel(n, n-1, "KNOWS")
				}
				if i%7 == 0 {
					g.RemoveNode(n)
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = g.NodeLabelsInUse()
					_ = g.RelationshipTypesInUse()
					_ = g.PropertyKeysInUse()
				}
			}
		}()
	}

	// Let writers finish, then stop readers.
	done := make(chan struct{})
	go func() {
		writersWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("writers did not finish in time")
	}
	close(stop)
	readersWG.Wait()
}
