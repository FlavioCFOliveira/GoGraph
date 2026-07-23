package count

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run. Per CLAUDE.md
// every package is integrated with go.uber.org/goleak; the count store spawns no
// goroutines, so this is a standing guard against a future regression.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
