//go:build gograph_crashinject

package crashpoint_test

// breakpoint_countdown_test.go — rmp #2302, the GOGRAPH_CRASH_AFTER countdown.
//
// The countdown exists so a breakpoint on a hot durability path can be made to
// fire in the STEADY STATE of a concurrent workload rather than on the process's
// very first commit, where a crash proves nothing because nothing has been
// acknowledged yet (see internal/crashinject/concurrent_writers_test.go).
//
// That makes its off-by-one the whole contract: with GOGRAPH_CRASH_AFTER=n the
// nth call must still RETURN and the (n+1)th must kill. A countdown that fired
// one call early would crash inside the warm-up; one that fired one call late
// would silently shift every scenario's crash point. Both arms are asserted
// here, in a subprocess, because the kill cannot be survived in-process.
//
// Compiled only under the gograph_crashinject build tag: without it Breakpoint
// is the production no-op and reads no environment variable at all.

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
)

// countdownChildEnv switches the test binary into countdown-child mode: it
// calls Breakpoint the requested number of times and exits with the count that
// returned. Handled by TestMain in breakpoint_selfkill_test.go.
const countdownChildEnv = "GOGRAPH_CRASHPOINT_COUNTDOWN_CALLS"

// runCountdownChild re-execs this test binary with the countdown armed at skip
// and asks it to call Breakpoint calls times. It returns whether the child was
// SIGKILL'd and, when it exited voluntarily, how many calls returned.
func runCountdownChild(t *testing.T, skip, calls int) (killed bool, survived int) {
	t.Helper()
	// os.Args[0] is the test binary's own path, not user-supplied input.
	cmd := exec.Command(os.Args[0], "-test.run", "^TestBreakpointSelfKillChildMarker$") //nolint:gosec // G702/G204: os.Args[0] is the test binary itself, not user input
	cmd.Env = append(os.Environ(),
		countdownChildEnv+"="+strconv.Itoa(calls),
		crashpoint.EnvCrashAt+"="+selfKillPoint,
		crashpoint.EnvCrashAfter+"="+strconv.Itoa(skip),
	)

	err := cmd.Run()
	if err == nil {
		return false, 0 // exit 0: zero calls returned, which is itself a result
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child failed but not with an ExitError: %v", err)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("could not obtain WaitStatus from child exit: %v", exitErr)
	}
	if ws.Signaled() {
		if ws.Signal() != syscall.SIGKILL {
			t.Fatalf("child died from %v, want SIGKILL", ws.Signal())
		}
		return true, 0
	}
	if code := ws.ExitStatus(); code == 43 {
		t.Fatalf("child rejected the countdown arguments (skip=%d calls=%d)", skip, calls)
	}
	return false, ws.ExitStatus()
}

// TestBreakpoint_Countdown pins both sides of the off-by-one.
func TestBreakpoint_Countdown(t *testing.T) {
	const skip = 3

	t.Run("the first n hits return", func(t *testing.T) {
		// Exactly n calls: every one must be allowed through, so the child
		// exits voluntarily having survived all of them.
		killed, survived := runCountdownChild(t, skip, skip)
		if killed {
			t.Fatalf("child was SIGKILL'd within the first %d hits — the countdown fires too early, "+
				"which would move every scenario's crash into its warm-up", skip)
		}
		if survived != skip {
			t.Fatalf("child reported %d surviving calls, want %d", survived, skip)
		}
	})

	t.Run("hit n+1 kills", func(t *testing.T) {
		if killed, survived := runCountdownChild(t, skip, skip+1); !killed {
			t.Fatalf("child survived %d hits with GOGRAPH_CRASH_AFTER=%d (%d calls returned) — "+
				"the countdown never drains, so the crash would never happen",
				skip+1, skip, survived)
		}
	})

	t.Run("an unset countdown kills on the first hit", func(t *testing.T) {
		// The default every scenario written before the countdown relies on.
		// Passing skip=0 exercises the same path an absent variable takes.
		if killed, survived := runCountdownChild(t, 0, 1); !killed {
			t.Fatalf("child survived its first hit with no countdown (%d calls returned) — "+
				"every pre-existing crash scenario depends on this firing immediately", survived)
		}
	})
}
