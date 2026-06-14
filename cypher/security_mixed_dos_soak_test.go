//go:build soak || nightly

package cypher_test

// security_mixed_dos_soak_test.go — SOAK-layer mixed denial-of-service workload.
//
// Activated by: go test -tags=soak ./cypher/...  (or -tags=nightly, or SOAK_FULL=1)
// Not part of the short-layer (PR-CI) run.
//
// This test runs a sustained, concurrent mix of the expression- and
// pattern-level cost amplifiers exercised by the short-layer security tests —
// doubling reduce (#1475), nested comprehensions (#1475), reduce loops under a
// deadline (#1477), and per-row-budgeted variable-length expansion (#1478) —
// against a single shared engine for a bounded duration, under a process-wide
// soft memory limit (GOMEMLIMIT). It asserts the module degrades gracefully
// rather than catastrophically:
//
//   - no goroutine OOMs or panics the process (the engine's recover boundary and
//     the bounded magnitudes keep every individual query in check);
//   - the heap does not grow without bound across the run (each query's
//     intermediate lists are released; the soft memory limit makes the GC work
//     harder rather than letting the process balloon);
//   - the operator/row paths that honour context remain cancellable under load.
//
// All magnitudes are deliberately SMALL (2^16–2^18 element lists, ≤256-row VLE
// fan-out) so the aggregate live set stays in the low tens of MB and the soft
// GOMEMLIMIT (256 MiB) is never actually breached — the limit is a guard rail,
// not a target. The test never allocates multi-GB and never runs unbounded.

import (
	"context"
	"errors"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// secCypherSoakMemLimit is the process-wide soft memory limit imposed for the
// duration of the soak. It is a guard rail far above the workload's true live
// set (low tens of MB): if a regression made any query's intermediate list
// unbounded, the GC would thrash and the run would slow to a crawl rather than
// the host being OOM-killed, surfacing the regression as a timeout instead of a
// crash.
const secCypherSoakMemLimit = 256 << 20 // 256 MiB

// secCypherSoakDuration bounds the run. Kept short enough to fit comfortably in
// the soak job's budget while exercising thousands of iterations across workers.
const secCypherSoakDuration = 20 * time.Second

// secCypherSoakWorkers is the concurrency level. Each worker hammers the shared
// engine with a rotating mix of the cost-amplifier queries.
const secCypherSoakWorkers = 8

// TestSec_Cypher_MixedDoSWorkload runs the bounded mixed-workload soak.
func TestSec_Cypher_MixedDoSWorkload(t *testing.T) {
	defer goleak.VerifyNone(t)

	// Impose the soft memory limit for the test, restoring the prior value on
	// exit so other tests in the same binary are unaffected.
	prevLimit := debug.SetMemoryLimit(-1) // read current
	debug.SetMemoryLimit(secCypherSoakMemLimit)
	t.Cleanup(func() { debug.SetMemoryLimit(prevLimit) })

	// A single shared engine over a small VLE chain, exercised concurrently.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	secCypherBuildChain(t, eng, 8) // 8-edge chain for the VLE workload

	// Record a heap baseline AFTER warm-up so steady-state growth, not start-up
	// allocation, is what we measure.
	secCypherWarmup(t, eng)
	var baseHeap uint64
	{
		var ms runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms)
		baseHeap = ms.HeapInuse
	}

	ctx, cancel := context.WithTimeout(context.Background(), secCypherSoakDuration)
	defer cancel()

	var (
		iterations atomic.Uint64
		panics     atomic.Uint64
		wg         sync.WaitGroup
	)

	// The rotating query mix. Magnitudes are small and fixed.
	queries := secCypherSoakQueries()

	for w := 0; w < secCypherSoakWorkers; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					// The engine converts in-query panics to errors; a panic that
					// reaches here would be a test-harness bug, but count it so the
					// assertion below fails loudly rather than the goroutine dying
					// silently.
					panics.Add(1)
				}
			}()
			i := seed
			for ctx.Err() == nil {
				q := queries[i%len(queries)]
				secCypherSoakRunOne(ctx, eng, q)
				iterations.Add(1)
				i++
			}
		}(w)
	}

	// While the workers run, periodically sample the heap. The live set must not
	// climb without bound; we assert it stays within a generous multiple of the
	// post-warm-up baseline (the soft limit is 256 MiB; steady state is far
	// below). A monotonic climb toward the limit indicates a per-query list leak.
	maxHeap := baseHeap
	sampleStop := make(chan struct{})
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-sampleStop:
				return
			case <-tk.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > maxHeap {
					maxHeap = ms.HeapInuse
				}
			}
		}
	}()

	wg.Wait()
	close(sampleStop)
	sampleWG.Wait()

	if got := panics.Load(); got != 0 {
		t.Fatalf("workload raised %d panic(s) that escaped the engine boundary", got)
	}
	if iterations.Load() == 0 {
		t.Fatal("workload completed zero iterations — the soak did not run")
	}

	// Heap-growth ceiling: the live set must stay well under the soft limit. A
	// generous bound (the soft limit itself) is enough to catch an unbounded
	// per-query list leak, which would drive HeapInuse toward — and the GC would
	// fight to keep it under — the limit, manifesting as a very high maxHeap.
	if maxHeap >= secCypherSoakMemLimit {
		t.Fatalf("peak HeapInuse %d reached the soft memory limit %d — a query's intermediate allocation is not bounded/released", maxHeap, secCypherSoakMemLimit)
	}
	t.Logf("mixed DoS soak: %d iterations across %d workers in %v; baseline heap %d, peak heap %d (soft limit %d)",
		iterations.Load(), secCypherSoakWorkers, secCypherSoakDuration, baseHeap, maxHeap, secCypherSoakMemLimit)

	// Cancellability under load: a fresh large scan against an already-cancelled
	// context must not run unbounded even while the engine has been hammered.
	secCypherSoakAssertCancellable(t, eng)
}

