//go:build !race && !gograph_debug

package lpg

// reentrancy_disabled_test.go — pins the PRODUCTION contract of the barrier
// re-entrancy guard (rmp #2168): without -race and without -tags gograph_debug
// the guard is compiled out entirely, so [Graph.View] costs exactly its
// RWMutex pair and allocates nothing.
//
// The enforcing behaviour — a nested acquisition panicking with an explanation
// instead of deadlocking — is pinned by reentrancy_test.go and
// reentrancy_queued_writer_test.go, which carry the opposite build tag and run
// in the local gate (`go test -race ./...`).
//
// Layer: short. Race-clean by construction (this file is not compiled under
// -race).

import (
	"sync"
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestBarrierGuard_ZeroSizedInProductionBuild pins the claim that a released
// Graph carries none of the guard's state. The enforcing form holds a
// map[int64]int, a sync.Mutex and an atomic.Int64; this form must hold nothing,
// so the guard costs no memory per Graph as well as no time per call.
func TestBarrierGuard_ZeroSizedInProductionBuild(t *testing.T) {
	t.Parallel()
	if got := unsafe.Sizeof(barrierGuard{}); got != 0 {
		t.Fatalf("unsafe.Sizeof(barrierGuard{}) = %d, want 0 in a non-race, non-debug build", got)
	}
}

// TestBarrierGuard_ViewAllocatesNothing pins acceptance criterion (1) of #2168:
// Graph.View must allocate zero bytes in a released build. Under the enforcing
// guard every call allocated 64 bytes for the runtime.Stack buffer that goID
// parses, on a path taken once per query.
//
// testing.AllocsPerRun is used rather than a benchmark so the property is
// asserted by the test suite, not merely observed by whoever runs -bench.
func TestBarrierGuard_ViewAllocatesNothing(t *testing.T) {
	// Not parallel: AllocsPerRun sets GOMAXPROCS to 1 for its duration and is
	// documented as unreliable when other goroutines are allocating.
	g := New[string, int64](adjlist.Config{Directed: true})

	// Warm up so first-call lazy initialisation is not counted.
	g.View(func() {})

	if got := testing.AllocsPerRun(200, func() { g.View(func() {}) }); got != 0 {
		t.Fatalf("Graph.View allocated %v objects per call, want 0 in a non-race, non-debug build", got)
	}
}

// TestBarrierGuard_ApplyAtomicallyAllocatesNothing is the write-side companion:
// the guard's writer stamp and clear must cost nothing either.
func TestBarrierGuard_ApplyAtomicallyAllocatesNothing(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	_ = g.ApplyAtomically(func() error { return nil })

	got := testing.AllocsPerRun(200, func() {
		_ = g.ApplyAtomically(func() error { return nil })
	})
	if got != 0 {
		t.Fatalf("Graph.ApplyAtomically allocated %v objects per call, want 0 in a non-race, non-debug build", got)
	}
}

// TestBarrierGuard_ConcurrentReadersAndWriterUnaffected keeps the concurrency
// coverage that reentrancy_test.go provides under -race, where its
// no-false-positive assertion lives: many concurrent View readers alongside an
// ApplyAtomically writer must all complete, in this build as in that one.
// Removing the guard must not have changed how visMu admits them.
func TestBarrierGuard_ConcurrentReadersAndWriterUnaffected(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	const readers = 32
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(readers + 1)

	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				g.View(func() { _ = g.LiveOrder() })
			}
		}()
	}
	go func() {
		defer wg.Done()
		for j := 0; j < iterations; j++ {
			if err := g.ApplyAtomically(func() error {
				return g.SetNodeProperty("a", "v", Int64Value(int64(j)))
			}); err != nil {
				t.Errorf("ApplyAtomically: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

// TestBarrierGuard_NestedViewDoesNotPanicInProductionBuild pins the contract
// this build deliberately gives up, exactly as
// search/overflow_assert_production_test.go pins its own: without enforcement a
// nested acquisition is NOT reported.
//
// The nesting below is safe to execute only because there is no concurrent
// writer on this graph: Go's RWMutex admits a recursive RLock while no writer is
// queued. That is precisely why the invariant needs enforcing rather than
// testing — with a writer in flight the same code deadlocks the engine, which is
// what the enforcing build panics on and what the public godoc forbids. The test
// therefore asserts only that production stays panic-free; it does not endorse
// the pattern.
func TestBarrierGuard_NestedViewDoesNotPanicInProductionBuild(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nested View panicked in a non-race, non-debug build: %v", r)
		}
	}()

	inner := false
	g.View(func() {
		g.View(func() { inner = true })
	})
	if !inner {
		t.Fatal("nested View body did not run")
	}
}
