package recovery

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run.
// Per CLAUDE.md: every package that spawns goroutines must integrate
// go.uber.org/goleak. The recovery package itself does not spawn
// goroutines, but its dependencies (wal, snapshot, txn) might; this
// guard catches any future regression where a Close path leaves a
// goroutine running across the recovery boundary.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
