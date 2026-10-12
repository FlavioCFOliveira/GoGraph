package search

import (
	"context"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestBiBFSCtx_SymmetricMarkSkipsScan proves BiBFSCtx on a CSR marked
// symmetric pays no symmetry scan: it allocates no more than BiBFSOnCtx
// walking the same CSR in both directions, and returns the same path. The
// pre-#3078 scan built an E-sized hash set per call, which this bound
// excludes.
func TestBiBFSCtx_SymmetricMarkSkipsScan(t *testing.T) {
	a := adjlist.New[int, struct{}](adjlist.Config{})
	const n = 400
	for i := 0; i < n; i++ {
		for _, d := range []int{1, 7, 31} {
			if err := a.AddEdge(i, (i+d)%n, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	c := csr.BuildFromAdjList(a).BuildSymmetric()
	ctx := context.Background()
	src, dst := graph.NodeID(0), graph.NodeID(n/2)
	got, err := BiBFSCtx(ctx, c, src, dst)
	if err != nil {
		t.Fatalf("BiBFSCtx: %v", err)
	}
	want, err := BiBFSOnCtx(ctx, c, c, src, dst)
	if err != nil {
		t.Fatalf("BiBFSOnCtx: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("BiBFSCtx path %v, BiBFSOnCtx path %v", got, want)
	}
	viaCtx := testing.AllocsPerRun(20, func() { _, _ = BiBFSCtx(ctx, c, src, dst) })
	direct := testing.AllocsPerRun(20, func() { _, _ = BiBFSOnCtx(ctx, c, c, src, dst) })
	if viaCtx > direct {
		t.Fatalf("BiBFSCtx allocated %.0f objects per call, BiBFSOnCtx %.0f: the symmetric mark was not consulted", viaCtx, direct)
	}
}
