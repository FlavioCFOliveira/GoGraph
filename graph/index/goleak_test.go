package index

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run.
// Per CLAUDE.md: every package that spawns goroutines must integrate
// go.uber.org/goleak.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
