package sim

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// dagFixture is an acyclic graph: A->B, B->C, A->C, A->D.
func dagFixture() *nameGraph {
	return oracleNameGraph(buildSearchOracle(
		[]string{"A", "B", "C", "D"},
		[][2]string{{"A", "B"}, {"B", "C"}, {"A", "C"}, {"A", "D"}},
	))
}

// cyclicFixture is a graph with one 3-cycle A->B->C->A and a sink edge C->D.
func cyclicFixture() *nameGraph {
	return oracleNameGraph(buildSearchOracle(
		[]string{"A", "B", "C", "D"},
		[][2]string{{"A", "B"}, {"B", "C"}, {"C", "A"}, {"C", "D"}},
	))
}

// TestIsAcyclic covers the three-colour DFS decision, including the self-loop case.
func TestIsAcyclic(t *testing.T) {
	t.Parallel()
	if !dagFixture().isAcyclic() {
		t.Fatal("DAG fixture reported cyclic")
	}
	if cyclicFixture().isAcyclic() {
		t.Fatal("cyclic fixture reported acyclic")
	}
	selfLoop := oracleNameGraph(buildSearchOracle([]string{"A", "B"}, [][2]string{{"A", "A"}, {"A", "B"}}))
	if selfLoop.isAcyclic() {
		t.Fatal("self-loop must be reported as cyclic")
	}
}

// TestOrderingChecks_CleanOnDAG asserts SCC, topo, and TC all agree with their
// references on the acyclic fixture (every node is its own SCC; a valid order
// exists).
func TestOrderingChecks_CleanOnDAG(t *testing.T) {
	t.Parallel()
	g := dagFixture()
	c := g.toCSR()
	fwd := g.forwardReachAll()
	if v := topoViolations(1, g, c); len(v) != 0 {
		t.Fatalf("topo on DAG: %v", v)
	}
	if v := sccViolations(1, g, c, fwd); len(v) != 0 {
		t.Fatalf("SCC on DAG: %v", v)
	}
	if v := tcViolations(1, g, c, fwd); len(v) != 0 {
		t.Fatalf("TC on DAG: %v", v)
	}
	// Every node is its own SCC.
	if sig := componentsToSig(g.naiveSCC(fwd)); sig != "0;1;2;3" {
		t.Fatalf("DAG SCC signature = %q, want \"0;1;2;3\"", sig)
	}
}

// TestOrderingChecks_CleanOnCyclic asserts the checks agree on the cyclic
// fixture: topo expects ErrCycle, and the SCC reference recovers the 3-cycle.
func TestOrderingChecks_CleanOnCyclic(t *testing.T) {
	t.Parallel()
	g := cyclicFixture()
	c := g.toCSR()
	fwd := g.forwardReachAll()
	if v := topoViolations(1, g, c); len(v) != 0 {
		t.Fatalf("topo on cyclic graph (expected ErrCycle handled): %v", v)
	}
	if v := sccViolations(1, g, c, fwd); len(v) != 0 {
		t.Fatalf("SCC on cyclic graph: %v", v)
	}
	if v := tcViolations(1, g, c, fwd); len(v) != 0 {
		t.Fatalf("TC on cyclic graph: %v", v)
	}
	// A,B,C form one SCC ({0,1,2}); D is a singleton ({3}).
	if sig := componentsToSig(g.naiveSCC(fwd)); sig != "0,1,2;3" {
		t.Fatalf("cyclic SCC signature = %q, want \"0,1,2;3\"", sig)
	}
}

// TestValidateTopoOrder_DetectsBadOrders asserts the order validator rejects a
// non-forward order and an order missing an edge-incident node.
func TestValidateTopoOrder_DetectsBadOrders(t *testing.T) {
	t.Parallel()
	// A->B only: A=0, B=1.
	g := oracleNameGraph(buildSearchOracle([]string{"A", "B"}, [][2]string{{"A", "B"}}))

	// Reversed order puts B before A, violating the A->B edge.
	if v := validateTopoOrder(1, g, []graph.NodeID{1, 0}); len(v) == 0 {
		t.Fatal("expected a violation for a non-forward order")
	}
	// An order missing B (an edge-incident node) must be rejected.
	if v := validateTopoOrder(1, g, []graph.NodeID{0}); len(v) == 0 {
		t.Fatal("expected a violation for an order missing an edge-incident node")
	}
	// The correct order passes.
	if v := validateTopoOrder(1, g, []graph.NodeID{0, 1}); len(v) != 0 {
		t.Fatalf("valid order rejected: %v", v)
	}
}

// TestTransitiveClosure_ReferenceReachability spot-checks the forward-reach
// reference the TC check relies on, on the cyclic fixture.
func TestTransitiveClosure_ReferenceReachability(t *testing.T) {
	t.Parallel()
	g := cyclicFixture() // A->B->C->A, C->D
	fwd := g.forwardReachAll()
	a, d := g.idx["A"], g.idx["D"]
	// A reaches everything (it is in the cycle that leads to D).
	for _, n := range []string{"A", "B", "C", "D"} {
		if !fwd[a][g.idx[n]] {
			t.Fatalf("A should reach %s", n)
		}
	}
	// D is a sink: it reaches only itself.
	for _, n := range []string{"A", "B", "C"} {
		if fwd[d][g.idx[n]] {
			t.Fatalf("D (a sink) must not reach %s", n)
		}
	}
	if !fwd[d][d] {
		t.Fatal("reachability is reflexive: D must reach itself")
	}
}
