package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake is a deterministic [Clock] whose time advances only when [Fake.Advance]
// or [Fake.Set] is called. It never reads wall time, so code driven by a Fake
// replays identically for a given sequence of advances. Timers and tickers
// created from a Fake fire synchronously during the Advance that crosses their
// deadline. A one-shot timer's single fire is never dropped (its buffered
// channel always holds the pending fire even with no goroutine waiting). A
// ticker, however, coalesces: its cap-1 channel drops a tick on a full buffer
// exactly as [time.Ticker] does, so a single Advance crossing several periods
// leaves at most one pending tick, not one per period.
//
// Fake is intended for tests and for the deterministic simulation harness.
//
// # Concurrency contract
//
// Fake is safe for concurrent use: every method takes an internal mutex. The
// channel sends performed during Advance are non-blocking (channels are
// buffered with capacity 1 and a full channel is left as-is, matching
// [time.Ticker]'s drop-on-backlog behaviour), so Advance never blocks on a
// slow consumer.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

// fakeWaiter is a pending timer or ticker registered against a [Fake].
type fakeWaiter struct {
	ch       chan time.Time
	deadline time.Time
	period   time.Duration // zero for one-shot timers; >0 for tickers
	stopped  bool
}

// NewFake returns a Fake positioned at start.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now reports the Fake's current instant.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since reports the duration elapsed since t against the Fake's current time.
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Until reports the duration until t against the Fake's current time.
func (f *Fake) Until(t time.Time) time.Duration { return t.Sub(f.Now()) }

// After returns a channel that fires once the Fake advances at least d past the
// current time.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	return f.NewTimer(d).C()
}

// NewTimer registers a one-shot timer that fires when the Fake advances at
// least d past the current time.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{ch: make(chan time.Time, 1), deadline: f.now.Add(d)}
	f.waiters = append(f.waiters, w)
	return &fakeTimer{fake: f, w: w}
}

// NewTicker registers a ticker that fires every d as the Fake advances. A
// non-positive d panics, matching [time.NewTicker].
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{ch: make(chan time.Time, 1), deadline: f.now.Add(d), period: d}
	f.waiters = append(f.waiters, w)
	return &fakeTicker{fake: f, w: w}
}

// Advance moves the Fake's clock forward by d, firing every timer and ticker
// whose deadline falls within the elapsed interval. Waiters are fired in
// deadline order so the sequence is deterministic. A negative d is treated as
// zero.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		d = 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advanceLocked(f.now.Add(d))
}

// Set moves the Fake's clock to t (which must not be before the current time),
// firing every waiter whose deadline falls in the interval. A target before the
// current time is ignored.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Before(f.now) {
		return
	}
	f.advanceLocked(t)
}

// advanceLocked advances now to target, firing due waiters in deadline order. The
// caller holds f.mu. Tickers re-arm to their next period (catching up if the
// jump spans multiple periods, firing once per period crossed, matching the
// coalescing semantics of [time.Ticker]).
func (f *Fake) advanceLocked(target time.Time) {
	for {
		// Find the earliest pending deadline at or before target.
		var next *fakeWaiter
		for _, w := range f.waiters {
			if w.stopped {
				continue
			}
			if w.deadline.After(target) {
				continue
			}
			if next == nil || w.deadline.Before(next.deadline) {
				next = w
			}
		}
		if next == nil {
			break
		}
		f.now = next.deadline
		f.deliver(next.ch, next.deadline)
		if next.period > 0 {
			next.deadline = next.deadline.Add(next.period)
		} else {
			next.stopped = true
		}
	}
	f.now = target
	f.compact()
}

// deliver performs a non-blocking send of t on ch, dropping the tick if the
// channel buffer is full — exactly how [time.Ticker] behaves when the consumer
// falls behind.
func (f *Fake) deliver(ch chan time.Time, t time.Time) {
	select {
	case ch <- t:
	default:
	}
}

// compact drops stopped one-shot waiters so the slice does not grow unbounded
// over a long simulation.
func (f *Fake) compact() {
	if len(f.waiters) == 0 {
		return
	}
	live := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.stopped {
			live = append(live, w)
		}
	}
	// Keep deterministic ordering by deadline for reproducible fan-out.
	sort.SliceStable(live, func(i, j int) bool {
		return live[i].deadline.Before(live[j].deadline)
	})
	f.waiters = live
}

func (f *Fake) stop(w *fakeWaiter) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	already := w.stopped
	w.stopped = true
	return !already
}

// fakeTimer is the [Timer] handle for a one-shot waiter on a [Fake].
type fakeTimer struct {
	fake *Fake
	w    *fakeWaiter
}

func (t *fakeTimer) C() <-chan time.Time { return t.w.ch }
func (t *fakeTimer) Stop() bool          { return t.fake.stop(t.w) }

// fakeTicker is the [Ticker] handle for a periodic waiter on a [Fake].
type fakeTicker struct {
	fake *Fake
	w    *fakeWaiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.ch }
func (t *fakeTicker) Stop()               { t.fake.stop(t.w) }
