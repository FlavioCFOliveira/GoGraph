package exec_test

// parallel_closer_test.go — regression gate for #1795 (sprint 250): the
// ParallelScan closer goroutine (op.wg.Wait(); close(outCh)) was not joined by
// Close(), so a caller that abandoned the operator without draining outCh left
// the closer running until the workers exited, and Close() could return before
// the closer had finished. Close() now joins the closer (closerWG), so NO
// goroutine outlives Close even with no intervening drain — verified
// deterministically (immediate count check, no settle/poll tolerance, which is
// what goleak's retry would otherwise mask).

import (
	"context"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

func TestParallelScan_CloseJoinsCloser_NoDrain_1795(t *testing.T) {
	// A large node set guarantees outCh fills and back-pressures the workers,
	// so without draining there is real in-flight work for Close to unwind.
	walker := buildWalker(100_000)

	// Let any transient runtime goroutines settle, then take a baseline.
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		op := exec.NewParallelScan(walker, 16) // tiny morsels => many sends
		if err := op.Init(context.Background()); err != nil {
			t.Fatalf("Init: %v", err)
		}
		// Deliberately do NOT drain outCh. Close must cancel workers AND join
		// the closer goroutine before returning.
		if err := op.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// Immediately after the loop, with no polling tolerance, the goroutine
	// count must be back at (or below) baseline. A lingering closer or worker
	// would show up here.
	if got := runtime.NumGoroutine(); got > base {
		t.Errorf("goroutine count after 50 Init/Close-without-drain = %d, baseline %d (closer/worker leaked)", got, base)
	}
}
