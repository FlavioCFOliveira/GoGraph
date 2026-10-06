package mvcc

// gate_cancel_test.go — a cancelled acquisition of [Gate] is withdrawn, not left
// queued (rmp #2983).
//
// StrongLockCtx used to run StrongLock on a helper goroutine and let only the
// caller stop waiting. The helper kept the strong flag raised until the in-flight
// weak holder left, so after the caller had its deadline error every NEW weak
// acquirer still blocked behind the abandoned request. These tests pin the
// withdrawal in each phase a request can be cancelled in, the cancelled weak
// request, the absence of any goroutine left behind, and a race-stress
// interleaving of all of it. The package's TestMain runs goleak.
//
// Every ordering asserted is a happens-before against a holder this test still
// owns; the backstops only keep a regression from hanging the package.

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cancelBackstop bounds a wait that a correct gate satisfies at once. Reaching it
// is the failure; it is not a latency bound.
const cancelBackstop = 10 * time.Second

// gateState is the gate's cold-path state, read under its mutex.
type gateState struct {
	strong      int32
	owned       bool
	slowHolders int
	queued      int
	weakParked  int
}

func (g *Gate) state() gateState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return gateState{
		strong:      g.strong.Load(),
		owned:       g.owned,
		slowHolders: g.slowHolders,
		queued:      len(g.queue),
		weakParked:  g.weakParked,
	}
}

// awaitState polls until cond holds for the gate's state, failing on the backstop.
func awaitState(t *testing.T, g *Gate, what string, cond func(gateState) bool) {
	t.Helper()
	deadline := time.Now().Add(cancelBackstop)
	for !cond(g.state()) {
		if time.Now().After(deadline) {
			t.Fatalf("gate never reached %s: %+v", what, g.state())
		}
		runtime.Gosched()
	}
}

// assertIdle fails unless the gate holds nothing and remembers no request.
func assertIdle(t *testing.T, g *Gate) {
	t.Helper()
	if s := g.state(); s.strong != 0 || s.owned || s.slowHolders != 0 || s.queued != 0 || s.weakParked != 0 {
		t.Fatalf("gate is not idle: %+v", s)
	}
	if n := g.WeakHolders(); n != 0 {
		t.Fatalf("gate is not idle: %d fast-path claims outstanding", n)
	}
}

// TestGateCancel_StrongWithdrawnWhileDrainingFastPath is the rmp #2983 shape: a
// weak holder is in flight, a strong request gives up draining it, and the gate
// must then admit new weak acquirers — on both paths — while that holder is still
// in place.
//
// The deadline arm needs the parked acquirer to be observed parked before the
// 50 ms deadline fires. That is a precondition the test constructs, not the
// property under test, so an attempt that misses it is retried rather than failed.
func TestGateCancel_StrongWithdrawnWhileDrainingFastPath(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			const attempts = 20
			for range attempts {
				if strongWithdrawnFastPathAttempt(t, mode) {
					return
				}
			}
			t.Fatalf("in %d attempts the parked weak acquirer was never observed before the "+
				"deadline fired; the precondition was not constructed", attempts)
		})
	}
}

