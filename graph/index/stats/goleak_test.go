package stats_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run. Per CLAUDE.md
// every package integrates go.uber.org/goleak; the stats package spawns no
// goroutines, so this is a standing guard against a future regression.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
