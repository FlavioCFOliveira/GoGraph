package ctxlock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// waitFree blocks until the lock is free, so a test never finishes while the
// helper goroutine ctxlock left behind is still queued. Without this the
// goroutine would still be running at package teardown.
func waitFree(t *testing.T, tryLock func() bool, unlock func()) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tryLock() {
			unlock()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("lock never became free; the abandoned acquire did not release it")
}

// TestAcquire_UncontendedTakesFastPath verifies the common case needs no helper
// goroutine: a free lock is taken by tryLock alone.
func TestAcquire_UncontendedTakesFastPath(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	if err := Acquire(context.Background(), mu.TryLock, mu.Lock, mu.Unlock); err != nil {
		t.Fatalf("Acquire on a free lock: %v", err)
	}
	if mu.TryLock() {
		mu.Unlock()
		t.Fatal("Acquire returned nil but the lock is not held")
	}
	mu.Unlock()
}

// TestAcquire_ExpiredContextTakesNothing pins that an already-finished context
// never acquires. This is the property that stops a caller receiving a resource
// it is no longer entitled to.
func TestAcquire_ExpiredContextTakesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ctx  func() context.Context
		want error
	}{
		{"cancelled", func() context.Context {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			return c
		}, context.Canceled},
		{"deadline elapsed", func() context.Context {
			c, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return c
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			err := Acquire(tc.ctx(), mu.TryLock, mu.Lock, mu.Unlock)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Acquire = %v, want %v", err, tc.want)
			}
			if !mu.TryLock() {
				t.Fatal("Acquire returned an error but the lock is held")
			}
			mu.Unlock()
		})
	}
}

// TestAcquire_ContendedHonoursDeadline is the core property: when the lock is
// held by someone else for far longer than the caller's deadline, Acquire
// returns at the deadline rather than at the holder's release.
func TestAcquire_ContendedHonoursDeadline(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	mu.Lock()

	const hold = 2 * time.Second
	const budget = 50 * time.Millisecond
	released := make(chan struct{})
	go func() {
		time.Sleep(hold)
		mu.Unlock()
		close(released)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	err := Acquire(ctx, mu.TryLock, mu.Lock, mu.Unlock)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire = %v, want context.DeadlineExceeded", err)
	}
	// The margin is generous because this asserts an ORDER OF MAGNITUDE, not a
	// scheduling guarantee: the point is that the caller returned near its own
	// deadline and nowhere near the holder's 2 s.
	if elapsed > budget+500*time.Millisecond {
		t.Fatalf("Acquire returned after %v, want close to the %v deadline", elapsed, budget)
	}
	if elapsed >= hold {
		t.Fatalf("Acquire waited %v — the full holder duration; the deadline was ignored", elapsed)
	}

	<-released
	waitFree(t, mu.TryLock, mu.Unlock)
}

// TestAcquire_AbandonedAcquireReleasesTheLock pins the contract that makes the
// abandoned wait safe: the helper left behind must release what it eventually
// acquires, or the lock would be held forever by nobody.
func TestAcquire_AbandonedAcquireReleasesTheLock(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	mu.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := Acquire(ctx, mu.TryLock, mu.Lock, mu.Unlock); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire = %v, want context.DeadlineExceeded", err)
	}

	// Release as the holder. The helper takes the lock next, then must free it.
	mu.Unlock()
	waitFree(t, mu.TryLock, mu.Unlock)
}

// TestAcquire_RWMutexExclusiveAgainstReaders covers the shape GoGraph actually
// uses: an exclusive acquire queued behind an in-flight reader. This is the
// visibility barrier's exact situation, where a Graph.View holds the read side
// for the length of a query.
func TestAcquire_RWMutexExclusiveAgainstReaders(t *testing.T) {
	t.Parallel()
	var mu sync.RWMutex
	mu.RLock()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Acquire(ctx, mu.TryLock, mu.Lock, mu.Unlock)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Acquire took %v; the reader should not have gated the deadline", elapsed)
	}

	mu.RUnlock()
	waitFree(t, mu.TryLock, mu.Unlock)
}

// TestAcquire_ExclusiveIsGenuinelyExclusive guards against an implementation
// that reports success without holding the lock: two concurrent Acquires must
// never both succeed at once.
func TestAcquire_ExclusiveIsGenuinelyExclusive(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var inside, maxInside int
	var guard sync.Mutex
	var wg sync.WaitGroup

	const workers = 16
	const rounds = 50
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if err := Acquire(context.Background(), mu.TryLock, mu.Lock, mu.Unlock); err != nil {
					t.Errorf("Acquire with no deadline: %v", err)
					return
				}
				guard.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				guard.Unlock()

				guard.Lock()
				inside--
				guard.Unlock()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("observed %d concurrent holders, want 1", maxInside)
	}
}
