package search

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// linear3Graph bundles a float64-weighted directed CSR with three nodes
// (keyed 0, 1, 2) and edges 0→1 and 1→2, each with weight 1.0, together
// with the resolved NodeIDs for src (key 0) and dst (key 2).
type linear3Graph struct {
	c   *csr.CSR[float64]
	src graph.NodeID
	dst graph.NodeID
}

// buildLinear3 builds a float64-weighted directed CSR with three nodes
// (keys 0, 1, 2) and edges 0→1 and 1→2, each with weight 1.0.
// The caller uses the returned src/dst NodeIDs; raw integer literals must
// not be used because the mapper assigns sparse, shard-padded NodeIDs.
func buildLinear3(tb testing.TB) linear3Graph {
	tb.Helper()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		tb.Fatalf("AddEdge(0→1): %v", err)
	}
	if err := a.AddEdge(1, 2, 1.0); err != nil {
		tb.Fatalf("AddEdge(1→2): %v", err)
	}
	c := csr.BuildFromAdjList(a)
	m := a.Mapper()
	src, ok0 := m.Lookup(0)
	dst, ok2 := m.Lookup(2)
	if !ok0 || !ok2 {
		tb.Fatalf("mapper: Lookup failed (ok0=%v ok2=%v)", ok0, ok2)
	}
	return linear3Graph{c: c, src: src, dst: dst}
}

func TestAStar_NaNHeuristic_ReturnsErrInvalidInput(t *testing.T) {
	t.Parallel()
	// 0→1→2, weights=1. h≡NaN.
	// Before fix: ErrNoPath. After fix: ErrInvalidInput.
	g := buildLinear3(t)
	h := func(graph.NodeID) float64 { return math.NaN() }
	_, _, err := AStarCtx(context.Background(), g.c, g.src, g.dst, h)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
}

func TestAStar_InfHeuristic_ReturnsErrInvalidInput(t *testing.T) {
	t.Parallel()
	g := buildLinear3(t)
	h := func(graph.NodeID) float64 { return math.Inf(1) }
	_, _, err := AStarCtx(context.Background(), g.c, g.src, g.dst, h)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
}

func TestAStar_NegInfHeuristic_ReturnsErrInvalidInput(t *testing.T) {
	t.Parallel()
	g := buildLinear3(t)
	h := func(graph.NodeID) float64 { return math.Inf(-1) }
	_, _, err := AStarCtx(context.Background(), g.c, g.src, g.dst, h)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
}

func TestAStar_ValidHeuristic_StillFinds(t *testing.T) {
	t.Parallel()
	g := buildLinear3(t)
	h := func(graph.NodeID) float64 { return 0 } // admissible constant-0
	path, cost, err := AStarCtx(context.Background(), g.c, g.src, g.dst, h)
	if err != nil || len(path) != 3 || cost != 2.0 {
		t.Fatalf("got path=%v cost=%v err=%v", path, cost, err)
	}
}
