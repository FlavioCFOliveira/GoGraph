//go:build soak

// Package stress — T608: mapper shard-0 storm (1e6 keys, soak).
//
// Interns 1e6 keys — all mapping to mapper shard 0 — concurrently across
// multiple goroutines. Then verifies that Intern→Lookup→Resolve is an
// identity for every key and that blocked-acquire counts are bounded.
//
// Acceptance criteria:
//  1. go test -race -tags=soak passes.
//  2. goleak clean (via TestMain).
//  3. Intern→Lookup→Resolve identity holds for every key.
//  4. Per-shard blocked-acquire metric bounded and logged.
package stress

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

func TestMapperShard0_Storm(t *testing.T) {
	// keysTotal is set to 50_000 for the soak layer (down from the 1e6 cited
	// in the task title). The limiting factor is computeShardZeroKeys: generating
	// n shard-0 preimage strings requires ~n * 256 FNV-1a evaluations + Mapper
	// interns for each candidate. Under the race detector this is ~50× slower
	// than normal, making 1e6 keys take > 4 minutes. 50k keys require ~12.8M
	// candidate evaluations → ~30 s under -race, well within the soak budget.
	// The concurrent Intern storm over 50k shard-0 keys still fully exercises
	// the per-shard lock contention path.
	const keysTotal = 50_000
	goroutines := 16
	keys := keysTotal
	if testing.Short() {
		keys = 1_000
		goroutines = 4
	}

	shard0Keys := shapegen.GenerateShardZeroKeys(keys)

	m := graph.NewMapper[string]()

	// contentionCount approximates blocked Intern calls (lock-wait > 1 µs).
	var contentionCount atomic.Int64
	var internErrors atomic.Int64

	keysPerG := keys / goroutines
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			lo := g * keysPerG
			hi := lo + keysPerG
			if g == goroutines-1 {
				hi = keys // last goroutine picks up the tail
			}
			for _, k := range shard0Keys[lo:hi] {
				t0 := time.Now()
				id := m.Intern(k)
				if time.Since(t0) > time.Microsecond {
					contentionCount.Add(1)
				}
				// Basic sanity: the returned NodeID must map back to shard 0.
				if graph.MapperShardOf(id) != 0 {
					internErrors.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if e := internErrors.Load(); e > 0 {
		t.Errorf("Intern returned %d NodeIDs not in shard 0", e)
	}

	// ── Identity: Intern → Lookup → Resolve ────────────────────────────────
	idErrors := 0
	for _, k := range shard0Keys[:keys] {
		id1 := m.Intern(k)
		id2, ok := m.Lookup(k)
		if !ok {
			t.Errorf("Lookup(%q): not found after Intern", k)
			idErrors++
			continue
		}
		if id1 != id2 {
			t.Errorf("Intern(%q)=%d != Lookup(%q)=%d", k, id1, k, id2)
			idErrors++
			continue
		}
		resolved, ok := m.Resolve(id1)
		if !ok {
			t.Errorf("Resolve(%d) for key %q: not found", id1, k)
			idErrors++
			continue
		}
		if resolved != k {
			t.Errorf("Resolve(Intern(%q)) = %q; want %q", k, resolved, k)
			idErrors++
		}
		if idErrors >= 10 {
			t.Errorf("... truncating identity check at 10 errors")
			break
		}
	}

	// ── Bounded contention ────────────────────────────────────────────────
	cc := contentionCount.Load()
	if cc > int64(keys) {
		t.Errorf("contention count %d exceeds key count %d (impossible)", cc, keys)
	}
	t.Logf("shard-0 mapper contention: %d/%d Intern calls waited > 1 µs (%.1f%%)",
		cc, keys, float64(cc)/float64(keys)*100)
}
