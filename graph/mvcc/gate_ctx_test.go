package mvcc

// gate_ctx_test.go — the context-bounded acquisition contract of [Gate], and the
// two bounded-resource properties it must not lose (rmp #2348).
//
// # Why these tests moved here
//
// internal/ctxlock owned this contract and these guards while graph/lpg's barrier
// was a sync.RWMutex. rmp #2337 replaced that barrier with a Gate, whose own
// WeakLockCtx and StrongLockCtx supply the bounded acquisition, and rmp #2348 retired
// ctxlock. Both of the gate's Ctx methods had regressed the properties ctxlock had
// established:
//
//   - TWO goroutines per abandoned acquire, not one — the helper plus a second
//     goroutine spawned on the ctx.Done branch to wait for it and unlock;
//   - no ctx re-check on the SUCCESS path, so a deadline that elapsed while queued
//     was reported as success and the caller was handed a lock it may no longer use.
//
// Both are fixed in [acquireCtx]. These tests are what stops them coming back a
// third time, which is precisely what deleting a package with its tests would not
// have done.

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitForGoroutines polls until the goroutine count drops to at most want, or the
// deadline elapses, and returns the last reading. Parked helpers finish
// asynchronously, so a single sample immediately after a release would race them.
func waitForGoroutines(want int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	n := runtime.NumGoroutine()
	for n > want && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// TestGateCtx_AbandonedAcquireCostsOneHelper asserts the bounded-resources property:
// N abandoned context-bounded acquires against a held gate leave N helpers parked,
// not 2N.
//
// The threshold is the midpoint between herd and 2*herd, so the old two-goroutine
// shape fails and ordinary test noise does not.
func TestGateCtx_AbandonedAcquireCostsOneHelper(t *testing.T) {
	const herd = 64
	var g Gate

	// A strong holder makes every weak Ctx acquire below contended, so each must
	// give up on its deadline.
	g.StrongLock()

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < herd; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
			defer cancel()
			if _, err := g.WeakLockCtx(ctx, uint64(i)); err == nil {
				t.Error("WeakLockCtx succeeded while a strong holder was in place; " +
					"the fixture is not contended")
			}
		}(i)
	}
	wg.Wait() // every caller has returned; only parked helpers remain

	parked := runtime.NumGoroutine() - baseline
	if parked > herd+herd/2 {
		t.Errorf("after %d abandoned acquires, %d helper goroutines are parked; want at "+
			"most one per acquire (~%d). Two per acquire means the second unlock-waiter "+
			"goroutine is back — the shape rmp #2260 removed and rmp #2348 found "+
			"reintroduced in this gate", herd, parked, herd)
	}

	// Liveness half: releasing the holder must let every helper finish.
	g.StrongUnlock()
	if n := waitForGoroutines(baseline+herd/4, 10*time.Second); n > baseline+herd/4 {
		t.Errorf("helpers did not drain after the strong holder released: %d goroutines, "+
			"baseline %d", n, baseline)
	}
}

// TestGateCtx_NeverLeavesTheGateHeldWithNoOwner drives the handoff race directly.
// The deadline is tuned to fire at roughly the moment the helper acquires, so across
// many iterations BOTH CAS arms are taken. After each iteration the gate must be
// free — verified by an uncontended strong acquire, which is the only observation
// that distinguishes "released" from "held by nobody".
func TestGateCtx_NeverLeavesTheGateHeldWithNoOwner(t *testing.T) {
	const iterations = 300
	for i := 0; i < iterations; i++ {
		var g Gate
		holderReleased := make(chan struct{})
		g.StrongLock()
		go func() {
			// Hold just long enough that the caller's deadline and the helper's
			// acquisition land close together.
			time.Sleep(time.Duration(i%7) * 100 * time.Microsecond)
			g.StrongUnlock()
			close(holderReleased)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Microsecond)
		slot, err := g.WeakLockCtx(ctx, uint64(i))
		if err == nil {
			g.WeakUnlock(slot)
		}
		cancel()
		<-holderReleased

		// Whichever arm was taken, the gate must end up free. A weak acquisition
		// stranded with no owner would block this strong acquire forever, so a
		// bounded wait for it is the oracle.
		free := make(chan struct{})
		go func() {
			g.StrongLock()
			g.StrongUnlock()
			close(free)
		}()
		select {
		case <-free:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: the gate was left held with no logical owner — a weak "+
				"acquisition landed after the caller abandoned and nothing released it", i)
		}
	}
}

