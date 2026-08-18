package recovery

import (
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// TestMain runs subproc.Dispatch first so that child processes spawned
// by cross-process tests in this package dispatch to their registered
// handler and exit before the test framework initialises. When running
// as the parent, Dispatch is a no-op and the test suite proceeds
// normally. goleak.VerifyTestMain follows to catch goroutine leaks.
//
// It also installs the process-scoped removal of the cached crashinject-helper
// binary (rmp #2527). The crash-injection tests in this package build that
// helper into an os.MkdirTemp directory that must outlive each individual test,
// so the only correct hook is this one; see [crashinject.RemoveHelperBinary] for
// why t.Cleanup would break the cache. The call is unconditional and a no-op
// when no helper was built, which is the case in the default build — the tests
// that spawn the helper are gated behind the gograph_crashinject tag — so it
// costs nothing there and cannot be forgotten when the tag is on.
//
// The hook goes through goleak.Cleanup rather than a defer because
// goleak.VerifyTestMain ends in os.Exit, which does not run deferred functions;
// the Cleanup option replaces that os.Exit, so this closure must exit itself.
func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m, goleak.Cleanup(func(exitCode int) {
		crashinject.RemoveHelperBinary()
		os.Exit(exitCode)
	}))
}
