//go:build soak || nightly || soakfull

// Package main_test — shared configuration and abstention rules for the soak
// instruments.
//
// # Why the abstention rule lives here
//
// The 2026-08-10 production-readiness certification recorded the soak layer as
// green for a run in which *neither* of the two assertions CLAUDE.md names as
// the soak acceptance criterion had actually evaluated anything:
// TestNoGrowth_HeapFDGoroutine logged "insufficient samples for regression
// (< 2); skipping slope check" and TestLatencyP99_Stable logged "insufficient
// post-warmup windows for regression; skipping slope check", and both then
// returned success. A third instrument, TestGCPause_Stable, took the same early
// return and in doing so skipped its 200 ms max-pause *ceiling* as well, which
// needs no regression at all. So "soak layer green" meant the workload ran, not
// that anything about it had been checked (rmp #2396).
//
// A gate that cannot fail is worse than no gate, because it is quoted as
// evidence. The rules below exist so that no soak instrument can ever again
// report success without either asserting its criterion or saying, unmissably,
// that it did not.
package main_test

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// minRegressionPoints and slopeGateDecide live in slopegate_test.go, which
// carries no build tag so that the rule is pinned by the short layer.

// soakEnvInt returns the positive integer value of environment variable key,
// or def when the variable is unset, empty, non-numeric, or non-positive.
func soakEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// soakEnvDuration returns the positive duration value of environment variable
// key (e.g. "30m", "4h"), or def when unset, empty, unparseable, or non-positive.
func soakEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// soakEnvAnySet reports whether any of keys is set to a non-empty value. It is
// how an instrument tells that the operator explicitly sized its measurement
// window, which changes what an abstention means (see requireRegressionPoints).
func soakEnvAnySet(keys ...string) bool {
	for _, k := range keys {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// soakFullRun reports whether this process is running a full-length soak
// variant, i.e. the configuration in which CLAUDE.md's soak acceptance
// criterion is expected to be evaluated.
func soakFullRun() bool {
	if testing.Short() {
		return false
	}
	return os.Getenv("SOAK_FULL") == "1" || os.Getenv("GOGRAPH_NIGHTLY") == "1"
}

// requireRegressionPoints decides what an instrument does when it holds too few
// samples to fit a meaningful slope. It returns true only when the caller may
// proceed to regress; it never returns false quietly.
//
// The decision turns on whether this run was expected to assert:
//
//   - Short layer, default window. Too few samples is a documented property of
//     the layer rather than a defect, so the test SKIPS. Go then reports it as
//     skipped instead of passed, which is the whole point: a reader of the gate
//     sees that the criterion was not evaluated.
//   - Full soak, nightly, or any run whose window the operator sized
//     explicitly through one of overrideKeys. Here the criterion is expected to
//     be evaluated, so too few samples is a FAILURE. A mandated acceptance
//     criterion must never be reported as met by a run that never reached it.
//
// howToExtend is quoted verbatim in both messages and must name the concrete
// environment setting that makes the check assert, so the reader is told not
// merely that the gate abstained but exactly how to make it fire.
func requireRegressionPoints(t *testing.T, label string, n int, howToExtend string, overrideKeys ...string) bool {
	t.Helper()
	mandated := soakFullRun() || soakEnvAnySet(overrideKeys...)
	msg := fmt.Sprintf(
		"%s: SLOPE CHECK DID NOT ASSERT — collected %d sample(s), need %d. "+
			"This run exercised the workload but evaluated no growth/stability criterion, "+
			"so its result is NOT evidence of stability. To make it assert: %s",
		label, n, minRegressionPoints, howToExtend)
	switch slopeGateDecide(n, mandated) {
	case slopeGateAssert:
		return true
	case slopeGateFail:
		// Expected to assert and could not: that is a red gate, not a note.
		t.Fatal(msg)
	case slopeGateSkip:
		t.Skip(msg)
	}
	return false
}
