package ctxlock

// main_test.go — goleak for the package (rmp #2260).
//
// This package spawns a goroutine in library code ([Acquire]'s helper), and the
// project mandate requires goleak in test teardown for EVERY package that does.
// It was the only such package in the module without it, which meant the liveness
// argument in ctxlock.go's package doc — that the helper always terminates once
// the holder releases — was asserted rather than enforced.
//
// goleak retries with backoff, which is what makes this workable: a helper whose
// lock is released at the end of a test is still draining when the test returns,
// and the retry window absorbs that without needing a sleep.

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
