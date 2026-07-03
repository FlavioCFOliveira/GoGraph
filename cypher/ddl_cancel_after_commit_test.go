package cypher_test

// ddl_cancel_after_commit_test.go — regression gate for rmp #1869
// (2026-07-02 production-readiness audit round 2, finding "Result.Err() can
// show context.Canceled for an already-durably-committed write").
//
// Background. Every DDL statement (CREATE/DROP INDEX/CONSTRAINT) applies its
// real, interruptible work first (a backfill scan, then the WAL append and
// fsync), then constructs its returned Result by draining a trivial,
// always-immediate confirmation row (exec.NewArgument, wrapped by
// emptyDDLResult). Before the fix, that confirmation drain reused the
// caller's query ctx, so a cancellation landing in the (arbitrarily small)
// window between the real work finishing and the confirmation row draining
// was observed via Result.Err() as context.Canceled -- on a statement that
// had already durably committed. The original audit measured this at 53/60
// CREATE INDEX trials cancelled near the commit boundary. A caller that
// reasonably retries on a reported cancellation then risks double-applying
// an already-successful write.
//
// This test reproduces the audit's own methodology: run CREATE INDEX
// repeatedly against a WAL-backed engine, cancelling the context at a range
// of offsets spanning the statement's execution window, and assert the
// invariant the fix restores: whenever the index is actually registered
// afterward (proof the DDL committed), Result.Err() must never report
// context.Canceled for that same statement.

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// TestDDL_CreateIndex_CommittedNeverReportsCanceled runs enough trials, with
// cancellation timed at a range of offsets spanning the statement's
// execution window, that the pre-fix bug (53/60 false positives in the
// original audit measurement) would almost certainly reproduce at least
// once if it still existed.
func TestDDL_CreateIndex_CommittedNeverReportsCanceled(t *testing.T) {
	const trials = 60
	falsePositives := 0
	committedCount := 0

	// Calibrate the delay spread to the CURRENT environment's actual
	// execution speed (which varies enormously between a plain run and a
	// -race-instrumented one) rather than a fixed guess: time one full,
	// uncancelled CREATE INDEX first, then spread trial delays across a
	// wide range straddling that measurement so at least some trials land
	// near the real commit boundary regardless of machine/instrumentation
	// speed.
	calEng, _, _ := newWALStoreEngine(t)
	drainOK(t, calEng, `CREATE (:P {v: 1})`)
	calStart := time.Now()
	drainOK(t, calEng, `CREATE INDEX idx_cal FOR (n:P) ON (n.v)`)
	baseline := time.Since(calStart)
	if baseline <= 0 {
		baseline = time.Microsecond
	}

	for i := 0; i < trials; i++ {
		eng, g, _ := newWALStoreEngine(t)
		drainOK(t, eng, `CREATE (:P {v: 1})`)

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel after a delay spread from ~3% to ~300% of the measured
		// baseline (i ranges 0..59, exponent step ~0.1 in log2 space), so
		// trials collectively straddle the statement's actual commit
		// boundary regardless of absolute execution speed, matching the
		// original audit's own sampling methodology.
		scale := math.Pow(2, float64(i)/6.0-5)
		delay := time.Duration(float64(baseline) * scale)
		timer := time.AfterFunc(delay, cancel)

		res, err := eng.RunInTx(ctx, `CREATE INDEX idx_p_v FOR (n:P) ON (n.v)`, nil)
		var drainErr error
		if err == nil {
			for res.Next() { //nolint:revive // drain to completion
			}
			drainErr = res.Err()
			_ = res.Close()
		} else {
			drainErr = err
		}
		timer.Stop()

		_, getErr := g.IndexManager().GetIndex("idx_p_v")
		committed := getErr == nil
		if committed {
			committedCount++
			if errors.Is(drainErr, context.Canceled) {
				falsePositives++
				t.Errorf("trial %d: index committed (found in IndexManager) but reported context.Canceled: %v", i, drainErr)
			}
		} else if !errors.Is(getErr, index.ErrIndexNotFound) {
			t.Fatalf("trial %d: unexpected IndexManager.GetIndex error: %v", i, getErr)
		}
	}

	if committedCount == 0 {
		t.Fatal("no trial committed the index — test is not exercising the intended race window, cannot validate the invariant")
	}
	if falsePositives > 0 {
		t.Fatalf("%d/%d committed trials falsely reported context.Canceled (want 0)", falsePositives, trials)
	}
}
