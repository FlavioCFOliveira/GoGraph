package exec

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
// spawns goroutines inside cypher/exec.
//
// Per CLAUDE.md: every package that spawns goroutines must integrate
// go.uber.org/goleak.
//
// Operator goroutine catalogue (as of this sprint):
//
//   - ParallelScan: spawns one splitter goroutine plus N worker goroutines
//     during Init. Workers exit when the output channel is closed or ctx is
//     cancelled. This is the only exec operator that directly calls go func.
//   - Apply, SemiApply, AntiSemiApply, CorrelatedApply, OptionalExpand,
//     RollupApply: drive inner pipelines synchronously; no goroutines spawned.
//   - EagerAggregation, GlobalAggregateAdapter, Sort, Top, Distinct, Union:
//     accumulate or merge rows synchronously; no goroutines spawned.
//   - VarLengthExpand: iterative BFS/DFS loop; no goroutines spawned.
//
// Without this guard, an operator that forgot to drain or cancel a child
// pipeline on the error path would leak a goroutine per affected test
// invocation, accumulating silently across the suite.
//
// goleak.IgnoreTopFunction is used sparingly: only for goroutines
// the Go runtime and the test driver legitimately leave behind
// (testing.(*M).Run's per-package runtime initialisation). Add to
// the ignore list only after confirming the goroutine is genuinely
// outside our control.
func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m,
		// The test driver itself occasionally leaves a tracker goroutine
		// blocked on a select. It is owned by stdlib/testing and is not
		// a cypher/exec leak.
		goleak.IgnoreTopFunction("testing.tRunner.func1"),
	)
}
