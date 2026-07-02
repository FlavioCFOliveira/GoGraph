package cypher_test

// breaker_byte_budget_test.go — engine-level WIRING guard for the 2026-07-02
// audit finding (#1841). The exec-level per-breaker byte caps are proven in
// cypher/exec/byte_budget_test.go; this test proves the Engine actually THREADS
// EngineOptions.MaxResultBytes into a pipeline-breaking operator via
// resultByteBudget, so a blocking breaker (here Sort, from ORDER BY) stops with
// its memory-cap sentinel while buffering — before the drain ever sees a row —
// instead of materialising the whole wide result. A control with the budget
// disabled returns every row, proving the trip is attributable to the budget.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// TestEngine_SortByteBudget_TripsBeforeDrain runs an ORDER BY (a blocking Sort)
// over a wide graph under a byte budget far below the buffered size but far above
// one row, with the row cap left at its default. The Sort byte budget therefore
// fires during collectAndSort with ErrSortMemoryExceeded, proving MaxResultBytes
// reached the breaker.
func TestEngine_SortByteBudget_TripsBeforeDrain(t *testing.T) {
	const (
		nodes   = 20
		blobLen = 4096
		budget  = 16 * 1024 // far below 20 * 4 KiB, far above one row
	)
	g := newWideGraph(t, nodes, blobLen)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultBytes: budget})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n.blob AS blob ORDER BY blob", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	for res.Next() { //nolint:revive // draining to reach the terminal error
	}
	if got := res.Err(); !errors.Is(got, exec.ErrSortMemoryExceeded) {
		t.Fatalf("Result.Err() = %v, want exec.ErrSortMemoryExceeded — MaxResultBytes was not threaded into the Sort breaker", got)
	}
}

// TestEngine_SortByteBudget_UnlimitedCompletes is the control: the identical
// ORDER BY query with the byte budget disabled returns every row with no error,
// proving the trip above is attributable to the budget, not the row cap.
func TestEngine_SortByteBudget_UnlimitedCompletes(t *testing.T) {
	const (
		nodes   = 20
		blobLen = 4096
	)
	g := newWideGraph(t, nodes, blobLen)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		MaxResultBytes: cypher.MaxResultBytesUnlimited,
	})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n.blob AS blob ORDER BY blob", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil with the byte budget disabled", err)
	}
	if count != nodes {
		t.Fatalf("got %d rows, want %d", count, nodes)
	}
}
