package invariants_test

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/invariants"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// directedLPG builds a directed *lpg.Graph[int, struct{}] from an
// edge list expressed as pairs of ints.
func directedLPG(nodes []int, edges [][2]int) *lpg.Graph[int, struct{}] {
	g := lpg.New[int, struct{}](adjlist.Config{Directed: true})
	for _, n := range nodes {
		_ = g.AddNode(n)
	}
	for _, e := range edges {
		_ = g.AddEdge(e[0], e[1], struct{}{})
	}
	return g
}

// undirectedLPG builds an undirected *lpg.Graph[int, struct{}].
func undirectedLPG(nodes []int, edges [][2]int) *lpg.Graph[int, struct{}] {
	g := lpg.New[int, struct{}](adjlist.Config{Directed: false})
	for _, n := range nodes {
		_ = g.AddNode(n)
	}
	for _, e := range edges {
		_ = g.AddEdge(e[0], e[1], struct{}{})
	}
	return g
}

// mockTB captures Errorf calls so positive/negative tests can assert
// on whether failures occurred.
type mockTB struct {
	testing.TB
	failures []string
}

func (m *mockTB) Helper()                   {}
func (m *mockTB) Errorf(f string, a ...any) { m.failures = append(m.failures, f) }

func failed(m *mockTB) bool { return len(m.failures) > 0 }
func passed(m *mockTB) bool { return len(m.failures) == 0 }

// ─── AssertConnected ──────────────────────────────────────────────

func TestAssertConnected_Positive(t *testing.T) {
	// Triangle: 1→2→3→1 — one WCC.
	g := directedLPG([]int{1, 2, 3}, [][2]int{{1, 2}, {2, 3}, {3, 1}})
	tb := &mockTB{}
	invariants.AssertConnected(tb, g)
	if failed(tb) {
		t.Errorf("connected graph: unexpected failure: %v", tb.failures)
	}
}

func TestAssertConnected_Negative(t *testing.T) {
	// Two isolated nodes: two WCCs.
	g := directedLPG([]int{1, 2}, nil)
	tb := &mockTB{}
	invariants.AssertConnected(tb, g)
	if passed(tb) {
		t.Error("disconnected graph: expected failure, got none")
	}
}

func TestAssertConnected_EmptyGraph(t *testing.T) {
	g := directedLPG(nil, nil)
	tb := &mockTB{}
	invariants.AssertConnected(tb, g)
	if failed(tb) {
		t.Errorf("empty graph: unexpected failure: %v", tb.failures)
	}
}

// ─── AssertDAG ───────────────────────────────────────────────────

func TestAssertDAG_Positive(t *testing.T) {
	// Simple DAG: 1→2→3.
	g := directedLPG([]int{1, 2, 3}, [][2]int{{1, 2}, {2, 3}})
	tb := &mockTB{}
	invariants.AssertDAG(tb, g)
	if failed(tb) {
		t.Errorf("DAG: unexpected failure: %v", tb.failures)
	}
}

func TestAssertDAG_Negative_Cycle(t *testing.T) {
	// Directed cycle: 1→2→3→1.
	g := directedLPG([]int{1, 2, 3}, [][2]int{{1, 2}, {2, 3}, {3, 1}})
	tb := &mockTB{}
	invariants.AssertDAG(tb, g)
	if passed(tb) {
		t.Error("cyclic graph: expected failure, got none")
	}
}

func TestAssertDAG_Negative_SelfLoop(t *testing.T) {
	// Self-loop: 1→1.
	g := directedLPG([]int{1}, [][2]int{{1, 1}})
	tb := &mockTB{}
	invariants.AssertDAG(tb, g)
	if passed(tb) {
		t.Error("self-loop: expected failure, got none")
	}
}

func TestAssertDAG_Positive_EmptyGraph(t *testing.T) {
	g := directedLPG(nil, nil)
	tb := &mockTB{}
	invariants.AssertDAG(tb, g)
	if failed(tb) {
		t.Errorf("empty graph: unexpected failure: %v", tb.failures)
	}
}

// ─── AssertBipartite ─────────────────────────────────────────────

func TestAssertBipartite_Positive_EvenCycle(t *testing.T) {
	// C4 (even cycle) is bipartite.
	g := undirectedLPG([]int{1, 2, 3, 4}, [][2]int{{1, 2}, {2, 3}, {3, 4}, {4, 1}})
	tb := &mockTB{}
	invariants.AssertBipartite(tb, g)
	if failed(tb) {
		t.Errorf("bipartite graph (C4): unexpected failure: %v", tb.failures)
	}
}

func TestAssertBipartite_Negative_OddCycle(t *testing.T) {
	// C3 (triangle) is NOT bipartite.
	g := undirectedLPG([]int{1, 2, 3}, [][2]int{{1, 2}, {2, 3}, {3, 1}})
	tb := &mockTB{}
	invariants.AssertBipartite(tb, g)
	if passed(tb) {
		t.Error("triangle (C3): expected bipartite failure, got none")
	}
}

