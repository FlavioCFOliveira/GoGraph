package mvcc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGate_StrongExcludesWeak is the safety property, and its oracle is the RACE
// DETECTOR rather than an assertion.
//
// guarded is a PLAIN int with no synchronisation of its own. Weak holders read it,
// strong holders write it. If the gate ever lets a strong holder run beside a weak
// one, that is a write/read data race on guarded and `go test -race` reports it.
// An assertion-based oracle would only catch the overlaps it happened to sample;
// the race detector catches the first one.
//
// The counters are POSITIVE CONTROLS. A test whose only oracle is "nothing bad was
// observed" passes just as well when nothing happened at all, so it must also prove
// that both sides genuinely ran.
//
// NOTE ON THE ORACLE. It counts goroutines that are INSIDE their weak critical
// section — incremented after WeakLock returns, decremented before WeakUnlock — and
// deliberately NOT [Gate.WeakHolders]. WeakHolders counts raw slot claims, which
// includes an acquirer that has claimed a slot and is about to back out because it
// found a strong holder; such a goroutine never enters its critical section, so
// counting it reports a violation that did not happen. The first version of this
// test used WeakHolders and failed for exactly that reason.
func TestGate_StrongExcludesWeak(t *testing.T) {
	var g Gate
	var guarded int // deliberately unsynchronised; the gate is what protects it

	var insideWeak atomic.Int64 // goroutines actually inside a weak critical section
	var weakRuns, strongRuns atomic.Int64
	var wg sync.WaitGroup

	const weakGoroutines = 8
	const iterations = 2000

	stop := make(chan struct{})
	for i := 0; i < weakGoroutines; i++ {
		wg.Add(1)
		go func(hint uint64) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tok := g.WeakLock(hint)
				insideWeak.Add(1)
				_ = guarded // read under the weak hold
				weakRuns.Add(1)
				insideWeak.Add(-1)
				g.WeakUnlock(tok)
			}
		}(uint64(i))
	}

	for i := 0; i < iterations; i++ {
		g.StrongLock()
		guarded++ // write under the strong hold
		strongRuns.Add(1)
		// While strong is held, nobody may be inside a weak critical section.
		if n := insideWeak.Load(); n != 0 {
			g.StrongUnlock()
			close(stop)
			wg.Wait()
			t.Fatalf("strong holder ran beside %d weak holders; want 0", n)
		}
		g.StrongUnlock()
	}

	close(stop)
	wg.Wait()

	if got := strongRuns.Load(); got != iterations {
		t.Fatalf("strong sections ran %d times, want %d", got, iterations)
	}
	// Positive control: the weak side must actually have run, or the exclusion
	// above was never exercised and this test proves nothing.
	if got := weakRuns.Load(); got == 0 {
		t.Fatal("no weak section ever ran: the exclusion was never exercised, " +
			"so this test would pass against a gate that excludes nothing")
	}
	if guarded != iterations {
		t.Fatalf("guarded = %d, want %d", guarded, iterations)
	}
}

// TestGate_WeakDoesNotExcludeWeak proves the whole point of the gate: weak holders
// are concurrent with one another. A gate that serialised them would be correct but
// useless, and would pass every exclusion test above.
//
// It waits for the outcome with a DEADLINE rather than spinning a fixed number of
// times, so it neither flakes on a slow machine nor passes vacuously on a fast one.
func TestGate_WeakDoesNotExcludeWeak(t *testing.T) {
	var g Gate
	const want = 4

	var inside atomic.Int64
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	var wg sync.WaitGroup
	for i := 0; i < want; i++ {
		wg.Add(1)
		go func(hint uint64) {
			defer wg.Done()
			tok := g.WeakLock(hint)
			if inside.Add(1) >= want {
				once.Do(func() { close(reached) })
			}
			<-release
			inside.Add(-1)
			g.WeakUnlock(tok)
		}(uint64(i))
	}

	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		close(release)
		wg.Wait()
		t.Fatalf("only %d of %d weak holders were inside at once: weak acquisitions "+
			"are excluding each other", inside.Load(), want)
	}
	close(release)
	wg.Wait()
}

// TestGate_StrongDrainsUnderWeakHammering pins the termination argument in the file
// header: a strong acquirer must complete even while weak acquirers loop as fast as
// they can. If new fast-path holders could keep appearing after the flag is raised,
// the drain would spin forever and this test would time out.
func TestGate_StrongDrainsUnderWeakHammering(t *testing.T) {
	var g Gate
	stop := make(chan struct{})
	var hammered atomic.Int64
	var wg sync.WaitGroup

	// running closes once the hammer is demonstrably in its loop. Without it the
	// strong acquisition can complete before the runtime has scheduled a single
	// hammer goroutine — the drain then contests nothing, `hammered` stays 0 and the
	// positive control below fires. That is not hypothetical: it failed exactly this
	// way under `make ci`'s coverage pass, where instrumentation slows goroutine
	// start-up. The contest has to be ESTABLISHED before it is measured.
	var once sync.Once
	running := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(hint uint64) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tok := g.WeakLock(hint)
				if hammered.Add(1) >= 100 {
					once.Do(func() { close(running) })
				}
				g.WeakUnlock(tok)
			}
		}(uint64(i))
	}

	select {
	case <-running:
	case <-time.After(30 * time.Second):
		close(stop)
		wg.Wait()
		t.Fatalf("the weak hammer only reached %d iterations, so the drain was never contested",
			hammered.Load())
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		g.StrongLock()
		g.StrongUnlock()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		close(stop)
		wg.Wait()
		t.Fatal("StrongLock did not complete under weak hammering: the drain is starving")
	}
	close(stop)
	wg.Wait()
}

