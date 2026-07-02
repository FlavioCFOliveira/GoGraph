package cypher_test

// aggregate_distinct_cap_test.go — engine-level WIRING guard for rmp #1867
// (2026-07-02 production-readiness audit round 2, finding "distinctAggregator
// has no memory cap"). The unit-level cap logic is proven in
// cypher/distinct_aggregator_internal_test.go; this test proves the ENGINE
// actually threads EngineOptions.MaxResultBytes into distinctAggregator via
// resultByteBudget — the same wiring pattern breaker_byte_budget_test.go
// proves for Sort — so count(DISTINCT ...) over enough large, genuinely
// DISTINCT values stops with the typed cap error while accumulating, instead
// of growing its seen-values set without bound. A control with the budget
// disabled completes normally, proving the trip is attributable to the
// budget rather than an unrelated failure.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newDistinctBlobGraph creates n nodes each carrying a genuinely UNIQUE
// large string property (unlike newWideGraph's identical blobs, which would
// collapse to a single DISTINCT value and never exercise this cap).
func newDistinctBlobGraph(tb testing.TB, n, blobLen int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		key := "n" + string(rune('A'+i))
		blob := string(rune('a'+i)) + strings.Repeat("x", blobLen)
		if err := g.SetNodeProperty(key, "blob", lpg.StringValue(blob)); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
		if err := g.SetNodeLabel(key, "N"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
	}
	return g
}

// TestEngine_AggregateDistinctByteBudget_TripsBeforeUnbounded runs
// count(DISTINCT n.blob) over enough large, distinct blobs to exceed a
// test-lowered MaxResultBytes long before the (huge, untouched)
// DefaultMaxAggregateDistinctValues count cap could ever fire — isolating
// the byte dimension.
func TestEngine_AggregateDistinctByteBudget_TripsBeforeUnbounded(t *testing.T) {
	const (
		nodes   = 20
		blobLen = 4096
		budget  = 16 * 1024 // far below 20 * 4 KiB, far above one blob
	)
	g := newDistinctBlobGraph(t, nodes, blobLen)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultBytes: budget})

	res, err := eng.Run(context.Background(), "MATCH (n:N) RETURN count(DISTINCT n.blob) AS c", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	for res.Next() { //nolint:revive // draining to reach the terminal error
	}
	if got := res.Err(); !errors.Is(got, cypher.ErrAggregateDistinctMemoryExceeded) {
		t.Fatalf("Result.Err() = %v, want cypher.ErrAggregateDistinctMemoryExceeded — MaxResultBytes was not threaded into distinctAggregator", got)
	}
}

// TestEngine_AggregateDistinctByteBudget_UnlimitedCompletes is the control:
// the identical query with the byte budget disabled returns the correct
// distinct count with no error, proving the trip above is attributable to
// the budget, not some unrelated failure.
func TestEngine_AggregateDistinctByteBudget_UnlimitedCompletes(t *testing.T) {
	const (
		nodes   = 20
		blobLen = 4096
	)
	g := newDistinctBlobGraph(t, nodes, blobLen)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		MaxResultBytes: cypher.MaxResultBytesUnlimited,
	})

	res, err := eng.Run(context.Background(), "MATCH (n:N) RETURN count(DISTINCT n.blob) AS c", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	mustInt(t, "count(DISTINCT n.blob)", rows[0]["c"], nodes)
}
