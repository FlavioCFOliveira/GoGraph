package centrality

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// buildBudgetFixtures returns a directed power-law CSR and its symmetric
// projection, both above pageRankParallelThreshold live nodes.
func buildBudgetFixtures(t testing.TB, n, m int) (directed, symmetric *csr.CSR[int64]) {
	t.Helper()
	g, err := shapegen.BarabasiAlbert(n, m, 7).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	directed = csr.BuildFromAdjList(g.AdjList())
	symmetric = directed.BuildSymmetric()
	return directed, symmetric
}

func assertRanksBitIdentical(t *testing.T, label string, got []float64, gotIt int, want []float64, wantIt int) {
	t.Helper()
	if gotIt != wantIt {
		t.Fatalf("%s: iterations %d != reference %d", label, gotIt, wantIt)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: len %d != reference %d", label, len(got), len(want))
	}
	for i := range got {
		if math.Float64bits(got[i]) != math.Float64bits(want[i]) {
			t.Fatalf("%s: rank[%d] = %v, reference %v", label, i, got[i], want[i])
		}
	}
}

func TestNewPageRankBudget_RejectsNonPositive(t *testing.T) {
	for _, c := range []int{0, -1} {
		if b, err := NewPageRankBudget(c); !errors.Is(err, ErrInvalidInput) || b != nil {
			t.Fatalf("NewPageRankBudget(%d) = %v, %v; want nil, ErrInvalidInput", c, b, err)
		}
	}
	b, err := NewPageRankBudget(3)
	if err != nil || b.Capacity() != 3 || b.InUse() != 0 {
		t.Fatalf("NewPageRankBudget(3) = cap %d inUse %d err %v", b.Capacity(), b.InUse(), err)
	}
}

// TestPageRank_BudgetBitIdentical pins that a budgeted run returns the serial
// reference's ranks and iteration count bit for bit for every budget
// capacity 1..16 (1 selects the inline pull on the symmetric CSR and the
// serial push on the directed one), for the one-shot and the PageRanker
// paths, under the rmp #2605 contract the unbudgeted parallel path is held to.
func TestPageRank_BudgetBitIdentical(t *testing.T) {
	directed, symmetric := buildBudgetFixtures(t, 4000, 3)
	opts := DefaultPageRankOptions()
	prev := runtime.GOMAXPROCS(1)
	refs := map[string][]float64{}
	refIts := map[string]int{}
	fixtures := map[string]*csr.CSR[int64]{"directed": directed, "symmetric": symmetric}
	for name, c := range fixtures {
		r, it, err := PageRank(c, opts)
		if err != nil {
			runtime.GOMAXPROCS(prev)
			t.Fatalf("%s serial reference: %v", name, err)
		}
		refs[name], refIts[name] = r, it
	}
	runtime.GOMAXPROCS(16)
	defer runtime.GOMAXPROCS(prev)

	for name, c := range fixtures {
		for capacity := 1; capacity <= 16; capacity++ {
			b, err := NewPageRankBudget(capacity)
			if err != nil {
				t.Fatal(err)
			}
			o := opts
			o.Budget = b
			got, it, err := PageRank(c, o)
			if err != nil {
				t.Fatalf("%s cap=%d: %v", name, capacity, err)
			}
			assertRanksBitIdentical(t, fmt.Sprintf("%s cap=%d one-shot", name, capacity), got, it, refs[name], refIts[name])

			got, it, err = NewPageRanker(c).Run(context.Background(), o)
			if err != nil {
				t.Fatalf("%s cap=%d ranker: %v", name, capacity, err)
			}
			assertRanksBitIdentical(t, fmt.Sprintf("%s cap=%d ranker", name, capacity), got, it, refs[name], refIts[name])
			if b.InUse() != 0 {
				t.Fatalf("%s cap=%d: %d slots still held after return", name, capacity, b.InUse())
			}
		}
	}
}

