// Package crashpoint holds the production-callable half of the
// crash-injection machinery: the [Breakpoint] hook and the environment
// variables that drive it. It deliberately depends on nothing beyond
// os and syscall so that production packages (for example store/wal and
// store/checkpoint) can embed crash-injection points without dragging
// the testing package — and the test-only subprocess runner — into
// their production binaries.
//
// The subprocess harness that spawns a child, sets these environment
// variables, and inspects the torn artefacts lives in the sibling
// package internal/crashinject; it re-exports the names defined here so
// existing call sites keep working.
package crashpoint

import (
	"os"
	"syscall"
)

// EnvCrashAt is the environment variable read by [Breakpoint] to decide
// which named point should trigger a crash.
const EnvCrashAt = "GOGRAPH_CRASH_AT"

// EnvCrashDir is the environment variable that tells the helper binary
// where to place its artefacts (WAL files, temp data).
const EnvCrashDir = "GOGRAPH_CRASH_DIR"

// Breakpoint checks whether GOGRAPH_CRASH_AT equals name; if so, it
// sends SIGKILL to the current process to simulate an abrupt crash at
// this exact execution point.
//
// name must be non-empty; an empty name is silently ignored so that
// callers cannot accidentally crash when the environment variable is
// unset (where os.Getenv returns "").
//
// In production (GOGRAPH_CRASH_AT unset or empty) this function is a
// no-op with no measurable overhead (one string comparison).
//
// Breakpoint is safe for concurrent use: it reads an environment
// variable set once at process startup and takes no locks.
func Breakpoint(name string) {
	if name == "" {
		return // guard: never match the empty env value
	}
	if at := os.Getenv(EnvCrashAt); at != "" && at == name {
		// Self-kill via SIGKILL; cannot be caught or deferred.
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		// Block until the signal is delivered (should be instant).
		select {} //nolint:staticcheck // unreachable after SIGKILL; guards against fallthrough
	}
}
