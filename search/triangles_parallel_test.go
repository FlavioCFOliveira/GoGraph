package search

import (
	"context"
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildUndirectedTriangleFixture returns a deterministic undirected
// circulant graph on n vertices: every vertex is joined to its next k
// neighbours (mod n), where k = degEachSide. A circulant graph is dense
// in triangles — consecutive chords (i,i+1), (i+1,i+2), (i,i+2) close a
// triangle — which makes it a non-vacuous fixture for the bit-equality
// assertion and a uniform-degree fixture for the scaling benchmark. It
// needs no RNG and is fully reproducible.
func buildUndirectedTriangleFixture(tb testing.TB, n, degEachSide int) *csr.CSR[struct{}] {
	tb.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for v := 0; v < n; v++ {
		for d := 1; d <= degEachSide; d++ {
			u := (v + d) % n
			if u == v {
				continue
			}
			if err := a.AddEdge(v, u, struct{}{}); err != nil {
				tb.Fatalf("AddEdge: %v", err)
			}
		}
	}
	return csr.BuildFromAdjList(a)
}

// assertTriangleCountsEqual fails unless the parallel total and perNode
// slice are exactly equal to the serial ones — bit-equality for an
// integer count.
func assertTriangleCountsEqual(t *testing.T, wantTotal int64, wantPerNode []int64, gotTotal int64, gotPerNode []int64) {
	t.Helper()
	if wantTotal != gotTotal {
		t.Fatalf("total: serial=%d parallel=%d", wantTotal, gotTotal)
	}
	if len(wantPerNode) != len(gotPerNode) {
		t.Fatalf("perNode length: serial=%d parallel=%d", len(wantPerNode), len(gotPerNode))
	}
	for i := range wantPerNode {
		if wantPerNode[i] != gotPerNode[i] {
			t.Fatalf("perNode[%d]: serial=%d parallel=%d", i, wantPerNode[i], gotPerNode[i])
		}
	}
}

// TestCountTrianglesParallel_K5 asserts the parallel count reproduces
// the known K5 result (10 triangles, every vertex in 6) across worker
// counts.
func TestCountTrianglesParallel_K5(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 5; i++ {
		for j := i + 1; j < 5; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)
	m := a.Mapper()
	for _, nw := range []int{1, 2, 4, 8} {
		total, perNode := CountTrianglesParallel(c, nw)
		if total != 10 {
			t.Fatalf("nw=%d: K5 total = %d, want 10", nw, total)
		}
		for i := 0; i < 5; i++ {
			id, _ := m.Lookup(i)
			if perNode[id] != 6 {
				t.Fatalf("nw=%d: K5 perNode[%d] = %d, want 6", nw, i, perNode[id])
			}
		}
	}
}

// TestCountTrianglesParallel_BitEqualSerial asserts the parallel count
// is bit-identical to the serial CountTriangles (total and perNode) on
// a dense deterministic fixture, across several worker counts to prove
// the result is independent of the worker count.
func TestCountTrianglesParallel_BitEqualSerial(t *testing.T) {
	t.Parallel()
	c := buildUndirectedTriangleFixture(t, 256, 12)
	wantTotal, wantPerNode := CountTriangles(c)
	if wantTotal == 0 {
		t.Fatalf("fixture has no triangles; the bit-equality assertion would be vacuous")
	}
	for _, nw := range []int{1, 2, 3, 4, 8, 16} {
		gotTotal, gotPerNode := CountTrianglesParallel(c, nw)
		assertTriangleCountsEqual(t, wantTotal, wantPerNode, gotTotal, gotPerNode)
	}
}

// TestCountTrianglesParallel_BitEqualSerial_Random is the property-based
// bit-equality guard: across many random undirected graphs the parallel
// count must reproduce the serial total and perNode exactly.
func TestCountTrianglesParallel_BitEqualSerial_Random(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(2, 30).Draw(r, "n")
		m := rapid.IntRange(0, 4*n).Draw(r, "m")
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := 0; i < m; i++ {
			u := rapid.IntRange(0, n-1).Draw(r, "u")
			v := rapid.IntRange(0, n-1).Draw(r, "v")
			if u == v {
				continue
			}
			if err := a.AddEdge(u, v, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		wantTotal, wantPerNode := CountTriangles(c)
		for _, nw := range []int{1, 4, 8} {
			gotTotal, gotPerNode := CountTrianglesParallel(c, nw)
			if wantTotal != gotTotal {
				r.Fatalf("nw=%d total: serial=%d parallel=%d", nw, wantTotal, gotTotal)
			}
			for i := range wantPerNode {
				if wantPerNode[i] != gotPerNode[i] {
					r.Fatalf("nw=%d perNode[%d]: serial=%d parallel=%d", nw, i, wantPerNode[i], gotPerNode[i])
				}
			}
		}
	})
}

// TestCountTrianglesParallel_Empty asserts the n==0 fast path returns
// (0, nil) without spawning workers.
func TestCountTrianglesParallel_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	c := csr.BuildFromAdjList(a)
	total, perNode := CountTrianglesParallel(c, 4)
	if total != 0 || perNode != nil {
		t.Fatalf("empty graph: total=%d perNode=%v, want 0, nil", total, perNode)
	}
}

// TestCountTrianglesParallel_CancellationCascades asserts that
// cancelling ctx mid-count returns promptly with context.Canceled.
func TestCountTrianglesParallel_CancellationCascades(t *testing.T) {
	t.Parallel()
	// A dense fixture so each worker has real pair-loop work before it
	// observes the cancel.
	c := buildUndirectedTriangleFixture(t, 2048, 64)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	var gotTotal int64
	var gotPerNode []int64
	var gotErr error
	go func() {
		defer close(done)
		gotTotal, gotPerNode, gotErr = CountTrianglesParallelCtx(ctx, c, 8)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("CountTrianglesParallelCtx did not return within 5s after ctx cancel: cancellation did not cascade")
	}
	if gotErr == nil {
		// It is acceptable for a very fast machine to finish before the
		// cancel lands; in that case the result must be the correct
		// serial count, not a partial one.
		wantTotal, wantPerNode := CountTriangles(c)
		assertTriangleCountsEqual(t, wantTotal, wantPerNode, gotTotal, gotPerNode)
		return
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", gotErr)
	}
	if gotPerNode != nil {
		t.Fatalf("got non-nil perNode on cancellation")
	}
}

// BenchmarkCountTriangles_Serial / _Parallel form a -cpu scaling pair
// on the same dense fixture: run with -cpu=1,8 and compare with
// benchstat to read the parallel speedup across core counts. The
// parallel variant uses numWorkers=0 so the worker count tracks
// GOMAXPROCS, which -cpu sets.
func benchTriangleFixture(tb testing.TB) *csr.CSR[struct{}] {
	tb.Helper()
	return buildUndirectedTriangleFixture(tb, 2000, 80)
}

func BenchmarkCountTriangles_Serial_Scaling(b *testing.B) {
	c := benchTriangleFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = CountTriangles(c)
	}
}

func BenchmarkCountTriangles_Parallel_Scaling(b *testing.B) {
	c := benchTriangleFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = CountTrianglesParallel(c, 0)
	}
}
