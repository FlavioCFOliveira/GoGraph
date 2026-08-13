package mvcc

// goleak_test.go — the leak gate for the concurrency substrate itself (rmp #2422).
//
// # Why this package needed one and did not have one
//
// The Reliability and Concurrency Mandates require that every goroutine the
// library spawns has a defined lifecycle, "verified via go.uber.org/goleak in
// test teardown for every package that spawns goroutines". This package spawns
// one — the context-aware acquisition helper at [acquireCtx] — and had no goleak
// verification at all: no import, no TestMain. The one package whose whole
// subject is concurrency was the one the leak gate did not cover.
//
// That is not a report of a leak. Reading acquireCtx, the helper always
// terminates: it blocks in lock(), then either hands the lock off and returns or
// unlocks and returns, so it cannot outlive the holder's tenure. The gap was that
// nothing verified it, so a future change that DID leak here would be caught by
// nothing.
//
// # Why the check is at TestMain rather than per test
//
// The helper's lifetime is bounded by ANOTHER party's lock tenure, so a
// goleak.VerifyNone at the end of a test that deliberately abandoned an
// acquisition can observe a helper still parked and report a false positive —
// the test has returned, but the holder it is queued behind belongs to a sibling.
// Checking once, after every test in the package has finished, removes that race
// without weakening the assertion: at that point no holder remains, so a parked
// helper really is a leak.
//
// goleak retries with backoff before failing, which absorbs the scheduling delay
// between a lock being released and the helper waking to observe it.

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
