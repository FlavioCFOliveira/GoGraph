package lpg

// reentrancy_test.go — task #1286
//
// The transaction-visibility barrier (Graph.View / Graph.ApplyAtomically) is
// backed by a NON-re-entrant sync.RWMutex. A goroutine that already holds the
// barrier and nests another acquisition deadlocks the whole engine. Production
// never nests today, but the invariant was unenforced. The guard added in
// reentrancy.go converts that silent hang into an immediate, clear panic.
//
// These tests prove:
//  1. each of the four deadlock-prone nestings (reader→reader, reader→writer,
//     writer→reader, writer→writer) PANICS with the guard message within a
//     watchdog timeout instead of hanging — and, for the reader-nested cases, a
//     real writer is parked on ApplyAtomically so the RWMutex nesting is
//     genuinely deadlock-prone (a nested RLock would block behind the queued
//     writer, a nested Lock would block behind itself);
//  2. legitimate non-nested and CONCURRENT different-goroutine use produces NO
//     false-positive panic, under -race.
//
// Layer: short. Race-clean.

import (
	"runtime"
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

// queueWriter starts a goroutine that calls ApplyAtomically (visMu.Lock) so it
// QUEUES behind an outer reader that already holds visMu.RLock. With a writer
// queued, Go's RWMutex stops admitting new readers (writer-starvation
// avoidance), so a nested View RLock from the outer reader would block behind
// the queued writer forever — making the reader→reader nesting genuinely
// deadlock-prone. queueWriter must be called from INSIDE the outer View, after
// its RLock is held.
//
// It returns only settle, which yields the scheduler a few times so the writer
// goroutine reaches its blocked-on-Lock state before the caller attempts the
// nested acquisition. settle is best-effort (the guard fires deterministically
// regardless, so the test never depends on exact timing — settle only makes the
// "would otherwise deadlock" condition real).
//
// Letting the writer proceed and joining it is deferred to t.Cleanup, which runs
// AFTER the test body has fully unwound and the outer View has released its
// RLock — so the queued writer can finally acquire Lock and exit. Closing the
// release channel from inside the still-RLock-holding View closure would
// deadlock (the join would wait for a writer that cannot acquire Lock until
// RUnlock, which runs later), hence the cleanup-time join.
func queueWriter(t *testing.T, g *Graph[string, int64]) (settle func()) {
	t.Helper()
	started := make(chan struct{}) // closed just before the writer blocks on Lock
	hold := make(chan struct{})    // closed at cleanup to let the writer finish
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		_ = g.ApplyAtomically(func() error {
			<-hold
			return nil
		})
	}()
	settle = func() {
		<-started
		// Yield so the writer goroutine advances from "started" into the blocked
		// ApplyAtomically -> visMu.Lock wait, queuing behind the outer RLock.
		for i := 0; i < 100; i++ {
			runtime.Gosched()
		}
	}
	t.Cleanup(func() {
		close(hold)
		wg.Wait()
	})
	return settle
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

// TestReentrancyGuard_NestedViewInView_PanicsNotHangs covers reader→reader:
// inside g.View (RLock held), a writer is queued on ApplyAtomically (visMu.Lock)
// so a nested RLock would block behind it and deadlock; the nested g.View must
// instead panic with the guard message within the watchdog rather than hanging.
func TestReentrancyGuard_NestedViewInView_PanicsNotHangs(t *testing.T) {
	t.Parallel()
	g := newReentrancyGraph(t)

	recovered := runWithWatchdog(t, func() {
		g.View(func() {
			// The outer RLock is now held. Queue a writer behind it so the
			// nested RLock below is genuinely deadlock-prone (a queued writer
			// blocks new readers), then attempt the nested View — the guard must
			// panic before the deadlock can form. The queued writer is released
			// and joined at t.Cleanup, after this View has unwound.
			settle := queueWriter(t, g)
			settle()
			g.View(func() {
				t.Errorf("nested View body must never run")
			})
		})
	})
	assertGuardPanic(t, recovered, "View", "View")
}

// TestReentrancyGuard_NestedApplyInView_PanicsNotHangs covers reader→writer:
// inside g.View, a nested g.ApplyAtomically (which would self-deadlock waiting
// for the in-flight read lock to release) panics with the guard message within
// the watchdog rather than hanging. No external writer is needed: reader→writer
// always deadlocks.
func TestReentrancyGuard_NestedApplyInView_PanicsNotHangs(t *testing.T) {
	t.Parallel()
	g := newReentrancyGraph(t)

	recovered := runWithWatchdog(t, func() {
		g.View(func() {
			_ = g.ApplyAtomically(func() error {
				t.Errorf("nested ApplyAtomically body must never run")
				return nil
			})
		})
	})
	assertGuardPanic(t, recovered, "ApplyAtomically", "View")
}

// TestReentrancyGuard_NestedViewInApply_PanicsNotHangs covers writer→reader:
// inside g.ApplyAtomically (holding visMu.Lock), a nested g.View (which would
// self-deadlock waiting for the write lock to release) panics with the guard
// message within the watchdog rather than hanging.
func TestReentrancyGuard_NestedViewInApply_PanicsNotHangs(t *testing.T) {
	t.Parallel()
	g := newReentrancyGraph(t)

	recovered := runWithWatchdog(t, func() {
		_ = g.ApplyAtomically(func() error {
			g.View(func() {
				t.Errorf("nested View body must never run")
			})
			return nil
		})
	})
	assertGuardPanic(t, recovered, "View", "ApplyAtomically")
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
	g.View(func() { _ = g.LiveOrder() })
}

// TestReentrancyGuard_NoFalsePositive_ConcurrentReadersAndWriter is the
// regression/sanity test: many concurrent DIFFERENT-goroutine View readers plus
// a serialised ApplyAtomically writer run cleanly, with no false-positive panic,
// under -race. Each goroutine also runs many sequential (non-nested) barrier
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
					_ = g.ApplyAtomically(func() error {
						_ = g.SetNodeLabel("u", "Hot")
						return nil
					})
				})
			}
		}()
	}

	// Readers: concurrent View, each a fresh non-nested acquisition; loop until
	// every writer finishes so reads and writes overlap for the whole run.
	readersWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readersWG.Done()
			for !stop.Load() {
				guarded(func() {
					g.View(func() {
						_ = g.LiveOrder()
						_ = g.HasNodeLabel("u", "Hot")
					})
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
