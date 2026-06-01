package graph

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run and
// dispatches subproc child modes before running tests. Per CLAUDE.md:
// every package that spawns goroutines must integrate go.uber.org/goleak.
func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m)
}
