package cypher_test

// global_memory_budget_test.go — REGRESSION GUARD for the 2026-07-02 audit
// finding (#1842): the result-memory budget was strictly PER query, so N
// concurrent connections could each materialise a per-query-capped result and
// their sum exhaust the host. The engine now carries an engine-wide ceiling
// (EngineOptions.GlobalMaxResultBytes) that every result charges its estimated
// materialised size against and releases on Close, rejecting a materialisation
// that would push the aggregate over the ceiling with ErrGlobalMemoryExceeded.
//
// The test models concurrent connections by holding multiple results open at
// once (the shared atomic counter — not goroutine timing — is what the ceiling
// bounds, so overlapping open results is a faithful and deterministic model):
// two ~20 KiB results fit under a 32 KiB ceiling one at a time but not together,
// and closing the first frees its charge so a third result succeeds.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

func TestEngine_GlobalMemoryBudget_AggregateCeilingAndRelease(t *testing.T) {
	const (
		nodes   = 5
		blobLen = 4096      // per-row estimate ≈ 16 + 4096 ≈ 4.1 KiB
		ceiling = 32 * 1024 // fits one ~20 KiB result, not two
		query   = "MATCH (n) RETURN n.blob AS blob"
	)
	g := newWideGraph(t, nodes, blobLen)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{GlobalMaxResultBytes: ceiling})
	ctx := context.Background()

	// Result A materialises (~20 KiB) and stays OPEN, holding its charge.
	resA, err := eng.Run(ctx, query, nil)
	if err != nil {
		t.Fatalf("Run A: %v", err)
	}
	if err := resA.Err(); err != nil {
		t.Fatalf("A must fit under the ceiling alone, got %v", err)
	}

	// Result B materialises while A is still charged; the aggregate (~40 KiB)
	// exceeds the 32 KiB ceiling, so B is rejected with ErrGlobalMemoryExceeded
	// and serves no rows.
	resB, err := eng.Run(ctx, query, nil)
	if err != nil {
		t.Fatalf("Run B: %v", err)
	}
	var bRows int
	for resB.Next() {
		bRows++
	}
	if got := resB.Err(); !errors.Is(got, cypher.ErrGlobalMemoryExceeded) {
		t.Fatalf("B.Err() = %v, want ErrGlobalMemoryExceeded (aggregate over the ceiling)", got)
	}
	if bRows != 0 {
		t.Fatalf("B served %d rows after a tripped global ceiling, want 0", bRows)
	}

	// Closing A (and B) frees their charge; a fresh result then fits again.
	if err := resA.Close(); err != nil {
		t.Fatalf("Close A: %v", err)
	}
	if err := resB.Close(); err != nil {
		t.Fatalf("Close B: %v", err)
	}

	resC, err := eng.Run(ctx, query, nil)
	if err != nil {
		t.Fatalf("Run C: %v", err)
	}
	defer resC.Close()
	var cRows int
	for resC.Next() {
		cRows++
	}
	if err := resC.Err(); err != nil {
		t.Fatalf("C.Err() = %v, want nil after A/B freed their charge", err)
	}
	if cRows != nodes {
		t.Fatalf("C returned %d rows, want %d — the released charge must let a new query proceed", cRows, nodes)
	}
}

// TestEngine_GlobalMemoryBudget_UnlimitedByDefault confirms that, absent a
// GOMEMLIMIT and without an explicit ceiling, the engine imposes no global bound:
// the same overlapping results that trip a small ceiling above all succeed. This
// pins the safe default — the module never rejects a legitimate workload on a
// host whose memory it cannot know.
func TestEngine_GlobalMemoryBudget_UnlimitedByDefault(t *testing.T) {
	const (
		nodes   = 5
		blobLen = 4096
		query   = "MATCH (n) RETURN n.blob AS blob"
	)
	g := newWideGraph(t, nodes, blobLen)
	// Default options: GlobalMaxResultBytes zero → GOMEMLIMIT-derived, and no
	// GOMEMLIMIT is set in the test process, so the ceiling is unlimited.
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	results := make([]*cypher.Result, 0, 8)
	defer func() {
		for _, r := range results {
			_ = r.Close()
		}
	}()
	for i := 0; i < 8; i++ {
		res, err := eng.Run(ctx, query, nil)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if err := res.Err(); err != nil {
			t.Fatalf("result %d errored under the default (unlimited) global ceiling: %v", i, err)
		}
		results = append(results, res) // keep open to accumulate charge
	}
}
