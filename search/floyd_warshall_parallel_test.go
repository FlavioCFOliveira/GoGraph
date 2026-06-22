package search

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// apspBitEqual reports whether two APSP results are byte-for-byte
// identical: same dimensions, same reachability bitmap, and same
// distance for every reachable cell. It is the bit-identity oracle for
// the parallel Floyd-Warshall variant against the serial reference.
func apspBitEqual[W Weight](t *testing.T, want, got *APSP[W]) {
	t.Helper()
	if want.live != got.live || want.maxID != got.maxID {
		t.Fatalf("dimension mismatch: serial live=%d maxID=%d, parallel live=%d maxID=%d",
			want.live, want.maxID, got.live, got.maxID)
	}
	if len(want.dist) != len(got.dist) || len(want.found) != len(got.found) {
		t.Fatalf("backing-slice length mismatch: dist %d/%d found %d/%d",
			len(want.dist), len(got.dist), len(want.found), len(got.found))
	}
	for idx := range want.dist {
		if want.found[idx] != got.found[idx] {
			t.Fatalf("found[%d]: serial=%v parallel=%v", idx, want.found[idx], got.found[idx])
		}
		if want.found[idx] && want.dist[idx] != got.dist[idx] {
			t.Fatalf("dist[%d]: serial=%v parallel=%v", idx, want.dist[idx], got.dist[idx])
		}
	}
}

// buildRandomDirectedInt64 builds a deterministic random directed graph
// with positive int64 weights, dense enough to cross floydParallelMinDim.
func buildRandomDirectedInt64(t testing.TB, n, edgesPerNode int, seed uint64) *csr.CSR[int64] {
	t.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	r := rand.New(rand.NewPCG(seed, seed*2+1)) //nolint:gosec // deterministic test RNG
	for i := 0; i < n*edgesPerNode; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(100)+1)); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	// Guarantee every node is live (an incident edge) so live == n and
	// the matrix dimension is deterministic across the (serial,parallel)
	// pair under comparison.
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, int64(r.IntN(100)+1)); err != nil {
			t.Fatalf("AddEdge ring: %v", err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// TestFloydWarshallParallel_BitEqualSerial_Fixtures asserts the parallel
// variant matches the serial reference bit-for-bit on the hand-built
// fixtures, across several worker counts including the serial-fallback
// (numWorkers == 1) and below-threshold paths.
func TestFloydWarshallParallel_BitEqualSerial_Fixtures(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	})
	serial := FloydWarshall(c)
	for _, nw := range []int{1, 2, 4, 8} {
		got := FloydWarshallParallel(c, nw)
		apspBitEqual(t, serial, got)
	}
}

// TestFloydWarshallParallel_BitEqualSerial_Random asserts bit-identity
// across a spread of random directed graphs that cross
// floydParallelMinDim, at several worker counts. This is the core
// determinism guarantee: the result must not depend on numWorkers or on
// how the destination rows are scheduled.
func TestFloydWarshallParallel_BitEqualSerial_Random(t *testing.T) {
	t.Parallel()
	for _, n := range []int{130, 200, 333} {
		c := buildRandomDirectedInt64(t, n, 4, uint64(n))
		serial := FloydWarshall(c)
		for _, nw := range []int{1, 2, 3, 4, 8} {
			got := FloydWarshallParallel(c, nw)
			apspBitEqual(t, serial, got)
		}
	}
}

