//go:build gograph_crashinject

package crashpoint

import (
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
)

// skipRemaining is the countdown of matching [Breakpoint] hits still to be
// allowed through before the next one self-kills. It is initialised from
// GOGRAPH_CRASH_AFTER at package load and decremented atomically on every
// match, so concurrent writers hitting the same breakpoint consume the
// countdown exactly once each.
//
// Zero (the default, and the value after the countdown drains) means "kill on
// this hit".
var skipRemaining atomic.Int64

func init() { skipRemaining.Store(parseSkip(os.Getenv(EnvCrashAfter))) }

// parseSkip decodes GOGRAPH_CRASH_AFTER. An empty, malformed, or negative
// value means no skipping — the historical behaviour, so every existing
// scenario keeps crashing on its first hit.
func parseSkip(v string) int64 {
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Breakpoint checks whether GOGRAPH_CRASH_AT equals name; if so, it
// sends SIGKILL to the current process to simulate an abrupt crash at
// this exact execution point.
//
// # The countdown
//
// When GOGRAPH_CRASH_AFTER=n is set, the first n matching hits return
// normally and the (n+1)th kills. A breakpoint on a hot durability path — a
// WAL frame append, a group-commit fsync — is reached by the very first
// commit a process makes, and killing there proves nothing: nothing has been
// acknowledged yet, so "every acknowledged transaction survived" holds
// vacuously. The countdown moves the crash past that degenerate window and
// into the steady state where several writers are genuinely in flight.
//
// It makes the crash deterministic in COUNT, not in interleaving: which
// goroutine consumes the nth hit is up to the scheduler, which is why the
// concurrent-writer battery asserts an invariant over the child's own
// acknowledgements rather than one hand-computed graph shape.
//
// Prior art: SQLite's OOM simulator counts down to its injected failure the
// same way — memfault.iCountdown, "Number of pending successes before a
// failure", decremented in faultsimStep() and configured by
// faultsimConfig(nDelay, nRepeat) (sqlite/sqlite @ 1b08739,
// src/test_malloc.c:27,65-71,119-120).
//
// This implementation is compiled only under the gograph_crashinject
// build tag — the deterministic crash-injection battery
// (internal/crashinject and cmd/crashinject-helper). Released binaries
// are built without the tag and instead link the no-op in
// crashpoint_disabled.go, so GOGRAPH_CRASH_AT can never terminate a
// production process.
//
// name must be non-empty; an empty name is silently ignored so that
// callers cannot accidentally crash when the environment variable is
// unset (where os.Getenv returns "").
//
// Breakpoint is safe for concurrent use: it reads an environment
// variable set once at process startup and decrements one atomic counter.
func Breakpoint(name string) {
	if name == "" {
		return // guard: never match the empty env value
	}
	if at := os.Getenv(EnvCrashAt); at != "" && at == name {
		// Consume one unit of the countdown. Add returns the value AFTER the
		// decrement, so a positive result means this hit was one of the n to
		// skip. Draining below zero is harmless — every later hit is also a
		// kill — and cannot wrap in any realistic run.
		if skipRemaining.Add(-1) >= 0 {
			return
		}
		// Self-kill via SIGKILL; cannot be caught or deferred.
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		// Block until the signal is delivered (should be instant).
		select {} //nolint:staticcheck // unreachable after SIGKILL; guards against fallthrough
	}
}
