package community

// leiden_pool_determinism_test.go — guards the cross-call buffer-set pool
// (#1725) against any leakage of stale state that would break Leiden's
// bit-for-bit reproducibility contract.
//
// The pool recycles aggregate()'s level buffer-sets across Leiden calls. If a
// recycled buffer-set were ever read before being zeroed or fully overwritten,
// a later call's result would depend on an earlier call's graph — a determinism
// break. These tests force heavy pool reuse and assert every result is
// bit-identical to a cold-pool baseline, both sequentially and concurrently.

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// buildPlantedCSR builds a deterministic planted-partition CSR for the pool
// determinism tests.
func buildPlantedCSR(t *testing.T, k, blockSize, pIn, pOut int, seed uint64) *csr.CSR[int64] {
	t.Helper()
	g, err := shapegen.PlantedPartition(k, blockSize, pIn, pOut, seed).
		Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("PlantedPartition Build: %v", err)
	}
	return csr.BuildFromAdjList(g.AdjList())
}

func partitionsEqual(a, b Partition) bool {
	if a.NumCommunities != b.NumCommunities || len(a.Community) != len(b.Community) {
		return false
	}
	for i := range a.Community {
		if a.Community[i] != b.Community[i] {
			return false
		}
	}
	return true
}

// TestLeiden_PoolReuse_Deterministic runs Leiden many times in sequence over
// graphs of DIFFERENT sizes so the cross-call pool hands later calls buffer-sets
// retired by earlier, differently-sized calls. Every result on a given graph
// must be bit-identical to that graph's baseline, proving the pool never leaks
// stale content between calls.
func TestLeiden_PoolReuse_Deterministic(t *testing.T) {
	// Distinct shapes so the pooled buffer-sets vary in size between calls.
	cSmall := buildPlantedCSR(t, 4, 40, 30, 1, 7)
	cBig := buildPlantedCSR(t, 12, 96, 25, 1, 11)

	baseSmall := Leiden(cSmall, DefaultLeidenOptions())
	baseBig := Leiden(cBig, DefaultLeidenOptions())

	// Interleave the two sizes so a small-graph call frequently reuses a
	// buffer-set retired by a big-graph call and vice versa.
	for i := 0; i < 50; i++ {
		if got := Leiden(cBig, DefaultLeidenOptions()); !partitionsEqual(got, baseBig) {
			t.Fatalf("iter %d: big-graph result diverged from baseline (pool leaked state)", i)
		}
		if got := Leiden(cSmall, DefaultLeidenOptions()); !partitionsEqual(got, baseSmall) {
			t.Fatalf("iter %d: small-graph result diverged from baseline (pool leaked state)", i)
		}
	}
}

// TestLeiden_PoolReuse_Concurrent runs many Leiden calls concurrently over the
// same graph. sync.Pool hands each goroutine a distinct free list, so each call
// must remain bit-identical to the single-threaded baseline. Run with -race to
// also catch any accidental sharing of pooled buffers across goroutines.
func TestLeiden_PoolReuse_Concurrent(t *testing.T) {
	c := buildPlantedCSR(t, 8, 80, 30, 1, 99)
	base := Leiden(c, DefaultLeidenOptions())

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]bool, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				if got := Leiden(c, DefaultLeidenOptions()); !partitionsEqual(got, base) {
					errs[idx] = true
					return
				}
			}
		}(g)
	}
	wg.Wait()
	for idx, bad := range errs {
		if bad {
			t.Fatalf("goroutine %d: concurrent Leiden result diverged from baseline", idx)
		}
	}
}
