package cypher_test

import (
	"testing"

	"go.uber.org/goleak"

	"gograph/internal/subproc"
)

// TestMain runs subproc.Dispatch first so that child processes spawned by
// cross-process tests in this package dispatch to their registered handler and
// exit before the test framework initialises. When running as the parent,
// Dispatch is a no-op and the test suite proceeds normally.
//
// goleak.VerifyTestMain follows to catch goroutine leaks in any test that
// spawns goroutines inside the cypher package.
func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m,
		// The test driver itself occasionally leaves a tracker goroutine
		// blocked on a select. It is owned by stdlib/testing and is not a
		// cypher leak.
		goleak.IgnoreTopFunction("testing.tRunner.func1"),
	)
}
