package lpg

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// TestMain dispatches to any registered subproc handler when the
// binary is re-executed as a child process (cross-process tests),
// then verifies no goroutine leaks at the end of the parent run.
func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m)
}
