package cypher_test

// result_bytes_cap_test.go — black-box behavioural tests for the aggregate-byte
// budget on result materialisation (#1328). These exercise the public Engine
// API exactly as a consumer would:
//   - a result whose rows carry large string properties trips
//     ErrResultBytesExceeded via Result.Err() while the row COUNT stays under
//     the (default, high) row cap — the residual wide-row case the row cap alone
//     cannot catch;
//   - the unlimited opt-out (MaxResultBytesUnlimited) returns every row with no
//     error;
//   - a normal small result is unaffected.
//
// The default budget (DefaultMaxResultBytes = 1 GiB) is exercised cheaply by
// lowering it through EngineOptions rather than building a multi-gigabyte result;
// the constructor wiring that proves the *default* itself is finite lives in the
// white-box result_bytes_cap_internal_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newWideGraph builds n nodes, each carrying a single string property "blob" of
// blobLen bytes, so a `MATCH (n) RETURN n.blob` result has a large aggregate
// encoded size relative to its row count.
func newWideGraph(tb testing.TB, n, blobLen int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	blob := strings.Repeat("x", blobLen)
	for i := 0; i < n; i++ {
		key := string(rune('A'+i%26)) + string(rune('0'+i%10)) + string(rune('a'+i/26))
		if err := g.SetNodeProperty(key, "blob", lpg.StringValue(blob)); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

// TestEngine_ResultByteBudget_TripsBoundedError is the core AC: a result whose
// cumulative estimated size exceeds a test-lowered MaxResultBytes, while the row
// COUNT stays well UNDER the row cap, stops with ErrResultBytesExceeded reported
// by Result.Err(). Each of the 20 rows carries a 4 KiB blob (~80 KiB total) and
// the byte budget is 16 KiB, but the row cap stays at its default 10M — so only
// the byte budget can trip, isolating the new guard from the row cap.
//
// Matching the row cap's established contract (#1292), a tripped materialisation
// guard makes Result.Next return false immediately: the partial rows are not
// served and Result.Err surfaces the sentinel. The test therefore asserts the
// error and the empty drain, not a partial row count. That the *same data* with
// the budget disabled returns every row (TestEngine_ResultByteBudget_RowCapUntouched)
// is what proves the trip is attributable to MaxResultBytes.
func TestEngine_ResultByteBudget_TripsBoundedError(t *testing.T) {
	const (
		nodes   = 20
		blobLen = 4096
		budget  = 16 * 1024 // far below 20 * 4 KiB, far above one row
	)
	g := newWideGraph(t, nodes, blobLen)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultBytes: budget})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n.blob AS blob", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if got := res.Err(); !errors.Is(got, cypher.ErrResultBytesExceeded) {
		t.Fatalf("Result.Err() = %v, want ErrResultBytesExceeded", got)
	}
	// A tripped materialisation guard serves no rows (consistent with the row cap):
	// the consumer must not have drained any.
	if count != 0 {
		t.Fatalf("drained %d rows after a tripped byte budget, want 0", count)
	}
}

// TestEngine_ResultByteBudget_RowCapUntouched proves the byte budget is the only
// guard firing in the trip test: under the same wide graph with the byte budget
// disabled but a high row cap, every row is returned with no error. This pins
// that the wide-row trip is attributable to MaxResultBytes, not an incidental
// row-cap hit.
func TestEngine_ResultByteBudget_RowCapUntouched(t *testing.T) {
	const (
		nodes   = 20
		blobLen = 4096
	)
	g := newWideGraph(t, nodes, blobLen)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		MaxResultBytes: cypher.MaxResultBytesUnlimited,
	})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n.blob AS blob", nil)
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
		t.Fatalf("got %d rows, want %d (no cap should fire)", count, nodes)
	}
}

// TestEngine_ResultByteBudget_DefaultAllowsSmallResult confirms the finite
// default (DefaultMaxResultBytes) does not interfere with ordinary, small
// results: a MATCH over a handful of nodes carrying modest properties returns
// every row with no error. This is the guard that the default is high enough for
// the TCK and the examples.
func TestEngine_ResultByteBudget_DefaultAllowsSmallResult(t *testing.T) {
	const (
		nodes   = 7
		blobLen = 64
	)
	g := newWideGraph(t, nodes, blobLen)
	eng := cypher.NewEngine(g) // default byte budget (1 GiB)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n.blob AS blob", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil under the default budget", err)
	}
	if count != nodes {
		t.Fatalf("got %d rows, want %d", count, nodes)
	}
}

// TestEngine_ResultByteBudget_UnlimitedOptOut proves the explicit opt-out: a
// wide result that would trip a low byte budget returns every row when the
// budget is set to the unlimited sentinel. Without the opt-out the same data
// under a low budget errors (TestEngine_ResultByteBudget_TripsBoundedError);
// here the sentinel removes the budget.
func TestEngine_ResultByteBudget_UnlimitedOptOut(t *testing.T) {
	const (
		nodes   = 30
		blobLen = 4096
	)
	g := newWideGraph(t, nodes, blobLen)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		MaxResultBytes: cypher.MaxResultBytesUnlimited,
	})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n.blob AS blob", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil for the unlimited opt-out", err)
	}
	if count != nodes {
		t.Fatalf("got %d rows, want %d (opt-out must return every row)", count, nodes)
	}
}
