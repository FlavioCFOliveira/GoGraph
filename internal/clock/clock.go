// Package clock provides a minimal, injectable wall-clock abstraction so that
// time-dependent code paths — the checkpoint cadence ([store/checkpoint]) and
// the Bolt session/connection deadlines ([bolt/server]) — can be driven by a
// deterministic fake clock under test (notably the deterministic simulation
// testing harness in internal/sim) instead of reading real wall time.
//
// The interface is deliberately the smallest subset the injection sites need:
// Now, Since, Until, After, NewTimer, and NewTicker. The production default,
// returned by [Real], delegates to the standard library and is byte-for-byte
// equivalent to calling the time package directly. Call sites that take a
// [Clock] therefore behave identically to the pre-injection code on the default
// path; only a test that supplies an alternative implementation observes any
// difference.
//
// # Why an interface and not the time package
//
// The GoGraph reliability mandate requires deterministic crash/recovery
// simulation. Wall-clock reads make a run non-reproducible. Routing the two
// time-sensitive control loops through a [Clock] lets a simulator advance time
// in fixed logical steps, so a given seed replays identically. The WAL
// group-commit path is intentionally NOT routed through this abstraction: it is
// pure sync.Cond leader/follower fsync coalescing with no time reads, so it is
// already time-deterministic and needs no injection.
//
// # Concurrency contract
//
// [Real] returns a stateless value that is safe for concurrent use by any
// number of goroutines. Implementations supplied by tests must document their
// own contract; the standard fake used by the simulation harness is driven from
// a single goroutine.
package clock

import "time"

// Clock is an injectable source of wall-clock time. The production
// implementation ([Real]) delegates to the standard library; tests may supply a
// deterministic fake.
//
// Every method mirrors the semantics of its [time] package counterpart so that
// substituting [Real] for direct time calls is behaviour-preserving.
type Clock interface {
	// Now reports the clock's current instant, mirroring [time.Now].
	Now() time.Time
	// Since reports the duration elapsed since t, mirroring [time.Since].
	// It is equivalent to Now().Sub(t).
	Since(t time.Time) time.Duration
	// Until reports the duration until t, mirroring [time.Until]. It is
	// equivalent to t.Sub(Now()).
	Until(t time.Time) time.Duration
	// After returns a channel that delivers the clock's current time after
	// at least d has elapsed, mirroring [time.After]. Unlike [NewTimer] the
	// underlying timer cannot be stopped, so prefer NewTimer when the wait
	// may be abandoned.
	After(d time.Duration) <-chan time.Time
	// NewTimer creates a [Timer] that fires once after at least d, mirroring
	// [time.NewTimer].
	NewTimer(d time.Duration) Timer
	// NewTicker creates a [Ticker] that fires repeatedly every d, mirroring
	// [time.NewTicker]. A non-positive d panics, matching [time.NewTicker].
	NewTicker(d time.Duration) Ticker
}

// Timer is the injectable analogue of [time.Timer]. Its channel delivers one
// tick when the timer fires; Stop prevents a not-yet-fired timer from firing.
type Timer interface {
	// C returns the timer's delivery channel, the analogue of the exported
	// C field on [time.Timer].
	C() <-chan time.Time
	// Stop prevents the timer from firing, mirroring [time.Timer.Stop]. It
	// returns true if it stopped the timer before it fired.
	Stop() bool
}

// Ticker is the injectable analogue of [time.Ticker]. Its channel delivers a
// tick on each period; Stop halts further ticks.
type Ticker interface {
	// C returns the ticker's delivery channel, the analogue of the exported
	// C field on [time.Ticker].
	C() <-chan time.Time
	// Stop halts the ticker, mirroring [time.Ticker.Stop]. It does not close
	// the channel.
	Stop()
}

// Real returns the production [Clock] backed by the standard library [time]
// package. The returned value is stateless and safe for concurrent use.
func Real() Clock { return realClock{} }

// realClock is the production [Clock]; every method delegates directly to the
// [time] package so the default path is behaviour-preserving.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (realClock) Until(t time.Time) time.Duration        { return time.Until(t) }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) NewTimer(d time.Duration) Timer   { return realTimer{time.NewTimer(d)} }
func (realClock) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

// realTimer adapts *[time.Timer] to the [Timer] interface.
type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

// realTicker adapts *[time.Ticker] to the [Ticker] interface.
type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }
