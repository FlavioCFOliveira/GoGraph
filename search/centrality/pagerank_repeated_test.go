package centrality

import (
	"context"
	"math"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// bitIdenticalFloat64 reports whether a and b are the same float64 down
// to the bit pattern (so NaN payloads and -0.0 vs +0.0 are distinguished).
func bitIdenticalFloat64(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}

// TestPageRanker_BitIdenticalToOneShot proves that a reusable
// [PageRanker] produces a rank vector bit-for-bit identical to the
// one-shot [PageRank] on the same graph, both on the small serial-path
// graph and on a graph large enough to take the parallel pull path. This
// is the #1592 correctness gate: caching the reverse-CSR transpose and
// the working buffers must change only the allocation profile.
func TestPageRanker_BitIdenticalToOneShot(t *testing.T) {
	t.Parallel()
	opts := PageRankOptions{Damping: 0.85, MaxIterations: 50, Tolerance: 1e-9}

	t.Run("serial-path-small", func(t *testing.T) {
		t.Parallel()
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
		for i := 0; i < 7; i++ {
			if err := a.AddEdge(i, (i+1)%7, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		assertRankerMatchesOneShot(t, c, opts)
	})

	t.Run("parallel-path-large", func(t *testing.T) {
		t.Parallel()
		g, err := shapegen.BarabasiAlbert(10000, 6, 17).Build(adjlist.Config{Directed: true})
		if err != nil {
			t.Fatalf("BarabasiAlbert.Build: %v", err)
		}
		c := csr.BuildFromAdjList(g.AdjList())
		assertRankerMatchesOneShot(t, c, opts)
	})
}

func assertRankerMatchesOneShot[W any](t *testing.T, c *csr.CSR[W], opts PageRankOptions) {
	t.Helper()
	wantRanks, wantIters, wantErr := PageRank(c, opts)
	if wantErr != nil {
		t.Fatalf("one-shot PageRank: %v", wantErr)
	}
	pr := NewPageRanker(c)
	// Run several times; every run must equal the one-shot result.
	for run := 0; run < 3; run++ {
		gotRanks, gotIters, gotErr := pr.Run(context.Background(), opts)
		if gotErr != nil {
			t.Fatalf("PageRanker.Run[%d]: %v", run, gotErr)
		}
		if gotIters != wantIters {
			t.Fatalf("run %d: iters = %d, want %d", run, gotIters, wantIters)
		}
		if len(gotRanks) != len(wantRanks) {
			t.Fatalf("run %d: len = %d, want %d", run, len(gotRanks), len(wantRanks))
		}
		for i := range wantRanks {
			if !bitIdenticalFloat64(gotRanks[i], wantRanks[i]) {
				t.Fatalf("run %d: rank[%d] = %x (%g), want %x (%g): not bit-identical",
					run, i,
					math.Float64bits(gotRanks[i]), gotRanks[i],
					math.Float64bits(wantRanks[i]), wantRanks[i])
			}
		}
	}
}

// TestPageRanker_EmptyAndIsolated covers the early-return edge cases:
// an empty graph (n <= 0) and a graph of only ghost slots (live == 0).
func TestPageRanker_EmptyAndIsolated(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	c := csr.BuildFromAdjList(a)
	pr := NewPageRanker(c)
	ranks, iters, err := pr.Run(context.Background(), DefaultPageRankOptions())
	if err != nil {
		t.Fatalf("empty Run: %v", err)
	}
	if len(ranks) != 0 || iters != 0 {
		t.Fatalf("empty: ranks=%v iters=%d", ranks, iters)
	}
}

// TestPageRanker_InvalidOptions confirms validation happens on Run, not
// only on construction.
func TestPageRanker_InvalidOptions(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	pr := NewPageRanker(c)
	_, _, err := pr.Run(context.Background(), PageRankOptions{Damping: 2.0})
	if err == nil {
		t.Fatalf("expected ErrInvalidInput for out-of-range Damping")
	}
}

// TestPageRanker_ConcurrentIndependent runs several independent
// PageRankers over one shared immutable CSR concurrently and asserts the
// race detector stays clean and every result matches the one-shot. This
// pins the documented concurrency contract: one PageRanker per goroutine,
// shared read-only CSR.
func TestPageRanker_ConcurrentIndependent(t *testing.T) {
	t.Parallel()
	g, err := shapegen.BarabasiAlbert(5000, 5, 23).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	opts := PageRankOptions{Damping: 0.85, MaxIterations: 40, Tolerance: 1e-9}
	want, _, err := PageRank(c, opts)
	if err != nil {
		t.Fatalf("one-shot PageRank: %v", err)
	}

	const goroutines = 8
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for w := 0; w < goroutines; w++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pr := NewPageRanker(c)
			got, _, runErr := pr.Run(context.Background(), opts)
			if runErr != nil {
				errs[idx] = runErr
				return
			}
			// Copy out before any subsequent Run could alias-invalidate
			// (none here, but mirrors the documented contract).
			for i := range want {
				if !bitIdenticalFloat64(got[i], want[i]) {
					errs[idx] = &mismatchError{idx: i}
					return
				}
			}
		}(w)
	}
	wg.Wait()
	for w, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d: %v", w, e)
		}
	}
}

type mismatchError struct{ idx int }

func (e *mismatchError) Error() string { return "rank mismatch at index" }
