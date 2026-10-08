//go:build race || gograph_debug

package lpg

// reentrancy_test.go — task #1286
//
// The schema barrier ([Graph.ApplyAtomically], the strong side of the graph's
// visGate, an [mvcc.Gate]) is NOT re-entrant. A goroutine that already holds it
// and nests another acquisition deadlocks the whole engine. Production never
// nests today, but the invariant was unenforced. The guard added in
// reentrancy_enabled.go converts that silent hang into an immediate, clear panic.
// Its reader half, and the nestings involving Graph.View, were removed with
// Graph.View by rmp #2344.
//
// These tests prove:
//  1. the writer→writer nesting PANICS with the guard message within a watchdog
//     timeout instead of hanging, and a panic inside fn clears the writer mark;
//  2. legitimate non-nested and CONCURRENT different-goroutine use produces NO
//     false-positive panic, under -race.
//
// Layer: short. Race-clean.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

const reentrancyWatchdog = 2 * time.Second

// runWithWatchdog runs body in a goroutine and recovers any panic it raises,
// returning the recovered value (or nil) once body completes. If body has not
// returned within reentrancyWatchdog it is assumed to have DEADLOCKED and the
// test fails fast — the whole point of the guard is that the nested call must
// not hang. The leaked goroutine is acceptable: a genuine deadlock here means
// the guard regressed, the test has already failed, and the process is exiting.
func runWithWatchdog(t *testing.T, body func()) (recovered any) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer func() {
			recovered = recover()
			close(done)
		}()
		body()
	}()
	select {
	case <-done:
		return recovered
	case <-time.After(reentrancyWatchdog):
		t.Fatalf("nested barrier acquisition deadlocked: no panic within %s "+
			"(the re-entrancy guard failed to trip)", reentrancyWatchdog)
		return nil
	}
}

func newReentrancyGraph(t *testing.T) *Graph[string, int64] {
	t.Helper()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("u"); err != nil {
		t.Fatalf("AddNode u: %v", err)
	}
	if err := g.AddNode("v"); err != nil {
		t.Fatalf("AddNode v: %v", err)
	}
	return g
}

// assertGuardPanic asserts that recovered is the re-entrancy guard panic and
// that its message names the expected nested and held methods.
func assertGuardPanic(t *testing.T, recovered any, wantNested, wantHeld string) {
	t.Helper()
	if recovered == nil {
		t.Fatalf("expected a re-entrancy panic, got none")
	}
	msg, ok := recovered.(string)
	if !ok {
		t.Fatalf("expected a string panic value, got %T: %v", recovered, recovered)
	}
	want := reentrancyMessage(wantNested, wantHeld)
	if msg != want {
		t.Fatalf("guard panic message mismatch\n got: %q\nwant: %q", msg, want)
	}
}

// TestReentrancyGuard_NestedApplyInApply_PanicsNotHangs covers writer→writer:
// inside g.ApplyAtomically, a nested g.ApplyAtomically (which would self-deadlock
// on the write lock it already holds) panics with the guard message within the
// watchdog rather than hanging.
func TestReentrancyGuard_NestedApplyInApply_PanicsNotHangs(t *testing.T) {
	t.Parallel()
	g := newReentrancyGraph(t)

	recovered := runWithWatchdog(t, func() {
		_ = g.ApplyAtomically(func() error {
			return g.ApplyAtomically(func() error {
				t.Errorf("nested ApplyAtomically body must never run")
				return nil
			})
		})
	})
	assertGuardPanic(t, recovered, "ApplyAtomically", "ApplyAtomically")
}

// TestReentrancyGuard_PanicInFnClearsWriterMark proves the deferred exit runs
// even when fn panics: after a panicking ApplyAtomically, the SAME goroutine can
// enter the barrier again with no spurious re-entrancy panic — i.e. the writer
// mark is not stranded by the unwind.
func TestReentrancyGuard_PanicInFnClearsWriterMark(t *testing.T) {
	t.Parallel()
	g := newReentrancyGraph(t)

	func() {
		defer func() { _ = recover() }()
		_ = g.ApplyAtomically(func() error {
			panic("boom inside fn")
		})
	}()

	// The mark must have been cleared on the panic unwind; a fresh acquisition
	// on the same goroutine must not be mistaken for re-entry.
	ran := false
	_ = g.ApplyAtomically(func() error {
		ran = true
		return nil
	})
	if !ran {
		t.Fatalf("post-panic ApplyAtomically did not run")
	}
	// Likewise for View.
	_ = g.LiveOrder()
}

// TestReentrancyGuard_NoFalsePositive_ConcurrentReadersAndWriter is the
// regression/sanity test: many concurrent DIFFERENT-goroutine lock-free readers
// plus serialised ApplyAtomically writers run cleanly, with no false-positive
// panic, under -race. Each writer also runs many sequential (non-nested) barrier
// acquisitions to prove the per-acquisition enter/exit bookkeeping never strands
// a goroutine id across calls.
func TestReentrancyGuard_NoFalsePositive_ConcurrentReadersAndWriter(t *testing.T) {
	t.Parallel()
	g := newReentrancyGraph(t)

	const (
		readers    = 16
		writers    = 4
		iterations = 2000
	)
	var (
		writersWG sync.WaitGroup
		readersWG sync.WaitGroup
		stop      atomic.Bool
		panics    atomic.Int64
	)

	guarded := func(body func()) {
		defer func() {
			if r := recover(); r != nil {
				panics.Add(1)
				t.Errorf("unexpected guard panic on legitimate use: %v", r)
			}
		}()
		body()
	}

	// Writers: serialised ApplyAtomically, each a fresh non-nested acquisition.
	writersWG.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer writersWG.Done()
			for i := 0; i < iterations; i++ {
				guarded(func() {
					_ = g.ApplyAtomicallyTx(func(tx WriteTx) error {
						_ = g.Writer(tx).SetNodeLabel("u", "Hot")
						return nil
					})
				})
			}
		}()
	}

	// Readers: concurrent lock-free reads; loop until every writer finishes so
	// reads and writes overlap for the whole run.
	readersWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readersWG.Done()
			for !stop.Load() {
				guarded(func() {
					_ = g.LiveOrder()
					_ = g.HasNodeLabel("u", "Hot")
				})
			}
		}()
	}

	// Join the writers first, then signal the readers to stop and join them, so
	// reads and the serialised writer overlap for the whole writer run.
	writersWG.Wait()
	stop.Store(true)
	readersWG.Wait()

	if got := panics.Load(); got != 0 {
		t.Fatalf("legitimate concurrent use produced %d false-positive panic(s)", got)
	}
}
