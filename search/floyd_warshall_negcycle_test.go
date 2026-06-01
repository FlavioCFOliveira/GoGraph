package search

// Task 927: FloydWarshall detects negative cycles via CLRS §25.2
// post-pass on the diagonal.
//
// Without the post-pass, the matrix silently reports finite
// distances polluted by the cycle. With the post-pass, the Ctx
// variant returns ErrNegativeCycle (the same sentinel used by
// BellmanFord); the simple wrapper returns nil.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestFloydWarshall_NegCycle_SimpleTriangle tests a 3-node directed
// cycle 0→1→2→0 with edge weight -1.0 throughout.
func TestFloydWarshall_NegCycle_SimpleTriangle(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	for _, e := range [][3]float64{{0, 1, -1.0}, {1, 2, -1.0}, {2, 0, -1.0}} {
		if err := a.AddEdge(int(e[0]), int(e[1]), e[2]); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	_, err := FloydWarshallCtx(context.Background(), c)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("3-cycle: expected ErrNegativeCycle, got %v", err)
	}
	// Simple wrapper must return nil on negative cycle (same shape as
	// the existing NaN/Inf behaviour: the Ctx variant carries the error,
	// the simple wrapper discards it and yields nil).
	if got := FloydWarshall(c); got != nil {
		t.Fatalf("FloydWarshall on negative cycle: expected nil, got non-nil APSP")
	}
}

// TestFloydWarshall_NegCycle_FiveCycle tests a 5-node cycle
// 0→1→2→3→4→0 with edge weight -1.0 throughout.
func TestFloydWarshall_NegCycle_FiveCycle(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	for i := 0; i < 5; i++ {
		if err := a.AddEdge(i, (i+1)%5, -1.0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	_, err := FloydWarshallCtx(context.Background(), c)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("5-cycle: expected ErrNegativeCycle, got %v", err)
	}
}

// TestFloydWarshall_NegCycle_ReachableViaPath builds a graph in
// which a positive-weight path 0→1→2 fans into a 3-node negative
// cycle 2→3→4→2. The cycle's negative weight must be detected even
// though the source vertex (0) is not itself on the cycle.
func TestFloydWarshall_NegCycle_ReachableViaPath(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	for _, e := range [][3]float64{
		{0, 1, 1.0},
		{1, 2, 1.0},
		// Negative cycle 2→3→4→2.
		{2, 3, -1.0},
		{3, 4, -1.0},
		{4, 2, -1.0},
	} {
		if err := a.AddEdge(int(e[0]), int(e[1]), e[2]); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	_, err := FloydWarshallCtx(context.Background(), c)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("reachable-cycle: expected ErrNegativeCycle, got %v", err)
	}
}

// TestFloydWarshall_NegEdges_NoCycle ensures that the post-pass does
// not produce false positives: a DAG with negative edges but no cycle
// must complete successfully and return a non-nil APSP.
func TestFloydWarshall_NegEdges_NoCycle(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	// Layered DAG: 0→1, 0→2, 1→3, 2→3 with negative weights but no cycle.
	for _, e := range [][3]float64{
		{0, 1, -2.0},
		{0, 2, -1.0},
		{1, 3, -3.0},
		{2, 3, -5.0},
	} {
		if err := a.AddEdge(int(e[0]), int(e[1]), e[2]); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	apsp, err := FloydWarshallCtx(context.Background(), c)
	if err != nil {
		t.Fatalf("acyclic DAG: unexpected error %v", err)
	}
	if apsp == nil {
		t.Fatalf("acyclic DAG: expected non-nil APSP")
	}
	// dist(0,3) via 0→2→3 = -1 + -5 = -6, via 0→1→3 = -2 + -3 = -5.
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	d, ok := apsp.At(src, dst)
	if !ok {
		t.Fatalf("dist(0,3): expected reachable")
	}
	if d != -6.0 {
		t.Fatalf("dist(0,3): expected -6.0, got %v", d)
	}
}

// TestFloydWarshall_NegCycle_IntegerWeight verifies the diagonal scan
// works on integer Weight types (no float-only short-circuit on the
// negative-cycle branch).
func TestFloydWarshall_NegCycle_IntegerWeight(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for _, e := range [][3]int64{{0, 1, -1}, {1, 2, -1}, {2, 0, -1}} {
		if err := a.AddEdge(int(e[0]), int(e[1]), e[2]); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	_, err := FloydWarshallCtx(context.Background(), c)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("int64 3-cycle: expected ErrNegativeCycle, got %v", err)
	}
}