// TestPageRankBudget_AcquireNeverExceedsCapacity hammers acquire/release from
// many goroutines and checks, inside every critical section, that the held
// total never exceeds capacity, and that every slot is returned.
func TestPageRankBudget_AcquireNeverExceedsCapacity(t *testing.T) {
	const capacity, goroutines, rounds = 5, 64, 200
	b, err := NewPageRankBudget(capacity)
	if err != nil {
		t.Fatal(err)
	}
	var held atomic.Int64 // independent tally of slots granted
	var granted atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				n := b.acquire(1 + (g+r)%4)
				if n > 0 {
					granted.Add(1)
					if h := held.Add(int64(n)); h > capacity {
						t.Errorf("held %d slots > capacity %d", h, capacity)
					}
					runtime.Gosched()
					held.Add(-int64(n))
				}
				b.release(n)
			}
		}(g)
	}
	wg.Wait()
	if b.InUse() != 0 {
		t.Fatalf("InUse = %d after all releases, want 0", b.InUse())
	}
	if granted.Load() == 0 {
		t.Fatal("no acquire was ever granted a slot")
	}
}

// TestPageRank_BudgetConcurrentNoLeak runs many concurrent budgeted calls
// (some cancelled before they start) over one budget and checks every
// result against the serial reference and that no slot leaks. goleak in
// TestMain checks that no worker goroutine outlives the calls.
func TestPageRank_BudgetConcurrentNoLeak(t *testing.T) {
	_, symmetric := buildBudgetFixtures(t, 3000, 2)
	opts := DefaultPageRankOptions()
	prev := runtime.GOMAXPROCS(1)
	ref, refIt, err := PageRank(symmetric, opts)
	runtime.GOMAXPROCS(prev)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPageRankBudget(3)
	if err != nil {
		t.Fatal(err)
	}
	opts.Budget = b
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	const callers = 16
	var wg sync.WaitGroup
	results := make([][]float64, callers)
	its := make([]int, callers)
	errs := make([]error, callers)
	for g := 0; g < callers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := context.Background()
			if g%4 == 3 {
				ctx = cancelled
			}
			results[g], its[g], errs[g] = PageRankCtx(ctx, symmetric, opts)
		}(g)
	}
	wg.Wait()
	for g := 0; g < callers; g++ {
		if g%4 == 3 {
			if !errors.Is(errs[g], context.Canceled) {
				t.Fatalf("caller %d: err %v, want context.Canceled", g, errs[g])
			}
			continue
		}
		if errs[g] != nil {
			t.Fatalf("caller %d: %v", g, errs[g])
		}
		assertRanksBitIdentical(t, fmt.Sprintf("caller %d", g), results[g], its[g], ref, refIt)
	}
	if b.InUse() != 0 {
		t.Fatalf("InUse = %d after all calls, want 0", b.InUse())
	}
}

// BenchmarkPageRank_ConcurrentCallers measures throughput of N concurrent
// one-shot PageRank callers on the row-20 shape (60 000-node BA, attach 4,
// symmetric projection), unbudgeted (each call starts GOMAXPROCS workers)
// against one shared PageRankBudget of GOMAXPROCS slots (rmp #3086).
// ns/op is wall time per call, the inverse of aggregate throughput.
func BenchmarkPageRank_ConcurrentCallers(b *testing.B) {
	_, c := buildBudgetFixtures(b, 60000, 4)
	p := runtime.GOMAXPROCS(0)
	for _, callers := range []int{1, 8, 64, 256, 1024} {
		for _, policy := range []string{"unbudgeted", "budget"} {
			b.Run(fmt.Sprintf("callers=%d/policy=%s", callers, policy), func(b *testing.B) {
				opts := DefaultPageRankOptions()
				if policy == "budget" {
					budget, err := NewPageRankBudget(p)
					if err != nil {
						b.Fatal(err)
					}
					opts.Budget = budget
				}
				var next atomic.Int64
				b.ReportAllocs()
				b.ResetTimer()
				var wg sync.WaitGroup
				for g := 0; g < callers; g++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for next.Add(1) <= int64(b.N) {
							if _, _, err := PageRank(c, opts); err != nil {
								b.Error(err)
								return
							}
						}
					}()
				}
				wg.Wait()
				b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "calls/s")
			})
		}
	}
}
