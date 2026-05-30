package crashpoint_test

import (
	"os"
	"testing"

	"gograph/internal/crashpoint"
)

// TestBreakpoint_NoOp verifies that Breakpoint is a no-op when
// GOGRAPH_CRASH_AT is unset or does not match the supplied name. This
// is the contract production code (store/wal, store/checkpoint) relies
// on: an inline Breakpoint call must never kill the process outside the
// crash-injection harness.
//
// The matching-name self-kill path cannot be exercised in-process (it
// SIGKILLs the test binary); it is covered end-to-end by the
// subprocess crash-injection tests in store/recovery.
func TestBreakpoint_NoOp(t *testing.T) {
	prev, had := os.LookupEnv(crashpoint.EnvCrashAt)
	if err := os.Unsetenv(crashpoint.EnvCrashAt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(crashpoint.EnvCrashAt, prev)
		}
	})

	// Env unset: must not kill.
	crashpoint.Breakpoint("checkpoint.mid-truncate")

	// Env set to a non-matching value: must not kill.
	if err := os.Setenv(crashpoint.EnvCrashAt, "some.other.point"); err != nil {
		t.Fatal(err)
	}
	crashpoint.Breakpoint("checkpoint.mid-truncate")

	// Empty name must always be a no-op (never matches the env value).
	if err := os.Setenv(crashpoint.EnvCrashAt, ""); err != nil {
		t.Fatal(err)
	}
	crashpoint.Breakpoint("")

	// Reached only if no SIGKILL fired.
}

// TestEnvConstants pins the environment-variable names so a rename is a
// conscious, reviewed change (the helper binary and the harness both
// depend on these exact strings).
func TestEnvConstants(t *testing.T) {
	if crashpoint.EnvCrashAt != "GOGRAPH_CRASH_AT" {
		t.Errorf("EnvCrashAt = %q, want GOGRAPH_CRASH_AT", crashpoint.EnvCrashAt)
	}
	if crashpoint.EnvCrashDir != "GOGRAPH_CRASH_DIR" {
		t.Errorf("EnvCrashDir = %q, want GOGRAPH_CRASH_DIR", crashpoint.EnvCrashDir)
	}
}
