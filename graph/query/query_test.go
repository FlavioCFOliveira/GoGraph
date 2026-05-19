package query

import (
	"sort"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
)

func setupSocialGraph() (*lpg.Graph[string, int64], *csr.CSR[int64]) {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	people := []string{"alice", "bob", "charlie", "dave", "erin"}
	for _, p := range people {
		g.SetNodeLabel(p, "Person")
	}
	g.SetNodeLabel("alice", "Admin")
	g.SetNodeLabel("dave", "Admin")
	g.SetNodeProperty("alice", "age", lpg.Int64Value(30))
	g.SetNodeProperty("bob", "age", lpg.Int64Value(25))
	g.SetNodeProperty("charlie", "age", lpg.Int64Value(30))
	g.AddEdge("alice", "bob", 1)
	g.AddEdge("alice", "charlie", 1)
	g.AddEdge("bob", "dave", 1)
	c := csr.BuildFromAdjList(g.AdjList())
	return g, c
}

func TestQuery_MatchByLabel(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	got := e.Match().Vertex(WithLabel[string, int64]("Admin")).Collect()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "alice" || got[1] != "dave" {
		t.Fatalf("got = %v, want [alice dave]", got)
	}
}

func TestQuery_MatchByMultipleLabels(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	got := e.Match().Vertex(
		WithLabel[string, int64]("Person"),
		WithLabel[string, int64]("Admin"),
	).Collect()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "alice" || got[1] != "dave" {
		t.Fatalf("got = %v, want [alice dave]", got)
	}
}

func TestQuery_MatchByProperty(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	got := e.Match().Vertex(
		WithLabel[string, int64]("Person"),
		WithProperty[string, int64]("age", lpg.Int64Value(30)),
	).Collect()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "alice" || got[1] != "charlie" {
		t.Fatalf("got = %v, want [alice charlie]", got)
	}
}

func TestQuery_OneHop(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	// MATCH (a:Admin)-->(b) RETURN b
	got := e.Match().
		Vertex(WithLabel[string, int64]("Admin")).
		Out().
		Collect()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "bob" || got[1] != "charlie" {
		t.Fatalf("got = %v, want [bob charlie]", got)
	}
}

func TestQuery_TwoHop(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	// MATCH (alice)-->()-->(c) RETURN c
	got := e.Match().
		Vertex(WithProperty[string, int64]("age", lpg.Int64Value(30)), WithLabel[string, int64]("Admin")).
		Out().Out().
		Collect()
	// alice -> bob -> dave (one), alice -> charlie -> (nothing)
	sort.Strings(got)
	if len(got) != 1 || got[0] != "dave" {
		t.Fatalf("got = %v, want [dave]", got)
	}
}

func TestQuery_UnknownLabel(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	got := e.Match().Vertex(WithLabel[string, int64]("Nonexistent")).Collect()
	if len(got) != 0 {
		t.Fatalf("unknown-label match must be empty, got %v", got)
	}
}

func TestQuery_Cardinality(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	if e.Match().Vertex(WithLabel[string, int64]("Person")).Cardinality() != 5 {
		t.Fatalf("Person count must be 5")
	}
}

func TestQuery_FullScanFallback(t *testing.T) {
	t.Parallel()
	// When no label is provided, the seed scans all NodeIDs.
	g, c := setupSocialGraph()
	e := New(g, c)
	got := e.Match().Vertex(
		WithProperty[string, int64]("age", lpg.Int64Value(25)),
	).Collect()
	if len(got) != 1 || got[0] != "bob" {
		t.Fatalf("got = %v, want [bob]", got)
	}
}
