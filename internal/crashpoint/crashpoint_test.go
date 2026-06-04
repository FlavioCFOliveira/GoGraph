package crashpoint_test

import (
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
)

// TestBreakpoint_NoOp verifies that Breakpoint does not kill the process
// when GOGRAPH_CRASH_AT is unset or does not match the supplied name.
// This holds in BOTH build modes (the enabled hook only fires on an
// exact match), so this test is build-tag agnostic.
//
// The untagged build additionally guarantees that even an EXACTLY
// matching value is inert; that stronger, tag-specific assertion lives
// in breakpoint_disabled_inert_test.go so it never runs under
// -tags gograph_crashinject (where a match would SIGKILL the binary).
// The matching-name self-kill path is covered end-to-end by the
// subprocess tests in breakpoint_selfkill_test.go (tagged) and the
// store/recovery crash-injection suite.
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
