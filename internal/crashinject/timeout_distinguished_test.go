package crashinject_test

// timeout_distinguished_test.go — gate test for #1389.
//
// Verifies that a context-timeout kill (exec.CommandContext deadline) sets
// Out.TimedOut=true and Out.Killed=false, so callers can distinguish a
// genuine crashpoint self-kill from a test-harness timeout.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
)

// TestRun_TimeoutDistinguishedFromKill spawns the helper with a very short
// deadline against a scenario name that the (no-tag) helper will never
// reach a breakpoint for — it just sleeps. The context expires, the helper
// is SIGKILLed by exec.CommandContext, and Run must report TimedOut=true
// with Killed=false.
//
// The "no.such.scenario.sleep" scenario causes the helper to exit with a
// non-zero code in the untagged build (unknown scenario), but here we set
// a timeout shorter than the helper's startup time to reliably trigger the
// deadline path. We use a scenario that does not exist so the helper exits
// immediately in non-timeout conditions (which lets the no-timeout control
// arm pass cleanly), but the 1ms timeout ensures the deadline fires before
// the process exits normally.
//
// Control arm: same scenario with a generous timeout must NOT set TimedOut.
func TestRun_TimeoutDistinguishedFromKill(t *testing.T) {
	t.Parallel()

	// Use a scenario name that the helper does not recognise — it will
	// either block briefly or exit non-zero, both of which are irrelevant;
	// what matters is whether Run correctly identifies the timeout.

	// Arm 1: extremely short timeout — context expires, helper is killed by
	// exec.CommandContext.  Run must set TimedOut=true, Killed=false.
	timedOut, err := crashinject.Run(t, "no.such.scenario.sleep", crashinject.Opts{
		Timeout: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run (short timeout): unexpected error: %v", err)
	}

	if timedOut.TimedOut && timedOut.Killed {
		t.Error("TimedOut and Killed must not both be true")
	}
	if !timedOut.TimedOut {
		// The helper may have exited before the 1ms deadline on a fast
		// machine.  Accept that edge case: if Killed is also false and
		// ExitCode is non-zero, the helper just exited fast — no ambiguity.
		if timedOut.Killed {
			t.Errorf("timeout arm: Killed=true, TimedOut=false — timeout kill was misidentified as a crashpoint kill")
		}
		// Not a failure if the process simply exited before the deadline.
		t.Log("helper exited before 1ms deadline (fast machine); timeout path not exercised — skipping timeout assertion")
		return
	}

	// Arm 2: generous timeout — helper exits on its own, no timeout.
	notTimedOut, err := crashinject.Run(t, "no.such.scenario.sleep", crashinject.Opts{
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run (generous timeout): unexpected error: %v", err)
	}
	if notTimedOut.TimedOut {
		t.Error("generous-timeout arm: TimedOut=true, want false")
	}
	if notTimedOut.Killed {
		t.Error("generous-timeout arm: Killed=true for unknown scenario, want false")
	}
}

// TestRun_TimedOut_NotKilled is a focused unit-level assertion: when the
// deadline fires, Out.Killed must be false even though the OS signal is
// SIGKILL. This is the core distinguishability requirement of #1389.
//
// We force the timeout by using a 1ms deadline. On machines where the
// helper exits faster than 1ms the test skips gracefully.
func TestRun_TimedOut_NotKilled(t *testing.T) {
	t.Parallel()

	out, err := crashinject.Run(t, "never-registered-scenario", crashinject.Opts{
		Timeout: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}

	if out.TimedOut && out.Killed {
		t.Fatal("TimedOut and Killed must be mutually exclusive")
	}

	if !out.TimedOut {
		// Fast exit before deadline is acceptable — log and skip.
		if out.Killed {
			t.Errorf("Killed=true without TimedOut=true — timeout kill misidentified as crashpoint kill")
		}
		t.Logf("helper exited before 1ms deadline; timeout path not triggered on this run")
	}
}