func TestAssertBipartite_Positive_Path(t *testing.T) {
	// Path graph P4: 1-2-3-4 is bipartite.
	g := undirectedLPG([]int{1, 2, 3, 4}, [][2]int{{1, 2}, {2, 3}, {3, 4}})
	tb := &mockTB{}
	invariants.AssertBipartite(tb, g)
	if failed(tb) {
		t.Errorf("path P4 (bipartite): unexpected failure: %v", tb.failures)
	}
}

func TestAssertBipartite_Negative_K33(t *testing.T) {
	// K_{3,3} is bipartite by construction — should pass.
	nodes := []int{1, 2, 3, 4, 5, 6}
	edges := [][2]int{{1, 4}, {1, 5}, {1, 6}, {2, 4}, {2, 5}, {2, 6}, {3, 4}, {3, 5}, {3, 6}}
	g := undirectedLPG(nodes, edges)
	tb := &mockTB{}
	invariants.AssertBipartite(tb, g)
	if failed(tb) {
		t.Errorf("K_3,3 (bipartite): unexpected failure: %v", tb.failures)
	}
}

// ─── AssertDistanceBound ─────────────────────────────────────────

// buildWeightedGraph constructs an lpg with int64 weights for
// Dijkstra testing.
func buildWeightedGraph(nodes []int64, edges [][3]int64) *lpg.Graph[int64, int64] {
	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	for _, n := range nodes {
		_ = g.AddNode(n)
	}
	for _, e := range edges {
		_ = g.AddEdge(e[0], e[1], e[2])
	}
	return g
}

func TestAssertDistanceBound_Positive(t *testing.T) {
	// 1 --1--> 2 --1--> 3 --1--> 4
	g := buildWeightedGraph(
		[]int64{1, 2, 3, 4},
		[][3]int64{{1, 2, 1}, {2, 3, 1}, {3, 4, 1}},
	)
	c := csr.BuildFromAdjList(g.AdjList())
	m := g.AdjList().Mapper()
	srcID, _ := m.Lookup(int64(1))

	bfsDepths, err := invariants.BuildBFSDepths(context.Background(), c, srcID)
	if err != nil {
		t.Fatalf("BuildBFSDepths: %v", err)
	}
	djDists, err := search.Dijkstra(c, srcID)
	if err != nil {
		t.Fatalf("Dijkstra: %v", err)
	}

	tb := &mockTB{}
	invariants.AssertDistanceBound(tb, bfsDepths, djDists)
	if failed(tb) {
		t.Errorf("unit-weight path: unexpected failure: %v", tb.failures)
	}
}

func TestAssertDistanceBound_Positive_WeightedPath(t *testing.T) {
	// 1 --3--> 2 --3--> 3
	// BFS depth: node2=1, node3=2; Dijkstra: node2=3, node3=6
	// 1 <= 3 and 2 <= 6 — bound holds.
	g := buildWeightedGraph(
		[]int64{1, 2, 3},
		[][3]int64{{1, 2, 3}, {2, 3, 3}},
	)
	c := csr.BuildFromAdjList(g.AdjList())
	m := g.AdjList().Mapper()
	srcID, _ := m.Lookup(int64(1))

	bfsDepths, _ := invariants.BuildBFSDepths(context.Background(), c, srcID)
	djDists, _ := search.Dijkstra(c, srcID)

	tb := &mockTB{}
	invariants.AssertDistanceBound(tb, bfsDepths, djDists)
	if failed(tb) {
		t.Errorf("weighted path: unexpected failure: %v", tb.failures)
	}
}

// ─── AssertShapeEqual ────────────────────────────────────────────

func TestAssertShapeEqual_Positive(t *testing.T) {
	a := directedLPG([]int{1, 2, 3}, [][2]int{{1, 2}, {2, 3}})
	b := directedLPG([]int{1, 2, 3}, [][2]int{{1, 2}, {2, 3}})
	tb := &mockTB{}
	invariants.AssertShapeEqual(tb, a, b)
	if failed(tb) {
		t.Errorf("identical graphs: unexpected failure: %v", tb.failures)
	}
}

func TestAssertShapeEqual_Negative_MissingEdge(t *testing.T) {
	a := directedLPG([]int{1, 2, 3}, [][2]int{{1, 2}, {2, 3}})
	b := directedLPG([]int{1, 2, 3}, [][2]int{{1, 2}}) // missing 2→3
	tb := &mockTB{}
	invariants.AssertShapeEqual(tb, a, b)
	if passed(tb) {
		t.Error("graphs differ by one edge: expected failure, got none")
	}
}

func TestAssertShapeEqual_Negative_OrderMismatch(t *testing.T) {
	a := directedLPG([]int{1, 2, 3}, [][2]int{{1, 2}})
	b := directedLPG([]int{1, 2}, [][2]int{{1, 2}})
	tb := &mockTB{}
	invariants.AssertShapeEqual(tb, a, b)
	if passed(tb) {
		t.Error("order mismatch: expected failure, got none")
	}
}
