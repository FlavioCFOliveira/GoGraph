package shapegen

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run.
// Per CLAUDE.md: every package that spawns goroutines — and every
// package that may be embedded into one that does — must integrate
// go.uber.org/goleak.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
