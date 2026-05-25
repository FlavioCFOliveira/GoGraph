package snapshot

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run.
// Per CLAUDE.md: every package that spawns goroutines must integrate
// go.uber.org/goleak. The snapshot package itself is synchronous, but
// its consumers (metrics, filesystem helpers) may spawn background
// goroutines; this guard catches any future regression.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
