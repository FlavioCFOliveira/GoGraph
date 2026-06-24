package cypher_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runWithDeadline runs fn and fails the test if it does not return within d. A
// timeout is the signature of a held visibility barrier (a writer that errored
// inside ApplyAtomically without releasing visMu would wedge every later reader
// and writer), so this watchdog turns a deadlock into a clear test failure
// instead of a hung run.
func runWithDeadline(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("%s did not complete within %s — likely a held visibility barrier (deadlock).\nGoroutine dump:\n%s", what, d, buf[:n])
	}
}

// TestConcurrent_AggregatorCapInBarrier_NoDeadlock chases the concurrency
// auditor's hypothesis #1: a capped buffering aggregator that trips
// MaxCollectItems INSIDE the write visibility barrier might hold the barrier on
// the error path, starving every concurrent reader and writer. It runs many
// concurrent cap-tripping aggregating WRITE queries alongside honest reads and
// writes against one engine with MaxCollectItems clamped, under a deadlock
// watchdog. If the error path leaked visMu, the readers would wedge and the
// watchdog would fire. A clean completion is evidence the barrier is released on
// every error path.
func TestConcurrent_AggregatorCapInBarrier_NoDeadlock(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxCollectItems: 8})
	ctx := context.Background()

	// Seed enough :T nodes that a collect over them exceeds the cap of 8.
	for i := 0; i < 64; i++ {
		if _, err := eng.RunInTx(ctx, fmt.Sprintf("CREATE (:T {v:%d})", i), nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	runWithDeadline(t, 30*time.Second, "concurrent aggregator-cap workload", func() {
		var wg sync.WaitGroup
		const goroutines = 16
		const iters = 25
		for w := 0; w < goroutines; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					switch w % 4 {
					case 0:
						// Cap-tripping aggregating WRITE: collect over >cap nodes
						// inside the write barrier, then write. Expected to error
						// (cap exceeded) — the barrier MUST be released regardless.
						res, err := eng.RunInTx(ctx,
							"MATCH (n:T) WITH collect(n.v) AS vs CREATE (:Agg {n: size(vs)})", nil)
						if err == nil {
							for res.Next() {
							}
							_ = res.Close()
						}
					case 1:
						// Honest concurrent read: must never be starved by a
						// barrier the errored writer failed to release.
						res, err := eng.Run(ctx, "MATCH (n) RETURN count(n)", nil)
						if err == nil {
							for res.Next() {
							}
							_ = res.Close()
						}
					case 2:
						// Honest concurrent write.
						res, err := eng.RunInTx(ctx, fmt.Sprintf("CREATE (:U {w:%d,i:%d})", w, i), nil)
						if err == nil {
							for res.Next() {
							}
							_ = res.Close()
						}
					default:
						// Cap-tripping aggregating READ (collect under the read barrier).
						res, err := eng.Run(ctx, "MATCH (n:T) RETURN collect(n.v)", nil)
						if err == nil {
							for res.Next() {
							}
							_ = res.Close()
						}
					}
				}
			}(w)
		}
		wg.Wait()
	})

	// The engine must remain fully usable after the storm of cap errors: a final
	// read succeeds, proving no barrier was permanently leaked.
	res, err := eng.Run(ctx, "MATCH (n) RETURN count(n)", nil)
	if err != nil {
		t.Fatalf("engine wedged after cap-error storm: %v", err)
	}
	_ = res.Close()
}

// TestConcurrent_ParallelBackfill_NoStarvation chases the concurrency auditor's
// hypothesis #2: the parallel CREATE INDEX backfill polls ctx but does not
// explicitly Gosched, so it might starve honest readers. The CORRECT,
// non-flaky property to assert is forward progress, not interleaving: whether a
// reader gets a turn DURING a brief CPU-bound build is statistical (Go's async
// preemption, the worker count, and core availability all bear on it), so —
// exactly as the cpu-starvation scenario declines to assert latency percentiles
// — this test asserts only that nothing DEADLOCKS or WEDGES. Concurrent readers
// run throughout an above-threshold parallel backfill; the whole fleet must
// complete within the watchdog deadline and the engine must serve reads cleanly
// afterwards. (GOMAXPROCS is left at the default so the parallel path actually
// engages: the gate forces serial when workers <= 1.)
func TestConcurrent_ParallelBackfill_NoStarvation(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("parallel backfill needs >= 2 GOMAXPROCS to engage; serial path covered elsewhere")
	}

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed > backfillParallelMinNodes (8192) labelled+named nodes so CREATE INDEX
	// engages the parallel backfill phase.
	const n = 12_000
	for i := 0; i < n; i++ {
		if _, err := eng.RunInTxAny(ctx, fmt.Sprintf("CREATE (:P {name:'p%d'})", i), nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	var readsDuringBuild int64
	runWithDeadline(t, 60*time.Second, "parallel backfill + concurrent readers", func() {
		var wg sync.WaitGroup
		stop := make(chan struct{})
		var reads [4]int

		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func(r int) {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					res, err := eng.Run(ctx, "MATCH (n:P) RETURN count(n)", nil)
					if err == nil {
						for res.Next() {
						}
						_ = res.Close()
						reads[r]++
					}
				}
			}(r)
		}

		// The parallel CREATE INDEX backfill, concurrent with the readers. It must
		// complete (no deadlock) regardless of how the scheduler interleaves.
		if _, err := eng.RunInTx(ctx, "CREATE INDEX p_name FOR (n:P) ON (n.name)", nil); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("CREATE INDEX backfill: %v", err)
		}
		close(stop)
		wg.Wait()
		for _, c := range reads {
			readsDuringBuild += int64(c)
		}
	})

	// The engine must serve reads cleanly after the build — proof it was never
	// wedged or permanently starved.
	res, err := eng.Run(ctx, "MATCH (n:P) RETURN count(n)", nil)
	if err != nil {
		t.Fatalf("engine wedged after the parallel backfill: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("post-backfill read drain failed: %v", err)
	}
	_ = res.Close()
	// Informational only — interleaving is statistical and deliberately NOT gated.
	t.Logf("readers completed %d count-queries during the parallel backfill (informational; not asserted)", readsDuringBuild)
}
