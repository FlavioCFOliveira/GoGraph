package search

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// assertAPSPBitEqual fails unless two APSP results are bit-identical
// across every (i, j) cell: same dimension, same reachability, and —
// where reachable — exactly equal distances (==, not within a
// tolerance). For float W this is bitwise equality of the IEEE-754
// values.
func assertAPSPBitEqual[W Weight](t *testing.T, want, got *APSP[W]) {
	t.Helper()
	if want.N() != got.N() {
		t.Fatalf("dimension mismatch: serial=%d parallel=%d", want.N(), got.N())
	}
	n := want.maxID
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			vS, okS := want.At(uint64ToNodeID(i), uint64ToNodeID(j))
			vP, okP := got.At(uint64ToNodeID(i), uint64ToNodeID(j))
			if okS != okP {
				t.Fatalf("(%d,%d): reachability serial=%v parallel=%v", i, j, okS, okP)
			}
			if okS && vS != vP {
				t.Fatalf("(%d,%d): serial=%v parallel=%v (not bit-identical)", i, j, vS, vP)
			}
		}
	}
}

// TestJohnsonAPSPParallel_BitEqualSerial_Fixtures asserts the parallel
// Johnson is bit-identical to the serial JohnsonAPSP on the canonical
// hand-built fixtures, including the CLRS negative-weight graph where
// the reweighting pass is load-bearing. Several worker counts are
// exercised to prove the output is independent of the worker count.
func TestJohnsonAPSPParallel_BitEqualSerial_Fixtures(t *testing.T) {
	t.Parallel()
	fixtures := map[string][]weightedEdge{
		"positive": {
			{0, 1, 10}, {0, 2, 3},
			{1, 3, 1},
			{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
			{3, 4, 7},
		},
		"clrs_negative": {
			{0, 1, 3}, {0, 2, 8}, {0, 4, -4},
			{1, 3, 1}, {1, 4, 7},
			{2, 1, 4},
			{3, 0, 2}, {3, 2, -5},
			{4, 3, 6},
		},
	}
	for name, edges := range fixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, _ := buildWeightedCSR(t, edges)
			serial, err := JohnsonAPSP(c)
			if err != nil {
				t.Fatalf("serial JohnsonAPSP: %v", err)
			}
			for _, nw := range []int{1, 2, 4, 8} {
				got, err := JohnsonAPSPParallel(c, nw)
				if err != nil {
					t.Fatalf("JohnsonAPSPParallel(nw=%d): %v", nw, err)
				}
				assertAPSPBitEqual(t, serial, got)
			}
		})
	}
}

// TestJohnsonAPSPParallel_BitEqualSerial_Random is the property-based
// bit-equality guard: across many random sparse mixed-sign graphs, the
// parallel Johnson must reproduce the serial output exactly whenever
// the serial run succeeds (no negative cycle). Integer weights make the
// arithmetic exact, so any mismatch is a genuine parallelisation bug.
func TestJohnsonAPSPParallel_BitEqualSerial_Random(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(2, 12).Draw(r, "n")
		m := rapid.IntRange(0, 3*n).Draw(r, "m")
		a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := 0; i < m; i++ {
			u := rapid.IntRange(0, n-1).Draw(r, "u")
			v := rapid.IntRange(0, n-1).Draw(r, "v")
			w := int64(rapid.IntRange(-3, 20).Draw(r, "w"))
			if err := a.AddEdge(u, v, w); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		serial, err := JohnsonAPSP(c)
		if err != nil {
			// Negative cycle: the parallel variant must reject it too,
			// with the same sentinel.
			if !errors.Is(err, ErrNegativeCycle) {
				r.Fatalf("serial JohnsonAPSP: unexpected error %v", err)
			}
			_, perr := JohnsonAPSPParallel(c, 4)
			if !errors.Is(perr, ErrNegativeCycle) {
				r.Fatalf("parallel did not reject negative cycle: %v", perr)
			}
			return
		}
		got, err := JohnsonAPSPParallel(c, 4)
		if err != nil {
			r.Fatalf("JohnsonAPSPParallel: %v", err)
		}
		assertAPSPBitEqualRapid(r, serial, got)
	})
}

