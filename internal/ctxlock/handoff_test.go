package ctxlock

// handoff_test.go — regression gate for rmp #2260.
//
// [Acquire] used to spawn TWO goroutines for every contended-and-abandoned
// acquire: one to block on the lock, and a second whose only job was to wait for
// the first and unlock. Measured against a barrier held for 3 s by acquirers with
// a 2 ms deadline, that reached 597 819 live goroutines and 1 677 MiB from 256
// callers — exactly 2x the number of ATTEMPTS, because the count tracks arrival
// rate times holder tenure rather than concurrency.
//
// The atomic three-state handoff folds the unlock into the single helper. These
// tests pin the two properties that change has to preserve:
//
//   - the per-abandoned-acquire helper count is ONE, not two;
//   - the lock is NEVER left held with no owner, in EITHER arm of the race
//     between the helper publishing and the caller's context firing.
//
// The second is the reason a plain boolean flag would not do. With a boolean the
// helper can read "not abandoned", publish, and have the caller simultaneously
// take the ctx.Done branch — after which the lock is held and nobody will ever
// release it. That is a permanent deadlock of the whole graph, so it is worth a
// dedicated adversarial test rather than a code comment.

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// waitForGoroutines polls until the goroutine count drops to at most want, or the
// deadline expires, and reports the last observed count. Polling rather than
// sleeping keeps the test fast when the helpers drain promptly.
func waitForGoroutines(want int, within time.Duration) int {
	deadline := time.Now().Add(within)
	n := runtime.NumGoroutine()
	for n > want && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// TestAcquire_AbandonedAcquireCostsOneHelper_2260 is the primary gate: N abandoned
// acquires against a held lock must leave N helpers parked, not 2N.
func TestAcquire_AbandonedAcquireCostsOneHelper_2260(t *testing.T) {
	const herd = 64

	var mu sync.Mutex
	mu.Lock() // hold it so every Acquire below is contended and must give up

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for range herd {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
			defer cancel()
			if err := Acquire(ctx, mu.TryLock, mu.Lock, mu.Unlock); err == nil {
				t.Error("Acquire succeeded against a held lock; the fixture is not contended")
			}
		}()
	}
	wg.Wait() // every caller has returned; only parked helpers remain

	// Each abandoned acquire parks exactly ONE helper. Allow generous slack for
	// the test's own goroutines, but 2x the herd (the old cost) must be excluded:
	// the midpoint between herd and 2*herd is the discriminating threshold.
	parked := runtime.NumGoroutine() - baseline
	if parked > herd+herd/2 {
		t.Errorf("after %d abandoned acquires, %d helper goroutines are parked; "+
			"want at most one per acquire (~%d). Two per acquire means the second "+
			"unlock-waiter goroutine is back", herd, parked, herd)
	}

	// Releasing the holder must let every helper finish, which is the liveness
	// half of the contract. goleak (main_test.go) would also catch a failure here,
	// but asserting it explicitly names the property.
	mu.Unlock()
	if n := waitForGoroutines(baseline+herd/4, 10*time.Second); n > baseline+herd/4 {
		t.Errorf("helpers did not drain after the holder released: %d goroutines, baseline %d", n, baseline)
	}
}

// TestAcquire_NeverLeavesLockHeldWithNoOwner_2260 drives the handoff race
// directly. The deadline is tuned to fire at roughly the same time the helper
// acquires, so across many iterations BOTH CAS arms are taken. After each
// iteration the lock must be free — verified by TryLock, which is the only
// observation that distinguishes "released" from "held by nobody".
func TestAcquire_NeverLeavesLockHeldWithNoOwner_2260(t *testing.T) {
	const iterations = 300

	var mu sync.Mutex
	for i := range iterations {
		// A holder that releases after a short, varying delay, so the helper's
		// acquisition lands near the caller's deadline.
		mu.Lock()
		release := time.Duration(i%5) * 100 * time.Microsecond
		go func() {
			time.Sleep(release)
			mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Microsecond)
		err := Acquire(ctx, mu.TryLock, mu.Lock, mu.Unlock)
		cancel()
		if err == nil {
			// We own it; release it as a normal caller would.
			mu.Unlock()
		}

		// Whichever arm ran, the lock must end up free. Poll briefly: when the
		// caller abandoned, the helper may not have reached its unlock yet.
		free := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if mu.TryLock() {
				mu.Unlock()
				free = true
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !free {
			t.Fatalf("iteration %d: the lock is still held and no goroutine will release it — "+
				"the handoff lost the race and left it owned by nobody (err=%v)", i, err)
		}
	}
}
