package csr

import (
	"testing"

	"go.uber.org/goleak"

	"gograph/internal/subproc"
)

// TestMain dispatches subproc child modes before running tests and
// verifies no goroutine leaks at the end of the test run. Per
// CLAUDE.md: every package that spawns goroutines must integrate
// go.uber.org/goleak.
func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m)
}