// strongWithdrawnFastPathAttempt runs one attempt and reports whether its
// precondition was constructed. It fails t on any property violation.
func strongWithdrawnFastPathAttempt(t *testing.T, mode string) bool {
	t.Helper()
	var g Gate
	holder := g.WeakLock(1) // the in-flight write

	ctx, cancel := context.WithCancel(context.Background())
	if mode == "deadline" {
		ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	}
	defer cancel()
	strongErr := make(chan error, 1)
	go func() { strongErr <- g.StrongLockCtx(ctx) }()

	// The request has raised the flag and is draining the holder's slot.
	awaitState(t, &g, "a raised strong flag or a returned request", func(s gateState) bool {
		return s.owned || len(strongErr) == 1
	})
	if len(strongErr) == 1 {
		<-strongErr
		g.WeakUnlock(holder)
		return false
	}

	// A weak acquirer arriving now parks behind the request.
	parked := make(chan int, 1)
	go func() { parked <- g.WeakLock(2) }()
	awaitState(t, &g, "a parked weak acquirer", func(s gateState) bool { return s.weakParked != 0 || !s.owned })
	if g.state().weakParked == 0 {
		// The deadline fired first; the acquirer may have taken the fast path.
		<-strongErr
		g.WeakUnlock(<-parked)
		g.WeakUnlock(holder)
		return false
	}

	if mode == "cancel" {
		cancel()
	}
	var err error
	select {
	case err = <-strongErr:
	case <-time.After(cancelBackstop):
		g.WeakUnlock(holder)
		t.Fatalf("StrongLockCtx did not return while the weak holder was in place")
	}
	want := context.DeadlineExceeded
	if mode == "cancel" {
		want = context.Canceled
	}
	if !errors.Is(err, want) {
		t.Fatalf("StrongLockCtx: err = %v, want %v", err, want)
	}

	// The holder is STILL in place. The parked acquirer must be admitted, and a
	// new one must take the fast path.
	var tok int
	select {
	case tok = <-parked:
	case <-time.After(cancelBackstop):
		g.WeakUnlock(holder)
		<-parked
		t.Fatal("a weak acquirer parked behind the withdrawn strong request was not " +
			"admitted until the in-flight holder left: the request is still queued")
	}
	if tok != gateSlow {
		t.Fatalf("parked acquirer token = %d, want the slow-path token", tok)
	}
	fast, ok := g.TryWeakLock(3)
	if !ok {
		t.Fatal("TryWeakLock failed after the strong request was withdrawn: the flag is still raised")
	}
	if s := g.state(); s.strong != 0 || s.owned || s.queued != 0 {
		t.Fatalf("withdrawn request left state behind: %+v", s)
	}

	g.WeakUnlock(fast)
	g.WeakUnlock(tok)
	g.WeakUnlock(holder)
	assertIdle(t, &g)
	// The gate is fully usable afterwards.
	if err := g.StrongLockCtx(context.Background()); err != nil {
		t.Fatalf("StrongLockCtx on the idle gate: %v", err)
	}
	g.StrongUnlock()
	assertIdle(t, &g)
	return true
}

// TestGateCancel_StrongWithdrawnWhileDrainingSlowPath cancels a strong request
// while it waits for a slow-path weak holder, the third phase of an acquisition.
func TestGateCancel_StrongWithdrawnWhileDrainingSlowPath(t *testing.T) {
	var g Gate
	// Build a slow-path holder: park a weak acquirer behind a strong holder, then
	// release the strong holder so it is admitted on the slow path.
	g.StrongLock()
	slow := make(chan int, 1)
	go func() { slow <- g.WeakLock(1) }()
	awaitState(t, &g, "a parked weak acquirer", func(s gateState) bool { return s.weakParked != 0 })
	g.StrongUnlock()
	tok := <-slow
	if tok != gateSlow {
		t.Fatalf("token = %d, want the slow-path token", tok)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.StrongLockCtx(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StrongLockCtx behind a slow-path holder: err = %v, want DeadlineExceeded", err)
	}
	// The slow-path holder is still in place; new weak acquirers get in on both paths.
	fast, ok := g.TryWeakLock(2)
	if !ok {
		t.Fatal("TryWeakLock failed after the strong request was withdrawn")
	}
	if s := g.state(); s.strong != 0 || s.owned || s.slowHolders != 1 {
		t.Fatalf("withdrawn request left state behind: %+v", s)
	}
	g.WeakUnlock(fast)
	g.WeakUnlock(tok)
	assertIdle(t, &g)
}

// TestGateCancel_StrongWithdrawnWhileQueued cancels a strong request queued behind
// another strong holder, the first phase. It must leave the queue, and the strong
// requests queued behind it must keep their order and proceed.
func TestGateCancel_StrongWithdrawnWhileQueued(t *testing.T) {
	var g Gate
	g.StrongLock()

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() { cancelled <- g.StrongLockCtx(ctx) }()
	awaitState(t, &g, "one queued strong request", func(s gateState) bool { return s.queued == 1 })

	next := make(chan struct{})
	go func() {
		g.StrongLock()
		close(next)
	}()
	awaitState(t, &g, "two queued strong requests", func(s gateState) bool { return s.queued == 2 })

	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued StrongLockCtx: err = %v, want context.Canceled", err)
	}
	if s := g.state(); s.queued != 1 || !s.owned {
		t.Fatalf("after withdrawing one of two queued requests: %+v, want 1 queued behind the owner", s)
	}
	select {
	case <-next:
		t.Fatal("the request queued behind the withdrawn one acquired while the gate was still held")
	default:
	}

	g.StrongUnlock()
	select {
	case <-next:
	case <-time.After(cancelBackstop):
		t.Fatal("the remaining queued strong request did not acquire once the holder released")
	}
	g.StrongUnlock()
	assertIdle(t, &g)
}

