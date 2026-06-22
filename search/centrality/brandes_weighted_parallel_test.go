package centrality

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildWeightedParallelFixture returns a deterministic undirected
// weighted CSR with enough alternative shortest paths to give most
// vertices a non-trivial betweenness score and to force an observable
// floating-point accumulation difference between the serial and
// parallel reduces.
func buildWeightedParallelFixture(tb testing.TB, n, edges int, seed1, seed2 uint64) *csr.CSR[float64] {
	tb.Helper()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	r := rand.New(rand.NewPCG(seed1, seed2)) //nolint:gosec // deterministic test RNG
	for i := 0; i < edges; i++ {
		u := r.IntN(n)
		v := r.IntN(n)
		// Strictly positive weights in [1, 10]; weighted Brandes is
		// undefined on non-positive arcs.
		w := float64(r.IntN(10) + 1)
		if err := a.AddEdge(u, v, w); err != nil {
			tb.Fatalf("AddEdge: %v", err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// TestWeightedBetweennessParallel_VsSerial asserts the parallel
// weighted implementation agrees with the serial weighted Brandes
// within 1e-9 per node across random graphs of varying topology. It
// deliberately does NOT assert bit-equality: parallelising over
// sources re-associates the cross-source dependency sum and IEEE-754
// addition is non-associative, so a small drift is expected and
// tolerated (see the godoc of WeightedBetweennessParallel).
func TestWeightedBetweennessParallel_VsSerial(t *testing.T) {
	t.Parallel()
	for seed := uint64(0); seed < 5; seed++ {
		const n = 64
		c := buildWeightedParallelFixture(t, n, 2*n, 197+seed, 199+seed)
		serial, err := WeightedBetweenness(c)
		if err != nil {
			t.Fatalf("seed=%d serial: %v", seed, err)
		}
		parallel, err := WeightedBetweennessParallel(c, 4)
		if err != nil {
			t.Fatalf("seed=%d parallel: %v", seed, err)
		}
		if len(serial) != len(parallel) {
			t.Fatalf("seed=%d length mismatch: serial=%d parallel=%d", seed, len(serial), len(parallel))
		}
		for i, sv := range serial {
			// 1e-9 absolute tolerance covers the float-summation
			// reorder that comes with the parallel reduce; the
			// algorithmic result is identical, only the addition order
			// differs.
			if math.Abs(sv-parallel[i]) > 1e-9 {
				t.Fatalf("seed=%d, node %d: serial=%f parallel=%f", seed, i, sv, parallel[i])
			}
		}
	}
}

// TestWeightedBetweennessParallel_DeterministicForFixedWorkers asserts
// the parallel output is reproducible run-to-run for a fixed
// numWorkers, even though it is not bit-identical to the serial path.
func TestWeightedBetweennessParallel_DeterministicForFixedWorkers(t *testing.T) {
	t.Parallel()
	const n = 96
	c := buildWeightedParallelFixture(t, n, 3*n, 421, 433)
	first, err := WeightedBetweennessParallel(c, 4)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	for rep := 0; rep < 8; rep++ {
		again, err := WeightedBetweennessParallel(c, 4)
		if err != nil {
			t.Fatalf("rep %d: %v", rep, err)
		}
		for i := range first {
			if first[i] != again[i] {
				t.Fatalf("rep %d node %d: %v != %v (not deterministic for fixed numWorkers)",
					rep, i, again[i], first[i])
			}
		}
	}
}

// TestWeightedBetweennessParallel_InvalidInput asserts the parallel
// variant enforces the same input contract as WeightedBetweenness:
// NaN/Inf -> ErrInvalidInput, non-positive -> ErrNonPositiveWeight,
// with a nil result on error.
func TestWeightedBetweennessParallel_InvalidInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		w    float64
		want error
	}{
		{"nan", math.NaN(), ErrInvalidInput},
		{"posinf", math.Inf(1), ErrInvalidInput},
		{"neginf", math.Inf(-1), ErrInvalidInput},
		{"zero", 0.0, ErrNonPositiveWeight},
		{"negative", -2.0, ErrNonPositiveWeight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := adjlist.New[int, float64](adjlist.Config{Directed: false})
			if err := a.AddEdge(0, 1, 1.0); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			if err := a.AddEdge(1, 2, tc.w); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			c := csr.BuildFromAdjList(a)
			got, err := WeightedBetweennessParallel(c, 4)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
			if got != nil {
				t.Fatalf("got=%v, want nil on invalid input", got)
			}
		})
	}
}

// TestWeightedBetweennessParallel_CancellationCascades asserts that
// when one worker observes ctx.Err() the others stop in bounded time
// rather than continuing to grind on their source stripes.
func TestWeightedBetweennessParallel_CancellationCascades(t *testing.T) {
	t.Parallel()
	const n = 2048
	c := buildWeightedParallelFixture(t, n, 4*n, 7, 11)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately so every worker observes ctx.Err() at
	// most one source into its stripe; with the cascade the total wall
	// time stays well under 1s on commodity hardware.
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
		got, gotErr = WeightedBetweennessParallelCtx(ctx, c, 8)
	}()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		t.Fatalf("WeightedBetweennessParallelCtx did not return within 5s after ctx cancel: cancellation did not cascade")
	}
	if got != nil {
		t.Fatalf("got non-nil result on cancellation: %v", got != nil)
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", gotErr)
	}
}

// TestWeightedBetweennessParallel_GodocNoBitIdentityClaim enforces that
// the godoc of WeightedBetweennessParallel does not claim bit-identity
// with the serial result. Bit-identity is false for numWorkers > 1 due
// to non-associative floating-point addition in the cross-source
// dependency reduce.
//
// This test fails when the source file claims "bit-identical" and
// passes once that claim is absent.
func TestWeightedBetweennessParallel_GodocNoBitIdentityClaim(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("brandes_weighted_parallel.go")
	if err != nil {
		t.Fatalf("cannot read brandes_weighted_parallel.go: %v", err)
	}
	if bytes.Contains(src, []byte("bit-identical")) {
		t.Fatal("brandes_weighted_parallel.go godoc still claims 'bit-identical' — " +
			"remove or replace with tolerance-based language")
	}
}

func buildWeightedBrandesBenchFixture(tb testing.TB) *csr.CSR[float64] {
	tb.Helper()
	const n = 512
	return buildWeightedParallelFixture(tb, n, 3*n, 211, 223)
}

// BenchmarkWeightedBetweenness_Serial / _Parallel form a -cpu scaling
// pair: run with -cpu=1,8 and compare with benchstat to read the
// parallel speedup across core counts. The parallel variant calls
// WeightedBetweennessParallel(c, 0) so numWorkers tracks GOMAXPROCS,
// which -cpu sets.
func BenchmarkWeightedBetweenness_Serial(b *testing.B) {
	c := buildWeightedBrandesBenchFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := WeightedBetweenness(c); err != nil {
			b.Fatalf("WeightedBetweenness: %v", err)
		}
	}
}

func BenchmarkWeightedBetweenness_Parallel(b *testing.B) {
	c := buildWeightedBrandesBenchFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := WeightedBetweennessParallel(c, 0); err != nil {
			b.Fatalf("WeightedBetweennessParallel: %v", err)
		}
	}
}
