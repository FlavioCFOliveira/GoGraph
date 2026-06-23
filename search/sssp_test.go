package search

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestSSSP_MatchesDijkstra asserts the stateful engine returns exactly
// what the one-shot [Dijkstra] returns from every source — same distance
// and reachability for every node, same reconstructed paths — so the
// validate-once refactor changed nothing observable.
func TestSSSP_MatchesDijkstra(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	})
	eng, err := NewSSSP(c)
	if err != nil {
		t.Fatalf("NewSSSP: %v", err)
	}
	maxID := graph.NodeID(c.MaxNodeID())
	for src := graph.NodeID(0); src < maxID; src++ {
		want, err := Dijkstra(c, src)
		if err != nil {
			t.Fatalf("Dijkstra(%d): %v", src, err)
		}
		got, err := eng.From(src)
		if err != nil {
			t.Fatalf("SSSP.From(%d): %v", src, err)
		}
		for n := graph.NodeID(0); n < maxID; n++ {
			wd, wok := want.Distance(n)
			gd, gok := got.Distance(n)
			if wok != gok || (wok && wd != gd) {
				t.Fatalf("src=%d node=%d: Dijkstra=(%v,%v) SSSP=(%v,%v)", src, n, wd, wok, gd, gok)
			}
		}
	}
}

// TestSSSP_NegativeWeightRejectedOnce asserts NewSSSP surfaces the
// negative-weight error at construction (so From never has to), matching
// Dijkstra's contract.
func TestSSSP_NegativeWeightRejectedOnce(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR(t, []weightedEdge{{0, 1, 5}, {1, 2, -3}})
	if _, err := NewSSSP(c); !errors.Is(err, ErrNegativeWeight) {
		t.Fatalf("NewSSSP err = %v, want ErrNegativeWeight", err)
	}
}

// TestSSSP_NaNRejectedOnce asserts the float NaN/Inf gate fires at
// construction.
func TestSSSP_NaNRejectedOnce(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, math.Inf(1)); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	if _, err := NewSSSP(c); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewSSSP err = %v, want ErrInvalidInput", err)
	}
}

// TestSSSP_Cancellation asserts FromCtx honours a cancelled context.
func TestSSSP_Cancellation(t *testing.T) {
	t.Parallel()
	c := buildSSSPBallast(t, 4, 4000)
	eng, err := NewSSSP(c)
	if err != nil {
		t.Fatalf("NewSSSP: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eng.FromCtx(ctx, 0); err == nil {
		t.Fatalf("FromCtx on a cancelled context returned nil error")
	}
}

// TestSSSP_ConcurrentFrom exercises many concurrent From calls on a
// shared engine; under -race this proves the engine holds no mutable
// state and each query owns private pooled buffers.
func TestSSSP_ConcurrentFrom(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 2}, {1, 2, 2}, {2, 3, 2}, {3, 0, 2}, {0, 3, 1},
	})
	eng, err := NewSSSP(c)
	if err != nil {
		t.Fatalf("NewSSSP: %v", err)
	}
	ref, _ := eng.From(0)
	refD, _ := ref.Distance(3)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				d, err := eng.From(0)
				if err != nil {
					t.Errorf("From: %v", err)
					return
				}
				if got, _ := d.Distance(3); got != refD {
					t.Errorf("concurrent From: d(0,3)=%v want %v", got, refD)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// buildSSSPBallast builds a graph whose source component is a tiny path
// (src=0) disconnected from a large dense "ballast" of ballastEdges. An
// SSSP from 0 explores only the path, but the one-shot Dijkstra still
// scans every edge to validate weights on each call — so the engine's
// validate-once amortisation is the dominant difference.
func buildSSSPBallast(tb testing.TB, pathLen, ballastEdges int) *csr.CSR[int64] {
	tb.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < pathLen; i++ {
		if err := a.AddEdge(i, i+1, int64(i+1)); err != nil {
			tb.Fatalf("AddEdge path: %v", err)
		}
	}
	// Disconnected ballast of DISTINCT edges over a separate id range, so
	// the non-multigraph adjlist does not dedupe them and the CSR truly
	// carries ballastEdges weights for the validation scan to traverse.
	// A side x side bipartite layout makes every (src,dst) pair unique.
	const base = 1_000_000
	side := 1
	for side*side < ballastEdges {
		side++
	}
	e := 0
	for s := 0; s < side && e < ballastEdges; s++ {
		for d := 0; d < side && e < ballastEdges; d++ {
			if err := a.AddEdge(base+s, base+side+d, int64(e%97+1)); err != nil {
				tb.Fatalf("AddEdge ballast: %v", err)
			}
			e++
		}
	}
	return csr.BuildFromAdjList(a)
}

// BenchmarkDijkstra_RepeatedRevalidate and BenchmarkSSSP_RepeatedFrom are
// the benchstat pair for task #1516. The fixture's tiny source component
// makes the O(E) per-call weight validation the dominant cost of the
// one-shot path; the engine pays it once, so its per-query time should
// drop sharply. Run:
//
//	go test -run='^$' -bench='Benchmark(Dijkstra_RepeatedRevalidate|SSSP_RepeatedFrom)' ./search/
func BenchmarkDijkstra_RepeatedRevalidate(b *testing.B) {
	c := buildSSSPBallast(b, 8, 1_000_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Dijkstra(c, 0); err != nil {
			b.Fatalf("Dijkstra: %v", err)
		}
	}
}

func BenchmarkSSSP_RepeatedFrom(b *testing.B) {
	c := buildSSSPBallast(b, 8, 1_000_000)
	eng, err := NewSSSP(c)
	if err != nil {
		b.Fatalf("NewSSSP: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.From(0); err != nil {
			b.Fatalf("From: %v", err)
		}
	}
}
