//go:build gograph_crashinject

package crashpoint

import (
	"os"
	"syscall"
)

// Breakpoint checks whether GOGRAPH_CRASH_AT equals name; if so, it
// sends SIGKILL to the current process to simulate an abrupt crash at
// this exact execution point.
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
