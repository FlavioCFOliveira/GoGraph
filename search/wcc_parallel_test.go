package search

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// wccEqual asserts two WCC results are identical: same K and the same
// per-NodeID component label at every slot. This is the determinism
// oracle for the parallel variant against the serial reference.
func wccEqual(t *testing.T, wantComp []int, wantK int, gotComp []int, gotK int) {
	t.Helper()
	if wantK != gotK {
		t.Fatalf("component count mismatch: serial K=%d, parallel K=%d", wantK, gotK)
	}
	if len(wantComp) != len(gotComp) {
		t.Fatalf("component slice length mismatch: serial=%d parallel=%d", len(wantComp), len(gotComp))
	}
	for i := range wantComp {
		if wantComp[i] != gotComp[i] {
			t.Fatalf("component[%d]: serial=%d parallel=%d", i, wantComp[i], gotComp[i])
		}
	}
}

// TestWCCParallel_EqualSerial_Shapes asserts the parallel variant returns
// the byte-identical partition the serial WCC returns, across a spread of
// graph shapes and worker counts (including the serial-fallback path at
// numWorkers == 1). Several shapes sit below wccParallelMinEdges (so the
// serial-union branch runs) and several above (so the genuinely-parallel
// shard+merge branch runs).
func TestWCCParallel_EqualSerial_Shapes(t *testing.T) {
	t.Parallel()
	type shapeCase struct {
		name string
		c    *csr.CSR[int64]
	}
	mk := func(t *testing.T, s shapegen.Shape[int, int64], directed bool) *csr.CSR[int64] {
		g, err := s.Build(adjlist.Config{Directed: directed})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return csr.BuildFromAdjList(g.AdjList())
	}
	cases := []shapeCase{
		{"path-1k", mk(t, shapegen.Path(1000, true), true)},
		{"cycle-1k", mk(t, shapegen.Cycle(1000, false), false)},
		{"disjoint-stars", mk(t, shapegen.DoubleStar(500, 500), false)},
		{"ba-5k-directed", mk(t, shapegen.BarabasiAlbert(5000, 6, 17), true)},
		{"er-1k", mk(t, shapegen.ErdosRenyiNM(1000, 8000, 23), false)},
		{"ba-40k-directed", mk(t, shapegen.BarabasiAlbert(40000, 10, 5), true)},
	}
	for _, sc := range cases {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			wantComp, wantK, err := WCC(sc.c)
			if err != nil {
				t.Fatalf("serial WCC: %v", err)
			}
			for _, nw := range []int{1, 2, 3, 4, 8} {
				gotComp, gotK, err := WCCParallel(sc.c, nw)
				if err != nil {
					t.Fatalf("WCCParallel(nw=%d): %v", nw, err)
				}
				wccEqual(t, wantComp, wantK, gotComp, gotK)
			}
		})
	}
}

// TestWCCParallel_EmptyAndGhost covers the degenerate inputs: an empty
// CSR and an all-ghost CSR must return exactly what serial WCC returns.
func TestWCCParallel_EmptyAndGhost(t *testing.T) {
	t.Parallel()
	// Empty graph.
	empty := csr.BuildFromAdjList(adjlist.New[int, int64](adjlist.Config{Directed: true}))
	wComp, wK, err := WCC(empty)
	if err != nil {
		t.Fatalf("serial WCC empty: %v", err)
	}
	gComp, gK, err := WCCParallel(empty, 8)
	if err != nil {
		t.Fatalf("parallel WCC empty: %v", err)
	}
	wccEqual(t, wComp, wK, gComp, gK)
}

// TestWCCParallel_Cancellation asserts a context cancelled before the
// call returns the wrapped ctx.Err() and no partition.
func TestWCCParallel_Cancellation(t *testing.T) {
	t.Parallel()
	g, err := shapegen.BarabasiAlbert(40000, 10, 5).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	comp, k, err := WCCParallelCtx(ctx, c, 8)
	if err == nil || comp != nil || k != 0 {
		t.Fatalf("cancelled call returned (comp=%v, k=%d, err=%v); want (nil, 0, ctx err)", comp, k, err)
	}
}

// buildWCCBenchGraph builds a large power-law graph (E well above
// wccParallelMinEdges) for the scaling benchmark.
func buildWCCBenchGraph(b *testing.B) *csr.CSR[int64] {
	g, err := shapegen.BarabasiAlbert(100000, 12, 7).Build(adjlist.Config{Directed: true})
	if err != nil {
		b.Fatalf("Build: %v", err)
	}
	return csr.BuildFromAdjList(g.AdjList())
}

// BenchmarkWCC_Serial_Scaling and BenchmarkWCC_Parallel_Scaling are the
// benchstat pair for task #1679. Run:
//
//	go test -run='^$' -bench='BenchmarkWCC_(Serial|Parallel)_Scaling' -cpu=1,8 ./search/
func BenchmarkWCC_Serial_Scaling(b *testing.B) {
	c := buildWCCBenchGraph(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := WCC(c); err != nil {
			b.Fatalf("WCC: %v", err)
		}
	}
}

func BenchmarkWCC_Parallel_Scaling(b *testing.B) {
	c := buildWCCBenchGraph(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := WCCParallel(c, 0); err != nil {
			b.Fatalf("WCCParallel: %v", err)
		}
	}
}