// TestGateCtx_ReturnsWithinItsBudget pins the contract cypher/exectx.go states for
// [Engine.BeginTx]: the call returns within the deadline plus a small bounded margin
// — "one scheduling hop, not the holder's remaining tenure".
//
// This is the property rmp #2174 was about. The round-3 audit measured BeginTx with a
// 50 ms deadline returning after 601 ms, and after 11.60 s under load, in both cases
// with err=nil and a live transaction. What makes that impossible is that the WAIT is
// abandoned on ctx while the queued ACQUISITION is left to a helper.
//
// # What this test does NOT claim, stated because the first version of it claimed it
//
// [acquireCtx] also re-checks ctx on the success path, so a deadline that elapsed
// while the caller was queued is reported rather than swallowed. That re-check is
// correct and free, but its window is ONE SCHEDULING QUANTUM: the caller can only
// take the success arm with an expired context when the helper's acquisition and the
// deadline become ready at the same instant, and in that case the elapsed time is
// still within budget. It is therefore not observable from outside this package, and
// no test here claims to cover it.
//
// The first version of this test did claim it, with the oracle "err == nil implies
// ctx.Err() == nil after the call". That oracle is WRONG: acquireCtx checks ctx and
// then returns, so the deadline can elapse in the gap before the caller looks, and a
// correct implementation fails it. It did — under `make ci`, at iteration 96.
func TestGateCtx_ReturnsWithinItsBudget(t *testing.T) {
	const (
		callers      = 64
		budget       = 5 * time.Millisecond
		holderTenure = 500 * time.Millisecond
	)
	// THE INSTRUMENT IS A HAPPENS-BEFORE, NOT A DURATION BUDGET (rmp #2574).
	//
	// This test used to assert `elapsed <= budget + 100ms`. That ceiling was the
	// thinnest deadline margin in the repository and it had no defensible value,
	// because it was squeezed from both sides:
	//
	//	worst observed, no deliberate load, 3 runs   5.81 / 6.03 / 5.86 ms
	//	worst observed, 300 spinners (30x cores)     15.69 / 38.66 / 20.06 /
	//	                                             30.04 / 40.07 / 35.70 ms
	//
	// NEITHER row was taken on a genuinely quiet host: the machine carried
	// unrelated concurrent builds throughout (an unrelated Rust build alone at
	// ~170% CPU, host load average reaching 88). So the first row is an UPPER
	// bound on the quiet figure, not the quiet figure, and the second overstates
	// what 300 spinners alone would cost. Both errors point the same way — the
	// noise floor is at worst what is written here — so the conclusion below is
	// conservative. Do not quote either row as a clean-machine baseline.
	//
	// So the noise floor is 40 ms, leaving the 105 ms ceiling 2.6x of headroom —
	// not the ~10x the task reported, which was measured at 10.02 ms and is four
	// times optimistic. Every sibling absolute ceiling the sweep called safe keeps
	// 30x or more. And the ceiling cannot simply be widened: holderTenure is
	// 500 ms, so a 60x ceiling (601 ms) would EXCEED it, and a caller that blocked
	// for the holder's whole tenure — the rmp #2174 defect, a 232x overrun
	// reported as err=nil — would PASS. Widening destroys the gate rather than
	// loosening it.
	//
	// The property was never really "returned inside 105 ms". It is "ABANDONED the
	// wait instead of blocking for the tenure", which is an ORDERING fact: did the
	// caller finish before the holder released? That needs no margin, because both
	// sides of it inflate together under load — the release is a real event in the
	// same run. The duration is still LOGGED, so the number stays available as a
	// diagnostic, but it is not asserted, because the measurements above show no
	// defensible value for it exists between 40 ms and 500 ms.
	//
	// TestGateCtx_BudgetGateDetectsBlocking below is the power control.
	// ONE holder and CONCURRENT callers, rather than a sequential loop: the
	// property is per-caller, so the callers can share the holder, and the test
	// costs one tenure instead of `callers` of them.
	var g Gate
	g.StrongLock()
	released := make(chan struct{})
	// holderReleased flips the instant the strong holder lets go. Each caller
	// samples it immediately after WeakLockCtx returns, which turns "did the
	// caller outlive the holder" into a plain boolean rather than a duration
	// compared against a tuned constant.
	var holderReleased atomic.Bool
	go func() {
		time.Sleep(holderTenure)
		holderReleased.Store(true)
		g.StrongUnlock()
		close(released)
	}()

	type outcome struct {
		elapsed time.Duration
		err     error
		slot    int
		// outlivedHolder records whether the strong holder had ALREADY released by
		// the time this caller's WeakLockCtx returned. True means the caller waited
		// out the tenure instead of abandoning on its context — the defect.
		outlivedHolder bool
	}
	results := make([]outcome, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			start := time.Now()
			slot, err := g.WeakLockCtx(ctx, uint64(i))
			results[i] = outcome{time.Since(start), err, slot, holderReleased.Load()}
		}(i)
	}
	wg.Wait()

	worst := time.Duration(0)
	for i, r := range results {
		if r.err == nil {
			// The holder is still in place, so a success here would mean the gate
			// handed out a weak hold while a strong holder was active — an
			// exclusion failure, not a timing one.
			g.WeakUnlock(r.slot)
			<-released
			t.Fatalf("caller %d: WeakLockCtx SUCCEEDED after %v while a strong holder was "+
				"still in place for %v", i, r.elapsed, holderTenure)
		}
		if r.outlivedHolder {
			<-released
			t.Fatalf("caller %d: WeakLockCtx did not return until AFTER the strong holder "+
				"released, %v into its %v budget. The wait must be abandoned on ctx and the "+
				"queued acquisition left to a helper; blocking for the holder's remaining "+
				"tenure (%v) is the rmp #2174 defect, measured there as a 232x overrun "+
				"reported as err=nil. This is an ordering fact, not a timing one, so machine "+
				"load cannot have caused it (rmp #2574)",
				i, r.elapsed, budget, holderTenure)
		}
		if r.elapsed > worst {
			worst = r.elapsed
		}
	}
	<-released
	// Logged, deliberately not asserted — see the instrument note above.
	t.Logf("%d concurrent contended acquires against a %v holder, all abandoned BEFORE the "+
		"holder released; budget %v, worst observed %v (not asserted: no defensible ceiling "+
		"exists between the 40ms saturated noise floor and the %v tenure)",
		callers, holderTenure, budget, worst, holderTenure)
}