// TestGate_StrongExcludesStrong checks the second half of the strong contract.
func TestGate_StrongExcludesStrong(t *testing.T) {
	var g Gate
	var concurrent atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				g.StrongLock()
				if n := concurrent.Add(1); n != 1 {
					t.Errorf("%d strong holders at once, want 1", n)
				}
				concurrent.Add(-1)
				g.StrongUnlock()
			}
		}()
	}
	wg.Wait()
}

// BenchmarkGate_WeakParallel and BenchmarkRWMutexShared_Parallel are the A/B that
// decides whether the gate is worth adopting. They must be read together and run in
// the same invocation; the gate is only justified if the second degrades with core
// count and the first does not.
func BenchmarkGate_WeakParallel(b *testing.B) {
	var g Gate
	// One hint per parallel worker, drawn ONCE outside the hot loop. Drawing it per
	// iteration would put a shared atomic back on the measured path and reproduce
	// the very design fault this benchmark exists to detect. Real callers pass a
	// transaction id they already hold, which costs them nothing here either.
	var seq atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		hint := seq.Add(1)
		for pb.Next() {
			tok := g.WeakLock(hint)
			g.WeakUnlock(tok)
		}
	})
}

func BenchmarkRWMutexShared_Parallel(b *testing.B) {
	var mu sync.RWMutex
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// No defer, deliberately: this measures the acquire/release pair itself,
			// and a defer would add its own bookkeeping to the number and make the
			// comparison against Gate dishonest. The gate arm is unrolled the same
			// way, so both arms measure the same thing.
			mu.RLock()
			//nolint:gocritic,staticcheck // SA2001/badLock: the empty critical section
			// IS the measurement — this benchmark prices the acquire/release pair, and
			// both adding a body and adding a defer would price something else.
			mu.RUnlock()
		}
	})
}

// TestGate_WeakLockCtxHonoursTheDeadline pins the bound that makes the gate
// adoptable at all. A weak acquirer blocked behind a DDL must give up when its
// caller's context expires — before rmp #2306 an autocommit write carrying a 200 ms
// deadline blocked for ten minutes because that bound was missing.
func TestGate_WeakLockCtxHonoursTheDeadline(t *testing.T) {
	var g Gate
	g.StrongLock() // a DDL is in progress for the whole test
	defer g.StrongUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := g.WeakLockCtx(ctx, 1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WeakLockCtx succeeded while a strong holder was present; want the context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("WeakLockCtx took %s to honour a 50ms deadline", elapsed)
	}
}

// TestGate_WeakLockCtxSucceedsWhenUncontended checks the fast path returns a usable
// token and does not consult the context needlessly.
func TestGate_WeakLockCtxSucceedsWhenUncontended(t *testing.T) {
	var g Gate
	tok, err := g.WeakLockCtx(context.Background(), 7)
	if err != nil {
		t.Fatalf("WeakLockCtx on an idle gate: %v", err)
	}
	g.WeakUnlock(tok)
}

// TestGate_TryWeakLockFailsUnderStrong checks the non-blocking probe, and that a
// failed probe leaves no claim behind — a leaked claim would stall the next DDL's
// drain forever.
func TestGate_TryWeakLockFailsUnderStrong(t *testing.T) {
	var g Gate
	if tok, ok := g.TryWeakLock(3); !ok {
		t.Fatal("TryWeakLock failed on an idle gate")
	} else {
		g.WeakUnlock(tok)
	}

	g.StrongLock()
	_, ok := g.TryWeakLock(3)
	if ok {
		t.Fatal("TryWeakLock succeeded while a strong holder was present")
	}
	if n := g.WeakHolders(); n != 0 {
		t.Fatalf("a failed TryWeakLock left %d claims behind; the next drain would hang", n)
	}
	g.StrongUnlock()

	// And the gate is still usable afterwards.
	tok, ok := g.TryWeakLock(3)
	if !ok {
		t.Fatal("TryWeakLock failed after the strong holder left")
	}
	g.WeakUnlock(tok)
}

// TestGate_WeakLockAutoWorksWithoutAHint checks the hintless entry point.
func TestGate_WeakLockAutoWorksWithoutAHint(t *testing.T) {
	var g Gate
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tok := g.WeakLockAuto()
				g.WeakUnlock(tok)
			}
		}()
	}
	wg.Wait()
	if n := g.WeakHolders(); n != 0 {
		t.Fatalf("%d claims outstanding after every holder released", n)
	}
}
