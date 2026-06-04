//go:build gograph_crashinject

package crashpoint_test

// This file exercises the SIGKILL self-kill path of Breakpoint, which
// exists only under the gograph_crashinject build tag. Without the tag
// Breakpoint is the production no-op (crashpoint_disabled.go), so this
// file — including its TestMain child hook — is excluded from the
// default `go test` build. Run it with: go test -tags gograph_crashinject.

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
)

// crashChildEnv, when set, switches the test binary into "child" mode: it
// calls Breakpoint with the matching name and is expected to be terminated
// by SIGKILL before it can return.
const crashChildEnv = "GOGRAPH_CRASHPOINT_SELFKILL_CHILD"

const selfKillPoint = "test.selfkill.point"

// TestMain intercepts the child-mode invocation. When crashChildEnv is set
// the process enables the crash point and calls Breakpoint, which must
// SIGKILL it; reaching os.Exit(0) would mean the self-kill path did not
// fire and the parent assertion catches that.
func TestMain(m *testing.M) {
	if os.Getenv(crashChildEnv) == "1" {
		// GOGRAPH_CRASH_AT is set by the parent to selfKillPoint, so this
		// call must self-kill via SIGKILL and never return.
		crashpoint.Breakpoint(selfKillPoint)
		// If we get here the breakpoint did not fire: signal that to the
		// parent with a distinctive non-killed exit code.
		os.Exit(42)
	}
	os.Exit(m.Run())
}

// TestBreakpoint_SelfKill exercises the matching-name self-kill path of
// Breakpoint (the SIGKILL branch and the trailing blocking select) in a
// subprocess, since the kill cannot be survived in-process. This is the
// one branch the in-process no-op test cannot reach.
func TestBreakpoint_SelfKill(t *testing.T) {
	// Re-exec this test binary, running only the child marker test, with
	// the crash environment armed so the child's Breakpoint self-kills.
	// os.Args[0] is the test binary's own path, not user-supplied input.
	cmd := exec.Command(os.Args[0], "-test.run", "^TestBreakpointSelfKillChildMarker$") //nolint:gosec // G702/G204: os.Args[0] is the test binary itself, not user input
	cmd.Env = append(os.Environ(),
		crashChildEnv+"=1",
		crashpoint.EnvCrashAt+"="+selfKillPoint,
	)

	err := cmd.Run()
	if err == nil {
		t.Fatal("child exited cleanly; Breakpoint did not self-kill on a matching name")
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child failed but not with an ExitError: %v", err)
	}

	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("could not obtain WaitStatus from child exit: %v", exitErr)
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("child did not die from SIGKILL: signaled=%v signal=%v exit=%d",
			ws.Signaled(), ws.Signal(), ws.ExitStatus())
	}
}

// TestBreakpointSelfKillChildMarker exists only so the child re-exec has a
// concrete test name to target with -test.run. Its body never executes:
// in child mode TestMain calls Breakpoint (which self-kills) before m.Run
// is reached. In the normal parent run -test.run does not select it.
func TestBreakpointSelfKillChildMarker(_ *testing.T) {}
