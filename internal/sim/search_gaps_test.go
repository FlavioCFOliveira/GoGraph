package sim

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// cleanAndDeterministic runs a tick-parameterised checker across a spread of
// ticks, asserting it flags nothing and is a pure function of the tick.
func cleanAndDeterministic(t *testing.T, name string, fn func(int64) []Violation) {
	t.Helper()
	ticks := []int64{0, 1, 2, 3, 7, 11, 42, 99, 1000, 123456, 7654321}
	for _, tick := range ticks {
		vs := fn(tick)
		if len(vs) != 0 {
			t.Errorf("%s(%d) = %d violation(s), want 0:", name, tick, len(vs))
			for _, v := range vs {
				t.Errorf("  %s", v)
			}
		}
		if len(fn(tick)) != len(vs) {
			t.Errorf("%s(%d) not deterministic across two runs", name, tick)
		}
	}
}

// TestTopoDAGChecks_Clean asserts the DAG topological-sort success-path check
// finds no divergence: TopologicalSort returns a valid order on every acyclic
// fixture.
func TestTopoDAGChecks_Clean(t *testing.T) {
	t.Parallel()
	cleanAndDeterministic(t, "topoDAGViolations", topoDAGViolations)
}

// TestTopoDAGValidate_DetectsBadOrder proves the DAG order validator flags a
// non-topological order rather than vacuously passing.
func TestTopoDAGValidate_DetectsBadOrder(t *testing.T) {
	t.Parallel()
	// DAG 0->1->2. A correct order is [0,1,2]; [0,2,1] violates edge 1->2.
	edges := [][2]int{{0, 1}, {1, 2}}
	good := []graph.NodeID{0, 1, 2}
	bad := []graph.NodeID{0, 2, 1}
	if vs := validateTopoOrderDAG(0, 3, edges, good); len(vs) != 0 {
		t.Fatalf("valid order flagged: %v", vs)
	}
	if vs := validateTopoOrderDAG(0, 3, edges, bad); len(vs) == 0 {
		t.Fatal("invalid order [0,2,1] not flagged")
	}
}

// TestDiameterChecks_Clean asserts the diameter bound check finds no divergence
// on the path/cycle/star fixtures.
func TestDiameterChecks_Clean(t *testing.T) {
	t.Parallel()
	cleanAndDeterministic(t, "diameterViolations", diameterViolations)
}

// TestDiameterReference_KnownShapes anchors the independent diameter reference
// against hand values: path P6 -> 5, cycle C6 -> 3, star (hub+5) -> 2.
func TestDiameterReference_KnownShapes(t *testing.T) {
	t.Parallel()
	p := diameterPath("p6", 6)
	if got := diameterReference(p.order, p.edges); got != 5 {
		t.Errorf("path6 diameter = %d, want 5", got)
	}
	c := diameterCycle("c6", 6)
	if got := diameterReference(c.order, c.edges); got != 3 {
		t.Errorf("cycle6 diameter = %d, want 3", got)
	}
	s := diameterStar("s6", 6)
	if got := diameterReference(s.order, s.edges); got != 2 {
		t.Errorf("star6 diameter = %d, want 2", got)
	}
}

// TestBFSDirectionOptChecks_Clean asserts the direction-optimising BFS check
// finds no divergence on the supernode fixtures.
func TestBFSDirectionOptChecks_Clean(t *testing.T) {
	t.Parallel()
	cleanAndDeterministic(t, "bfsDoViolations", bfsDoViolations)
}

// TestEulerUndirectedChecks_Clean asserts the undirected-Hierholzer check finds
// no divergence: circuits/paths yield valid trails and >2-odd graphs yield
// ErrNoEulerian.
func TestEulerUndirectedChecks_Clean(t *testing.T) {
	t.Parallel()
	cleanAndDeterministic(t, "eulerUndirectedViolations", eulerUndirectedViolations)
}

// TestExternChecks_Clean asserts the external-memory check finds no divergence:
// extern.BFS/PageRank agree with their in-memory counterparts over a csrfile.
func TestExternChecks_Clean(t *testing.T) {
	t.Parallel()
	cleanAndDeterministic(t, "externViolations", externViolations)
}

// TestKShortestParallelChecks_Clean asserts the multigraph k-shortest check
// finds no divergence: Yen matches the node-sequence-min reference and the
// best-first loopless enumerators match the per-edge reference.
func TestKShortestParallelChecks_Clean(t *testing.T) {
	t.Parallel()
	ticks := []int64{0, 1, 2, 3, 7, 11, 42, 99, 1000}
	for _, tick := range ticks {
		seed := NewSeed(uint64(tick) ^ kshortestSeedMix)
		// Advance past the simple fixtures to isolate the parallel ones.
		for f := 0; f < kshortestFixtures; f++ {
			_ = kshortestFixtureViolations(tick, seed)
		}
		for f := 0; f < kshortestParallelFixtures; f++ {
			if vs := kshortestParallelFixtureViolations(tick, seed); len(vs) != 0 {
				t.Errorf("kshortestParallelFixtureViolations(tick=%d) flagged %d:", tick, len(vs))
				for _, v := range vs {
					t.Errorf("  %s", v)
				}
			}
		}
	}
}
