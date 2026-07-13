//go:build linux || darwin || freebsd || netbsd || openbsd

package main

// selfkill_unix.go supplies the two SIGKILL-dependent primitives the real-crash
// demonstration needs on POSIX targets: hardKillSelf (raise SIGKILL on the
// current process) and interpretChildExit (decode a child's POSIX wait status).
// The build-tagged pair keeps crash.go portable.

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// crashDemoSupported reports that this platform can deliver SIGKILL to itself,
// so the real cross-process crash demonstration can run.
const crashDemoSupported = true

// hardKillSelf hard-kills the current process with SIGKILL, modelling an abrupt
// crash (kill -9) that no defer, buffer flush, or graceful shutdown can
// intercept. It never returns on this platform; the error return exists only so
// the no-op stub on unsupported platforms can share the signature.
func hardKillSelf() error {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} //nolint:staticcheck // unreachable after SIGKILL; blocks until the signal is delivered
}

// interpretChildExit reports whether runErr (the result of exec.Cmd.Run on the
// crash child) indicates the child was terminated by SIGKILL, plus a short
// human-readable description of how it terminated.
func interpretChildExit(runErr error) (bool, string) {
	if runErr == nil {
		return false, "exit-0"
	}
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		return false, runErr.Error()
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		return false, ee.Error()
	}
	if ws.Signaled() {
		return ws.Signal() == syscall.SIGKILL, "signal:" + ws.Signal().String()
	}
	return false, "exit-" + strconv.Itoa(ws.ExitStatus())
}
