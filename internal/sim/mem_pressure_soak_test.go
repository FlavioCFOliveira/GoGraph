package sim

import (
	"context"
	"math"
	"runtime/debug"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestMemPressure_Soak is the soak-layer companion to the deterministic
// mem-pressure scenario: it imposes a real heap ceiling with
// debug.SetMemoryLimit and drives an overload-heavy concurrent workload over the
// Bolt wire, asserting the engine degrades gracefully under genuine GC pressure
// rather than panicking, deadlocking, or leaking. Unlike the deterministic
// variant (which clamps logical row/collect budgets and is bit-reproducible),
// this exercises the real allocate-and-collect path under contention and is
// therefore NOT bit-reproducible — it is convergence/leak-guarded.
//
// It is soak-gated: the tight memory limit forces aggressive, minutes-long GC
// under the concurrent overload mix.
func TestMemPressure_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	// Impose a soft heap ceiling for the duration. A soft limit makes the GC far
	// more aggressive as the heap approaches it (the realistic "memory pressure"
	// signal) without itself OOM-killing the process; the engine's own bounded
	// result caps remain the hard backstop. Restore the (effectively unlimited)
	// default afterwards so later tests on the shared runtime are unaffected.
	const heapCeiling = 192 << 20 // 192 MiB
	prev := debug.SetMemoryLimit(heapCeiling)
	defer debug.SetMemoryLimit(prev)
	// SetMemoryLimit returns the previous limit; guard against a env-provided
	// GOMEMLIMIT leaving prev at a small value by restoring to unlimited if so.
	if prev <= 0 {
		defer debug.SetMemoryLimit(math.MaxInt64)
	}

	seeds := []uint64{0x3E30, 0x3E31, 0x3E32}
	for _, seed := range seeds {
		seed := seed
		t.Run(seedName(seed), func(t *testing.T) {
			srv, err := NewSimServer(SimEngineForServer(), clock.Real())
			if err != nil {
				t.Fatalf("NewSimServer: %v", err)
			}
			defer func() { _ = srv.Close() }()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			// Overload-heavy mix under the heap ceiling: most connections push the
			// engine's bounds (giant UNWIND / Cartesian / large CREATE / deep VLE)
			// while honest writers keep real state. Every over-budget op must come
			// back as a typed bounded error, never a panic or a wedged connection.
			res, err := RunConcurrent(ctx, srv, ConcurrentConfig{
				Seed:        seed,
				Connections: 64,
				OpsPerConn:  100,
				Mix:         &ConcurrentMix{WriterWeight: 0.25, ReaderWeight: 0.15, OverloadWeight: 0.6},
			})
			if err != nil {
				t.Fatalf("seed %#x concurrent run under memory limit: %v", seed, err)
			}
			if res.Panics != 0 {
				t.Fatalf("seed %#x: %d panics under memory pressure (must degrade, never panic)", seed, res.Panics)
			}
			if res.TransportErrors != 0 {
				t.Fatalf("seed %#x: %d transport errors under memory pressure (a wedged/broken connection)", seed, res.TransportErrors)
			}
			if !res.Consistent() {
				t.Fatalf("seed %#x inconsistent under memory pressure: engine=%d acked=%d",
					seed, res.EngineNodeCount, res.AckedCreates)
			}
		})
	}
}