// TestGateCancel_ParkedWeakAdmittedBeforeNextStrong pins the order a release
// admits waiters in: weak acquirers parked behind one strong tenure get in before a
// queued strong acquirer starts the next, so a run of strong acquirers cannot
// starve them. The sync.RWMutex the gate used before rmp #2983 gave the same order.
func TestGateCancel_ParkedWeakAdmittedBeforeNextStrong(t *testing.T) {
	var g Gate
	g.StrongLock()
	parked := make(chan int, 1)
	go func() { parked <- g.WeakLock(1) }()
	awaitState(t, &g, "a parked weak acquirer", func(s gateState) bool { return s.weakParked == 1 })
	next := make(chan struct{})
	go func() {
		g.StrongLock()
		close(next)
	}()
	awaitState(t, &g, "a queued strong request", func(s gateState) bool { return s.queued == 1 })

	g.StrongUnlock()
	var tok int
	select {
	case tok = <-parked:
	case <-next:
		t.Fatal("the queued strong request acquired before the weak acquirer parked behind the " +
			"previous tenure was admitted")
	case <-time.After(cancelBackstop):
		t.Fatal("the parked weak acquirer was not admitted after the strong holder released")
	}
	select {
	case <-next:
		t.Fatal("the queued strong request acquired while the admitted weak holder was still in place")
	default:
	}
	g.WeakUnlock(tok)
	select {
	case <-next:
	case <-time.After(cancelBackstop):
		t.Fatal("the queued strong request did not acquire once the weak holder left")
	}
	g.StrongUnlock()
	assertIdle(t, &g)
}

// TestGateCancel_WeakWithdrawnWhileParked cancels a weak request parked behind a
// strong holder. It must take nothing, so the strong holder's release and a later
// strong acquire see no weak holder.
func TestGateCancel_WeakWithdrawnWhileParked(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var g Gate
			g.StrongLock()

			ctx, cancel := context.WithCancel(context.Background())
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
			}
			defer cancel()
			weakErr := make(chan error, 1)
			go func() {
				tok, err := g.WeakLockCtx(ctx, 1)
				if err == nil {
					g.WeakUnlock(tok)
				}
				weakErr <- err
			}()
			awaitState(t, &g, "a parked weak acquirer", func(s gateState) bool { return s.weakParked != 0 })
			if mode == "cancel" {
				cancel()
			}
			var err error
			select {
			case err = <-weakErr:
			case <-time.After(cancelBackstop):
				g.StrongUnlock()
				t.Fatal("WeakLockCtx did not return while the strong holder was in place")
			}
			want := context.DeadlineExceeded
			if mode == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				g.StrongUnlock()
				t.Fatalf("WeakLockCtx: err = %v, want %v", err, want)
			}
			if s := g.state(); s.slowHolders != 0 || s.weakParked != 0 {
				t.Fatalf("a cancelled weak request was admitted or left parked: %+v", s)
			}
			g.StrongUnlock()
			assertIdle(t, &g)
			g.StrongLock() // must not wait for a phantom weak holder
			g.StrongUnlock()
			assertIdle(t, &g)
		})
	}
}

// TestGateCancel_AbandonedAcquiresLeaveNoGoroutine asserts that abandoning an
// acquisition costs no goroutine: with the holder STILL in place, once every
// abandoning caller has returned, the goroutine count is back at its baseline.
// The previous shape parked one helper per abandoned acquire for the holder's
// whole tenure, so the count could not return to the baseline while it held.
func TestGateCancel_AbandonedAcquiresLeaveNoGoroutine(t *testing.T) {
	const herd = 32
	for _, side := range []string{"weak", "strong"} {
		t.Run(side, func(t *testing.T) {
			var g Gate
			var holderTok int
			if side == "weak" {
				g.StrongLock() // weak requests wait behind a strong holder
			} else {
				holderTok = g.WeakLock(0) // strong requests wait to drain a weak holder
			}
			baseline := runtime.NumGoroutine()

			var wg sync.WaitGroup
			var refused atomic.Int64
			for i := range herd {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
					defer cancel()
					var err error
					if side == "weak" {
						_, err = g.WeakLockCtx(ctx, uint64(i))
					} else {
						err = g.StrongLockCtx(ctx)
					}
					if err != nil {
						refused.Add(1)
					}
				}()
			}
			wg.Wait()
			if got := refused.Load(); got != herd {
				t.Fatalf("%d of %d acquires were refused; the fixture is not contended", got, herd)
			}
			// A returning caller's goroutine exits a moment after wg.Done, so poll
			// down to the baseline; a helper parked behind the holder never gets there.
			if n := waitForGoroutines(baseline, cancelBackstop); n > baseline {
				t.Fatalf("%d goroutines remain after %d abandoned %s acquires returned, baseline %d: "+
					"an abandoned acquire left a goroutine queued behind the holder", n, herd, side, baseline)
			}
			if side == "weak" {
				g.StrongUnlock()
			} else {
				g.WeakUnlock(holderTok)
			}
			assertIdle(t, &g)
		})
	}
}

