package lpg

// reentrancy_queued_writer_test.go — task #1355 (reliability audit 2026-06-10)
//
// Gate: the re-entrancy guard must NOT be defeated by a writer QUEUED on
// visMu. Before the fix, enterWriter stamped writerGID BEFORE acquiring
// visMu.Lock, so a second writer G2 queuing behind the active writer G1
// overwrote writerGID with gid(G2) while G1 still held the lock. G1's nested
// View then saw writerGID != gid(G1), sailed past the guard into the
// non-re-entrant RWMutex, and deadlocked the engine permanently — the exact
// silent hang the guard (#1286) promises to convert into a panic. exitWriter's
// unconditional Store(0) likewise erased the OTHER writer's stamp.
//
// The fix stamps writerGID only AFTER visMu.Lock succeeds and clears it (CAS
// on its own gid) BEFORE visMu.Unlock, so writerGID is exactly "the goroutine
// currently holding visMu in write mode" and a queued writer can never clobber
// it. The queued writer needs no registration of its own: between its entry
// check and blocking on Lock it executes no user code, and while blocked it
// cannot call anything, so no same-goroutine nested acquisition can originate
// from it.
//
// Layer: short. Race-clean.

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// waitUntilQueuedOnWriteLock blocks until the goroutine with id gid is inside
// sync.(*RWMutex).Lock beneath Graph.ApplyAtomically — i.e. it is queued on
// visMu behind the caller, which holds the write lock. Detection is a bounded
// poll on observable runtime state (the goroutine's own stack via
// runtime.Stack), not sleep-as-synchronisation: once the frames appear the
// condition is stable until the caller releases visMu, because the lock
// holder is the caller itself. On timeout it records the failure (Errorf is
// safe off the test goroutine) and panics with a distinct message so the
// caller's watchdog recover reports a clear mismatch instead of letting the
// test proceed from a bogus precondition.
func waitUntilQueuedOnWriteLock(t *testing.T, gid int64) {
	t.Helper()
	prefix := fmt.Sprintf("goroutine %d ", gid)
	deadline := time.Now().Add(reentrancyWatchdog)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		for n == len(buf) {
			buf = make([]byte, 2*len(buf))
			n = runtime.Stack(buf, true)
		}
		for _, seg := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.HasPrefix(seg, prefix) &&
				strings.Contains(seg, "sync.(*RWMutex).Lock") &&
				strings.Contains(seg, "ApplyAtomically") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Errorf("writer goroutine %d did not queue on visMu within %s", gid, reentrancyWatchdog)
	panic("test: queued-writer precondition not reached")
}

// TestReentrancyGuard_NotDefeatedByQueuedWriter is the #1355 regression gate.
// Interleaving (channel-synchronised, deterministic):
//
//	G1 enters ApplyAtomically and holds visMu;
//	G2 enters ApplyAtomically and is verified QUEUED on visMu (stack poll);
//	G1 nests View.
//
// Contract: G1's nested View must PANIC with the documented guard message
// (writer→reader re-entrancy) within the watchdog. Before the fix it
// deadlocked instead — G2's pre-lock stamp had overwritten writerGID, so the
// guard no longer recognised G1 as the in-barrier writer. After the panic the
// queued writer G2 must complete normally (its transaction commits) and the
// graph must remain fully usable: the unwind released visMu and cleared only
// G1's own stamp, never G2's.
func TestReentrancyGuard_NotDefeatedByQueuedWriter(t *testing.T) {
	t.Parallel()
	g := newReentrancyGraph(t)

	g2gid := make(chan int64, 1)  // G2's goroutine id, sent before it queues
	g2go := make(chan struct{})   // closed by G1 (visMu held) to release G2
	g2done := make(chan error, 1) // G2's ApplyAtomically result

	go func() { // G2: the innocent queued writer
		g2gid <- goID()
		<-g2go
		g2done <- g.ApplyAtomically(func() error {
			return g.SetNodeLabel("u", "Queued")
		})
	}()

	recovered := runWithWatchdog(t, func() { // body runs as G1
		_ = g.ApplyAtomically(func() error {
			gid2 := <-g2gid
			close(g2go) // visMu is held: G2 must queue behind this writer
			waitUntilQueuedOnWriteLock(t, gid2)
			// Pre-fix: writerGID holds gid(G2) here, so this nested View
			// walks past the guard into RLock and deadlocks (the watchdog
			// fires). Post-fix: the guard panics, as #1286 documents.
			g.View(func() {
				t.Errorf("nested View body must never run")
			})
			return nil
		})
	})
	assertGuardPanic(t, recovered, "View", "ApplyAtomically")

	// The queued writer must be unharmed: G1's unwind released visMu, so G2
	// acquires it and commits normally.
	select {
	case err := <-g2done:
		if err != nil {
			t.Fatalf("queued writer failed after the guard panic: %v", err)
		}
	case <-time.After(reentrancyWatchdog):
		t.Fatalf("queued writer did not complete within %s after the guard panic", reentrancyWatchdog)
	}
	if !g.HasNodeLabel("u", "Queued") {
		t.Fatalf("queued writer's transaction was lost")
	}

	// The graph remains fully usable for both roles from fresh acquisitions.
	ran := false
	if err := g.ApplyAtomically(func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("post-panic ApplyAtomically did not run cleanly (ran=%v err=%v)", ran, err)
	}
	g.View(func() { _ = g.LiveOrder() })
}

// BenchmarkBarrier_View measures the read-path cost of the visibility barrier
// (guard check + RLock/RUnlock bookkeeping) with an empty body — the hot path
// that #1355 must not regress: the guard's read-side check stays one atomic
// load.
func BenchmarkBarrier_View(b *testing.B) {
	g := New[string, int64](adjlist.Config{Directed: true})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.View(func() {})
	}
}

// BenchmarkBarrier_ApplyAtomically measures the write-path cost of the
// visibility barrier (guard bookkeeping + Lock/Unlock) with an empty
// transaction.
func BenchmarkBarrier_ApplyAtomically(b *testing.B) {
	g := New[string, int64](adjlist.Config{Directed: true})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.ApplyAtomically(func() error { return nil })
	}
}
