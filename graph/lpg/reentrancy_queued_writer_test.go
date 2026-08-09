//go:build race || gograph_debug

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
				strings.Contains(seg, "Gate).StrongLock") &&
				strings.Contains(seg, "ApplyAtomically") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Errorf("writer goroutine %d did not queue on visMu within %s", gid, reentrancyWatchdog)
	panic("test: queued-writer precondition not reached")
}

// BenchmarkBarrier_View and BenchmarkBarrier_ApplyAtomically moved to
// barrier_bench_test.go with rmp #2168, which build-tags this file out of
// released binaries. The benchmarks must run in BOTH builds — comparing them is
// the evidence that the guard is gone from the production path — so they cannot
// live behind the same tag as the guard's own tests.
