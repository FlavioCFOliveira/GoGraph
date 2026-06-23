package exec

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
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
//     cancelled.
//   - ParallelCountScan: spawns up to GOMAXPROCS worker goroutines during Init,
//     each accumulating a private count. Workers exit when the work channel drains
//     or ctx is cancelled; Next joins via wg.Wait, Close cancels then joins.
//   - ParallelScanProject (#1682): spawns up to GOMAXPROCS worker goroutines
//     during Init, each building and draining an independent fused
//     scan→filter→project sub-plan over its morsels into a private buffer. Workers
//     exit when the work channel drains, a sub-plan errors, or ctx is cancelled;
//     the first Next joins via wg.Wait, and Close cancels then joins any worker a
//     never-called or partially-drained Next left running. No closer goroutine —
//     the join happens on the Next/Close goroutine.
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
