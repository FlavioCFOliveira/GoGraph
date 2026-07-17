package search

import (
	"context"
	"errors"
	"testing"
)

func TestKShortestPathsLoopless_TwoPaths(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	got := KShortestPathsLoopless(c, src, dst, 2) //nolint:staticcheck // deliberately exercises the deprecated bare entry to keep it covered
	if len(got) != 2 {
		t.Fatalf("got %d paths, want 2", len(got))
	}
	if got[0].Cost != 3 {
		t.Fatalf("first path cost = %d, want 3", got[0].Cost)
	}
	if got[1].Cost != 4 {
		t.Fatalf("second path cost = %d, want 4", got[1].Cost)
	}
}

func TestKShortestPathsLoopless_NoPath(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{{0, 1, 1}, {2, 3, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	if got := KShortestPathsLoopless(c, src, dst, 3); len(got) != 0 { //nolint:staticcheck // deliberately exercises the deprecated bare entry to keep it covered
		t.Fatalf("expected no paths, got %d", len(got))
	}
}

func TestKShortestPathsLoopless_VsYen(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
		{0, 3, 10},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	yen := YenKShortest(c, src, dst, 3)
	loopless := KShortestPathsLoopless(c, src, dst, 3) //nolint:staticcheck // deliberately exercises the deprecated bare entry to keep it covered
	if len(yen) != len(loopless) {
		t.Fatalf("yen len=%d, loopless len=%d", len(yen), len(loopless))
	}
	for i := range yen {
		if yen[i].Cost != loopless[i].Cost {
			t.Fatalf("path %d: yen cost=%d loopless cost=%d", i, yen[i].Cost, loopless[i].Cost)
		}
	}
}

func TestKShortestPathsLooplessCtx_Cancellation(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 1}, {1, 2, 1}, {2, 3, 1}, {0, 3, 100},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := KShortestPathsLooplessCtx(ctx, c, src, dst, 3); err != nil {
		// expected: either a clean return (queue drains before the
		// 4096-pop check) or context.Canceled — both are acceptable.
		if err != context.Canceled { //nolint:errorlint // sentinel returned directly
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

// TestKShortestPathsLooplessCtx_CancelledMidEnumeration proves that the
// 4096-pop cancellation gate inside the best-first loopless enumerator actually
// fires (rmp #1997): on a graph whose frontier is far larger than 4096 pops, a
// cancelled context surfaces as context.Canceled rather than being swallowed.
// It complements TestKShortestPathsLooplessCtx_Cancellation above, which only
// covers the tiny-graph "frontier drains before the gate is reached" path.
func TestKShortestPathsLooplessCtx_CancelledMidEnumeration(t *testing.T) {
	t.Parallel()
	// A depth-13 diamond chain has a super-polynomial frontier: the first
	// complete src->dst path (cost depth+1) is reached only after popping every
	// cheaper prefix (~2^(depth+1) ≈ 16384 > 4096), so the tick&0xFFF gate is
	// guaranteed to observe the cancelled context before k paths are found.
	edges, src, dst := diamondChainEdges(13, true)
	c, a := buildWeightedCSR(t, edges)
	srcID, _ := a.Mapper().Lookup(src)
	dstID, _ := a.Mapper().Lookup(dst)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel up front; the gate must observe it mid-enumeration

	paths, err := KShortestPathsLooplessCtx(ctx, c, srcID, dstID, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from the mid-enumeration gate, got err=%v (%d partial paths)", err, len(paths))
	}
}

// TestEppsteinKShortest_DeprecatedAlias keeps the deprecated alias
// covered so the wrapper does not silently break in future refactors.
//
//nolint:staticcheck // intentional exercise of the deprecated API
func TestEppsteinKShortest_DeprecatedAlias(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	got := EppsteinKShortest(c, src, dst, 2)
	if len(got) != 2 {
		t.Fatalf("deprecated alias: got %d paths, want 2", len(got))
	}
	gotCtx, err := EppsteinKShortestCtx(context.Background(), c, src, dst, 2)
	if err != nil {
		t.Fatalf("deprecated Ctx alias: %v", err)
	}
	if len(gotCtx) != 2 {
		t.Fatalf("deprecated Ctx alias: got %d paths, want 2", len(gotCtx))
	}
}
