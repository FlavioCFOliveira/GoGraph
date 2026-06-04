package checkpoint

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// triggerWatchdog runs fn (a single Trigger call) and waits at most
// perCallTimeout for it to return. It reports whether the call returned
// in time and, if so, the error it produced. A call that does not return
// is the bug this task fixes: the Trigger caller blocking forever on a
// result the departed loop will never deliver. The spawned goroutine is
// abandoned on timeout (it is the leak under test), so goleak will also
// flag the failure at the end of the run.
func triggerWatchdog(perCallTimeout time.Duration, fn func() error) (returned bool, err error) {
	res := make(chan error, 1)
	go func() { res <- fn() }()
	select {
	case err = <-res:
		return true, err
	case <-time.After(perCallTimeout):
		return false, nil
	}
}

// newRaceCheckpointer builds a started checkpointer over a small graph
// with the ticker disabled, so every checkpoint is Trigger-driven and the
// only loop-exit causes are Stop or context cancellation. It returns the
// checkpointer, the cancel func for its Start context, and a cleanup that
// releases the WAL.
func newRaceCheckpointer(t *testing.T) (cp *Checkpointer[string, int64], cancel context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, e := range [][2]string{{"a", "b"}, {"b", "c"}, {"c", "a"}} {
		if err := g.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e[0], e[1], err)
		}
	}

	var mu sync.Mutex
	cp = New(Config{Dir: dir, MaxAge: 0}, g, w, &mu)
	var ctx context.Context
	ctx, cancel = context.WithCancel(context.Background())
	cp.Start(ctx)
	return cp, cancel
}

// assertTriggerErr fails the test if err is not one of the two outcomes a
// racing Trigger may legitimately produce: nil / a checkpoint error while
// the loop is alive, or ErrCheckpointerStopped once it has stopped. The
// forbidden outcome — a wrapped context error from Trigger's
// context.Background() — can only arise if the wait edges started keying
// off a cancellable context, which would be a regression.
func assertTriggerErr(t *testing.T, err error) {
	t.Helper()
	if err == nil || errors.Is(err, ErrCheckpointerStopped) {
		return
	}
	// A checkpoint may genuinely fail mid-shutdown (e.g. the WAL is being
	// closed); that is acceptable. Only a context error is impossible from
	// Trigger and signals the gate logic regressed.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Trigger returned a context error %v; Trigger uses context.Background() and must never see one", err)
	}
}

// TestCheckpoint_TriggerRacingStop is the acceptance test for the
// drain-on-exit fix. Many goroutines hammer Trigger in tight loops while
// another goroutine calls Stop. Before the fix, a Trigger whose request
// was buffered into triggerCh just as the loop exited was orphaned: the
// caller blocked forever on the result channel (Trigger has no cancellable
// context), leaking its goroutine. After the fix every in-flight Trigger
// returns promptly — with the stopped sentinel once the loop is gone — and
// goleak (via TestMain) confirms no goroutine leaks.
func TestCheckpoint_TriggerRacingStop(t *testing.T) {
	t.Parallel()
	const (
		workers        = 32
		perCallTimeout = 5 * time.Second
	)
	cp, cancel := newRaceCheckpointer(t)
	defer cancel()

	var stopped atomic.Bool // set once Stop has returned
	var hung atomic.Bool    // set if any Trigger blew the watchdog
	var wg sync.WaitGroup

	// Triggering workers: each loops until Stop has been observed, then
	// makes one final post-stop call to prove the stopped path also
	// returns promptly. Every call is bounded by the per-call watchdog.
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stopped.Load() {
				returned, err := triggerWatchdog(perCallTimeout, cp.Trigger)
				if !returned {
					hung.Store(true)
					return
				}
				assertTriggerErr(t, err)
			}
			// One guaranteed post-stop call: must return the sentinel.
			returned, err := triggerWatchdog(perCallTimeout, cp.Trigger)
			if !returned {
				hung.Store(true)
				return
			}
			if !errors.Is(err, ErrCheckpointerStopped) {
				t.Errorf("post-stop Trigger = %v, want ErrCheckpointerStopped", err)
			}
		}()
	}

	// Let the workers build up a backlog so requests are genuinely
	// in-flight and buffered when Stop lands.
	time.Sleep(20 * time.Millisecond)
	cp.Stop()
	stopped.Store(true)

	wg.Wait()
	if hung.Load() {
		t.Fatalf("at least one Trigger did not return within %v (orphaned by Stop)", perCallTimeout)
	}

	// After Stop, every Trigger must be the prompt stopped sentinel.
	if err := cp.Trigger(); !errors.Is(err, ErrCheckpointerStopped) {
		t.Fatalf("Trigger after Stop = %v, want ErrCheckpointerStopped", err)
	}
}

// TestCheckpoint_TriggerRacingCtxCancel is the context-cancellation
// variant. The loop exits because the Start context is cancelled, NOT
// because Stop closed stopCh — so stopCh stays open and only the internal
// stoppedCh gate can unblock callers. This exercises the late-submitter
// path: a TriggerCtx(context.Background()) arriving after a ctx-driven
// exit must still return ErrCheckpointerStopped rather than block on a
// full or unread buffer.
func TestCheckpoint_TriggerRacingCtxCancel(t *testing.T) {
	t.Parallel()
	const (
		workers        = 32
		perCallTimeout = 5 * time.Second
	)
	cp, cancel := newRaceCheckpointer(t)

	var cancelled atomic.Bool
	var hung atomic.Bool
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !cancelled.Load() {
				returned, err := triggerWatchdog(perCallTimeout, cp.Trigger)
				if !returned {
					hung.Store(true)
					return
				}
				assertTriggerErr(t, err)
			}
			returned, err := triggerWatchdog(perCallTimeout, cp.Trigger)
			if !returned {
				hung.Store(true)
				return
			}
			if !errors.Is(err, ErrCheckpointerStopped) {
				t.Errorf("post-cancel Trigger = %v, want ErrCheckpointerStopped", err)
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)
	cancel()  // drive the loop out via ctx.Done(), not Stop
	cp.Stop() // join the goroutine deterministically (idempotent)
	cancelled.Store(true)

	wg.Wait()
	if hung.Load() {
		t.Fatalf("at least one Trigger did not return within %v after ctx cancel", perCallTimeout)
	}

	if err := cp.Trigger(); !errors.Is(err, ErrCheckpointerStopped) {
		t.Fatalf("Trigger after ctx cancel = %v, want ErrCheckpointerStopped", err)
	}
}

// TestCheckpoint_TriggerCtxStoppedSentinel pins the public contract that
// both Trigger and TriggerCtx return the exported ErrCheckpointerStopped
// sentinel (matchable with errors.Is) once the checkpointer has stopped,
// and do so promptly rather than blocking.
func TestCheckpoint_TriggerCtxStoppedSentinel(t *testing.T) {
	t.Parallel()
	cp, cancel := newRaceCheckpointer(t)
	defer cancel()
	cp.Stop()

	if err := cp.Trigger(); !errors.Is(err, ErrCheckpointerStopped) {
		t.Fatalf("Trigger after Stop = %v, want ErrCheckpointerStopped", err)
	}
	if err := cp.TriggerCtx(context.Background()); !errors.Is(err, ErrCheckpointerStopped) {
		t.Fatalf("TriggerCtx after Stop = %v, want ErrCheckpointerStopped", err)
	}
}
