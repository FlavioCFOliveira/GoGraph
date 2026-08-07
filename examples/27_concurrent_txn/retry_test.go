package main

// retry_test.go — the retry bound and its diagnosis, validated in BOTH directions.
//
// The bound exists to catch a HANG: an object that stops becoming writable, which
// graph/mvcc/conflict.go records as having really happened before rmp #2318 gave the
// vacuum an unconditional wake. It must NOT fire on contention, which is what the
// attempt count it replaced did — under parallel WAL load this example failed 2 runs
// in 6 with "25 serialization conflicts on one transfer" while passing 8 of 8 alone.
//
// So both directions are asserted here: a wedged head must still fail, and a long
// but progressing chain must not. A bound that only ever passes proves nothing, and
// three of this project's own instruments have been caught reporting a number they
// could only have produced.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// testBudget is short enough to keep these tests in the short layer and long
// enough to admit several backoff steps, so the exhaustion path is reached the
// way it would be in production rather than on the first attempt.
const testBudget = 60 * time.Millisecond

// TestRetryOnConflictFailsOnWedgedHead is the direction that matters: one
// in-flight transaction holds the head forever, and the loop must give up and say
// so. Before rmp #2318 this was a real engine state, not a hypothetical.
func TestRetryOnConflictFailsOnWedgedHead(t *testing.T) {
	const stuckHead = mvcc.TxIDBase + 77
	var c conflictCounters
	var attempts atomic.Int64

	err := retryOnConflict(context.Background(), testBudget, &c, func() error {
		attempts.Add(1)
		// The head never changes and the snapshot never advances: the object is
		// wedged behind a transaction that neither commits nor aborts.
		return mvcc.NewConflict(mvcc.StoreNodeProperties, stuckHead, 500, mvcc.TxIDBase+78)
	})
	if err == nil {
		t.Fatal("a permanently wedged head must exhaust the budget, got nil error")
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("the exhaustion error must still unwrap to the conflict: %v", err)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("the loop must actually retry before giving up, got %d attempt(s)", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "HANG") {
		t.Errorf("diagnosis must classify a wedged head as a HANG, got:\n%s", msg)
	}
	if !strings.Contains(msg, "STUCK on one version") {
		t.Errorf("diagnosis must report the head as stuck, got:\n%s", msg)
	}
	if c.conflicts.Load() != attempts.Load() {
		t.Errorf("every refused attempt must be counted: conflicts=%d attempts=%d",
			c.conflicts.Load(), attempts.Load())
	}
}

// TestRetryOnConflictFailsOnAbortedHead pins the OTHER hang, which has a different
// cause and therefore a different fix: an aborted version the vacuum has not
// withdrawn. Reporting it as a generic conflict is what sent rmp #2333 hunting.
func TestRetryOnConflictFailsOnAbortedHead(t *testing.T) {
	var c conflictCounters
	err := retryOnConflict(context.Background(), testBudget, &c, func() error {
		return mvcc.NewConflict(mvcc.StoreNodeProperties, mvcc.AbortedTS, 500, mvcc.TxIDBase+1)
	})
	if err == nil {
		t.Fatal("an un-withdrawn aborted head must exhaust the budget, got nil error")
	}
	if msg := err.Error(); !strings.Contains(msg, "ABORTED") || !strings.Contains(msg, "vacuum") {
		t.Errorf("diagnosis must name the aborted head and the vacuum, got:\n%s", msg)
	}
}

// TestRetryOnConflictSurvivesLongProgressingChain is the regression that the
// attempt count could not pass. Forty refusals, each against a DIFFERENT head and
// a fresh snapshot, is a progressing system and must succeed — the old bound of 24
// attempts failed it by construction, whatever the engine was doing.
func TestRetryOnConflictSurvivesLongProgressingChain(t *testing.T) {
	const refusals = 40
	var c conflictCounters
	var n int64

	err := retryOnConflict(context.Background(), conflictRetryBudget, &c, func() error {
		n++
		if n > refusals {
			return nil
		}
		// Both the head and the snapshot move: other transactions are committing
		// throughout, so this is contention and not a hang.
		return mvcc.NewConflict(mvcc.StoreNodeProperties,
			mvcc.TxIDBase+uint64(n)*2, 1000+uint64(n), mvcc.TxIDBase+uint64(n)*2+1)
	})
	if err != nil {
		t.Fatalf("a progressing chain of %d refusals must still commit, got: %v", refusals, err)
	}
	if got := c.retriesMax.Load(); got != refusals {
		t.Errorf("retriesMax must record the chain depth: got %d want %d", got, refusals)
	}
	if c.waitMaxNs.Load() <= 0 {
		t.Error("waitMaxNs must record the wall time a retried transfer spent; it sizes the budget")
	}
}

// TestRetryOnConflictPassesThroughNonConflictErrors keeps the loop from turning an
// unrelated failure into a retry storm.
func TestRetryOnConflictPassesThroughNonConflictErrors(t *testing.T) {
	sentinel := errors.New("disk on fire")
	var c conflictCounters
	var attempts int

	err := retryOnConflict(context.Background(), conflictRetryBudget, &c, func() error {
		attempts++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("a non-conflict error must be returned as-is, got %v", err)
	}
	if attempts != 1 {
		t.Errorf("a non-conflict error must not be retried, got %d attempts", attempts)
	}
	if c.conflicts.Load() != 0 {
		t.Errorf("a non-conflict error must not be counted as a conflict, got %d", c.conflicts.Load())
	}
}

// TestRetryOnConflictHonoursContext proves the loop does not outlive a cancelled
// run: the budget is 30 s and a cancelled context must beat it.
func TestRetryOnConflictHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var c conflictCounters
	var attempts int

	err := retryOnConflict(ctx, conflictRetryBudget, &c, func() error {
		attempts++
		if attempts == 3 {
			cancel()
		}
		return mvcc.NewConflict(mvcc.StoreNodeProperties, mvcc.TxIDBase+9, 1, mvcc.TxIDBase+10)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context must end the retry loop, got %v", err)
	}
}

// TestConflictChainClassifiesStarvation covers the third verdict: the engine was
// live and this writer simply never got a turn. It is a different finding from a
// hang and must not be reported as one.
func TestConflictChainClassifiesStarvation(t *testing.T) {
	var chain conflictChain
	for i := uint64(1); i <= 30; i++ {
		chain.record(mvcc.NewConflict(mvcc.StoreNodeProperties,
			mvcc.TxIDBase+i*3, 100+i, mvcc.TxIDBase+i*3+1))
	}
	d := chain.diagnosis()
	if !strings.Contains(d, "STARVATION") {
		t.Errorf("a moving head with a moving snapshot is starvation, got:\n%s", d)
	}
	if strings.Contains(d, "HANG") {
		t.Errorf("starvation must not be reported as a hang, got:\n%s", d)
	}
	if !strings.Contains(d, "attempts: 30") {
		t.Errorf("the diagnosis must report the true attempt count, got:\n%s", d)
	}
}

// TestConflictChainClassifiesStalledFrontier covers the fourth verdict: the
// snapshot never advanced, so no attempt after the first was a retry at all.
func TestConflictChainClassifiesStalledFrontier(t *testing.T) {
	var chain conflictChain
	for i := uint64(1); i <= 8; i++ {
		// The head moves, but every attempt runs at the same instant.
		chain.record(mvcc.NewConflict(mvcc.StoreNodeProperties, mvcc.TxIDBase+i, 117, mvcc.TxIDBase+i+1))
	}
	if d := chain.diagnosis(); !strings.Contains(d, "NEVER ADVANCED") {
		t.Errorf("a frozen snapshot must be named, got:\n%s", d)
	}
}

// TestConflictChainBoundsRetentionWithoutLosingTheVerdict is the reason the chain
// summarises incrementally: a 30 s budget against a 5 ms backoff ceiling can refuse
// thousands of times, and the verdict must still be computed over ALL of them
// rather than over whatever the sample happened to keep.
func TestConflictChainBoundsRetentionWithoutLosingTheVerdict(t *testing.T) {
	const total = 500
	var chain conflictChain
	// Every attempt but one has the same head. If the verdict were computed from a
	// retained window at the ends, this single divergence in the elided middle
	// would be missed and the chain would be misreported as STUCK.
	for i := 0; i < total; i++ {
		head := uint64(mvcc.TxIDBase + 5)
		if i == total/2 {
			head = mvcc.TxIDBase + 6
		}
		chain.record(mvcc.NewConflict(mvcc.StoreNodeProperties, head, 42, mvcc.TxIDBase+7))
	}

	d := chain.diagnosis()
	if !strings.Contains(d, "attempts: 500") {
		t.Errorf("the true attempt count must survive sampling, got:\n%s", d)
	}
	if !strings.Contains(d, "elided") {
		t.Errorf("a chain of %d must elide its middle rather than print it all, got:\n%s", total, d)
	}
	if strings.Contains(d, "STUCK on one version") {
		t.Errorf("the divergence in the elided middle must still count, got:\n%s", d)
	}
	if lines := strings.Count(d, "head="); lines > 2*conflictSample+4 {
		t.Errorf("retention must stay bounded, got %d rendered attempts", lines)
	}
}

// TestConflictChainRecordsUntypedConflicts keeps the attempt count in the
// diagnosis equal to the one in the error even when the engine returns a conflict
// it did not describe.
func TestConflictChainRecordsUntypedConflicts(t *testing.T) {
	var chain conflictChain
	chain.record(mvcc.ErrSerializationConflict)
	chain.record(mvcc.NewConflict(mvcc.StoreNodeProperties, mvcc.TxIDBase+1, 5, mvcc.TxIDBase+2))
	if d := chain.diagnosis(); !strings.Contains(d, "attempts: 2") || !strings.Contains(d, "untyped") {
		t.Errorf("an undescribed conflict must still be counted and marked, got:\n%s", d)
	}
}
