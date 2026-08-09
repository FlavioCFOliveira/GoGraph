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
				_ = g.LiveOrder()
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
