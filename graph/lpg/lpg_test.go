package lpg

import (
	"sort"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestLabelRegistry(t *testing.T) {
	t.Parallel()
	r := NewLabelRegistry()
	a := r.Intern("Person")
	b := r.Intern("Account")
	if a == b {
		t.Fatalf("distinct names should produce distinct IDs")
	}
	if r.Intern("Person") != a {
		t.Fatalf("Intern not idempotent")
	}
	if _, ok := r.Lookup("Person"); !ok {
		t.Fatalf("Lookup should find Person")
	}
	if _, ok := r.Lookup("Unknown"); ok {
		t.Fatalf("Lookup should miss Unknown")
	}
	if name, _ := r.Resolve(a); name != "Person" {
		t.Fatalf("Resolve %d = %q, want Person", a, name)
	}
	if _, ok := r.Resolve(LabelID(9999)); ok {
		t.Fatalf("Resolve must reject unknown ID")
	}
}

func TestGraph_NodeLabels(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("alice", "Active"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("bob", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if !g.HasNodeLabel("alice", "Person") || !g.HasNodeLabel("alice", "Active") {
		t.Fatalf("alice should carry Person + Active")
	}
	if g.HasNodeLabel("alice", "Inactive") {
		t.Fatalf("alice should not carry Inactive")
	}
	labels := g.NodeLabels("alice")
	sort.Strings(labels)
	if len(labels) != 2 || labels[0] != "Active" || labels[1] != "Person" {
		t.Fatalf("NodeLabels(alice) = %v", labels)
	}
	g.RemoveNodeLabel("alice", "Active")
	if g.HasNodeLabel("alice", "Active") {
		t.Fatalf("Active should be gone after RemoveNodeLabel")
	}
}

func TestGraph_EdgeLabels(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel("alice", "bob", "KNOWS")
	g.SetEdgeLabel("alice", "bob", "FOLLOWS")
	if !g.HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatalf("edge should carry KNOWS")
	}
	if g.HasEdgeLabel("alice", "bob", "BLOCKS") {
		t.Fatalf("edge must not carry unrecorded label")
	}
}

func TestGraph_SetEdgeLabel_NoEdge(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	g.SetEdgeLabel("alice", "bob", "KNOWS") // no edge yet
	if g.HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatalf("SetEdgeLabel on missing edge must be a no-op")
	}
}

func TestGraph_LabelIndex_Query(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"alice", "bob", "charlie"} {
		if err := g.SetNodeLabel(n, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	if err := g.SetNodeLabel("alice", "Active"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("charlie", "Active"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	personID, _ := g.Registry().Lookup("Person")
	activeID, _ := g.Registry().Lookup("Active")
	persons := g.NodeIndex().Intersect(uint32(personID))
	if persons.GetCardinality() != 3 {
		t.Fatalf("Persons cardinality = %d, want 3", persons.GetCardinality())
	}
	activePersons := g.NodeIndex().Intersect(uint32(personID), uint32(activeID))
	if activePersons.GetCardinality() != 2 {
		t.Fatalf("Active Persons cardinality = %d, want 2", activePersons.GetCardinality())
	}
}

func TestGraph_Concurrent(t *testing.T) {
	t.Parallel()
	g := New[int, int64](adjlist.Config{Directed: true, Multigraph: false})
	var wg sync.WaitGroup
	const goroutines = 64
	const perWorker = 128
	wg.Add(goroutines)
	for w := 0; w < goroutines; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				n := w*perWorker + i
				if err := g.SetNodeLabel(n, "Person"); err != nil {
					t.Errorf("SetNodeLabel: %v", err)
					return
				}
				if (i & 1) == 0 {
					if err := g.SetNodeLabel(n, "Active"); err != nil {
						t.Errorf("SetNodeLabel: %v", err)
						return
					}
				}
				_ = g.HasNodeLabel(n, "Person")
			}
		}(w)
	}
	wg.Wait()
}