// assertAPSPBitEqualRapid mirrors assertAPSPBitEqual for the rapid.T
// harness.
func assertAPSPBitEqualRapid[W Weight](r *rapid.T, want, got *APSP[W]) {
	if want.N() != got.N() {
		r.Fatalf("dimension mismatch: serial=%d parallel=%d", want.N(), got.N())
	}
	n := want.maxID
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			vS, okS := want.At(uint64ToNodeID(i), uint64ToNodeID(j))
			vP, okP := got.At(uint64ToNodeID(i), uint64ToNodeID(j))
			if okS != okP {
				r.Fatalf("(%d,%d): reachability serial=%v parallel=%v", i, j, okS, okP)
			}
			if okS && vS != vP {
				r.Fatalf("(%d,%d): serial=%v parallel=%v (not bit-identical)", i, j, vS, vP)
			}
		}
	}
}

// TestJohnsonAPSPParallel_FloatBitEqualSerial asserts bit-equality on a
// floating-point weighted graph: the recovery arithmetic is per cell
// with no cross-source reduce, so parallel and serial agree to the bit
// even though Johnson's float output itself may differ from
// Floyd-Warshall (a separate, documented caveat).
func TestJohnsonAPSPParallel_FloatBitEqualSerial(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	fedges := [][3]float64{
		{0, 1, 1.5}, {0, 2, 4.25},
		{1, 2, 0.5}, {1, 3, 7.125},
		{2, 3, 2.75}, {3, 0, 0.125},
	}
	for _, e := range fedges {
		if err := a.AddEdge(int(e[0]), int(e[1]), e[2]); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	serial, err := JohnsonAPSP(c)
	if err != nil {
		t.Fatalf("serial JohnsonAPSP: %v", err)
	}
	for _, nw := range []int{1, 2, 4, 8} {
		got, err := JohnsonAPSPParallel(c, nw)
		if err != nil {
			t.Fatalf("JohnsonAPSPParallel(nw=%d): %v", nw, err)
		}
		assertAPSPBitEqual(t, serial, got)
	}
}

// TestJohnsonAPSPParallel_NaNRejected asserts the parallel variant
// enforces the same NaN/+-Inf gate as the serial JohnsonAPSP.
func TestJohnsonAPSPParallel_NaNRejected(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, math.NaN()); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	got, err := JohnsonAPSPParallel(c, 4)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v, want ErrInvalidInput", err)
	}
	if got != nil {
		t.Fatalf("got=%v, want nil on invalid input", got)
	}
}

// TestJohnsonAPSPParallel_CancellationCascades asserts that cancelling
// ctx during the Dijkstra pass returns promptly with context.Canceled
// rather than letting workers grind their full stripes.
func TestJohnsonAPSPParallel_CancellationCascades(t *testing.T) {
	t.Parallel()
	const n = 600
	c := sparseRandomCSR(t, n, 91, 97, 0) // strictly positive: no negative cycle

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	var got *APSP[int64]
	var gotErr error
	go func() {
		defer close(done)
		got, gotErr = JohnsonAPSPParallelCtx(ctx, c, 8)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("JohnsonAPSPParallelCtx did not return within 5s after ctx cancel: cancellation did not cascade")
	}
	if got != nil {
		t.Fatalf("got non-nil result on cancellation")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", gotErr)
	}
}

// BenchmarkJohnsonAPSP_Serial / _Parallel form a -cpu scaling pair on
// the same V=512, E~=2V positive-weight fixture: run with -cpu=1,8 and
// compare with benchstat to read the per-source-Dijkstra parallel
// speedup across core counts. The parallel variant uses numWorkers=0
// so the worker count tracks GOMAXPROCS, which -cpu sets.
func BenchmarkJohnsonAPSP_Serial_Scaling(b *testing.B) {
	c := sparseRandomCSR(b, 512, 131, 173, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := JohnsonAPSP(c); err != nil {
			b.Fatalf("JohnsonAPSP: %v", err)
		}
	}
}

func BenchmarkJohnsonAPSP_Parallel_Scaling(b *testing.B) {
	c := sparseRandomCSR(b, 512, 131, 173, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := JohnsonAPSPParallel(c, 0); err != nil {
			b.Fatalf("JohnsonAPSPParallel: %v", err)
		}
	}
}
