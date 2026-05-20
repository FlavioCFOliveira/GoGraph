package centrality

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run.
// Per CLAUDE.md: every package that spawns goroutines must integrate
// go.uber.org/goleak. centrality.BetweennessParallel spawns one
// goroutine per worker, so a leak here would surface either a
// missing wg.Done() or a worker that exited via panic without
// recovery — exactly the failure modes goleak is built to catch.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
