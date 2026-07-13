package sim

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestNegWeight_CleanOnFixtures asserts the negative-weight battery finds no
// divergence across a spread of ticks: BellmanFord/FloydWarshall/JohnsonAPSP
// agree with the naive reference on the DAG fixtures, and all three detect the
// planted negative cycles.
func TestNegWeight_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	ticks := []int64{0, 1, 2, 3, 7, 11, 42, 99, 1000, 123456, 7654321}
	for _, tick := range ticks {
		if vs := negWeightViolations(tick); len(vs) != 0 {
			t.Errorf("negWeightViolations(%d) = %d violation(s), want 0:", tick, len(vs))
			for _, v := range vs {
				t.Errorf("  %s", v)
			}
		}
	}
}

// TestNegWeight_Deterministic asserts the checker is a pure function of the tick.
func TestNegWeight_Deterministic(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{0, 5, 50, 500, 5000} {
		if a, b := negWeightViolations(tick), negWeightViolations(tick); len(a) != len(b) {
			t.Fatalf("negWeightViolations(%d) not deterministic: run1=%d run2=%d", tick, len(a), len(b))
		}
	}
}

// TestNegRefBellmanFord_KnownDAG anchors the naive reference on a hand-checked
// DAG with a negative edge: 0->1 (2), 1->2 (-5), 0->2 (10). The shortest 0->2
// distance is 2 + (-5) = -3 (via 1), not the direct 10.
func TestNegRefBellmanFord_KnownDAG(t *testing.T) {
	t.Parallel()
	edges := []negEdge{{0, 1, 2}, {1, 2, -5}, {0, 2, 10}}
	dist, reach := negRefBellmanFord(3, edges, 0)
	if !reach[2] || dist[2] != -3 {
		t.Fatalf("reference 0->2 = (%v, %v), want (-3, true)", dist[2], reach[2])
	}
	// The production BellmanFord must agree on the same graph.
	c := negBuildCSR(3, edges)
	d, err := search.BellmanFord(c, 0)
	if err != nil {
		t.Fatalf("BellmanFord: %v", err)
	}
	got, ok := d.Distance(2)
	if !ok || got != -3 {
		t.Fatalf("BellmanFord 0->2 = (%v, %v), want (-3, true)", got, ok)
	}
}

// TestNegWeight_DetectsNegativeCycle proves each negative-tolerant algorithm
// surfaces ErrNegativeCycle on the fixed planted 3-cycle, and — as a meta-check
// that the assertion is not vacuous — that a positive-weight DAG does NOT.
func TestNegWeight_DetectsNegativeCycle(t *testing.T) {
	t.Parallel()
	seed := NewSeed(0)
	// Fixed planted cycle (idx 0): 0->1->2->0 summing to -3.
	n, edges := negGenNegativeCycle(seed, 0)
	c := negBuildCSR(n, edges)

	if _, err := search.BellmanFord(c, 0); !errors.Is(err, search.ErrNegativeCycle) {
		t.Errorf("BellmanFord on negative cycle: err=%v, want ErrNegativeCycle", err)
	}
	if _, err := search.FloydWarshallCtx(context.Background(), c); !errors.Is(err, search.ErrNegativeCycle) {
		t.Errorf("FloydWarshallCtx on negative cycle: err=%v, want ErrNegativeCycle", err)
	}
	if _, err := search.JohnsonAPSP(c); !errors.Is(err, search.ErrNegativeCycle) {
		t.Errorf("JohnsonAPSP on negative cycle: err=%v, want ErrNegativeCycle", err)
	}

	// A negative-weight DAG (no cycle) must NOT be flagged.
	dagEdges := []negEdge{{0, 1, -3}, {1, 2, -2}, {0, 2, 1}}
	dag := negBuildCSR(3, dagEdges)
	if _, err := search.BellmanFord(dag, 0); err != nil {
		t.Errorf("BellmanFord on negative DAG: err=%v, want nil", err)
	}
	if _, err := search.FloydWarshallCtx(context.Background(), dag); err != nil {
		t.Errorf("FloydWarshallCtx on negative DAG: err=%v, want nil", err)
	}
	if _, err := search.JohnsonAPSP(dag); err != nil {
		t.Errorf("JohnsonAPSP on negative DAG: err=%v, want nil", err)
	}
}
