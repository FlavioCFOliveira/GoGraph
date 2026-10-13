package cypher

// merge_unique_retry_internal_test.go — the boundaries of the rmp #2987 retry,
// asserted through the race marker and the retry seam rather than by timing.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mergeRetryEngine returns an in-memory engine with a UNIQUE constraint on
// (K).k and a retry seam counting every re-run.
func mergeRetryEngine(t *testing.T, retries *atomic.Int64) *Engine {
	t.Helper()
	e := NewEngine(lpg.New[string, float64](adjlist.Config{}))
	t.Cleanup(func() { _ = e.Close() })
	e.mergeRaceRetryHookForTest = func(int) { retries.Add(1) }
	if err := mergeRetryRun(context.Background(), e, "CREATE CONSTRAINT k_u FOR (n:K) REQUIRE n.k IS UNIQUE"); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	return e
}

func mergeRetryRun(ctx context.Context, e *Engine, q string) error {
	res, err := e.RunInTx(ctx, q, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

func mergeRetryCount(t *testing.T, e *Engine) int64 {
	t.Helper()
	res, err := e.Run(context.Background(), "MATCH (n:K {k: 0}) RETURN count(n) AS c", nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("count returned no row: %v", res.Err())
	}
	c, err := strconv.ParseInt(fmt.Sprint(res.Record()["c"]), 10, 64)
	if err != nil {
		t.Fatalf("count = %#v: %v", res.Record()["c"], err)
	}
	return c
}

// TestMergeUniqueRace_VisibleHolderIsNotRetried: a holder the statement can see
// makes a deterministic violation. It must be unmarked and never re-run — a
// marker here would buy up to mergeRaceMaxAttempts executions of a statement
// that cannot succeed.
func TestMergeUniqueRace_VisibleHolderIsNotRetried(t *testing.T) {
	t.Parallel()
	var retries atomic.Int64
	e := mergeRetryEngine(t, &retries)
	ctx := context.Background()
	if err := mergeRetryRun(ctx, e, "CREATE (:K {k: 0, x: 1})"); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	for _, q := range []string{
		"MERGE (n:K {k: 0, x: 2})",            // node MERGE: pattern differs in x
		"MERGE (n:K {k: 0, x: 2})-[:R]->(:B)", // path MERGE: same node position
	} {
		err := mergeRetryRun(ctx, e, q)
		if !errors.Is(err, exec.ErrConstraintViolation) {
			t.Fatalf("%s: err = %v, want a constraint violation", q, err)
		}
		if isMergeUniqueRace(err) {
			t.Errorf("%s: a violation against a VISIBLE holder was marked as a race: %v", q, err)
		}
	}
	if n := retries.Load(); n != 0 {
		t.Errorf("deterministic violations were re-run %d times, want 0", n)
	}
}

// TestMergeUniqueRace_AutocommitConvergesOnInFlightHolder CONSTRUCTS the race the
// retry exists for: an explicit transaction holds k=0 uncommitted, an autocommit
// MERGE of k=0 loses to it, and the holder commits between the loser's first and
// second attempt (inside the seam, so the interleaving is not left to the
// scheduler). The loser must then match the holder's node.
func TestMergeUniqueRace_AutocommitConvergesOnInFlightHolder(t *testing.T) {
	t.Parallel()
	var retries atomic.Int64
	e := mergeRetryEngine(t, &retries)
	ctx := context.Background()

	holder, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := drainExecForRetry(holder.Exec("MERGE (n:K {k: 0}) ON CREATE SET n.who = 'holder'", nil)); err != nil {
		t.Fatalf("holder MERGE: %v", err)
	}
	var commitErr error
	committed := false
	e.mergeRaceRetryHookForTest = func(attempt int) {
		retries.Add(1)
		if attempt == 1 && !committed {
			committed = true
			commitErr = holder.Commit()
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- mergeRetryRun(ctx, e, "MERGE (n:K {k: 0}) ON CREATE SET n.who = 'loser'")
	}()
	select {
	case err = <-done:
	case <-time.After(30 * time.Second): // liveness guard only; never the verdict
		t.Fatal("autocommit MERGE did not return")
	}
	if commitErr != nil {
		t.Fatalf("holder commit: %v", commitErr)
	}
	if !committed {
		t.Fatal("the autocommit MERGE never lost the race; the precondition was not constructed")
	}
	if err != nil {
		t.Fatalf("autocommit MERGE against an in-flight holder that then committed: %v", err)
	}
	if c := mergeRetryCount(t, e); c != 1 {
		t.Fatalf("count(K {k:0}) = %d, want 1", c)
	}
	res, err := e.Run(ctx, "MATCH (n:K {k: 0}) RETURN n.who AS who", nil)
	if err != nil {
		t.Fatalf("MATCH: %v", err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() || strings.Trim(fmt.Sprint(res.Record()["who"]), `"`) != "holder" {
		t.Fatalf("the surviving node's ON CREATE marker = %v, want the holder's alone", res.Record())
	}
}

// TestMergeUniqueRace_AutocommitCreatesAfterHolderAborts is the other outcome of
// the holder: it rolls back between the loser's attempts, so the value is free
// and the loser's re-run CREATES.
func TestMergeUniqueRace_AutocommitCreatesAfterHolderAborts(t *testing.T) {
	t.Parallel()
	var retries atomic.Int64
	e := mergeRetryEngine(t, &retries)
	ctx := context.Background()

	holder, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := drainExecForRetry(holder.Exec("MERGE (n:K {k: 0})", nil)); err != nil {
		t.Fatalf("holder MERGE: %v", err)
	}
	var rbErr error
	aborted := false
	e.mergeRaceRetryHookForTest = func(attempt int) {
		if attempt == 1 && !aborted {
			aborted = true
			rbErr = holder.Rollback()
		}
	}
	if err := mergeRetryRun(ctx, e, "MERGE (n:K {k: 0}) ON CREATE SET n.who = 'loser'"); err != nil {
		t.Fatalf("autocommit MERGE after the holder aborted: %v", err)
	}
	if rbErr != nil {
		t.Fatalf("holder rollback: %v", rbErr)
	}
	if !aborted {
		t.Fatal("the autocommit MERGE never lost the race; the precondition was not constructed")
	}
	if c := mergeRetryCount(t, e); c != 1 {
		t.Fatalf("count(K {k:0}) = %d, want 1", c)
	}
}

// TestMergeUniqueRace_ExplicitTxLoserUnchanged pins the behaviour the retry
// deliberately leaves alone: a statement of an EXPLICIT transaction that loses the
// race still fails with the constraint violation, and is never re-run — its
// snapshot is the transaction's, so a re-run inside it could not see the winner.
func TestMergeUniqueRace_ExplicitTxLoserUnchanged(t *testing.T) {
	t.Parallel()
	var retries atomic.Int64
	e := mergeRetryEngine(t, &retries)
	ctx := context.Background()

	holder, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx holder: %v", err)
	}
	if _, err := drainExecForRetry(holder.Exec("MERGE (n:K {k: 0})", nil)); err != nil {
		t.Fatalf("holder MERGE: %v", err)
	}
	loser, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx loser: %v", err)
	}
	_, err = drainExecForRetry(loser.Exec("MERGE (n:K {k: 0})", nil))
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Fatalf("explicit-tx loser: err = %v, want a constraint violation", err)
	}
	_ = loser.Rollback() // the loser's statement already failed; its outcome is the assertion above
	if err := holder.Commit(); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	if n := retries.Load(); n != 0 {
		t.Errorf("an explicit-transaction statement was re-run %d times, want 0", n)
	}
	if c := mergeRetryCount(t, e); c != 1 {
		t.Fatalf("count(K {k:0}) = %d, want 1", c)
	}
}

// drainExecForRetry drains an ExplicitTx.Exec result and reports its error.
func drainExecForRetry(res *Result, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return n, err
	}
	return n, res.Close()
}
