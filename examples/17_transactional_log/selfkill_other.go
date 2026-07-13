//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package main

// selfkill_other.go supplies no-op stubs of the SIGKILL-dependent primitives on
// platforms without a self-directed SIGKILL (notably Windows). The real-crash
// demonstration degrades to a clean errCrashUnsupported there; the in-process
// crash demonstration in run is unaffected and remains the default.

// crashDemoSupported reports that this platform cannot deliver SIGKILL to
// itself, so the real cross-process crash demonstration is unavailable.
const crashDemoSupported = false

// hardKillSelf cannot model a hard crash without SIGKILL; it reports the demo is
// unsupported so callers degrade gracefully.
func hardKillSelf() error { return errCrashUnsupported }

// interpretChildExit always reports "not killed" on platforms without SIGKILL.
func interpretChildExit(_ error) (bool, string) { return false, "unsupported" }