// TestGateCtx_BudgetGateDetectsBlocking is the power control for the gate above:
// it drives a caller that BLOCKS for the holder's whole tenure — the rmp #2174
// shape — and requires the happens-before instrument to see it.
//
// Without it the gate above is unfalsifiable in a healthy tree: the engine
// abandons correctly, the pre-fix build cannot be rebuilt, and an instrument that
// had quietly stopped discriminating would read exactly like one that is passing.
// This matters more for an ordering check than for a duration one, because there
// is no number in the output to eyeball.
func TestGateCtx_BudgetGateDetectsBlocking(t *testing.T) {
	const holderTenure = 200 * time.Millisecond
	var g Gate
	g.StrongLock()

	var holderReleased atomic.Bool
	released := make(chan struct{})
	go func() {
		time.Sleep(holderTenure)
		holderReleased.Store(true)
		g.StrongUnlock()
		close(released)
	}()

	// WeakLock, NOT WeakLockCtx: the blocking form has no deadline to abandon, so
	// it waits out the tenure. That is exactly what the defect did.
	slot := g.WeakLock(0)
	outlived := holderReleased.Load()
	g.WeakUnlock(slot)
	<-released

	if !outlived {
		t.Fatalf("a caller that BLOCKED on the gate with no deadline still returned before "+
			"the %v holder released: the happens-before instrument in "+
			"TestGateCtx_ReturnsWithinItsBudget cannot see a caller waiting out the tenure, "+
			"so that gate has lost its power", holderTenure)
	}
	t.Logf("power control: a deadline-less WeakLock waited out the %v holder and the "+
		"instrument saw it (outlivedHolder=true)", holderTenure)
}

// TestGateCtx_ExpiredContextTakesNothing asserts the cheapest branch: an
// already-expired context never acquires at all, so a caller cannot smuggle work
// past its own deadline by arriving late at an uncontended gate.
func TestGateCtx_ExpiredContextTakesNothing(t *testing.T) {
	var g Gate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := g.WeakLockCtx(ctx, 0); err == nil {
		t.Error("WeakLockCtx acquired on an already-cancelled context")
	}
	if err := g.StrongLockCtx(ctx); err == nil {
		t.Error("StrongLockCtx acquired on an already-cancelled context")
	}
	// Both must have taken nothing: a strong acquire now must succeed immediately.
	done := make(chan struct{})
	go func() {
		g.StrongLock()
		g.StrongUnlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the gate is held after two acquires on an expired context took nothing")
	}
}

// TestGateCtx_UncontendedSucceeds is the positive control for the three tests above:
// without it, every one of them would pass against a Gate whose Ctx methods always
// failed.
func TestGateCtx_UncontendedSucceeds(t *testing.T) {
	var g Gate
	slot, err := g.WeakLockCtx(context.Background(), 3)
	if err != nil {
		t.Fatalf("WeakLockCtx on an uncontended gate: %v", err)
	}
	g.WeakUnlock(slot)

	if err := g.StrongLockCtx(context.Background()); err != nil {
		t.Fatalf("StrongLockCtx on an uncontended gate: %v", err)
	}
	g.StrongUnlock()
}