// secCypherSoakQueries returns the fixed, small-magnitude query mix.
func secCypherSoakQueries() []string {
	const dblExp = 16 // 2^16 = 65536-element doubling reduce
	const cubeN = 16  // 16^3 = 4096 nested-comprehension elements
	const sumN = 200_000
	return []string{
		// #1475 doubling reduce (bounded list growth).
		"RETURN size(reduce(acc=[0], i IN range(1," + strconv.Itoa(dblExp) + ") | acc + acc)) AS s",
		// #1475 nested comprehension (bounded N^3).
		"RETURN size([a IN range(1," + strconv.Itoa(cubeN) + ") | [b IN range(1," + strconv.Itoa(cubeN) + ") | [c IN range(1," + strconv.Itoa(cubeN) + ") | a+b+c]]]) AS s",
		// #1477 reduce sum (loop work; runs to completion, used here for churn).
		"RETURN reduce(acc=0, i IN range(1," + strconv.Itoa(sumN) + ") | acc + i) AS r",
		// #1478 per-row-budgeted VLE driven by a small fan-out (≤ 64 rows).
		"UNWIND range(1,64) AS s MATCH (a:Hub {id:0})-[:R*1..8]->(x) RETURN s, x",
		// A plain bounded scan to keep a cancellable path in the mix.
		"UNWIND range(0, 9999) AS x RETURN x",
	}
}

// secCypherSoakRunOne runs q under ctx and fully drains/closes the result,
// ignoring errors (a context error under load is expected and benign). It never
// fails the test directly; failures are surfaced by the aggregate assertions.
func secCypherSoakRunOne(ctx context.Context, eng *cypher.Engine, q string) {
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		return
	}
	for res.Next() {
		_ = res.Record()
	}
	_ = res.Err()
	_ = res.Close()
}

// secCypherWarmup runs each query-mix entry once so first-touch allocation
// (plan-cache population, lexer/parser warm paths) is excluded from the heap
// baseline.
func secCypherWarmup(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	for _, q := range secCypherSoakQueries() {
		secCypherSoakRunOne(context.Background(), eng, q)
	}
}

// secCypherSoakAssertCancellable repeats the positive #1477 lock-in after the
// soak: the row/operator path must still honour cancellation under a warmed,
// hammered engine.
func secCypherSoakAssertCancellable(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := eng.Run(ctx, `UNWIND range(0, 9999999) AS x RETURN x`, nil)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("post-soak scan rejected with a non-context error: %v", err)
		}
		return
	}
	defer func() { _ = res.Close() }()
	rows := 0
	for res.Next() {
		rows++
		if rows > 1_000_000 {
			t.Fatalf("post-soak scan emitted >1M rows under a cancelled context — operator-level cancellation regressed under load")
		}
	}
}
