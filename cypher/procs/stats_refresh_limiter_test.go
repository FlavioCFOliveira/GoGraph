package procs

// stats_refresh_limiter_test.go — the rate limiter that makes db.stats.refresh() safe to
// expose to untrusted CALL (#2196).
//
// The rebuild is an O(nodes x properties) scan reachable by any client, so the bound is
// not a convenience — it is the reason the read-only procedure fence admits the procedure
// at all. These tests pin the two properties that matter: the window is enforced, and two
// simultaneous callers cannot both pass.
//
// The clock is injected so the tests are deterministic and instant; a sleeping test would
// be both slow and flaky.
//
// Layer: short.

import (
	"sync"
	"testing"
	"time"
)

func TestStatsRefreshLimiter_EnforcesWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	l := &statsRefreshLimiter{now: func() time.Time { return now }}

	// First call always passes: nothing has been stamped.
	if ok, wait := l.allow(); !ok {
		t.Fatalf("first call refused with wait %s; it must pass", wait)
	}

	// Inside the window: refused, with the remaining time reported so the client can be
	// told when to retry.
	now = now.Add(statsRefreshMinInterval / 2)
	ok, wait := l.allow()
	if ok {
		t.Fatal("a call halfway through the window was allowed")
	}
	if wantAbout := statsRefreshMinInterval / 2; wait > wantAbout+time.Second || wait <= 0 {
		t.Errorf("reported wait %s, want about %s", wait, wantAbout)
	}

	// Exactly at the boundary the window has elapsed, so the call passes.
	now = now.Add(statsRefreshMinInterval / 2)
	if ok, wait := l.allow(); !ok {
		t.Fatalf("a call exactly at the window boundary was refused with wait %s", wait)
	}

	// And that call re-stamped, so the next one is refused again.
	if ok, _ := l.allow(); ok {
		t.Fatal("the permitted call did not re-stamp the clock, so the window is not sliding")
	}
}

// TestStatsRefreshLimiter_OnlyOneConcurrentCallerPasses pins that the check-and-stamp is
// one critical section. If it were not, two clients arriving together would both be
// admitted and both start a full scan — exactly the amplification the limit exists to
// prevent, and the case a naive read-then-write implementation gets wrong.
func TestStatsRefreshLimiter_OnlyOneConcurrentCallerPasses(t *testing.T) {
	t.Parallel()
	frozen := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	l := &statsRefreshLimiter{now: func() time.Time { return frozen }}

	const callers = 64
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	start := make(chan struct{})
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			<-start // release everyone at once to maximise the race
			if ok, _ := l.allow(); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if allowed != 1 {
		t.Fatalf("%d of %d simultaneous callers were allowed; exactly 1 must pass, or a "+
			"burst of clients each triggers a full scan", allowed, callers)
	}
}
