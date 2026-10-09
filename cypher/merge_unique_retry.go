package cypher

// merge_unique_retry.go — concurrent autocommit MERGE under a UNIQUE constraint
// converges on the winner's node with every caller succeeding (rmp #2987).
//
// # The defect
//
// A UNIQUE value is reserved EAGERLY, in the shared value-set, the moment a
// statement writes it ([exec.ConstraintRegistry.ReserveSetProperty]). Two
// concurrent `MERGE (n:K {k:0})` therefore both search their snapshots, both find
// nothing, and both reach the create branch; the first to reserve k=0 wins and
// every other is refused with a constraint violation. The refusal is correct as
// far as the registry can tell — it has no owner for the value to wait on — but
// it is the wrong answer for MERGE, whose contract (F10, cypher/merge_race_test.go)
// is that the losers MATCH the winner. Measured at HEAD before this file: 65 to
// 112 of 128 callers failed in TestMerge_ConcurrentUniqueKey_EveryCallerSucceeds.
//
// # The fix: re-run the losing AUTOCOMMIT statement on a later snapshot
//
// The MERGE operator marks a violation of its own pattern key whose holder is
// INVISIBLE to the statement's snapshot (exec/merge_unique_race.go); a holder the
// snapshot can see makes a genuine, deterministic violation, which is never
// retried. A marked autocommit statement has been rolled back in full — every
// write, every ON CREATE effect, every reservation, and its transaction ended as
// an abort (rmp #2976) — and none of its rows has reached the caller, because the
// result is materialised inside the bracket. Running it again is therefore
// indistinguishable, to the caller and to every other transaction, from having
// run it once at the later instant. Before the re-run, the loser waits for every
// commit already allocated to become visible ([lpg.Graph.AwaitAllocatedCommits]),
// so a winner that has reached its commit is in the next snapshot, and the re-run
// matches it.
//
// This mirrors what PostgreSQL does for INSERT ... ON CONFLICT when the
// conflicting index entry belongs to a STILL-RUNNING transaction: the inserter
// waits for that transaction (SpeculativeInsertionWait) and starts over against
// its outcome (postgres/postgres, src/backend/access/nbtree/nbtinsert.c,
// _bt_check_unique; the same source exec.ConstraintRegistry.ReserveSetProperty
// cites). Memgraph does NOT converge: it validates UNIQUE at commit against the
// last committed version and returns a constraint violation to the loser
// (memgraph/memgraph @ f58e8b4c, src/storage/v2/inmemory/unique_constraints.cpp,
// Validate and LastCommittedVersionHasLabelProperty), which is exactly what #2987
// reports as a defect against GoGraph's own contract.
//
// # What is NOT retried, and why
//
//   - A statement of an EXPLICIT transaction. Its snapshot is the transaction's,
//     so re-running the statement inside it cannot see the winner, and re-running
//     the transaction is the client's decision: earlier statements' results were
//     already returned to it. The violation surfaces exactly as before.
//   - A violation whose holder the snapshot CAN see, and every violation not
//     raised by a MERGE's own pattern key (a CREATE, a SET, an ON CREATE SET):
//     re-running cannot change them, or they are not MERGE's to converge.
//
// # Bound
//
// The winner never waits on a loser — a rolled-back loser holds no reservation —
// so the race always resolves once the holder finishes. A holder can still stay
// in flight for long (an explicit transaction across client think-time), so the
// retry is bounded by [mergeRaceMaxAttempts] attempts with a capped exponential
// pause, and by the caller's context throughout. When the bound is reached the
// last attempt's violation is returned, which is the pre-#2987 behaviour.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

const (
	// mergeRaceMaxAttempts bounds how many times one autocommit MERGE statement
	// runs while it keeps losing a UNIQUE creation race. With the pause below it
	// caps the wait for one in-flight holder at about 0.3 s.
	mergeRaceMaxAttempts = 64
	// mergeRaceBackoffMin and mergeRaceBackoffMax bound the pause before every
	// re-run after the first: the first re-run follows the commit-frontier wait
	// alone, which is all a winner that has already reached its commit needs.
	mergeRaceBackoffMin = 50 * time.Microsecond
	mergeRaceBackoffMax = 5 * time.Millisecond
)

// runInTxMergeRaceRetry runs an autocommit statement and, while it fails as the
// loser of a MERGE UNIQUE creation race, re-runs it on a snapshot that includes
// the winner. See the file comment for why the re-run is sound and bounded.
func (e *Engine) runInTxMergeRaceRetry(ctx context.Context, sess *lpg.Session[string, float64], query string, params map[string]expr.Value, profile bool) (*Result, error) {
	for attempt := 1; ; attempt++ {
		res, err := e.runInTxAttempt(ctx, sess, query, params, profile)
		if err != nil || res == nil || attempt >= mergeRaceMaxAttempts || !isMergeUniqueRace(res.Err()) {
			return res, err
		}
		// Rolled back inside the bracket already; Close only releases the
		// materialised result, whose sole content is the violation being retried.
		_ = res.Close() // the error it reports is the race being retried
		cmetrics.IncCounter("cypher.RunInTx.mergeUniqueRaceRetries", 1)
		if h := e.mergeRaceRetryHookForTest; h != nil {
			h(attempt)
		}
		if werr := e.awaitMergeRaceHolder(ctx, attempt); werr != nil {
			return nil, fmt.Errorf("cypher: MERGE awaiting a concurrent UNIQUE holder: %w", werr)
		}
	}
}

// awaitMergeRaceHolder is the pause before re-run number attempt+1: a capped
// exponential sleep from the second re-run on, then a wait for every commit
// allocated by then to become visible, so the next snapshot includes a holder
// that has reached its commit. It returns ctx's error if ctx finishes first.
func (e *Engine) awaitMergeRaceHolder(ctx context.Context, attempt int) error {
	if attempt > 1 {
		d := mergeRaceBackoffMax
		if shift := attempt - 2; shift < 7 {
			d = min(mergeRaceBackoffMin<<shift, mergeRaceBackoffMax)
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return e.g.AwaitAllocatedCommits(ctx)
}

// isMergeUniqueRace reports whether err carries the marker the MERGE operator
// puts on a UNIQUE violation of its own pattern key whose holder the statement's
// snapshot could not see (exec/merge_unique_race.go).
func isMergeUniqueRace(err error) bool {
	var m interface{ MergeUniqueRace() bool }
	return errors.As(err, &m) && m.MergeUniqueRace()
}
