package centrality

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestBetweennessParallel_VsSerial asserts the parallel
// implementation produces bit-identical output to the serial
// Brandes on random graphs of varying topology.
func TestBetweennessParallel_VsSerial(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(197, 199)) //nolint:gosec // deterministic
	for seed := 0; seed < 5; seed++ {
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		const n = 64
		for i := 0; i < 2*n; i++ {
			a.AddEdge(r.IntN(n), r.IntN(n), struct{}{})
		}
		c := csr.BuildFromAdjList(a)
		serial := Betweenness(c)
		parallel := BetweennessParallel(c, 4)
		for i, sv := range serial {
			// 1e-9 absolute tolerance covers the float-summation
			// reorder that comes with the parallel reduce; the
			// algorithmic result is identical, only the addition
			// order differs.
			if math.Abs(sv-parallel[i]) > 1e-9 {
				t.Fatalf("seed=%d, node %d: serial=%f parallel=%f", seed, i, sv, parallel[i])
			}
		}
	}
}

// TestBetweennessParallel_CancellationCascades asserts that when one
// worker observes ctx.Err() the others stop in bounded time rather
// than continuing to grind on their source ranges. Without the
// cascade introduced for #192 this test would time out on a
// sufficiently large input.
func TestBetweennessParallel_CancellationCascades(t *testing.T) {
	t.Parallel()
	const n = 2048
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	r := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic
	for i := 0; i < 4*n; i++ {
		a.AddEdge(r.IntN(n), r.IntN(n), struct{}{})
	}
	c := csr.BuildFromAdjList(a)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately so every worker observes ctx.Err()
	// at most one source into its range; with the cascade the total
	// wall time stays well under 1s on commodity hardware.
	go func() {
		time.Sleep(1 * time.Millisecond)
		cancel()
	}()

	deadline := time.Now().Add(5 * time.Second)
	done := make(chan struct{})
	var got []float64
	var gotErr error
	go func() {
		defer close(done)
		got, gotErr = BetweennessParallelCtx(ctx, c, 8)
	}()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		t.Fatalf("BetweennessParallelCtx did not return within 5s after ctx cancel: cancellation did not cascade")
	}
	if got != nil {
		t.Fatalf("got non-nil result on cancellation: %v", got != nil)
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", gotErr)
	}
}

func BenchmarkBetweenness_Serial(b *testing.B) {
	c, _ := buildSerialBrandesFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Betweenness(c)
	}
}

func BenchmarkBetweenness_Parallel(b *testing.B) {
	c, _ := buildSerialBrandesFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BetweennessParallel(c, 0)
	}
}

func buildSerialBrandesFixture() (c *csr.CSR[struct{}], n int) {
	n = 512
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	r := rand.New(rand.NewPCG(211, 223)) //nolint:gosec // deterministic
	for i := 0; i < 3*n; i++ {
		a.AddEdge(r.IntN(n), r.IntN(n), struct{}{})
	}
	c = csr.BuildFromAdjList(a)
	return c, n
}