// TestGateCancel_RaceStress interleaves blocking and cancellable acquires and
// releases of both modes, with cancellations landing in every phase.
//
// The exclusion oracle is the race detector on guarded, a plain int read under a
// weak hold and written under a strong one, plus explicit occupancy counters. After
// the run the gate must be idle, and the package's goleak gate requires that no
// goroutine survives.
func TestGateCancel_RaceStress(t *testing.T) {
	const (
		workers    = 16
		iterations = 400
	)
	var (
		g                    Gate
		guarded              int
		insideWeak, insideSt atomic.Int64
		weakOK, weakCancel   atomic.Int64
		strongOK, strongCnl  atomic.Int64
		wg                   sync.WaitGroup
	)
	fail := make(chan string, 1)
	report := func(msg string) {
		select {
		case fail <- msg:
		default:
		}
	}
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 2983)) //nolint:gosec // G404: per-worker reproducible op sequence; a CSPRNG would destroy it.
			for range iterations {
				op := rng.IntN(4)
				var (
					ctx    = context.Background()
					cancel = func() {}
				)
				if op == 1 || op == 3 {
					// Deadlines from already-expired to a few holder tenures, and an
					// explicit cancel from another goroutine, so cancellation lands in
					// every phase of an acquisition.
					if rng.IntN(2) == 0 {
						ctx, cancel = context.WithTimeout(ctx, time.Duration(rng.IntN(300))*time.Microsecond)
					} else {
						var c context.CancelFunc
						ctx, c = context.WithCancel(ctx)
						tm := time.AfterFunc(time.Duration(rng.IntN(300))*time.Microsecond, c)
						cancel = func() { tm.Stop(); c() }
					}
				}
				switch op {
				case 0, 1:
					var tok int
					if op == 0 {
						tok = g.WeakLock(rng.Uint64())
					} else {
						var err error
						if tok, err = g.WeakLockCtx(ctx, rng.Uint64()); err != nil {
							weakCancel.Add(1)
							cancel()
							continue
						}
					}
					insideWeak.Add(1)
					if insideSt.Load() != 0 {
						report("a weak holder ran beside a strong holder")
					}
					_ = guarded
					for range rng.IntN(3) {
						runtime.Gosched()
					}
					insideWeak.Add(-1)
					g.WeakUnlock(tok)
					weakOK.Add(1)
				case 2, 3:
					if op == 2 {
						g.StrongLock()
					} else if err := g.StrongLockCtx(ctx); err != nil {
						strongCnl.Add(1)
						cancel()
						continue
					}
					if insideSt.Add(1) != 1 {
						report("two strong holders at once")
					}
					if insideWeak.Load() != 0 {
						report("a strong holder ran beside a weak holder")
					}
					guarded++
					for range rng.IntN(3) {
						runtime.Gosched()
					}
					insideSt.Add(-1)
					g.StrongUnlock()
					strongOK.Add(1)
				}
				cancel()
			}
		}()
	}
	wg.Wait()
	select {
	case msg := <-fail:
		t.Fatal(msg)
	default:
	}
	assertIdle(t, &g)
	if guarded != int(strongOK.Load()) {
		t.Fatalf("guarded = %d, want one increment per strong hold (%d)", guarded, strongOK.Load())
	}
	// Positive controls: the blocking ops always succeed, so both modes ran.
	if weakOK.Load() == 0 || strongOK.Load() == 0 {
		t.Fatalf("no acquisitions of one mode: weak %d, strong %d", weakOK.Load(), strongOK.Load())
	}
	t.Logf("weak ok=%d cancelled=%d, strong ok=%d cancelled=%d",
		weakOK.Load(), weakCancel.Load(), strongOK.Load(), strongCnl.Load())
}
