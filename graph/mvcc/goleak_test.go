package mvcc

// goleak_test.go — the leak gate for the concurrency substrate itself (rmp #2422).
//
// # Why this package needed one and did not have one
//
// The Reliability and Concurrency Mandates require that every goroutine the
// library spawns has a defined lifecycle, "verified via go.uber.org/goleak in
// test teardown for every package that spawns goroutines". This package used to
// spawn one — a context-aware acquisition helper on [Gate] — and had no goleak
// verification at all: no import, no TestMain. The one package whose whole
// subject is concurrency was the one the leak gate did not cover.
//
// rmp #2983 removed that helper: every wait on the gate now selects on the
// caller's context directly, so the package spawns no goroutine of its own. The
// gate stays, because the tests here spawn many, and a future change that brought
// a helper back would otherwise be caught by nothing.
//
// # Why the check is at TestMain rather than per test
//
// Several tests start goroutines whose lifetime is bounded by ANOTHER party's
// tenure, so a goleak.VerifyNone at the end of one test can observe a sibling's
// goroutine still parked and report a false positive. Checking once, after every
// test in the package has finished, removes that race without weakening the
// assertion: at that point no holder remains, so a parked goroutine really is a
// leak.
//
// goleak retries with backoff before failing, which absorbs the scheduling delay
// between a lock being released and a waiter waking to observe it.

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
