package store_test

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// TestMain verifies no goroutine leaks at the end of the test run.
// Per CLAUDE.md: every package that spawns goroutines (here, the
// background checkpointer the composed DB owns) must integrate
// go.uber.org/goleak. The whole point of store.DB.Close is that the
// checkpoint goroutine is stopped before the WAL is closed; this gate
// turns "the goroutine leaked past Close" into a hard test failure.
//
// subproc.Dispatch runs first so a child process spawned by the crash-cycle
// test in open_test.go runs its registered handler and exits before the test
// framework initialises; in the parent it is a no-op.
func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m)
}
