package cypher_test

// result_cap_write_only_test.go — regression gate for task #1425:
// MaxResultRows must bound write-only queries (no RETURN clause), not only
// queries that produce visible rows.
//
// Before the fix, the write-only drain loop in newResultWithLimit iterated
// rs.Next() with no row counter, so `UNWIND range(1,50) CREATE (:Z{v:i})`
// with cap=10 executed all 50 creates and committed them — the cap was silently
// ignored on the write side.
//
// The fix adds a row counter to the write-only drain; when the counter exceeds
// maxRows the loop sets r.rowsErr = ErrResultRowsExceeded and breaks.
// commitUnderBarrier already treats a non-nil rowsErr as a failure and calls
// rollbackUnderBarrier, so the atomic rollback is obtained for free.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestWriteOnlyCap_TripsErrResultRowsExceeded verifies that a write-only query
// (no RETURN clause) whose write-iteration count exceeds MaxResultRows returns
// ErrResultRowsExceeded and rolls back all mutations atomically.
func TestWriteOnlyCap_TripsErrResultRowsExceeded(t *testing.T) {
	const (
		cap     = 10
		creates = 50
	)
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: cap})

	res, err := eng.RunInTx(context.Background(),
		"UNWIND range(1, 50) AS i CREATE (:Z {v: i})", nil)
	if err != nil {
		t.Fatalf("RunInTx: unexpected engine error: %v", err)
	}
	defer func() { _ = res.Close() }()

	// Write-only results expose no rows to Next(); drain to be safe.
	for res.Next() {
	}

	if got := res.Err(); !errors.Is(got, cypher.ErrResultRowsExceeded) {
		t.Fatalf("Result.Err() = %v, want ErrResultRowsExceeded", got)
	}
}

// TestWriteOnlyCap_RollsBackAtomically verifies that when the cap trips on a
// write-only query the graph holds ZERO nodes — the partial mutations applied
// before the cap tripped are fully rolled back inside the visibility barrier.
func TestWriteOnlyCap_RollsBackAtomically(t *testing.T) {
	const cap = 10

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: cap})

	res, err := eng.RunInTx(context.Background(),
		"UNWIND range(1, 50) AS i CREATE (:Z {v: i})", nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	for res.Next() {
	}
	_ = res.Close()

	// All mutations must have been rolled back; the live graph must be empty.
	if n := liveNodeCount(g); n != 0 {
		t.Fatalf("live graph holds %d nodes after write-only cap trip, want 0 (partial commit)", n)
	}
}

// TestWriteOnlyCap_BelowCapCommits confirms that a write-only query whose
// create count is at or below the cap commits normally (the cap does not
// over-eagerly reject legitimate writes).
func TestWriteOnlyCap_BelowCapCommits(t *testing.T) {
	const cap = 50

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: cap})

	res, err := eng.RunInTx(context.Background(),
		"UNWIND range(1, 10) AS i CREATE (:Z {v: i})", nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	for res.Next() {
	}
	if rerr := res.Err(); rerr != nil {
		t.Fatalf("Result.Err() = %v, want nil for sub-cap write", rerr)
	}
	_ = res.Close()

	if n := liveNodeCount(g); n != 10 {
		t.Fatalf("live graph holds %d nodes, want 10 after successful sub-cap write", n)
	}
}
