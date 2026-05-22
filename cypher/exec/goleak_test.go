package exec

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies that no goroutine leaks at the end of the test run.
// Per CLAUDE.md: every package that spawns goroutines must integrate
// go.uber.org/goleak.
//
// cypher/exec spawns goroutines for morsel-parallel evaluation
// (ParallelScan, Apply, SemiApply, Sort, Top, Distinct, Union,
// Aggregation, OptionalExpand, VarLengthExpand, …). Without this
// guard, an operator that forgot to drain or cancel a child pipeline
// on the error path would leak a goroutine per affected test
// invocation, accumulating silently across the suite.
//
// goleak.IgnoreTopFunction is used sparingly: only for goroutines
// the Go runtime and the test driver legitimately leave behind
// (testing.(*M).Run's per-package runtime initialisation). Add to
// the ignore list only after confirming the goroutine is genuinely
// outside our control.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// The test driver itself occasionally leaves a tracker goroutine
		// blocked on a select. It is owned by stdlib/testing and is not
		// a cypher/exec leak.
		goleak.IgnoreTopFunction("testing.tRunner.func1"),
	)
}