// TestFloydWarshallParallel_FloatBitEqualSerial asserts bit-identity on
// float64 weights, where any change in the relaxation order would surface
// as an ULP-level divergence. The snapshot kernel performs a single
// min(ik+kj) per cell with no cross-worker reduction, so the parallel
// output must match the serial output exactly.
func TestFloydWarshallParallel_FloatBitEqualSerial(t *testing.T) {
	t.Parallel()
	const n = 160
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	r := rand.New(rand.NewPCG(424242, 99)) //nolint:gosec // deterministic test RNG
	for i := 0; i < n*5; i++ {
		w := r.Float64()*9 + 0.0009765625 // exact-ish positive floats
		if err := a.AddEdge(r.IntN(n), r.IntN(n), w); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, r.Float64()+0.5); err != nil {
			t.Fatalf("AddEdge ring: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	serial := FloydWarshall(c)
	for _, nw := range []int{2, 4, 8} {
		got := FloydWarshallParallel(c, nw)
		apspBitEqual(t, serial, got)
	}
}

// TestFloydWarshallParallel_NegCycle asserts the parallel variant detects
// negative-weight cycles identically to the serial reference: the simple
// entry returns nil, the Ctx variant returns ErrNegativeCycle.
func TestFloydWarshallParallel_NegCycle(t *testing.T) {
	t.Parallel()
	// A 3-cycle whose total weight is negative (0->1->2->0 sums to -3),
	// padded with a disjoint positive ring over nodes 3..199 so live
	// crosses floydParallelMinDim and the genuinely-parallel path runs.
	edges := []weightedEdge{{0, 1, 1}, {1, 2, 1}, {2, 0, -5}}
	for i := 3; i < 199; i++ {
		edges = append(edges, weightedEdge{i, i + 1, 1})
	}
	edges = append(edges, weightedEdge{199, 3, 1})
	c, _ := buildWeightedCSR(t, edges)
	if got := FloydWarshallParallel(c, 8); got != nil {
		t.Fatalf("FloydWarshallParallel on a negative cycle returned non-nil; want nil")
	}
	_, err := FloydWarshallParallelCtx(context.Background(), c, 8)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("FloydWarshallParallelCtx err = %v, want ErrNegativeCycle", err)
	}
	// Serial agrees.
	if _, serr := FloydWarshallCtx(context.Background(), c); !errors.Is(serr, ErrNegativeCycle) {
		t.Fatalf("serial FloydWarshallCtx err = %v, want ErrNegativeCycle", serr)
	}
}

// TestFloydWarshallParallel_NaNRejected asserts the NaN/Inf input gate
// fires on the parallel path exactly as on the serial path.
func TestFloydWarshallParallel_NaNRejected(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, math.NaN()); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	if _, err := FloydWarshallParallelCtx(context.Background(), c, 8); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

// TestFloydWarshallParallel_Cancellation asserts a context cancelled
// before the call returns the wrapped ctx.Err() rather than a matrix.
func TestFloydWarshallParallel_Cancellation(t *testing.T) {
	t.Parallel()
	c := buildRandomDirectedInt64(t, 200, 4, 7)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := FloydWarshallParallelCtx(ctx, c, 8)
	if err == nil || out != nil {
		t.Fatalf("cancelled call returned (out=%v, err=%v); want (nil, ctx err)", out, err)
	}
}

// BenchmarkFloydWarshall_Serial_Scaling and
// BenchmarkFloydWarshall_Parallel_Scaling are the benchstat pair for
// task #1680. Run:
//
//	go test -run='^$' -bench='BenchmarkFloydWarshall_(Serial|Parallel)_Scaling' -cpu=1,8 ./search/
//
// The parallel benchmark distributes each pivot's rows across
// GOMAXPROCS workers; on a dense V=512 graph the speedup should track
// the host's physical core count.
func benchFWDenseGraph(b *testing.B) *csr.CSR[int64] {
	const n = 512
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	r := rand.New(rand.NewPCG(11, 13)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < n*n/4; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(100)+1)); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	return csr.BuildFromAdjList(a)
}

func BenchmarkFloydWarshall_Serial_Scaling(b *testing.B) {
	c := benchFWDenseGraph(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FloydWarshall(c)
	}
}

func BenchmarkFloydWarshall_Parallel_Scaling(b *testing.B) {
	c := benchFWDenseGraph(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FloydWarshallParallel(c, 0)
	}
}
