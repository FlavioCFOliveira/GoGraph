package bulk

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// TestMain runs subproc.Dispatch first so that child processes spawned
// by cross-process tests dispatch to their registered handler and exit
// before the test framework initialises. When running as the parent,
// Dispatch is a no-op and the test suite proceeds normally.
// goleak.VerifyTestMain follows to catch goroutine leaks.
func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m)
}
