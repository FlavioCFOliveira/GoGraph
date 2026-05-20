package search

import (
	"context"
	"testing"
)

func TestKShortestPathsLoopless_TwoPaths(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	got := KShortestPathsLoopless(c, src, dst, 2)
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
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 1}, {2, 3, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	if got := KShortestPathsLoopless(c, src, dst, 3); len(got) != 0 {
		t.Fatalf("expected no paths, got %d", len(got))
	}
}

func TestKShortestPathsLoopless_VsYen(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
		{0, 3, 10},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	yen := YenKShortest(c, src, dst, 3)
	loopless := KShortestPathsLoopless(c, src, dst, 3)
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
	c, a := buildWeightedCSR([]weightedEdge{
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

// TestEppsteinKShortest_DeprecatedAlias keeps the deprecated alias
// covered so the wrapper does not silently break in future refactors.
//
//nolint:staticcheck // intentional exercise of the deprecated API
func TestEppsteinKShortest_DeprecatedAlias(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{
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
