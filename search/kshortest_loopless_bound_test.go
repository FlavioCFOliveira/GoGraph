package search

import (
	"context"
	"errors"
	"testing"
)

// diamondChainEdges returns the edge list for a diamond-chain graph of the
// given depth (see KShortestPathsLoopless godoc for the exponential-pops
// argument). withShortcut adds a single expensive src→dst edge.
//
// Node numbering:
//
//	0           = src
//	1 .. depth  = top chain
//	depth+1 .. 2*depth = bottom chain
//	2*depth+1   = dst
func diamondChainEdges(depth int, withShortcut bool) (edges []weightedEdge, src, dst int) {
	nNodes := 2*depth + 2
	src = 0
	dst = nNodes - 1
	topStart := 1
	botStart := depth + 1

	edges = append(edges, weightedEdge{src, topStart, 1}, weightedEdge{src, botStart, 1})
	for i := 0; i < depth; i++ {
		top := topStart + i
		bot := botStart + i
		nextTop := topStart + i + 1
		nextBot := botStart + i + 1
		if i < depth-1 {
			edges = append(edges,
				weightedEdge{top, nextTop, 1},
				weightedEdge{bot, nextBot, 1},
				weightedEdge{top, nextBot, 1},
				weightedEdge{bot, nextTop, 1},
			)
		} else {
			edges = append(edges,
				weightedEdge{top, dst, 1},
				weightedEdge{bot, dst, 1},
			)
		}
	}
	if withShortcut {
		edges = append(edges, weightedEdge{src, dst, int64((depth + 1) * 2)})
	}
	return edges, src, dst
}

// TestKShortestPathsLoopless_MaxPops_Returns_Error verifies that
// KShortestPathsLooplessCtxWithOpts returns ErrResourceBudgetExceeded
// when the MaxPops limit is reached.
func TestKShortestPathsLoopless_MaxPops_Returns_Error(t *testing.T) {
	t.Parallel()
	edges, src, dst := diamondChainEdges(10, true)
	c, a := buildWeightedCSR(t, edges)
	srcID, _ := a.Mapper().Lookup(src)
	dstID, _ := a.Mapper().Lookup(dst)

	paths, err := KShortestPathsLooplessCtxWithOpts(
		context.Background(), c, srcID, dstID, 2,
		KShortestPathsLooplessOpts{MaxPops: 100},
	)

	if !errors.Is(err, ErrResourceBudgetExceeded) {
		t.Fatalf("expected ErrResourceBudgetExceeded, got err=%v paths=%v", err, paths)
	}
	// Any partial results returned must be valid paths.
	for i, p := range paths {
		if len(p.Nodes) == 0 {
			t.Errorf("partial path %d has empty Nodes", i)
		}
		if p.Cost < 0 {
			t.Errorf("partial path %d has negative cost %v", i, p.Cost)
		}
	}
}

// TestKShortestPathsLoopless_MaxQueueBytes_Returns_Error verifies that
// KShortestPathsLooplessCtxWithOpts returns ErrResourceBudgetExceeded
// when the MaxQueueBytes limit is reached.
func TestKShortestPathsLoopless_MaxQueueBytes_Returns_Error(t *testing.T) {
	t.Parallel()
	edges, src, dst := diamondChainEdges(10, true)
	c, a := buildWeightedCSR(t, edges)
	srcID, _ := a.Mapper().Lookup(src)
	dstID, _ := a.Mapper().Lookup(dst)

	paths, err := KShortestPathsLooplessCtxWithOpts(
		context.Background(), c, srcID, dstID, 2,
		KShortestPathsLooplessOpts{MaxQueueBytes: 1024},
	)

	if !errors.Is(err, ErrResourceBudgetExceeded) {
		t.Fatalf("expected ErrResourceBudgetExceeded, got err=%v paths=%v", err, paths)
	}
	for i, p := range paths {
		if len(p.Nodes) == 0 {
			t.Errorf("partial path %d has empty Nodes", i)
		}
	}
}

// TestKShortestPathsLoopless_ZeroOpts_Unchanged verifies that
// KShortestPathsLooplessCtxWithOpts with zero opts produces the same
// results as KShortestPathsLooplessCtx on a small graph.
func TestKShortestPathsLoopless_ZeroOpts_Unchanged(t *testing.T) {
	t.Parallel()
	// No shortcut so the graph has exactly two equal-length paths.
	edges, src, dst := diamondChainEdges(5, false)
	c, a := buildWeightedCSR(t, edges)
	srcID, _ := a.Mapper().Lookup(src)
	dstID, _ := a.Mapper().Lookup(dst)
	ctx := context.Background()

	want, wantErr := KShortestPathsLooplessCtx(ctx, c, srcID, dstID, 2)
	got, gotErr := KShortestPathsLooplessCtxWithOpts(ctx, c, srcID, dstID, 2, KShortestPathsLooplessOpts{})

	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("error mismatch: KShortestPathsLooplessCtx=%v WithOpts=%v", wantErr, gotErr)
	}
	if len(want) != len(got) {
		t.Fatalf("path count mismatch: KShortestPathsLooplessCtx=%d WithOpts=%d", len(want), len(got))
	}
	for i := range want {
		if want[i].Cost != got[i].Cost {
			t.Errorf("path %d cost: KShortestPathsLooplessCtx=%v WithOpts=%v", i, want[i].Cost, got[i].Cost)
		}
	}
}
