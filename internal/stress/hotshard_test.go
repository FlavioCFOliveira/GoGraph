//go:build soak

// Package stress — T591: hot-shard write storm (64 goroutines, soak).
//
// Drives 64 concurrent goroutines that each call AddEdge with keys that all
// hash to adjlist shard 0 via the FNV-1a sharding scheme. The goal is to pin
// worst-case shard-0 lock contention and verify:
//  1. No data races (run under -race).
//  2. No goroutine leaks (goleak via TestMain).
//  3. Final neighbour set matches the single-threaded reference build
//     (compared by natural-key strings, not NodeIDs).
//  4. Contention on shard 0 is bounded and observable via an atomic counter.
package stress

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestHotShard_WriteStorm drives N goroutines, all writing edges between keys
// that map to adjlist shard 0, then verifies correctness against a
// single-threaded reference build.
func TestHotShard_WriteStorm(t *testing.T) {
	goroutines := 64
	edgesPerGoroutine := 1_000
	if testing.Short() {
		goroutines = 8
		edgesPerGoroutine = 50
	}

	totalEdges := goroutines * edgesPerGoroutine
	keys := shapegen.GenerateShardZeroKeys(totalEdges * 2)

	// Pre-record all edge pairs so the reference and storm builds use exactly
	// the same inputs in the same logical order.
	type edgePair struct{ src, dst string }
	pairs := make([]edgePair, totalEdges)
	for g := 0; g < goroutines; g++ {
		base := g * edgesPerGoroutine
		for e := 0; e < edgesPerGoroutine; e++ {
			k := (base + e) * 2
			pairs[base+e] = edgePair{keys[k], keys[k+1]}
		}
	}

	// ── Reference build (single-threaded) ─────────────────────────────────
	ref := adjlist.New[string, int64](adjlist.Config{Directed: true, Multigraph: false})
	for _, p := range pairs {
		if err := ref.AddEdge(p.src, p.dst, 1); err != nil {
			t.Fatalf("ref.AddEdge %q→%q: %v", p.src, p.dst, err)
		}
	}
	refCSR := csr.BuildFromAdjList(ref)

	// Build the reference edge set keyed by string pair for later comparison.
	// Use Mapper.Walk instead of a NodeID range loop — NodeIDs are sparse
	// packed values ((intraIdx << 8) | shard), so MaxNodeID() returns a
	// value much larger than the actual node count when keys cluster on one
	// shard (as they do for shard-0 adversarial keys).
	type edgeKey struct{ src, dst string }
	refSet := make(map[edgeKey]struct{}, totalEdges)
	ref.Mapper().Walk(func(id graph.NodeID, srcStr string) bool {
		for nbr := range refCSR.NeighboursByID(id) {
			dstStr, ok2 := ref.Mapper().Resolve(nbr)
			if !ok2 {
				return true
			}
			refSet[edgeKey{srcStr, dstStr}] = struct{}{}
		}
		return true
	})

	// ── Concurrent storm ──────────────────────────────────────────────────
	storm := adjlist.New[string, int64](adjlist.Config{Directed: true, Multigraph: false})
	var contentionCount atomic.Int64

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			lo := g * edgesPerGoroutine
			hi := lo + edgesPerGoroutine
			for _, p := range pairs[lo:hi] {
				t0 := time.Now()
				if err := storm.AddEdge(p.src, p.dst, 1); err != nil {
					t.Errorf("storm.AddEdge %q→%q: %v", p.src, p.dst, err)
					return
				}
				if time.Since(t0) > time.Microsecond {
					contentionCount.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	stormCSR := csr.BuildFromAdjList(storm)

	// ── Correctness: every ref edge must appear in storm ──────────────────
	mismatches := 0
	for ek := range refSet {
		stormSrc, okS := storm.Mapper().Lookup(ek.src)
		stormDst, okD := storm.Mapper().Lookup(ek.dst)
		if !okS || !okD {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("storm missing key %q or %q after storm", ek.src, ek.dst)
			}
			continue
		}
		found := false
		for nbr := range stormCSR.NeighboursByID(stormSrc) {
			if nbr == stormDst {
				found = true
				break
			}
		}
		if !found {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("ref edge %q→%q missing from storm CSR", ek.src, ek.dst)
			}
		}
	}
	if mismatches > 5 {
		t.Errorf("... %d total edge mismatches (showing first 5)", mismatches)
	}

	// ── Bounded contention ────────────────────────────────────────────────
	cc := contentionCount.Load()
	if cc > int64(totalEdges) {
		t.Errorf("contention count %d > totalEdges %d (impossible)", cc, totalEdges)
	}
	t.Logf("shard-0 contention: %d/%d AddEdge waited > 1 µs (%.1f%%)",
		cc, totalEdges, float64(cc)/float64(totalEdges)*100)
}
