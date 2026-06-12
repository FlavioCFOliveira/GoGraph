package crashinject_test

// helper_build_unique_path_test.go — gate test for #1390.
//
// Verifies that concurrent calls to Run from the same process all succeed
// (no ETXTBSY, no "binary does not exist"), proving the helper binary lives
// in a process-unique directory rather than a shared flat path.

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
)

// TestHelperBinary_ConcurrentRunsSucceed launches several goroutines that
// all call Run concurrently. All invocations must succeed — no error from
// a missing or partially-written binary — confirming the per-process unique
// directory prevents cross-goroutine (and by extension cross-process)
// collisions.
func TestHelperBinary_ConcurrentRunsSucceed(t *testing.T) {
	t.Parallel()

	const concurrency = 4

	type result struct {
		out crashinject.Out
		err error
	}
	results := make([]result, concurrency)
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := range concurrency {
		go func(idx int) {
			defer wg.Done()
			// Use an unknown scenario so the helper exits quickly.
			out, err := crashinject.Run(t, "no.such.scenario", crashinject.Opts{})
			results[idx] = result{out, err}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d: Run returned error: %v", i, r.err)
		}
		// Unknown scenario → helper exits with non-zero code, not Killed.
		if r.out.Killed {
			t.Errorf("goroutine %d: Killed=true for unknown scenario, want false", i)
		}
	}
}
